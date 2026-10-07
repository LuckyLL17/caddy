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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

// BodyReplayScope identifies a part of the handler chain at which a
// spooled request body may be replayed (read a second time).
type BodyReplayScope uint8

const (
	// BodyReplayScopeNestedRoutes covers handlers, subroutes and invoked
	// named routes executing inside the configuring handler's subtree.
	// Replay inside the subtree is always enabled when a policy is.
	BodyReplayScopeNestedRoutes BodyReplayScope = 1 << iota

	// BodyReplayScopeErrorRoutes covers the server-level error handler
	// chain as well as error routes attached to a subroute.
	BodyReplayScopeErrorRoutes

	// BodyReplayScopeRetries covers retry candidates, such as another
	// reverse_proxy upstream attempt after the first attempt consumed
	// the body without producing a response.
	BodyReplayScopeRetries
)

// Names of the replay scopes, used by JSON config and the Caddyfile.
const (
	BodyReplayScopeNestedRoutesName = "nested_routes"
	BodyReplayScopeErrorRoutesName  = "error_routes"
	BodyReplayScopeRetriesName      = "retries"
)

// ParseBodyReplayScope parses a scope name into a BodyReplayScope.
func ParseBodyReplayScope(name string) (BodyReplayScope, bool) {
	switch name {
	case BodyReplayScopeNestedRoutesName:
		return BodyReplayScopeNestedRoutes, true
	case BodyReplayScopeErrorRoutesName:
		return BodyReplayScopeErrorRoutes, true
	case BodyReplayScopeRetriesName:
		return BodyReplayScopeRetries, true
	}
	return 0, false
}

const (
	bodyReplayOnExceedReject  = "reject"
	bodyReplayOnExceedDegrade = "degrade"

	bodyReplayOnCancelDelete = "delete"
	bodyReplayOnCancelRetain = "retain"

	// defaultBodyReplayMemoryMaxSize is the default amount of a
	// request body that is kept in memory before spilling to disk.
	defaultBodyReplayMemoryMaxSize int64 = 1 << 20 // 1 MiB

	// bodyReplayVarKey stores the per-request *bodyReplay state in
	// the request's variable table; the map is shared by every clone
	// of the request, including the one seen by error routes.
	bodyReplayVarKey = "body_replay.state"

	bodyReplaySpillPrefix = "caddy-body-"
)

var (
	// ErrBodyReplayIncomplete is returned by a replay reader when it
	// catches up to the portion of the body that has actually been
	// consumed from the client before the source reached EOF. It is a
	// deterministic replacement for a silently truncated body: the
	// caller has received every byte that was available, and the
	// remainder was never read.
	ErrBodyReplayIncomplete = errors.New("request body replay: source body was not read to completion")

	// ErrBodyReplayUnavailable is returned when a body larger than
	// the replay limit was streamed in degrade mode: subsequent
	// handlers still received the full body, but no replay copy exists.
	ErrBodyReplayUnavailable = errors.New("request body replay: unavailable because the body exceeded the configured limit")
)

// BodyReplayConfig is an optional policy for spooling an HTTP request
// body so that it can be read more than once within bounded memory and
// disk usage. Without a configured policy, request bodies keep their
// default one-shot streaming behavior.
//
// Bytes are stored in memory up to MemoryMaxSize; anything beyond that
// is spilled to a temporary file in SpillDir, up to MaxSize bytes in
// total. The spool is created lazily on the first read and exists only
// for the lifetime of the request; there is no global cache.
type BodyReplayConfig struct {
	// MaxSize is the maximum number of bytes that may be spooled for
	// replay. If zero, it defaults to MemoryMaxSize, which disables
	// disk spilling entirely.
	MaxSize int64 `json:"max_size,omitempty"`

	// MemoryMaxSize is the maximum number of spooled bytes kept in
	// memory before the remainder spills to disk. Default: 1 MiB.
	MemoryMaxSize int64 `json:"memory_max_size,omitempty"`

	// SpillDir is the directory in which overflow temp files are
	// created. If empty, the OS temporary directory is used. The
	// directory is created on demand; temp files are removed when the
	// request ends or is canceled unless OnCancel is "retain".
	SpillDir string `json:"spill_dir,omitempty"`

	// Allow lists the handler-chain scopes in which the spooled body
	// may be replayed. "nested_routes" (the default) covers handlers,
	// subroutes and invoked named routes inside the configuring
	// handler's subtree; "error_routes" covers error handler chains;
	// "retries" covers retry candidates such as reverse_proxy retries.
	Allow []string `json:"allow,omitempty"`

	// OnExceed defines what happens when a body exceeds MaxSize:
	// "reject" (default) fails the request with HTTP 413, and
	// "degrade" keeps streaming the full body but disables replay.
	OnExceed string `json:"on_exceed,omitempty"`

	// OnCancel defines what happens to a spilled temp file once the
	// request ends or the client cancels: "delete" (default) removes
	// it immediately; "retain" closes it and leaves it on disk for
	// inspection, with the path logged.
	OnCancel string `json:"on_cancel,omitempty"`

	// scopes is the parsed Allow bitmask.
	scopes BodyReplayScope
}

// Provision validates the policy and fills in defaults.
func (c *BodyReplayConfig) Provision(_ caddy.Context) error {
	if c.MemoryMaxSize == 0 {
		c.MemoryMaxSize = defaultBodyReplayMemoryMaxSize
	}
	if c.MaxSize == 0 {
		c.MaxSize = c.MemoryMaxSize
	}
	if c.MemoryMaxSize > c.MaxSize {
		return fmt.Errorf("request body replay: memory_max_size (%d) must not exceed max_size (%d)", c.MemoryMaxSize, c.MaxSize)
	}

	switch c.OnExceed {
	case "", bodyReplayOnExceedReject:
		c.OnExceed = bodyReplayOnExceedReject
	case bodyReplayOnExceedDegrade:
	default:
		return fmt.Errorf("request body replay: unknown on_exceed %q; expected %q or %q", c.OnExceed, bodyReplayOnExceedReject, bodyReplayOnExceedDegrade)
	}

	switch c.OnCancel {
	case "", bodyReplayOnCancelDelete:
		c.OnCancel = bodyReplayOnCancelDelete
	case bodyReplayOnCancelRetain:
	default:
		return fmt.Errorf("request body replay: unknown on_cancel %q; expected %q or %q", c.OnCancel, bodyReplayOnCancelDelete, bodyReplayOnCancelRetain)
	}

	c.scopes = BodyReplayScopeNestedRoutes
	for _, name := range c.Allow {
		scope, ok := ParseBodyReplayScope(name)
		if !ok {
			return fmt.Errorf("request body replay: unknown replay scope %q", name)
		}
		c.scopes |= scope
	}

	return nil
}

// Allows reports whether replay is permitted in scope.
func (c BodyReplayConfig) Allows(scope BodyReplayScope) bool {
	return c.scopes&scope != 0
}

// EnableBodyReplay wraps r.Body so that bytes read downstream are
// spooled per cfg and can be replayed within the configured scopes.
// The returned request carries the wrapped body and should be used in
// place of the original. It is a no-op for absent bodies and when a
// policy further out in the chain already enabled replay; in that case
// the outermost policy owns the body for the whole request.
func EnableBodyReplay(r *http.Request, cfg *BodyReplayConfig, logger *zap.Logger) *http.Request {
	if r.Body == nil || r.Body == http.NoBody {
		return r
	}
	if getBodyReplay(r.Context()) != nil {
		if logger != nil {
			logger.Debug("request body replay already enabled by an enclosing request_body handler; ignoring nested policy")
		}
		return r
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	state := &bodyReplay{
		cfg:        cfg,
		logger:     logger,
		src:        r.Body,
		scopeStack: []BodyReplayScope{BodyReplayScopeNestedRoutes},
	}
	SetVar(r.Context(), bodyReplayVarKey, state)
	r.Body = &bodyReplayLiveReader{state: state}

	// Guarantee cleanup even if the body is replaced (for example by
	// the body placeholder or an error-route reset) and therefore no
	// longer closed directly by the HTTP server. The request context
	// is canceled when the request ends or the client disconnects.
	context.AfterFunc(r.Context(), state.close)

	return r
}

// PrepareBodyReplay enters a replay boundary such as an error route
// or a retry attempt and installs a fresh reader over the spooled
// request body when the active policy permits replay in scope. The
// returned request (carrying the possibly replaced body) must be used
// from the boundary onward. When no policy is active, or the policy
// does not permit the scope, the body is left untouched and one-shot
// semantics are preserved, but readers produced before the boundary
// stop serving inside it. The installed reader yields every spooled
// byte in order with an unchanged Content-Length; if the source was
// not read to completion its Read ends with ErrBodyReplayIncomplete
// instead of a silently truncated body. Non-terminal boundaries
// (subroutes, retries) must call LeaveBodyReplayScope when they exit.
func PrepareBodyReplay(r *http.Request, scope BodyReplayScope) *http.Request {
	state := getBodyReplay(r.Context())
	if state == nil {
		return r
	}
	state.enterScope(scope)
	if state.cfg.Allows(scope) {
		r.Body = state.newReader()
	}
	return r
}

// LeaveBodyReplayScope exits a replay boundary previously entered with
// PrepareBodyReplay, restoring the enclosing scope. It is safe to call
// when no policy is active.
func LeaveBodyReplayScope(r *http.Request) {
	if state := getBodyReplay(r.Context()); state != nil {
		state.leaveScope()
	}
}

// bodyReplayReadable reports whether the spool may be consumed from
// the request's current scope: the primary handler chain maps to
// nested routes, while error routes and retries carry their own
// boundaries.
func bodyReplayReadable(ctx context.Context) bool {
	state := getBodyReplay(ctx)
	return state != nil && state.cfg.Allows(state.currentScope())
}

// getBodyReplay returns the per-request replay state, or nil.
func getBodyReplay(ctx context.Context) *bodyReplay {
	state, _ := GetVar(ctx, bodyReplayVarKey).(*bodyReplay)
	return state
}

// BodyReplayAllows reports whether an active replay policy permits
// replay in scope. It does not modify the request.
func BodyReplayAllows(r *http.Request, scope BodyReplayScope) bool {
	state := getBodyReplay(r.Context())
	return state != nil && state.cfg.Allows(scope)
}

// bodyReplay is the per-request spool state. It is never shared
// between requests; its temporary resources are owned exclusively by
// the request that created them.
type bodyReplay struct {
	cfg    *BodyReplayConfig
	logger *zap.Logger
	src    io.ReadCloser

	closeOnce sync.Once

	mu        sync.Mutex
	mem       []byte // spooled prefix while f is nil
	f         *os.File
	spillPath string
	size      int64 // total spooled bytes

	complete bool  // src returned io.EOF
	srcErr   error // sticky terminal error from src
	degraded bool  // past MaxSize in degrade mode; spooling stopped
	fatal    error // sticky error to serve from every subsequent read
	closed   bool  // close ran

	// scopeStack is the chain of replay boundaries the request is
	// inside; the top decides whether spool readers may serve. It
	// starts in the policy subtree (nested routes).
	scopeStack []BodyReplayScope
}

// enterScope pushes a replay boundary.
func (b *bodyReplay) enterScope(scope BodyReplayScope) {
	b.mu.Lock()
	b.scopeStack = append(b.scopeStack, scope)
	b.mu.Unlock()
}

// leaveScope pops the most recently entered boundary.
func (b *bodyReplay) leaveScope() {
	b.mu.Lock()
	if len(b.scopeStack) > 1 {
		b.scopeStack = b.scopeStack[:len(b.scopeStack)-1]
	}
	b.mu.Unlock()
}

// currentScope returns the boundary the request is currently in.
func (b *bodyReplay) currentScope() BodyReplayScope {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scopeStack[len(b.scopeStack)-1]
}

// liveRead is the Read implementation of the body installed on the
// request. It streams from the original body and tees every delivered
// byte into the spool, enforcing MaxSize.
func (b *bodyReplay) liveRead(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.mu.Lock()
	if b.fatal != nil {
		err := b.fatal
		b.mu.Unlock()
		return 0, err
	}
	if b.closed {
		b.mu.Unlock()
		return 0, io.EOF
	}
	b.mu.Unlock()

	// Read without holding the lock so a slow client does not stall
	// replay readers; this is the only goroutine that advances src.
	n, rerr := b.src.Read(p)

	b.mu.Lock()
	defer b.mu.Unlock()

	// close may have run while this Read was blocked on the client;
	// never append into a released spool or reopen a spill file
	if b.closed {
		if rerr != nil {
			return n, rerr
		}
		return 0, io.EOF
	}

	if n > 0 && !b.degraded {
		if b.cfg.MaxSize > 0 && b.size+int64(n) > b.cfg.MaxSize {
			allowed := b.cfg.MaxSize - b.size
			if allowed > 0 {
				written, err := b.appendLocked(p[:int(allowed)])
				b.size += int64(written)
				if err != nil {
					b.fatal = Error(http.StatusInternalServerError, fmt.Errorf("writing request body spill file: %w", err))
					return written, b.fatal
				}
			}
			if b.cfg.OnExceed == bodyReplayOnExceedReject {
				// Mirror http.MaxBytesReader so the 413, the access log
				// and the body placeholder behave exactly like a
				// request_body max_size rejection.
				b.fatal = Error(http.StatusRequestEntityTooLarge, &http.MaxBytesError{Limit: b.cfg.MaxSize})
				return int(allowed), b.fatal
			}
			// Degrade: the bytes past the cap stream through to the
			// downstream unspooled; the full body is still delivered.
			b.degraded = true
		} else {
			written, err := b.appendLocked(p[:n])
			b.size += int64(written)
			if err != nil {
				b.fatal = Error(http.StatusInternalServerError, fmt.Errorf("writing request body spill file: %w", err))
				return n, b.fatal
			}
		}
	}

	switch {
	case rerr == io.EOF:
		b.complete = true
	case rerr != nil:
		// Errors produced by this policy already carry their state in
		// fatal; record other source errors for replay readers.
		var handlerErr HandlerError
		if !errors.As(rerr, &handlerErr) {
			b.srcErr = rerr
		}
	}

	return n, rerr
}

// appendLocked appends p to memory or, once the memory cap is reached,
// the spill file. It returns the number of bytes actually stored and
// the first write error, if any. The caller must hold b.mu.
func (b *bodyReplay) appendLocked(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.f == nil && int64(len(b.mem)) < b.cfg.MemoryMaxSize {
		room := int(b.cfg.MemoryMaxSize - int64(len(b.mem)))
		if len(p) <= room {
			b.mem = append(b.mem, p...)
			return len(p), nil
		}
		if err := b.openSpillLocked(); err != nil {
			return 0, err
		}
	}
	if b.f == nil {
		if err := b.openSpillLocked(); err != nil {
			return 0, err
		}
	}
	return b.f.Write(p)
}

// openSpillLocked creates the overflow file, flushes the memory prefix
// into it and releases the in-memory copy. The caller must hold b.mu.
func (b *bodyReplay) openSpillLocked() error {
	dir := b.cfg.SpillDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, bodyReplaySpillPrefix+"*")
	if err != nil {
		return err
	}
	if _, err := f.Write(b.mem); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	b.f = f
	b.spillPath = f.Name()
	b.mem = nil
	return nil
}

// newReader returns an independent reader over the complete spool,
// starting at the first byte.
func (b *bodyReplay) newReader() *bodyReplayReader {
	return &bodyReplayReader{state: b}
}

// stats returns counters for the replay status placeholders.
func (b *bodyReplay) stats() (size int64, inMemory, complete, degraded bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size, b.f == nil, b.complete, b.degraded
}

// close releases the original body and any spill file. It is safe to
// call multiple times and runs at most once.
func (b *bodyReplay) close() {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		src := b.src
		f := b.f
		path := b.spillPath
		retain := f != nil && b.cfg.OnCancel == bodyReplayOnCancelRetain
		b.f = nil
		b.closed = true
		b.mu.Unlock()

		if src != nil {
			_ = src.Close()
		}
		if f != nil {
			_ = f.Close()
		}
		if path != "" && !retain {
			_ = os.Remove(path)
		}
		if retain {
			b.logger.Warn("retained request body spill file for inspection; remove it manually", zap.String("path", path))
		}
	})
}

// bodyReplayLiveReader is the live, body-owned reader installed on the
// request. Closing it releases the whole per-request spool.
type bodyReplayLiveReader struct {
	state *bodyReplay
}

func (r *bodyReplayLiveReader) Read(p []byte) (int, error) {
	return r.state.liveRead(p)
}

func (r *bodyReplayLiveReader) Close() error {
	r.state.close()
	return nil
}

// bodyReplayReader is an independent, non-owning reader over the
// spooled bytes. Closing it never releases the spool; the request
// context owns the spool's lifetime. A reader only serves while the
// boundary the request is in is permitted by the policy, so a reader
// handed to a nested handler cannot leak the body into an error route
// or retry that the policy did not permit.
type bodyReplayReader struct {
	state *bodyReplay
	off   int64
}

func (r *bodyReplayReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b := r.state

	b.mu.Lock()
	defer b.mu.Unlock()

	currentScope := b.scopeStack[len(b.scopeStack)-1]
	if !b.cfg.Allows(currentScope) {
		return 0, ErrBodyReplayUnavailable
	}

	if r.off < b.size {
		available := b.size - r.off
		if int64(len(p)) > available {
			p = p[:int(available)]
		}
		var n int
		if b.f != nil {
			// ReadAt is safe concurrently with appends to the same
			// descriptor and never mutates the file offset.
			n, _ = b.f.ReadAt(p, r.off)
		} else {
			n = copy(p, b.mem[r.off:])
		}
		r.off += int64(n)
		return n, nil
	}

	switch {
	case b.fatal != nil:
		return 0, b.fatal
	case b.closed:
		return 0, io.ErrClosedPipe
	case b.degraded:
		// even if the source reached EOF, only the capped prefix was
		// spooled; replay must not present the prefix as the full body
		return 0, ErrBodyReplayUnavailable
	case b.complete:
		return 0, io.EOF
	case b.srcErr != nil:
		return 0, b.srcErr
	default:
		return 0, ErrBodyReplayIncomplete
	}
}

func (r *bodyReplayReader) Close() error { return nil }

// readAllForPlaceholder drains the live body for the
// {http.request.body} placeholders and replaces the request body with
// a replay reader, so later handlers can still consume the body. A
// max_size rejection is converted to the shared RequestBodyLimitError
// marker, exactly like the legacy, unspooled placeholder path; other
// read errors are ignored and the buffered prefix is installed, also
// matching the legacy behavior.
func (b *bodyReplay) readAllForPlaceholder(req *http.Request) ([]byte, error) {
	body, err := io.ReadAll(req.Body)
	var handlerErr HandlerError
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &handlerErr) && errors.As(err, &maxBytesErr) {
		return nil, RequestBodyLimitError{Err: maxBytesErr}
	}
	req.Body = b.newReader()
	return body, nil
}

// bodyReplayStatus is the replacer-facing snapshot of a replay state.
type bodyReplayStatus struct {
	spooled  int64
	inMemory bool
	spilled  bool
	complete bool
	degraded bool
}

// bodyReplayStats returns the replay status for req, or zero values
// when no replay policy is active.
func bodyReplayStats(req *http.Request) bodyReplayStatus {
	state := getBodyReplay(req.Context())
	if state == nil {
		return bodyReplayStatus{}
	}
	size, inMemory, complete, degraded := state.stats()
	return bodyReplayStatus{
		spooled:  size,
		inMemory: inMemory,
		spilled:  !inMemory,
		complete: complete,
		degraded: degraded,
	}
}

// Interface guards
var (
	_ io.ReadCloser     = (*bodyReplayLiveReader)(nil)
	_ io.ReadCloser     = (*bodyReplayReader)(nil)
	_ caddy.Provisioner = (*BodyReplayConfig)(nil)
)
