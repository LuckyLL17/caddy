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

package requestbody

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func newReplayRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/upload", strings.NewReader(body))
	ctx := req.Context()
	ctx = context.WithValue(ctx, caddy.ReplacerCtxKey, caddy.NewReplacer())
	ctx = context.WithValue(ctx, caddyhttp.VarsCtxKey, map[string]any{})
	return req.WithContext(ctx)
}

// TestServeHTTPReplayRoundTrip verifies the middleware installs the
// replay policy and later handlers inside the subtree can re-read the
// same body without changing Content-Length.
func TestServeHTTPReplayRoundTrip(t *testing.T) {
	const body = "the replayable payload"

	rb := RequestBody{
		Replay: &caddyhttp.BodyReplayConfig{
			MemoryMaxSize: 64,
			MaxSize:       4096,
			SpillDir:      t.TempDir(),
			Allow:         []string{caddyhttp.BodyReplayScopeErrorRoutesName},
		},
		logger: zap.NewNop(),
	}
	if err := rb.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}

	req := newReplayRequest(t, body)

	var first, second string
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		first = string(data)

		if !caddyhttp.BodyReplayAllows(r, caddyhttp.BodyReplayScopeNestedRoutes) {
			return errors.New("nested replay not prepared")
		}
		r = caddyhttp.PrepareBodyReplay(r, caddyhttp.BodyReplayScopeNestedRoutes)
		data, err = io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		second = string(data)
		return nil
	})

	if err := rb.ServeHTTP(httptest.NewRecorder(), req, next); err != nil {
		t.Fatalf("handler failed: %v", err)
	}
	if first != body || second != body {
		t.Fatalf("body reads differ: first=%q second=%q want=%q", first, second, body)
	}
	if req.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", req.ContentLength, len(body))
	}

	// cleanup runs through the request-scoped state
	req.Body.Close()
}

// TestServeHTTPNoReplayKeepsOneShotStream verifies the default
// behavior: without a replay block, a consumed body is gone.
func TestServeHTTPNoReplayKeepsOneShotStream(t *testing.T) {
	rb := RequestBody{}

	req := newReplayRequest(t, "one-shot")
	next := caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return err
		}
		if caddyhttp.BodyReplayAllows(r, caddyhttp.BodyReplayScopeNestedRoutes) {
			return errors.New("replay must not be prepared without a policy")
		}
		return nil
	})

	if err := rb.ServeHTTP(httptest.NewRecorder(), req, next); err != nil {
		t.Fatalf("handler failed: %v", err)
	}
}

// TestServeHTTPReplayReject verifies the policy rejects oversized
// bodies with 413.
func TestServeHTTPReplayReject(t *testing.T) {
	rb := RequestBody{
		Replay: &caddyhttp.BodyReplayConfig{MemoryMaxSize: 8, MaxSize: 16},
		logger: zap.NewNop(),
	}
	if err := rb.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}

	req := newReplayRequest(t, strings.Repeat("a", 100))
	next := caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
		_, err := io.ReadAll(r.Body)
		return err
	})

	err := rb.ServeHTTP(httptest.NewRecorder(), req, next)
	var handlerErr caddyhttp.HandlerError
	if !errors.As(err, &handlerErr) || handlerErr.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 handler error, got %v", err)
	}
}

// TestParseCaddyfileReplay verifies the replay block adaptation.
func TestParseCaddyfileReplay(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
		request_body {
			max_size 10MB
			replay {
				max_size 5MB
				memory 1MB
				spill_dir /var/tmp/caddy
				allow error_routes retries
				on_exceed degrade
				on_cancel retain
			}
		}`)
	h := httpcaddyfile.Helper{Dispenser: d}
	mh, err := parseCaddyfile(h)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	rb := mh.(*RequestBody)

	if rb.MaxSize != 10_000_000 {
		t.Errorf("max_size = %d, want 10000000", rb.MaxSize)
	}
	if rb.Replay == nil {
		t.Fatal("replay policy not parsed")
	}
	if rb.Replay.MaxSize != 5_000_000 {
		t.Errorf("replay max_size = %d, want 5000000", rb.Replay.MaxSize)
	}
	if rb.Replay.MemoryMaxSize != 1_000_000 {
		t.Errorf("replay memory = %d, want 1000000", rb.Replay.MemoryMaxSize)
	}
	if rb.Replay.SpillDir != "/var/tmp/caddy" {
		t.Errorf("spill_dir = %q", rb.Replay.SpillDir)
	}
	if rb.Replay.OnExceed != "degrade" || rb.Replay.OnCancel != "retain" {
		t.Errorf("modes = %q/%q", rb.Replay.OnExceed, rb.Replay.OnCancel)
	}
	if err := rb.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision failed: %v", err)
	}
	if !rb.Replay.Allows(caddyhttp.BodyReplayScopeErrorRoutes) ||
		!rb.Replay.Allows(caddyhttp.BodyReplayScopeRetries) {
		t.Error("parsed scopes not enabled after provision")
	}
}

// TestParseCaddyfileReplayErrors verifies malformed replay blocks fail.
func TestParseCaddyfileReplayErrors(t *testing.T) {
	for _, input := range []string{
		`request_body { replay { on_exceed explode } }`,
		`request_body { replay { on_cancel maybe } }`,
		`request_body { replay { allow universe } }`,
		`request_body { replay { memory not-a-size } }`,
		`request_body { replay { bogus } }`,
	} {
		t.Run(input, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(input)
			h := httpcaddyfile.Helper{Dispenser: d}
			if _, err := parseCaddyfile(h); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}
