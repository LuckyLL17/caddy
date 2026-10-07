// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyhttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"sync"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(HandleHandler{})
}

// HandleHandler executes the first matching branch, like the mutually
// exclusive handle blocks produced by the Caddyfile. An optional fallback
// plan allows further matching branches to be tried deterministically
// when an attempt produces a configured status code or error. Without a
// fallback plan, behavior is identical to ordinary mutually exclusive
// handle blocks.
//
// When a fallback plan is configured, every attempt is isolated until
// it is committed: its response is buffered in a private header map,
// request mutations and variables are rolled back, and the request body
// is replayed when possible. Responses that cannot be taken back once
// sent (1xx responses, hijacks, and responses exceeding the configured
// buffer limit) commit the attempt immediately and prevent further
// fallbacks.
type HandleHandler struct {
	// The ordered list of candidate branches. The first branch that
	// matches the request is attempted first; on a fallible outcome,
	// subsequent still-matching branches are tried in array order.
	Branches []HandleBranch `json:"branches,omitempty"`

	// The optional fallback plan governing when and how often
	// subsequent branches are tried, and how the final response
	// is handled once candidates are exhausted.
	Fallback *FallbackPlan `json:"fallback,omitempty"`

	logger *zap.Logger
}

// HandleBranch is one candidate in a handle handler: a set of routes
// guarded by request matchers.
type HandleBranch struct {
	// The matcher sets which qualify a request for this branch.
	// Multiple matcher sets are OR'ed; matchers inside a set are
	// AND'ed. A branch without matchers matches every request.
	MatcherSetsRaw RawMatcherSets `json:"match,omitempty" caddy:"namespace=http.matchers"`

	// The routes executed when the branch is attempted. They are
	// compiled with a no-op tail, so a branch that produces no
	// response does not flow into sibling branches.
	Routes RouteList `json:"routes,omitempty"`

	// decoded values
	MatcherSets MatcherSets `json:"-"`
}

// Provision loads the branch matchers and provisions its routes.
func (b *HandleBranch) Provision(ctx caddy.Context) error {
	matchersIface, err := ctx.LoadModule(b, "MatcherSetsRaw")
	if err != nil {
		return fmt.Errorf("loading branch matcher modules: %v", err)
	}
	if err = b.MatcherSets.FromInterface(matchersIface); err != nil {
		return err
	}
	return b.Routes.Provision(ctx)
}

// FallbackPlan describes when matched branches may be retried with the
// next candidate, and how the request/response are buffered to allow
// that safely.
type FallbackPlan struct {
	// The maximum number of branches that may be attempted, including
	// the first attempt. If 0, every matching branch can be tried.
	MaxAttempts int `json:"max_attempts,omitempty"`

	// Response status codes (exact codes, or one-digit classes such
	// as 5 for all 5xx codes) that, when produced by an attempt,
	// cause the next branch to be tried.
	StatusCodes []int `json:"status_codes,omitempty"`

	// Additional response-level criteria (status and headers) that
	// trigger a fallback. The configured matchers are OR'ed with
	// each other and with StatusCodes.
	ResponseMatchers []*ResponseMatcher `json:"response_matchers,omitempty"`

	// Status codes associated with a returned HandlerError (exact
	// codes, or one-digit classes) that trigger a fallback.
	ErrorStatusCodes []int `json:"error_status_codes,omitempty"`

	// Request matcher sets evaluated against the request after the
	// attempt's error has been placed in its context, so the
	// {http.error.*} placeholders are available. Any match triggers
	// a fallback; OR'ed with ErrorStatusCodes.
	ErrorMatcherSetsRaw RawMatcherSets `json:"error_match,omitempty" caddy:"namespace=http.matchers"`

	// What to do when no further branch can be tried:
	//
	//   - "commit" (default): write the last buffered response if the
	//     last attempt produced one; otherwise return its error.
	//   - "error": discard every buffered response and return the last
	//     error; if the last attempt failed only with a trigger status,
	//     a HandlerError carrying that status is returned so error
	//     routes can handle it.
	OnExhausted string `json:"on_exhausted,omitempty"`

	// Maximum request body size, in bytes, buffered before the first
	// attempt so it can be replayed for subsequent attempts. 0
	// disables buffering (the default, like reverse_proxy retries);
	// -1 buffers without a limit. A body that is not buffered and has
	// been consumed cannot be replayed, which ends the fallback.
	RequestBodyBuffer int64 `json:"request_body_buffer,omitempty"`

	// Maximum response body size, in bytes, buffered per attempt. 0
	// (the default) buffers without a limit. If exceeded, the attempt
	// is committed immediately by streaming the buffered prefix and
	// all subsequent bytes straight to the client.
	ResponseBodyBuffer int64 `json:"response_body_buffer,omitempty"`

	// decoded values
	ErrorMatcherSets MatcherSets `json:"-"`
}

// CaddyModule returns the Caddy module information.
func (HandleHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.handle",
		New: func() caddy.Module { return new(HandleHandler) },
	}
}

// Provision sets up the branches and fallback matchers.
func (h *HandleHandler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()

	for i := range h.Branches {
		if err := h.Branches[i].Provision(ctx); err != nil {
			return fmt.Errorf("provisioning branch %d: %v", i, err)
		}
	}

	if h.Fallback == nil {
		return nil
	}

	if h.Fallback.ErrorMatcherSetsRaw != nil {
		matchersIface, err := ctx.LoadModule(h.Fallback, "ErrorMatcherSetsRaw")
		if err != nil {
			return fmt.Errorf("loading fallback error matchers: %v", err)
		}
		if err = h.Fallback.ErrorMatcherSets.FromInterface(matchersIface); err != nil {
			return err
		}
	}

	return nil
}

// Validate ensures the fallback plan is usable.
func (h *HandleHandler) Validate() error {
	if len(h.Branches) == 0 {
		return errors.New("handle requires at least one branch")
	}
	for i := range h.Branches {
		if len(h.Branches[i].Routes) == 0 {
			return fmt.Errorf("branch %d has no routes", i)
		}
	}

	if h.Fallback == nil {
		return nil
	}

	p := h.Fallback
	if p.MaxAttempts < 0 {
		return errors.New("max_attempts must be at least 0")
	}
	switch p.OnExhausted {
	case "", onExhaustedCommit, onExhaustedError:
	default:
		return fmt.Errorf("unrecognized on_exhausted policy '%s'; must be %q or %q", p.OnExhausted, onExhaustedCommit, onExhaustedError)
	}
	if p.RequestBodyBuffer < -1 {
		return errors.New("request_body_buffer must be -1, 0, or a positive byte count")
	}
	if p.ResponseBodyBuffer < 0 {
		return errors.New("response_body_buffer must be 0 or a positive byte count")
	}
	return nil
}

const (
	onExhaustedCommit = "commit"
	onExhaustedError  = "error"
)

// ServeHTTP runs the first matching branch, retrying later branches per
// the fallback plan when configured.
func (h *HandleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next Handler) error {
	if h.Fallback == nil {
		return h.serveExclusive(w, r, next)
	}
	return h.serveWithFallback(w, r, next)
}

// serveExclusive reproduces mutually exclusive handle block semantics:
// the first matching branch runs with next as its tail; if none match,
// next is invoked.
func (h *HandleHandler) serveExclusive(w http.ResponseWriter, r *http.Request, next Handler) error {
	for i := range h.Branches {
		match, err := h.Branches[i].MatcherSets.AnyMatchWithError(r)
		if err != nil {
			return err
		}
		if match {
			return h.Branches[i].Routes.Compile(next).ServeHTTP(w, r)
		}
	}
	return next.ServeHTTP(w, r)
}

// attemptOutcome categorizes the result of trying one branch.
type attemptOutcome int

const (
	outcomeTriggerResponse attemptOutcome = iota // a response matching the response triggers
	outcomeTriggerError                          // an error matching the error triggers
	outcomeUnhandled                             // the branch matched but wrote nothing
)

func (h *HandleHandler) serveWithFallback(w http.ResponseWriter, r *http.Request, next Handler) error {
	plan := h.Fallback

	repl, _ := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	replSnap := repl.Snapshot()

	// every attempt restores the request from this snapshot
	snap := snapshotRequest(r)

	body, err := newAttemptBody(r, plan.RequestBodyBuffer)
	if err != nil {
		return err
	}
	defer body.close()

	maxAttempts := plan.MaxAttempts
	if maxAttempts == 0 || maxAttempts > len(h.Branches) {
		maxAttempts = len(h.Branches)
	}

	var (
		attempts     int
		cursor       int
		lastOutcome  attemptOutcome
		lastErr      error
		lastStatus   int
		lastRecorder *fallbackResponseWriter

		// resources of the most recent fallible attempt; retained
		// until a newer attempt discards them or the final response
		// is committed
		pendingBuf        *bytes.Buffer
		pendingBodyReader io.ReadCloser
	)

	// release resources retained by a discarded fallible attempt
	recyclePending := func() {
		if pendingBuf != nil {
			pendingBuf.Reset()
			fallbackBufferPool.Put(pendingBuf)
			pendingBuf = nil
		}
		if pendingBodyReader != nil && body.kind != attemptBodyStream {
			pendingBodyReader.Close()
		}
		pendingBodyReader = nil
	}
	defer recyclePending()

	for attempts < maxAttempts {
		recyclePending()

		// each attempt gets a restored request, an isolated vars map
		// and a fresh route group map
		ar := h.attemptRequest(r, snap)

		bodyReader, bodyErr := body.reader()
		if bodyErr != nil {
			if errors.Is(bodyErr, errBodyNotReplayable) {
				// the last fallible attempt consumed the body, so the
				// plan has to be finalized without another attempt
				break
			}
			repl.Restore(replSnap)
			return bodyErr
		}
		ar.Body = bodyReader

		// find the next matching candidate from the cursor
		idx := -1
		for i := cursor; i < len(h.Branches); i++ {
			match, matchErr := h.Branches[i].MatcherSets.AnyMatchWithError(ar)
			if matchErr != nil {
				repl.Restore(replSnap)
				return matchErr
			}
			if match {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		cursor = idx + 1
		attempts++

		buf := fallbackBufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		rec := newFallbackResponseWriter(w, buf, plan.ResponseBodyBuffer)

		if c := h.logger.Check(zap.DebugLevel, "trying handle branch"); c != nil {
			c.Write(zap.Int("branch", idx), zap.Int("attempt", attempts))
		}

		attemptErr := h.Branches[idx].Routes.Compile(emptyHandler).ServeHTTP(rec, ar)
		lastErr = attemptErr
		lastStatus = rec.Status()
		lastRecorder = rec

		// a canceled client cannot be served by another branch
		if attemptErr != nil && errors.Is(attemptErr, context.Canceled) {
			buf.Reset()
			fallbackBufferPool.Put(buf)
			if bodyReader != nil && body.kind != attemptBodyStream {
				bodyReader.Close()
			}
			repl.Restore(replSnap)
			return attemptErr
		}

		// hijacks, 1xx responses and buffer spills are already on the
		// wire, so no further fallback is possible
		if rec.Committed() {
			buf.Reset()
			fallbackBufferPool.Put(buf)
			if bodyReader != nil && body.kind != attemptBodyStream {
				bodyReader.Close()
			}
			return attemptErr
		}

		if attemptErr != nil {
			matched, matchErr := plan.errorTriggers(ar, attemptErr)
			if matchErr != nil {
				buf.Reset()
				fallbackBufferPool.Put(buf)
				repl.Restore(replSnap)
				return matchErr
			}
			if matched {
				lastOutcome = outcomeTriggerError
				pendingBuf = buf
				pendingBodyReader = bodyReader
				h.logFallback(attemptErr, rec.Status(), attempts)
				repl.Restore(replSnap)
				continue
			}
			// an error outside the fallback plan follows normal error
			// handling, as if this handler did not exist
			buf.Reset()
			fallbackBufferPool.Put(buf)
			if bodyReader != nil && body.kind != attemptBodyStream {
				bodyReader.Close()
			}
			repl.Restore(replSnap)
			return attemptErr
		}

		if !rec.Wrote() {
			// the branch deferred without producing a response; try
			// the next candidate, like a route that flowed into next
			lastOutcome = outcomeUnhandled
			pendingBuf = buf
			pendingBodyReader = bodyReader
			repl.Restore(replSnap)
			continue
		}

		if plan.responseTriggers(rec.Status(), rec.Header()) {
			lastOutcome = outcomeTriggerResponse
			pendingBuf = buf
			pendingBodyReader = bodyReader
			h.logFallback(nil, rec.Status(), attempts)
			repl.Restore(replSnap)
			continue
		}

		// the attempt succeeded; replay it and keep the request,
		// variables and placeholders as the winning attempt left them
		commitErr := rec.commit()
		buf.Reset()
		fallbackBufferPool.Put(buf)
		if bodyReader != nil && body.kind != attemptBodyStream {
			bodyReader.Close()
		}
		return commitErr
	}

	// no branch ever matched, or the last branch deferred: fall through
	// to the outer handler just like ordinary handle blocks
	if attempts == 0 || lastOutcome == outcomeUnhandled {
		repl.Restore(replSnap)
		h.attachReplayableBody(r, body)
		return next.ServeHTTP(w, r)
	}

	// candidates (or attempts) were exhausted on fallible outcomes
	switch plan.OnExhausted {
	case onExhaustedError:
		repl.Restore(replSnap)
		if lastOutcome == outcomeTriggerError {
			return lastErr
		}
		return Error(lastStatus, errFallbackExhausted)

	default: // commit
		if lastOutcome == outcomeTriggerError {
			repl.Restore(replSnap)
			return lastErr
		}
		// replay the last buffered fallible response, keeping its state
		if lastRecorder != nil && lastRecorder.Wrote() {
			return lastRecorder.commit()
		}
		repl.Restore(replSnap)
		h.attachReplayableBody(r, body)
		return next.ServeHTTP(w, r)
	}
}

// attachReplayableBody restores a previously buffered request body onto
// r so it remains available when the request falls through to next
// without being handled.
func (h *HandleHandler) attachReplayableBody(r *http.Request, body *attemptBody) {
	if body.kind == attemptBodyBuffered {
		r.Body = io.NopCloser(bytes.NewReader(body.buf.Bytes()))
	}
}

func (h *HandleHandler) logFallback(err error, status, attempt int) {
	if c := h.logger.Check(zap.DebugLevel, "falling back to next handle branch"); c != nil {
		c.Write(zap.Int("attempt", attempt), zap.Int("status", status), zap.Error(err))
	}
}

// attemptRequest derives a per-attempt request from r with the restored
// request fields, an isolated copy of the variables table and a fresh
// route group table.
func (h *HandleHandler) attemptRequest(r *http.Request, snap requestSnapshot) *http.Request {
	ctx := r.Context()

	if vars, ok := ctx.Value(VarsCtxKey).(map[string]any); ok {
		newVars := make(map[string]any, len(vars))
		maps.Copy(newVars, vars)
		ctx = context.WithValue(ctx, VarsCtxKey, newVars)
	}
	ctx = context.WithValue(ctx, routeGroupCtxKey, make(map[string]struct{}))

	ar := r.WithContext(ctx)
	snap.apply(ar)
	return ar
}

// responseTriggers reports whether a produced status and header set
// match any of the fallback response criteria.
func (p *FallbackPlan) responseTriggers(status int, header http.Header) bool {
	for _, code := range p.StatusCodes {
		if StatusCodeMatches(status, code) {
			return true
		}
	}
	for _, rm := range p.ResponseMatchers {
		if rm.Match(status, header) {
			return true
		}
	}
	return false
}

// errorTriggers reports whether err matches any fallback error
// criterion. Error matcher sets see the attempt's request with the
// error placed in its context and the {http.error.*} placeholders set.
func (p *FallbackPlan) errorTriggers(r *http.Request, err error) (bool, error) {
	status := 0
	if handlerErr, ok := err.(HandlerError); ok {
		status = handlerErr.StatusCode
	}
	for _, code := range p.ErrorStatusCodes {
		if StatusCodeMatches(status, code) {
			return true, nil
		}
	}
	if len(p.ErrorMatcherSets) == 0 {
		return false, nil
	}
	errReq := (&HTTPErrorConfig{}).WithError(r, err)
	return p.ErrorMatcherSets.AnyMatchWithError(errReq)
}

// requestSnapshot captures the request fields that handlers may mutate
// so they can be restored identically for every attempt.
type requestSnapshot struct {
	method        string
	host          string
	requestURI    string
	remoteAddr    string
	contentLength int64
	url           *url.URL
	header        http.Header
}

func snapshotRequest(r *http.Request) requestSnapshot {
	u := new(url.URL)
	cloneURL(r.URL, u)
	return requestSnapshot{
		method:        r.Method,
		host:          r.Host,
		requestURI:    r.RequestURI,
		remoteAddr:    r.RemoteAddr,
		contentLength: r.ContentLength,
		url:           u,
		header:        r.Header.Clone(),
	}
}

// apply restores the snapshot onto r.
func (s requestSnapshot) apply(r *http.Request) {
	r.Method = s.method
	r.Host = s.host
	r.RequestURI = s.requestURI
	r.RemoteAddr = s.remoteAddr
	r.ContentLength = s.contentLength
	u := new(url.URL)
	cloneURL(s.url, u)
	r.URL = u
	r.Header = s.header.Clone()
}

// errFallbackExhausted is returned with the last status code when
// on_exhausted is "error" and attempts ended on a fallible response.
var errFallbackExhausted = errors.New("all fallback branches returned a fallible response")

// fallbackBufferPool reuses the per-attempt response body buffers.
var fallbackBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// attempt body strategies
const (
	attemptBodyNone = iota
	attemptBodyGetBody
	attemptBodyBuffered
	attemptBodyStream
)

// attemptBody provides a fresh body reader for every attempt, or
// errBodyNotReplayable when a consumed unbuffered body cannot rewind.
type attemptBody struct {
	kind int

	getBody func() (io.ReadCloser, error) // attemptBodyGetBody
	buf     *bytes.Buffer                 // attemptBodyBuffered

	stream io.ReadCloser // attemptBodyStream: shared reader
	read   func() int64  // bytes consumed from the shared reader
}

// errBodyNotReplayable indicates that an attempt failed after the
// request body had been consumed without a replay buffer configured.
var errBodyNotReplayable = errors.New("request body was consumed and cannot be replayed; configure request_body_buffer to buffer it")

func newAttemptBody(r *http.Request, limit int64) (*attemptBody, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return &attemptBody{kind: attemptBodyNone}, nil
	}

	if r.GetBody != nil {
		return &attemptBody{kind: attemptBodyGetBody, getBody: r.GetBody}, nil
	}

	if limit != 0 {
		buf := fallbackBufferPool.Get().(*bytes.Buffer)
		buf.Reset()

		if limit < 0 {
			_, _ = io.Copy(buf, r.Body)
			_ = r.Body.Close()
			return &attemptBody{kind: attemptBodyBuffered, buf: buf}, nil
		}

		// read at most limit+1 bytes so we can tell whether the
		// entire body fit within the limit
		n, err := io.CopyN(buf, r.Body, limit+1)
		if n <= limit && isBodyEOF(err) {
			_ = r.Body.Close()
			return &attemptBody{kind: attemptBodyBuffered, buf: buf}, nil
		}
		if err != nil && err != io.EOF && !isBodyEOF(err) {
			buf.Reset()
			fallbackBufferPool.Put(buf)
			return nil, err
		}

		// body exceeds the limit: the buffered prefix is spliced in
		// front of the original body and the combined reader becomes
		// the request body, so the server closes it (returning the
		// pooled buffer) when the request ends
		counter := &countingReadCloser{
			ReadCloser: &pooledReadCloser{
				Reader:   io.MultiReader(buf, r.Body),
				origBody: r.Body,
				poolBuf:  buf,
			},
		}
		r.Body = counter
		return abStream(counter), nil
	}

	counter := &countingReadCloser{ReadCloser: r.Body}
	r.Body = counter
	return abStream(counter), nil
}

// abStream builds an attempt body strategy around a shared counting
// reader that only permits replays while zero bytes were consumed.
func abStream(counter *countingReadCloser) *attemptBody {
	return &attemptBody{
		kind:   attemptBodyStream,
		stream: counter,
		read:   func() int64 { return counter.n },
	}
}

// reader returns the body to hand to the next attempt.
func (ab *attemptBody) reader() (io.ReadCloser, error) {
	switch ab.kind {
	case attemptBodyNone:
		return nil, nil
	case attemptBodyGetBody:
		return ab.getBody()
	case attemptBodyBuffered:
		return io.NopCloser(bytes.NewReader(ab.buf.Bytes())), nil
	case attemptBodyStream:
		if ab.read() > 0 {
			return nil, errBodyNotReplayable
		}
		return ab.stream, nil
	default:
		panic("unknown attempt body kind")
	}
}

// close releases resources held for body replay. The original request
// body (and any spliced-in pooled prefix) is owned by the server when
// the strategy is streaming, so only a fully buffered body is released
// here.
func (ab *attemptBody) close() {
	if ab.kind == attemptBodyBuffered && ab.buf != nil {
		ab.buf.Reset()
		fallbackBufferPool.Put(ab.buf)
		ab.buf = nil
	}
}

// pooledReadCloser splices a buffered request body prefix in front of
// the original request body, closing the original body and returning
// the prefix buffer to the pool once the server closes the request.
type pooledReadCloser struct {
	io.Reader
	origBody io.ReadCloser
	poolBuf  *bytes.Buffer
	once     sync.Once
}

func (p *pooledReadCloser) Close() error {
	p.once.Do(func() {
		_ = p.origBody.Close()
		p.poolBuf.Reset()
		fallbackBufferPool.Put(p.poolBuf)
	})
	return nil
}

// isBodyEOF reports whether err is an io.EOF possibly wrapped by the
// server's request body implementation.
func isBodyEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// countingReadCloser tracks how many bytes of a shared request body
// have been consumed.
type countingReadCloser struct {
	io.ReadCloser
	n int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// Interface guards
var (
	_ caddy.Provisioner = (*HandleHandler)(nil)
	_ caddy.Validator   = (*HandleHandler)(nil)
	_ MiddlewareHandler = (*HandleHandler)(nil)
)
