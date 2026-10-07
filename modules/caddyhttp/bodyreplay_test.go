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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

// replayTestEnv bundles a request whose body is wrapped by an enabled
// replay policy.
type replayTestEnv struct {
	req    *http.Request
	live   io.ReadCloser // original live body captured before any replay
	cancel context.CancelFunc
	state  *bodyReplay
	dir    string // when non-empty the directory is removed on cleanup
}

func newReplayTestEnv(t *testing.T, body string, cfg *BodyReplayConfig) *replayTestEnv {
	t.Helper()

	if cfg == nil {
		cfg = &BodyReplayConfig{}
	}
	if err := cfg.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://localhost/upload", strings.NewReader(body))

	ctx, cancel := context.WithCancel(req.Context())
	ctx = context.WithValue(ctx, VarsCtxKey, map[string]any{})
	ctx = context.WithValue(ctx, caddy.ReplacerCtxKey, caddy.NewReplacer())
	req = req.WithContext(ctx)

	req = EnableBodyReplay(req, cfg, zap.NewNop())

	env := &replayTestEnv{
		req:    req,
		live:   req.Body,
		cancel: cancel,
		state:  getBodyReplay(req.Context()),
	}
	t.Cleanup(func() {
		cancel()
		_ = req.Body.Close()
		state := getBodyReplay(req.Context())
		if state != nil {
			state.close()
		}
		if env.dir != "" {
			_ = os.RemoveAll(env.dir)
		}
	})
	return env
}

// readAll is io.ReadAll on the given reader.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	return string(data)
}

func TestBodyReplayConfigProvision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     BodyReplayConfig
		wantErr bool
		check   func(t *testing.T, c *BodyReplayConfig)
	}{
		{
			name: "defaults",
			cfg:  BodyReplayConfig{},
			check: func(t *testing.T, c *BodyReplayConfig) {
				if c.MemoryMaxSize != defaultBodyReplayMemoryMaxSize {
					t.Errorf("default memory cap = %d, want %d", c.MemoryMaxSize, defaultBodyReplayMemoryMaxSize)
				}
				if c.MaxSize != c.MemoryMaxSize {
					t.Errorf("default max size = %d, want %d", c.MaxSize, c.MemoryMaxSize)
				}
				if c.OnExceed != "reject" || c.OnCancel != "delete" {
					t.Errorf("unexpected defaults: on_exceed=%q on_cancel=%q", c.OnExceed, c.OnCancel)
				}
				if !c.Allows(BodyReplayScopeNestedRoutes) {
					t.Error("nested routes should be allowed by default")
				}
				if c.Allows(BodyReplayScopeErrorRoutes) || c.Allows(BodyReplayScopeRetries) {
					t.Error("error routes and retries should be opt-in")
				}
			},
		},
		{
			name:    "memory larger than max",
			cfg:     BodyReplayConfig{MaxSize: 10, MemoryMaxSize: 20},
			wantErr: true,
		},
		{
			name:    "bad on_exceed",
			cfg:     BodyReplayConfig{OnExceed: "explode"},
			wantErr: true,
		},
		{
			name:    "bad on_cancel",
			cfg:     BodyReplayConfig{OnCancel: "maybe"},
			wantErr: true,
		},
		{
			name:    "bad scope",
			cfg:     BodyReplayConfig{Allow: []string{"everything"}},
			wantErr: true,
		},
		{
			name: "scopes accumulate with nested implicit",
			cfg:  BodyReplayConfig{Allow: []string{BodyReplayScopeErrorRoutesName, BodyReplayScopeRetriesName}},
			check: func(t *testing.T, c *BodyReplayConfig) {
				if !c.Allows(BodyReplayScopeNestedRoutes) ||
					!c.Allows(BodyReplayScopeErrorRoutes) ||
					!c.Allows(BodyReplayScopeRetries) {
					t.Error("all three scopes should be allowed")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Provision(caddy.Context{})
			if tc.wantErr && err == nil {
				t.Fatal("expected provision error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected provision error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, &tc.cfg)
			}
		})
	}
}

func TestBodyReplayMemoryRoundTrip(t *testing.T) {
	const body = "hello, replayable world"
	env := newReplayTestEnv(t, body, nil)

	if env.state == nil {
		t.Fatal("expected replay state in request vars")
	}
	if env.req.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength changed: got %d, want %d", env.req.ContentLength, len(body))
	}

	// first pass streams from the client and fills the memory spool
	if got := readAll(t, env.req.Body); got != body {
		t.Fatalf("live read = %q, want %q", got, body)
	}

	// repeated replays are independent and return the exact body,
	// each terminated by EOF
	for i := 0; i < 3; i++ {
		env.req = PrepareBodyReplay(env.req, BodyReplayScopeNestedRoutes)
		if got := readAll(t, env.req.Body); got != body {
			t.Fatalf("replay %d = %q, want %q", i, got, body)
		}
	}

	size, inMemory, complete, degraded := env.state.stats()
	if size != int64(len(body)) || !inMemory || !complete || degraded {
		t.Errorf("stats = %d/%v/%v/%v, want %d/true/true/false", size, inMemory, complete, degraded, len(body))
	}
}

func TestBodyReplayByteByByteAcrossSpillBoundary(t *testing.T) {
	body := strings.Repeat("abcdefgh", 100) // 800 bytes
	cfg := &BodyReplayConfig{MemoryMaxSize: 64, MaxSize: 4096}
	env := newReplayTestEnv(t, body, cfg)
	env.dir = cfg.SpillDir

	// one byte at a time forces repeated memory->disk decisions
	var live strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := env.req.Body.Read(buf)
		live.Write(buf[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatalf("live read error: %v", err)
			}
			break
		}
		if n != 1 {
			t.Fatalf("read %d bytes, want 1", n)
		}
	}
	if live.String() != body {
		t.Fatal("byte-by-byte live read mismatched the source body")
	}

	env.state.mu.Lock()
	spillPath := env.state.spillPath
	f := env.state.f
	env.state.mu.Unlock()
	if f == nil || spillPath == "" {
		t.Fatal("expected a spill file once the memory cap was crossed")
	}

	// replay in tiny chunks must yield the identical stream
	env.req = PrepareBodyReplay(env.req, BodyReplayScopeNestedRoutes)
	var replay strings.Builder
	small := make([]byte, 3)
	for {
		n, err := env.req.Body.Read(small)
		replay.Write(small[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("replay read error: %v", err)
		}
	}
	if replay.String() != body {
		t.Fatal("replay across the spill boundary changed the bytes")
	}

	// cleanup unlinks the spill file
	env.state.close()
	if _, err := os.Stat(spillPath); !os.IsNotExist(err) {
		t.Fatalf("spill file %s not removed on close: %v", spillPath, err)
	}
}

func TestBodyReplayRejectOverMax(t *testing.T) {
	body := strings.Repeat("x", 100)
	cfg := &BodyReplayConfig{MemoryMaxSize: 16, MaxSize: 32, OnExceed: "reject"}
	env := newReplayTestEnv(t, body, cfg)

	n, err := io.ReadAll(env.req.Body)
	if err == nil {
		t.Fatal("expected an error when the body exceeds max_size")
	}
	var handlerErr HandlerError
	var maxBytesErr *http.MaxBytesError
	if !errors.As(err, &handlerErr) || handlerErr.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 HandlerError, got %T: %v", err, err)
	}
	if !errors.As(err, &maxBytesErr) {
		t.Fatalf("error must wrap *http.MaxBytesError for the placeholder marker, got %v", err)
	}
	if int64(len(n)) > cfg.MaxSize {
		t.Errorf("delivered %d bytes, at most %d allowed", len(n), cfg.MaxSize)
	}

	// the limit is sticky for every subsequent read
	if _, err := env.req.Body.Read(make([]byte, 1)); err == nil {
		t.Error("expected the limit error to stick on later reads")
	}
}

func TestBodyReplayDegradeOverMax(t *testing.T) {
	body := strings.Repeat("y", 100)
	cfg := &BodyReplayConfig{MemoryMaxSize: 16, MaxSize: 32, OnExceed: "degrade"}
	env := newReplayTestEnv(t, body, cfg)

	// the whole body still streams through unmodified
	if got := readAll(t, env.req.Body); got != body {
		t.Fatalf("degraded live read truncated: got %d bytes, want %d", len(got), len(body))
	}
	if _, _, _, degraded := env.state.stats(); !degraded {
		t.Error("state should be marked degraded")
	}

	// replay covers the spooled prefix and then reports that replay
	// is unavailable rather than pretending the body ended
	env.req = PrepareBodyReplay(env.req, BodyReplayScopeNestedRoutes)
	data, err := io.ReadAll(env.req.Body)
	if !errors.Is(err, ErrBodyReplayUnavailable) {
		t.Fatalf("expected ErrBodyReplayUnavailable, got data=%d err=%v", len(data), err)
	}
	if int64(len(data)) != cfg.MaxSize {
		t.Errorf("prefix = %d bytes, want %d", len(data), cfg.MaxSize)
	}
}

func TestBodyReplayIncompleteAfterEarlyStop(t *testing.T) {
	body := "0123456789ABCDEFGHIJ"
	env := newReplayTestEnv(t, body, nil)

	// downstream stops after five bytes
	head := make([]byte, 5)
	if _, err := io.ReadFull(env.req.Body, head); err != nil {
		t.Fatalf("partial read failed: %v", err)
	}

	env.req = PrepareBodyReplay(env.req, BodyReplayScopeNestedRoutes)
	data, err := io.ReadAll(env.req.Body)
	if !errors.Is(err, ErrBodyReplayIncomplete) {
		t.Fatalf("expected ErrBodyReplayIncomplete, got %v", err)
	}
	if string(data) != body[:5] {
		t.Errorf("replayed prefix = %q, want %q", data, body[:5])
	}
}

func TestBodyReplayIncompleteBecomesComplete(t *testing.T) {
	body := "abcdefghijklmnopqrstuvwxyz"
	env := newReplayTestEnv(t, body, nil)

	head := make([]byte, 7)
	if _, err := io.ReadFull(env.req.Body, head); err != nil {
		t.Fatalf("partial read failed: %v", err)
	}

	// first replay can only promise the prefix
	env.req = PrepareBodyReplay(env.req, BodyReplayScopeNestedRoutes)
	if _, err := io.ReadAll(env.req.Body); !errors.Is(err, ErrBodyReplayIncomplete) {
		t.Fatal("expected incomplete replay before EOF")
	}

	// now drain the rest of the live source to completion; the
	// original live reader is still positioned after the 7 prefix bytes
	rest := readAll(t, env.live)
	if rest != body[7:] {
		t.Fatalf("tail drain = %q, want %q", rest, body[7:])
	}

	env.req = PrepareBodyReplay(env.req, BodyReplayScopeNestedRoutes)
	if got := readAll(t, env.req.Body); got != body {
		t.Fatalf("complete replay = %q, want %q", got, body)
	}
}

func TestBodyReplaySourceError(t *testing.T) {
	sentinel := errors.New("boom")
	src := &errorAfterReader{data: []byte("partial-body-"), err: sentinel}

	cfg := &BodyReplayConfig{}
	if err := cfg.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", src)
	ctx := context.WithValue(req.Context(), VarsCtxKey, map[string]any{})
	req = req.WithContext(ctx)
	req = EnableBodyReplay(req, cfg, zap.NewNop())
	t.Cleanup(func() { getBodyReplay(req.Context()).close() })

	n, err := io.Copy(io.Discard, req.Body)
	if !errors.Is(err, sentinel) {
		t.Fatalf("live read err = %v, want %v; read %d bytes", err, sentinel, n)
	}

	req = PrepareBodyReplay(req, BodyReplayScopeNestedRoutes)
	data, err := io.ReadAll(req.Body)
	if !errors.Is(err, sentinel) {
		t.Fatalf("replay err = %v, want source error %v", err, sentinel)
	}
	if string(data) != "partial-body-" {
		t.Errorf("replayed prefix = %q", data)
	}
}

func TestBodyReplayDiskWriteFailure(t *testing.T) {
	// a regular file in the spill path makes MkdirAll fail
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &BodyReplayConfig{MemoryMaxSize: 4, MaxSize: 1024, SpillDir: blocker}
	if err := cfg.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", io.NopCloser(strings.NewReader(strings.Repeat("z", 50))))
	ctx := context.WithValue(req.Context(), VarsCtxKey, map[string]any{})
	req = req.WithContext(ctx)
	req = EnableBodyReplay(req, cfg, zap.NewNop())
	t.Cleanup(func() { getBodyReplay(req.Context()).close() })

	_, err := io.ReadAll(req.Body)
	var handlerErr HandlerError
	if !errors.As(err, &handlerErr) || handlerErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 HandlerError on disk failure, got %v", err)
	}

	// cleanup must not panic or leave files
	state := getBodyReplay(req.Context())
	state.mu.Lock()
	path := state.spillPath
	state.mu.Unlock()
	state.close()
	if path != "" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("partial spill file left behind: %v", err)
		}
	}
}

func TestBodyReplayCleanupOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	cfg := &BodyReplayConfig{MemoryMaxSize: 8, MaxSize: 4096, SpillDir: dir}
	if err := cfg.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}

	src := &slowReader{}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", src)
	ctx, cancel := context.WithCancel(req.Context())
	ctx = context.WithValue(ctx, VarsCtxKey, map[string]any{})
	req = req.WithContext(ctx)
	req = EnableBodyReplay(req, cfg, zap.NewNop())

	// force the spill file into existence
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 64))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state := getBodyReplay(req.Context())
		state.mu.Lock()
		path := state.spillPath
		state.mu.Unlock()
		if path != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spill file was never created")
		}
		time.Sleep(time.Millisecond)
	}

	state := getBodyReplay(req.Context())
	state.mu.Lock()
	path := state.spillPath
	state.mu.Unlock()

	cancel()

	select {
	case <-src.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("source body was not closed on cancel")
	}

	deadline = time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spill file %s survived request cancellation", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBodyReplayRetainOnCancel(t *testing.T) {
	dir := t.TempDir()
	cfg := &BodyReplayConfig{MemoryMaxSize: 4, MaxSize: 4096, SpillDir: dir, OnCancel: "retain"}
	if err := cfg.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}

	env := newReplayTestEnv(t, strings.Repeat("q", 40), cfg)
	_ = readAll(t, env.req.Body)
	state := getBodyReplay(env.req.Context())
	state.mu.Lock()
	path := state.spillPath
	state.mu.Unlock()
	if path == "" {
		t.Fatal("expected spill file")
	}

	state.close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retained spill file should exist after close: %v", err)
	}
	// test owns cleanup of the deliberately retained file
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestBodyReplayConcurrentReaders(t *testing.T) {
	body := strings.Repeat("0123456789", 500) // 5000 bytes, spills to disk
	cfg := &BodyReplayConfig{MemoryMaxSize: 128, MaxSize: 1 << 20, SpillDir: t.TempDir()}
	env := newReplayTestEnv(t, body, cfg)
	env.dir = cfg.SpillDir

	if got := readAll(t, env.req.Body); got != body {
		t.Fatal("live read mismatch")
	}

	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			r := env.state.newReader()
			data, err := io.ReadAll(r)
			if err != nil {
				errs <- err
				return
			}
			if string(data) != body {
				errs <- errors.New("replay mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestBodyReplayScopeGating(t *testing.T) {
	env := newReplayTestEnv(t, "scope-body", nil)

	// denied boundaries mark the scope on the context but must leave
	// the one-shot live body untouched
	env.req = PrepareBodyReplay(env.req, BodyReplayScopeErrorRoutes)
	if _, ok := env.req.Body.(*bodyReplayLiveReader); !ok {
		t.Errorf("r.Body after denied error boundary = %T, want live reader", env.req.Body)
	}
	if bodyReplayReadable(env.req.Context()) {
		t.Error("spool must not be readable from a denied error boundary")
	}
	env.req = PrepareBodyReplay(env.req, BodyReplayScopeRetries)
	if _, ok := env.req.Body.(*bodyReplayLiveReader); !ok {
		t.Errorf("r.Body after denied retry boundary = %T, want live reader", env.req.Body)
	}
	if bodyReplayReadable(env.req.Context()) {
		t.Error("spool must not be readable from a denied retry boundary")
	}

	env.state.cfg.scopes |= BodyReplayScopeErrorRoutes
	env.req = PrepareBodyReplay(env.req, BodyReplayScopeErrorRoutes)
	if _, ok := env.req.Body.(*bodyReplayReader); !ok {
		t.Errorf("r.Body = %T, want replay reader", env.req.Body)
	}
	if !bodyReplayReadable(env.req.Context()) {
		t.Error("spool should be readable from an allowed error boundary")
	}
}

func TestEnableBodyReplayNestedPolicyIgnored(t *testing.T) {
	env := newReplayTestEnv(t, "outer", &BodyReplayConfig{MemoryMaxSize: 8, MaxSize: 64})
	first := getBodyReplay(env.req.Context())

	other := &BodyReplayConfig{MemoryMaxSize: 1, MaxSize: 2}
	_ = other.Provision(caddy.Context{})
	env.req = EnableBodyReplay(env.req, other, zap.NewNop())

	if second := getBodyReplay(env.req.Context()); second != first {
		t.Error("the outermost replay policy must own the request")
	}
}

func TestPrepareBodyReplayWithoutPolicy(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", io.NopCloser(strings.NewReader("x")))
	original := req.Body
	r2 := PrepareBodyReplay(req, BodyReplayScopeErrorRoutes)
	if r2.Body != original {
		t.Error("replay preparation must leave the body untouched without a policy")
	}
}

// TestBodyReplaySubrouteErrorRoute drives a subroute whose primary
// handler consumes the whole body and then errors; its error route must
// receive a fresh copy when error_routes are allowed, and the consumed
// one-shot body when they are not.
func TestBodyReplaySubrouteErrorRoute(t *testing.T) {
	const body = "error-route-payload"

	for _, tc := range []struct {
		name     string
		allow    []string
		wantBody string
	}{
		{name: "replay allowed", allow: []string{BodyReplayScopeErrorRoutesName}, wantBody: body},
		{name: "replay not allowed", allow: nil, wantBody: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &BodyReplayConfig{Allow: tc.allow}
			env := newReplayTestEnv(t, body, cfg)

			var seenByErrorRoute string
			primary := HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Fatalf("primary read: %v", err)
				}
				return Error(http.StatusTeapot, errors.New("primary failed on purpose"))
			})
			errorHandler := HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					return err
				}
				seenByErrorRoute = string(data)
				w.WriteHeader(http.StatusOK)
				return nil
			})

			sr := &Subroute{
				Routes: RouteList{{
					middleware: []Middleware{func(Handler) Handler { return primary }},
				}},
				Errors: &HTTPErrorConfig{
					Routes: RouteList{{
						middleware: []Middleware{func(Handler) Handler { return errorHandler }},
					}},
				},
			}

			rec := httptest.NewRecorder()
			if err := sr.ServeHTTP(rec, env.req, emptyHandler); err != nil {
				t.Fatalf("subroute returned an error: %v", err)
			}
			if seenByErrorRoute != tc.wantBody {
				t.Errorf("error route body = %q, want %q", seenByErrorRoute, tc.wantBody)
			}
		})
	}
}

// TestBodyReplayPlaceholder verifies that {http.request.body} drains
// through the spool and leaves a replayable body for later handlers,
// and that the replay status placeholders report live state.
func TestBodyReplayPlaceholder(t *testing.T) {
	const body = "placeholder-body"
	env := newReplayTestEnv(t, body, nil)
	repl := NewTestReplacer(env.req)

	if got := repl.ReplaceAll("{http.request.body}", ""); got != body {
		t.Fatalf("placeholder = %q, want %q", got, body)
	}

	// the placeholder must not have consumed the body for later handlers
	if got := readAll(t, env.req.Body); got != body {
		t.Fatalf("body after placeholder = %q, want %q", got, body)
	}

	if got := repl.ReplaceAll("{http.request.body.replay.complete}", ""); got != "true" {
		t.Errorf("complete placeholder = %q, want true", got)
	}
	if got := repl.ReplaceAll("{http.request.body.replay.spooled_bytes}", ""); got != "16" {
		t.Errorf("spooled_bytes placeholder = %q, want 16", got)
	}
	if got := repl.ReplaceAll("{http.request.body.replay.degraded}", ""); got != "false" {
		t.Errorf("degraded placeholder = %q, want false", got)
	}
}

// TestBodyReplayPlaceholderReject verifies a replay limit rejection
// surfaces through the body placeholder as the shared limit marker,
// the same one request_body max_size produces.
func TestBodyReplayPlaceholderReject(t *testing.T) {
	cfg := &BodyReplayConfig{MemoryMaxSize: 8, MaxSize: 16}
	env := newReplayTestEnv(t, strings.Repeat("z", 100), cfg)
	repl := NewTestReplacer(env.req)

	_, found := repl.Get("http.request.body")
	if !found {
		t.Fatal("placeholder missing")
	}
	value, _ := repl.Get("http.request.body")
	if _, ok := value.(RequestBodyLimitError); !ok {
		t.Fatalf("placeholder value = %T, want RequestBodyLimitError", value)
	}
}

// TestBodyReplayStatusPlaceholdersWithoutPolicy verifies the status
// placeholders are known and evaluate to zero values without a policy.
func TestBodyReplayStatusPlaceholdersWithoutPolicy(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", io.NopCloser(strings.NewReader("x")))
	repl := NewTestReplacer(req)

	cases := map[string]string{
		"{http.request.body.replay.spooled_bytes}": "0",
		"{http.request.body.replay.in_memory}":     "false",
		"{http.request.body.replay.spilled}":       "false",
		"{http.request.body.replay.complete}":      "false",
		"{http.request.body.replay.degraded}":      "false",
	}
	for key, want := range cases {
		if got := repl.ReplaceAll(key, "MISSING"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// errorAfterReader returns its bytes then err.
type errorAfterReader struct {
	data []byte
	err  error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *errorAfterReader) Close() error { return nil }

// slowReader produces data slowly but can be interrupted by Close.
type slowReader struct {
	mu     sync.Mutex
	closed chan struct{}
}

func (r *slowReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.closed == nil {
		r.closed = make(chan struct{})
	}
	r.mu.Unlock()
	select {
	case <-r.closed:
		return 0, io.EOF
	case <-time.After(10 * time.Millisecond):
	}
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func (r *slowReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed == nil {
		r.closed = make(chan struct{})
	}
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}
