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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

// testHandler adapts a function into a MiddlewareHandler for tests.
type testHandler struct {
	fn func(w http.ResponseWriter, r *http.Request, next Handler) error
}

func (t testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next Handler) error {
	return t.fn(w, r, next)
}

func staticHandler(status int, body ...string) testHandler {
	return testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = io.WriteString(w, body[0])
		}
		return nil
	}}
}

func errorHandler(status int, msg string) testHandler {
	return testHandler{fn: func(http.ResponseWriter, *http.Request, Handler) error {
		return Error(status, errors.New(msg))
	}}
}

// newTestHandleRequest builds a request with the same context values
// the HTTP server installs for real requests.
func newTestHandleRequest(t *testing.T, method, target, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, bodyReader)
	w := httptest.NewRecorder()
	r = PrepareRequest(r, caddy.NewReplacer(), w, nil)
	return r, w
}

// testBranch builds a provisioned-equivalent branch from decoded
// matchers and handlers, bypassing module loading.
func testBranch(ms MatcherSets, group string, handlers ...MiddlewareHandler) HandleBranch {
	r := Route{Group: group, Handlers: handlers}
	mid := make([]Middleware, 0, len(handlers))
	for _, mh := range handlers {
		mid = append(mid, wrapMiddleware(caddy.Context{}, mh))
	}
	r.middleware = mid
	return HandleBranch{MatcherSets: ms, Routes: RouteList{r}}
}

func newTestHandler(branches []HandleBranch, plan *FallbackPlan) *HandleHandler {
	return &HandleHandler{
		Branches: branches,
		Fallback: plan,
		logger:   zap.NewNop(),
	}
}

func nextStatus(status int, body ...string) Handler {
	return HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = io.WriteString(w, body[0])
		}
		return nil
	})
}

func pathMatcher(paths ...string) MatcherSets {
	return MatcherSets{{MatchPath(paths)}}
}

func TestHandleExclusiveSemantics(t *testing.T) {
	r, w := newTestHandleRequest(t, "GET", "/a", "")

	h := newTestHandler([]HandleBranch{
		testBranch(pathMatcher("/a"), "", staticHandler(201, "a")),
		testBranch(nil, "", staticHandler(202, "b")),
	}, nil)

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 201 || w.Body.String() != "a" {
		t.Errorf("expected 201 'a', got %d %q", w.Code, w.Body.String())
	}

	// no match falls through to next
	r2, w2 := newTestHandleRequest(t, "GET", "/nope", "")
	h2 := newTestHandler([]HandleBranch{
		testBranch(pathMatcher("/z"), "", staticHandler(201)),
	}, nil)
	if err := h2.ServeHTTP(w2, r2, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w2.Code != 204 {
		t.Errorf("expected fallthrough 204, got %d", w2.Code)
	}
}

func TestHandleFallbackByStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		codes      []int
		wantStatus int
	}{
		{name: "exact", codes: []int{502, 503}, wantStatus: 200},
		{name: "class", codes: []int{5}, wantStatus: 200},
		{name: "no match", codes: []int{404}, wantStatus: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w := newTestHandleRequest(t, "GET", "/", "")
			h := newTestHandler([]HandleBranch{
				testBranch(nil, "", staticHandler(502, "bad")),
				testBranch(nil, "", staticHandler(200, "ok")),
			}, &FallbackPlan{StatusCodes: tc.codes})

			err := h.ServeHTTP(w, r, nextStatus(204))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w.Code != tc.wantStatus {
				t.Errorf("expected %d, got %d", tc.wantStatus, w.Code)
			}
			if tc.wantStatus == 200 {
				if w.Body.String() != "ok" {
					t.Errorf("expected winner body 'ok', got %q", w.Body.String())
				}
				if w.Header().Get("X-Fail") != "" {
					t.Error("failed attempt's header leaked into the committed response")
				}
			}
		})
	}
}

func TestHandleFallbackResponseIsolation(t *testing.T) {
	fail := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.Header().Set("X-Fail", "1")
		w.Header().Set("X-Shared", "fail")
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "bad body")
		return nil
	}}
	win := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.Header().Set("X-Shared", "win")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "good body")
		return nil
	}}

	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", fail),
		testBranch(nil, "", win),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 200 || w.Body.String() != "good body" {
		t.Errorf("expected 200 'good body', got %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Values("X-Fail"); len(got) != 0 {
		t.Errorf("failed attempt header leaked: %v", got)
	}
	if got := w.Header().Get("X-Shared"); got != "win" {
		t.Errorf("expected winner header to win, got %q", got)
	}
}

func TestHandleFallbackByResponseMatcher(t *testing.T) {
	fail := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.Header().Set("X-Upstream", "slow")
		w.WriteHeader(504)
		return nil
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", fail),
		testBranch(nil, "", staticHandler(200, "ok")),
	}, &FallbackPlan{
		ResponseMatchers: []*ResponseMatcher{{
			StatusCode: []int{504},
			Headers:    http.Header{"X-Upstream": []string{"slow"}},
		}},
	})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleFallbackByError(t *testing.T) {
	t.Run("error status triggers", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", errorHandler(503, "down")),
			testBranch(nil, "", staticHandler(200, "ok")),
		}, &FallbackPlan{ErrorStatusCodes: []int{5, 503}})

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 200 || w.Body.String() != "ok" {
			t.Errorf("expected 200 'ok', got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("error match placeholder triggers", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		plan := &FallbackPlan{
			ErrorMatcherSets: MatcherSets{{
				VarsMatcher{"{http.error.status_code}": []string{"503"}},
			}},
		}
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", errorHandler(503, "down")),
			testBranch(nil, "", staticHandler(200, "ok")),
		}, plan)

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 200 {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("unmatched error propagates", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", errorHandler(500, "boom")),
			testBranch(nil, "", staticHandler(200)),
		}, &FallbackPlan{ErrorStatusCodes: []int{503}})

		err := h.ServeHTTP(w, r, nextStatus(204))
		if err == nil {
			t.Fatal("expected error to propagate")
		}
		he, ok := err.(HandlerError)
		if !ok || he.StatusCode != 500 {
			t.Errorf("expected HandlerError 500, got %#v", err)
		}
		if w.Code != 200 {
			t.Errorf("erroring attempt must not commit a response, got %d", w.Code)
		}
	})
}

func TestHandleFallbackMaxAttempts(t *testing.T) {
	var calls int32
	counting := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(502)
		return nil
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", counting),
		testBranch(nil, "", counting),
		testBranch(nil, "", counting),
	}, &FallbackPlan{StatusCodes: []int{502}, MaxAttempts: 2})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected exactly 2 attempts, got %d", calls)
	}
	if w.Code != 502 {
		t.Errorf("expected last 502 committed, got %d", w.Code)
	}
}

func TestHandleFallbackOnExhaustedError(t *testing.T) {
	t.Run("synthesized from last status", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", staticHandler(502, "bad")),
		}, &FallbackPlan{StatusCodes: []int{502}, OnExhausted: "error"})

		err := h.ServeHTTP(w, r, nextStatus(204))
		if err == nil {
			t.Fatal("expected synthesized error")
		}
		he, ok := err.(HandlerError)
		if !ok || he.StatusCode != 502 {
			t.Errorf("expected HandlerError 502, got %#v", err)
		}
		if w.Code != 200 {
			t.Errorf("buffered response must not be committed, got %d", w.Code)
		}
	})

	t.Run("original error returned", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", errorHandler(503, "real-down")),
		}, &FallbackPlan{ErrorStatusCodes: []int{503}, OnExhausted: "error"})

		err := h.ServeHTTP(w, r, nextStatus(204))
		he, ok := err.(HandlerError)
		if !ok || he.StatusCode != 503 || !strings.Contains(err.Error(), "real-down") {
			t.Errorf("expected original 503 error, got %#v", err)
		}
	})
}

func TestHandleFallbackPlaceholderIsolation(t *testing.T) {
	setFail := testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
		SetVar(r.Context(), "foo", "1")
		repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
		repl.Set("foo", "1")
		w.WriteHeader(502)
		return nil
	}}
	readSet := testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
		repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
		foo, _ := repl.GetString("foo")
		repl.Set("bar", "2")
		w.Header().Set("X-Foo", foo)
		w.WriteHeader(200)
		return nil
	}}

	r, w := newTestHandleRequest(t, "GET", "/", "")
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", setFail),
		testBranch(nil, "", readSet),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Foo"); got != "" {
		t.Errorf("failed attempt placeholder leaked into winner: %q", got)
	}
	if v, _ := repl.GetString("foo"); v != "" {
		t.Errorf("failed attempt placeholder survived on shared replacer: %q", v)
	}
	if v, _ := repl.GetString("bar"); v != "2" {
		t.Errorf("winner placeholder should survive, got %q", v)
	}
	if v := GetVar(r.Context(), "foo"); v != nil {
		t.Errorf("failed attempt vars leaked: %v", v)
	}
}

func TestHandleFallbackRequestRestored(t *testing.T) {
	mutate := testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
		r.URL.Path = "/changed"
		r.Header.Set("X-Mutated", "1")
		r.Method = "POST"
		w.WriteHeader(502)
		return nil
	}}
	echoPath := testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
		if r.Method != "GET" || r.Header.Get("X-Mutated") != "" || r.URL.Path != "/start" {
			w.WriteHeader(500)
			return nil
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, r.URL.Path)
		return nil
	}}

	r, w := newTestHandleRequest(t, "GET", "/start", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", mutate),
		testBranch(pathMatcher("/start"), "", echoPath),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 200 || w.Body.String() != "/start" {
		t.Errorf("request not restored for next candidate: %d %q", w.Code, w.Body.String())
	}
}

func TestHandleFallbackBodyReplay(t *testing.T) {
	consumeAndFail := testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
		return nil
	}}
	echoBody := testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write(body)
		return nil
	}}

	t.Run("buffered replay", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "POST", "/", "hello")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", consumeAndFail),
			testBranch(nil, "", echoBody),
		}, &FallbackPlan{StatusCodes: []int{503}, RequestBodyBuffer: -1})

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 200 || w.Body.String() != "hello" {
			t.Errorf("expected replayed body 'hello', got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("unbuffered consumed body stops fallback", func(t *testing.T) {
		var secondCalled int32
		r, w := newTestHandleRequest(t, "POST", "/", "hello")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", consumeAndFail),
			testBranch(nil, "", testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
				atomic.StoreInt32(&secondCalled, 1)
				return echoBody.ServeHTTP(w, r, emptyHandler)
			}}),
		}, &FallbackPlan{StatusCodes: []int{503}})

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 503 {
			t.Errorf("expected first 503 committed, got %d", w.Code)
		}
		if atomic.LoadInt32(&secondCalled) != 0 {
			t.Error("second branch must not run after unbuffered body was consumed")
		}
	})

	t.Run("unbuffered untouched body can fall back", func(t *testing.T) {
		failBeforeRead := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
			return Error(503, errors.New("before read"))
		}}
		r, w := newTestHandleRequest(t, "POST", "/", "hello")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", failBeforeRead),
			testBranch(nil, "", echoBody),
		}, &FallbackPlan{ErrorStatusCodes: []int{503}})

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 200 || w.Body.String() != "hello" {
			t.Errorf("expected body 'hello' on zero-read fallback, got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("buffer limit exceeded stops fallback", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "POST", "/", "0123456789")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", consumeAndFail),
			testBranch(nil, "", echoBody),
		}, &FallbackPlan{StatusCodes: []int{503}, RequestBodyBuffer: 4})

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 503 {
			t.Errorf("expected first 503 committed with over-limit body, got %d", w.Code)
		}
	})
}

func TestHandleFallbackUnhandledBranches(t *testing.T) {
	deferring := testHandler{fn: func(http.ResponseWriter, *http.Request, Handler) error {
		// writes nothing and returns nil: the runtime treats this
		// like a branch flowing into its no-op tail
		return nil
	}}

	t.Run("unhandled then handled", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", deferring),
			testBranch(nil, "", staticHandler(200, "ok")),
		}, &FallbackPlan{StatusCodes: []int{502}})

		if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 200 || w.Body.String() != "ok" {
			t.Errorf("expected 200 'ok', got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("all unhandled falls through", func(t *testing.T) {
		r, w := newTestHandleRequest(t, "GET", "/", "")
		h := newTestHandler([]HandleBranch{
			testBranch(nil, "", deferring),
		}, &FallbackPlan{StatusCodes: []int{502}})

		if err := h.ServeHTTP(w, r, nextStatus(204, "outer")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w.Code != 204 || w.Body.String() != "outer" {
			t.Errorf("expected fallthrough 204 'outer', got %d %q", w.Code, w.Body.String())
		}
	})
}

func TestHandleFallbackInformationalCommits(t *testing.T) {
	var secondCalled int32
	hints := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.WriteHeader(http.StatusEarlyHints)
		_, _ = io.WriteString(w, "hints")
		return nil
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", hints),
		testBranch(nil, "", testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
			atomic.StoreInt32(&secondCalled, 1)
			return staticHandler(200).ServeHTTP(w, r, emptyHandler)
		}}),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&secondCalled) != 0 {
		t.Error("1xx must commit the attempt and prevent fallback")
	}
	if w.Body.String() != "hints" {
		t.Errorf("expected the 1xx attempt body to pass through, got %q", w.Body.String())
	}
}

func TestHandleFallbackFlushBuffered(t *testing.T) {
	flushing := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		//nolint:bodyclose
		_ = http.NewResponseController(w).Flush()
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "first")
		return nil
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", flushing),
		testBranch(nil, "", staticHandler(200, "second")),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 200 || w.Body.String() != "second" {
		t.Errorf("flush must be buffered until commit; got %d %q", w.Code, w.Body.String())
	}
}

func TestHandleFallbackHijackCommits(t *testing.T) {
	var secondCalled int32
	hijacker := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		//nolint:bodyclose
		_, _, err := http.NewResponseController(w).Hijack()
		return err
	}}
	w := newHijackRespWriter()
	r := httptest.NewRequest("GET", "/", nil)
	r = PrepareRequest(r, caddy.NewReplacer(), w, nil)
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", hijacker),
		testBranch(nil, "", testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
			atomic.StoreInt32(&secondCalled, 1)
			return staticHandler(200).ServeHTTP(w, r, emptyHandler)
		}}),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&secondCalled) != 0 {
		t.Error("hijack must commit the attempt and prevent fallback")
	}
}

func TestHandleFallbackTrailersCommitted(t *testing.T) {
	withTrailers := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.Header().Set("Trailer", "X-At-End")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "body")
		w.Header().Set(http.TrailerPrefix+"X-At-End", "done")
		//nolint:bodyclose
		_ = http.NewResponseController(w).Flush()
		return nil
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", withTrailers),
	}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	result := w.Result()
	defer result.Body.Close()
	if got := result.Trailer.Get("X-At-End"); got != "done" {
		t.Errorf("expected trailer X-At-End=done, got %q (headers=%v)", got, result.Header)
	}
}

func TestHandleFallbackResponseSpill(t *testing.T) {
	var secondCalled int32
	big := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "0123456789")
		return nil
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", big),
		testBranch(nil, "", testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
			atomic.StoreInt32(&secondCalled, 1)
			return staticHandler(200).ServeHTTP(w, r, emptyHandler)
		}}),
	}, &FallbackPlan{StatusCodes: []int{502}, ResponseBodyBuffer: 4})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 502 || w.Body.String() != "0123456789" {
		t.Errorf("expected spilled 502 full body, got %d %q", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&secondCalled) != 0 {
		t.Error("spill must prevent fallback")
	}
}

func TestHandleFallbackClientCancel(t *testing.T) {
	var secondCalled int32
	canceling := testHandler{fn: func(http.ResponseWriter, *http.Request, Handler) error {
		return context.Canceled
	}}
	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{
		testBranch(nil, "", canceling),
		testBranch(nil, "", testHandler{fn: func(w http.ResponseWriter, r *http.Request, _ Handler) error {
			atomic.StoreInt32(&secondCalled, 1)
			return staticHandler(200).ServeHTTP(w, r, emptyHandler)
		}}),
	}, &FallbackPlan{ErrorStatusCodes: []int{503}})

	err := h.ServeHTTP(w, r, nextStatus(204))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if atomic.LoadInt32(&secondCalled) != 0 {
		t.Error("client cancel must prevent fallback")
	}
}

func TestHandleFallbackInnerGroupsFreshPerAttempt(t *testing.T) {
	// inner route groups are scoped to a single attempt: group state
	// produced while trying one branch must not cause grouped routes
	// in a later branch to skip (JSON group names are arbitrary and
	// may collide across branches)
	failingInner := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.WriteHeader(502)
		return nil
	}}
	firstBranch := testBranch(nil, "g1", failingInner)
	firstBranch.Routes = append(firstBranch.Routes, testBranch(nil, "g1", staticHandler(200, "skipped")).Routes...)

	secondInner := testHandler{fn: func(w http.ResponseWriter, _ *http.Request, _ Handler) error {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
		return nil
	}}
	secondBranch := testBranch(nil, "g1", secondInner)

	r, w := newTestHandleRequest(t, "GET", "/", "")
	h := newTestHandler([]HandleBranch{firstBranch, secondBranch}, &FallbackPlan{StatusCodes: []int{502}})

	if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Errorf("expected second branch's grouped route to run: %d %q", w.Code, w.Body.String())
	}
}

func TestHandleFallbackConcurrent(t *testing.T) {
	h := newTestHandler([]HandleBranch{
		testBranch(pathMatcher("/fail"), "", staticHandler(502)),
		testBranch(nil, "", staticHandler(200, "ok")),
	}, &FallbackPlan{StatusCodes: []int{502}, RequestBodyBuffer: -1})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := "/"
			if i%2 == 0 {
				target = "/fail"
			}
			r, w := newTestHandleRequest(t, "POST", target, fmt.Sprintf("body-%d", i))
			if err := h.ServeHTTP(w, r, nextStatus(204)); err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if i%2 == 0 {
				if w.Code != 200 || w.Body.String() != "ok" {
					t.Errorf("expected fallback 200 'ok', got %d %q", w.Code, w.Body.String())
				}
			} else {
				if w.Code != 200 {
					t.Errorf("expected direct 200, got %d", w.Code)
				}
			}
		}(i)
	}
	wg.Wait()
}
