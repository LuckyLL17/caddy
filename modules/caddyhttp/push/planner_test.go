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

package push

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// fakeResponseWriter records push attempts and response writes.
type fakeResponseWriter struct {
	header      http.Header
	pushes      []pushedRequest
	failTargets map[string]error
	statusCode  int
	wroteHeader bool
	flushed     bool
	body        bytes.Buffer
}

type pushedRequest struct {
	target string
	method string
	header http.Header
}

func newFakeResponseWriter() *fakeResponseWriter {
	return &fakeResponseWriter{
		header:      http.Header{},
		failTargets: map[string]error{},
		statusCode:  http.StatusOK,
	}
}

func (f *fakeResponseWriter) Header() http.Header { return f.header }

func (f *fakeResponseWriter) WriteHeader(statusCode int) {
	f.wroteHeader = true
	f.statusCode = statusCode
}

func (f *fakeResponseWriter) Write(b []byte) (int, error) {
	f.wroteHeader = true
	return f.body.Write(b)
}

func (f *fakeResponseWriter) Flush() { f.flushed = true }

func (f *fakeResponseWriter) Push(target string, opts *http.PushOptions) error {
	if err, ok := f.failTargets[target]; ok {
		return err
	}
	f.pushes = append(f.pushes, pushedRequest{
		target: target,
		method: opts.Method,
		header: opts.Header,
	})
	return nil
}

func (f *fakeResponseWriter) pushTargets() []string {
	targets := make([]string, len(f.pushes))
	for i, p := range f.pushes {
		targets[i] = p.target
	}
	return targets
}

// newPlannedRequest builds a request with the context values the
// push handler requires.
func newPlannedRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	ctx := context.Background()
	ctx = context.WithValue(ctx, caddy.ReplacerCtxKey, caddy.NewReplacer())
	ctx = context.WithValue(ctx, caddyhttp.ServerCtxKey, &caddyhttp.Server{})
	ctx = context.WithValue(ctx, caddyhttp.VarsCtxKey, map[string]any{})
	r := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	return r
}

func newTestPlanner(h Handler, plan ResourcePlan, fp *fakeResponseWriter, r *http.Request) *pushPlanner {
	hdr := http.Header{}
	hdr.Set(pushHeader, "1")
	return newPushPlanner(h, plan, fp, hdr, r, caddy.NewReplacer(), false)
}

func TestCanonicalizeTarget(t *testing.T) {
	reqURL := mustParseURL(t, "https://example.com/dir/page?q=1")
	reqURL.Host = "" // server-side requests carry the host in r.Host

	for i, tc := range []struct {
		name       string
		raw        string
		reqHost    string
		wantTarget string
		wantReason string
	}{
		{
			name:       "absolute path",
			raw:        "/static/app.js",
			reqHost:    "example.com",
			wantTarget: "/static/app.js",
		},
		{
			name:       "fragment stripped",
			raw:        "/static/app.js#chunk",
			reqHost:    "example.com",
			wantTarget: "/static/app.js",
		},
		{
			name:       "query kept",
			raw:        "/static/app.js?v=1#x",
			reqHost:    "example.com",
			wantTarget: "/static/app.js?v=1",
		},
		{
			name:       "relative resolved",
			raw:        "sub/style.css",
			reqHost:    "example.com",
			wantTarget: "/dir/sub/style.css",
		},
		{
			name:       "parent relative resolved",
			raw:        "../style.css",
			reqHost:    "example.com",
			wantTarget: "/style.css",
		},
		{
			name:       "dot segments cleaned",
			raw:        "/a/./b/../c/",
			reqHost:    "example.com",
			wantTarget: "/a/c/",
		},
		{
			name:       "query only reference",
			raw:        "?x=2",
			reqHost:    "example.com",
			wantTarget: "/dir/page?x=2",
		},
		{
			name:       "fragment only reference",
			raw:        "#section",
			reqHost:    "example.com",
			wantTarget: "/dir/page?q=1",
		},
		{
			name:       "same authority https absolute",
			raw:        "https://example.com/a?x=1",
			reqHost:    "example.com",
			wantTarget: "/a?x=1",
		},
		{
			name:       "same authority case insensitive host",
			raw:        "https://EXAMPLE.com/a",
			reqHost:    "example.com",
			wantTarget: "/a",
		},
		{
			name:       "same authority default port omitted",
			raw:        "https://example.com/a",
			reqHost:    "example.com:443",
			wantTarget: "/a",
		},
		{
			name:       "protocol relative same authority",
			raw:        "//example.com/a",
			reqHost:    "example.com",
			wantTarget: "/a",
		},
		{
			name:       "different authority absolute",
			raw:        "https://other.com/a",
			reqHost:    "example.com",
			wantReason: skipExternal,
		},
		{
			name:       "different authority protocol relative",
			raw:        "//other.com/a",
			reqHost:    "example.com",
			wantReason: skipExternal,
		},
		{
			name:       "non http scheme",
			raw:        "mailto:a@b.com",
			reqHost:    "example.com",
			wantReason: skipExternal,
		},
		{
			name:       "empty",
			raw:        "   ",
			reqHost:    "example.com",
			wantReason: skipInvalidTarget,
		},
		{
			name:       "malformed",
			raw:        "://bad",
			reqHost:    "example.com",
			wantReason: skipInvalidTarget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, key, reason := canonicalizeTarget(tc.raw, reqURL, tc.reqHost)
			if reason != tc.wantReason {
				t.Fatalf("case %d: expected reason %q, got %q", i, tc.wantReason, reason)
			}
			if reason == "" && target != tc.wantTarget {
				t.Fatalf("case %d: expected target %q, got %q", i, tc.wantTarget, target)
			}
			if reason == "" && key != target {
				t.Fatalf("case %d: expected dedup key %q, got %q", i, target, key)
			}
		})
	}
}

func TestResourcePlanValidate(t *testing.T) {
	for i, tc := range []struct {
		plan    ResourcePlan
		wantErr bool
	}{
		{ResourcePlan{Source: "configured", Order: "configured_first", OnError: "abort"}, false},
		{ResourcePlan{Source: "link", Order: "link_first", OnError: "continue"}, false},
		{ResourcePlan{Source: "both"}, false},
		{ResourcePlan{}, false},
		{ResourcePlan{Source: "nope"}, true},
		{ResourcePlan{Order: "later"}, true},
		{ResourcePlan{OnError: "panic"}, true},
		{ResourcePlan{MaxResources: -1}, true},
		{ResourcePlan{Budget: -1}, true},
	} {
		plan := tc.plan
		plan.normalize()
		err := plan.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("case %d: expected error for %+v", i, tc.plan)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("case %d: unexpected error: %v", i, err)
		}
	}
}

func TestPlannerConfiguredDedup(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{
		logger: zap.NewNop(),
		Resources: []Resource{
			{Target: "/a"},
			{Target: "/a#frag"},
			{Method: http.MethodHead, Target: "/a?x=1"},
			{Target: "/a?x=1#top"},
			{Target: "/b/"},
			{Target: "/b"},
		},
	}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceConfigured}, fp, r)
	planner.commitConfiguredPhase()

	// same canonical URI pushes once, first declaration wins;
	// /b/ and /b are distinct because the trailing slash matters
	got := fp.pushTargets()
	want := []string{"/a", "/a?x=1", "/b/", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}

	// dedup keeps the first declaration's method: /a?x=1 was
	// first declared with HEAD
	if fp.pushes[0].method != http.MethodGet || fp.pushes[1].method != http.MethodHead {
		t.Errorf("expected dedup to keep first declaration method, got %+v", fp.pushes)
	}
}

func TestPlannerCrossSourceDedupConfiguredFirst(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{{Target: "/a"}, {Target: "/b"}}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceBoth, Order: orderConfiguredFirst}, fp, r)

	planner.commitConfiguredPhase()
	planner.commitLinkPhase([]string{"</a>,</c>"})

	got := fp.pushTargets()
	want := []string{"/a", "/b", "/c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPlannerLinkFirstOrder(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{{Target: "/b"}}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceBoth, Order: orderLinkFirst}, fp, r)

	// configured phase must not push early in link-first order
	planner.commitConfiguredPhase()
	if len(fp.pushes) != 0 {
		t.Fatalf("expected no early configured push, got %v", fp.pushTargets())
	}

	planner.commitLinkPhase([]string{"</c>,</b>"})

	got := fp.pushTargets()
	want := []string{"/c", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPlannerLinkFirstWithoutLinkHeaders(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{{Target: "/b"}}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceBoth, Order: orderLinkFirst}, fp, r)

	planner.commitConfiguredPhase()
	// response carries no Link header fields
	planner.commitLinkPhase(nil)

	if got, want := fp.pushTargets(), []string{"/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected configured resources to still commit, got %v", got)
	}
}

func TestPlannerMaxResources(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{{Target: "/a"}, {Target: "/b"}, {Target: "/c"}}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceConfigured, MaxResources: 2}, fp, r)

	planner.commitConfiguredPhase()

	got := fp.pushTargets()
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	if planner.count != 2 {
		t.Errorf("expected count 2, got %d", planner.count)
	}
}

func TestPlannerBudget(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{
		{Target: "/a"},  // 2 bytes
		{Target: "/bb"}, // 3 bytes, total 5
		{Target: "/c"},  // 2 bytes, would total 7
	}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceConfigured, Budget: 5}, fp, r)

	planner.commitConfiguredPhase()

	got := fp.pushTargets()
	want := []string{"/a", "/bb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	if planner.spent != 5 {
		t.Errorf("expected 5 spent bytes, got %d", planner.spent)
	}
}

func TestPlannerNopushAndExternal(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop()}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceLink}, fp, r)

	planner.commitLinkPhase([]string{
		"</local>; nopush",
		"</ok>; rel=preload; as=style",
		"<https://other.com/remote>",
		"<//example.com/same>",
		"<relative/app.js>",
	})

	got := fp.pushTargets()
	want := []string{"/ok", "/same", "/relative/app.js"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPlannerLinkPhaseOnce(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop()}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceLink}, fp, r)

	planner.commitLinkPhase([]string{"</a>"})
	planner.commitLinkPhase([]string{"</a>,</b>"})

	got := fp.pushTargets()
	want := []string{"/a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPlannerCrossHandlerLinkGuard(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	caddyhttp.SetVar(r.Context(), pushedLink, true)
	h := Handler{logger: zap.NewNop()}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceLink}, fp, r)

	planner.commitLinkPhase([]string{"</a>"})

	if len(fp.pushes) != 0 {
		t.Fatalf("expected no pushes when another handler already pushed links, got %v", fp.pushTargets())
	}
}

func TestPlannerPushErrorAbortAndContinue(t *testing.T) {
	pushErr := errors.New("concurrent streams are full")

	for i, tc := range []struct {
		name string
		mode string
		want []string
	}{
		{"abort", errorAbort, []string{"/a"}},
		{"continue", errorContinue, []string{"/a", "/c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakeResponseWriter()
			fp.failTargets["/b"] = pushErr
			r := newPlannedRequest(t, "https://example.com/index")
			h := Handler{logger: zap.NewNop(), Resources: []Resource{
				{Target: "/a"}, {Target: "/b"}, {Target: "/c"},
			}}
			planner := newTestPlanner(h, ResourcePlan{Source: sourceConfigured, OnError: tc.mode}, fp, r)
			planner.commitConfiguredPhase()

			got := fp.pushTargets()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("case %d: expected %v, got %v", i, tc.want, got)
			}
			if tc.mode == errorAbort && !planner.stopped {
				t.Errorf("case %d: expected planner to be stopped", i)
			}
		})
	}
}

func TestPlannerCanceledContext(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	cancel()

	h := Handler{logger: zap.NewNop(), Resources: []Resource{{Target: "/a"}, {Target: "/b"}}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceConfigured, OnError: errorContinue}, fp, r)
	planner.commitConfiguredPhase()

	if len(fp.pushes) != 0 {
		t.Fatalf("expected no pushes on canceled context, got %v", fp.pushTargets())
	}
	if !planner.stopped {
		t.Error("expected planner to be stopped")
	}
}

func TestPlannerInvalidMethod(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{
		{Method: http.MethodPost, Target: "/post"},
		{Method: http.MethodHead, Target: "/head"},
		{Target: "/get"},
	}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceConfigured}, fp, r)
	planner.commitConfiguredPhase()

	if got, want := fp.pushTargets(), []string{"/head", "/get"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	if fp.pushes[0].method != http.MethodHead {
		t.Errorf("expected HEAD method, got %q", fp.pushes[0].method)
	}
}

func TestPlannedLinkPusherWriteInterception(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop()}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceBoth, Order: orderLinkFirst}, fp, r)
	lp := &plannedLinkPusher{
		ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: fp},
		planner:               planner,
	}

	// implicit write via Write: links are committed from the
	// header snapshot before the body bytes go out
	fp.Header().Set("Link", "</a>,</b>")
	n, err := lp.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("unexpected write result: n=%d err=%v", n, err)
	}
	if !fp.wroteHeader {
		t.Error("expected underlying write to occur")
	}

	// mutations after the response started do not add pushes
	fp.Header().Set("Link", "</c>")
	lp.WriteHeader(http.StatusOK)
	lp.Flush()
	lp.finish()

	if got, want := fp.pushTargets(), []string{"/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPlannedLinkPusherFlushTriggers(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop()}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceLink}, fp, r)
	lp := &plannedLinkPusher{
		ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: fp},
		planner:               planner,
	}

	fp.Header().Set("Link", "</a>")
	lp.Flush()

	if !fp.flushed {
		t.Error("expected underlying flush to occur")
	}
	if got, want := fp.pushTargets(), []string{"/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPlannedLinkPusherFinishWithoutWrite(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{logger: zap.NewNop(), Resources: []Resource{{Target: "/b"}}}
	planner := newTestPlanner(h, ResourcePlan{Source: sourceBoth, Order: orderLinkFirst}, fp, r)
	lp := &plannedLinkPusher{
		ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: fp},
		planner:               planner,
	}

	// handler sets links but never writes; finish gives the
	// deferred phase exactly one chance
	fp.Header().Set("Link", "</a>")
	lp.finish()
	lp.finish()

	if got, want := fp.pushTargets(), []string{"/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestSnapshotLinksIsIndependent(t *testing.T) {
	hdr := http.Header{}
	hdr.Add("Link", "</a>")
	snapshot := snapshotLinks(hdr)

	hdr.Set("Link", "</changed>")
	hdr.Add("Link", "</extra>")

	if got, want := snapshot, []string{"</a>"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestServeHTTPNonPusherSkipped(t *testing.T) {
	// plain httptest.ResponseRecorder does not implement Pusher
	r := newPlannedRequest(t, "https://example.com/index")
	rec := httptest.NewRecorder()
	called := false
	h := Handler{
		logger:    zap.NewNop(),
		Resources: []Resource{{Target: "/a"}},
		Plan:      &ResourcePlan{Source: sourceConfigured},
	}
	err := h.ServeHTTP(rec, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		called = true
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected next handler to be called")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
}

func TestServeHTTPRecursivePushShortCircuit(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	r.Header.Set(pushHeader, "1")
	called := false
	h := Handler{
		logger:    zap.NewNop(),
		Resources: []Resource{{Target: "/a"}},
		Plan:      &ResourcePlan{Source: sourceConfigured},
	}
	err := h.ServeHTTP(fp, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		called = true
		return nil
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called || len(fp.pushes) != 0 {
		t.Errorf("expected no pushes and next handler called, got called=%v pushes=%v", called, fp.pushes)
	}
}

func TestServeHTTPlannedConfiguredFirstEndToEnd(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{
		logger:    zap.NewNop(),
		Resources: []Resource{{Target: "/a"}},
		Plan:      &ResourcePlan{Source: sourceBoth, Order: orderConfiguredFirst},
	}
	err := h.ServeHTTP(fp, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Link", "</a>,</b>")
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// /a configured pushes first and is not repeated from Link
	if got, want := fp.pushTargets(), []string{"/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestServeHTTPlannedNextErrorStillFinishes(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	nextErr := errors.New("handler failed")
	h := Handler{
		logger:    zap.NewNop(),
		Resources: []Resource{{Target: "/b"}},
		Plan:      &ResourcePlan{Source: sourceBoth, Order: orderLinkFirst},
	}
	err := h.ServeHTTP(fp, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Link", "</a>")
		return nextErr
	}))
	if !errors.Is(err, nextErr) {
		t.Fatalf("expected next error to propagate, got %v", err)
	}
	// link-first ordering still commits links then configured
	if got, want := fp.pushTargets(), []string{"/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestServeHTTPlannedSourceConfiguredSkipsLinks(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{
		logger:    zap.NewNop(),
		Resources: []Resource{{Target: "/a"}},
		Plan:      &ResourcePlan{Source: sourceConfigured},
	}
	err := h.ServeHTTP(fp, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Link", "</should-not-push>")
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := fp.pushTargets(), []string{"/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// TestServeHTTPLegacyBehaviorUnchanged proves the unplanned path
// still pushes configured resources verbatim, including duplicates,
// and parses Link headers as before.
func TestServeHTTPLegacyBehaviorUnchanged(t *testing.T) {
	fp := newFakeResponseWriter()
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{
		logger: zap.NewNop(),
		Resources: []Resource{
			{Target: "/a"},
			{Target: "/a"},
		},
	}
	err := h.ServeHTTP(fp, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Link", "</b>; rel=preload, </c>; nopush")
		w.WriteHeader(http.StatusOK)
		// a second WriteHeader must not push links again
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := fp.pushTargets(), []string{"/a", "/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestServeHTTPLegacyPushErrorStopsConfigured(t *testing.T) {
	fp := newFakeResponseWriter()
	fp.failTargets["/a"] = errors.New("push failed")
	r := newPlannedRequest(t, "https://example.com/index")
	h := Handler{
		logger:    zap.NewNop(),
		Resources: []Resource{{Target: "/a"}, {Target: "/b"}},
	}
	err := h.ServeHTTP(fp, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// legacy behavior: a failed configured push breaks the loop
	if got, want := fp.pushTargets(), []string{}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("could not parse %q: %v", raw, err)
	}
	return u
}
