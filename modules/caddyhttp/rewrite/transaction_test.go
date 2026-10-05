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

package rewrite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

var errDownstream = errors.New("simulated downstream failure")

// trackingBody records whether it was read or replaced.
type trackingBody struct {
	read bool
}

func (b *trackingBody) Read(_ []byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func (b *trackingBody) Close() error { return nil }

// newTxnTestRequest prepares a request the same way the HTTP server
// does before handlers run: it carries a replacer and a variable table.
func newTxnTestRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	repl := caddy.NewReplacer()
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	r.RequestURI = r.URL.RequestURI()
	return caddyhttp.PrepareRequest(r, repl, nil, nil)
}

func newTxnRewrite(uri string, txn *Transaction) Rewrite {
	return Rewrite{URI: uri, Transaction: txn, logger: zap.NewNop()}
}

// mutateDownstream is invoked after the rewrite; it touches every part
// of the request state that a rollback must restore.
func mutateDownstream(w http.ResponseWriter, r *http.Request) error {
	r.Method = http.MethodDelete
	r.Host = "mutated.example"
	r.URL.Path = "/downstream"
	r.URL.RawPath = ""
	r.URL.RawQuery = "down=yes"
	r.URL.Fragment = "downfrag"
	r.RequestURI = r.URL.RequestURI()
	r.Header.Set("X-Downstream", "seen")
	caddyhttp.SetVar(r.Context(), "downstream_var", "set")
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	repl.Set("downstream_static", "set")
	repl.Map(func(key string) (any, bool) {
		if key == "downstream.mapped" {
			return "mapped", true
		}
		return nil, false
	})
	return nil
}

func TestTransactionRollbackOnError(t *testing.T) {
	r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig?keep=1#origfrag")
	body := &trackingBody{}
	r.Body = body
	urlPtr := r.URL
	headerMap := r.Header

	// a provider registered before the checkpoint must survive a rollback
	r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer).Map(func(key string) (any, bool) {
		if key == "txn.pre" {
			return "pre", true
		}
		return nil, false
	})

	rewr := newTxnRewrite("/rewritten?new=1#newfrag", &Transaction{OnError: transactionPolicyRollback})

	var downstreamSaw string
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		downstreamSaw = req.RequestURI
		if err := mutateDownstream(w, req); err != nil {
			return err
		}
		return caddyhttp.Error(http.StatusInternalServerError, errDownstream)
	})

	err := rewr.ServeHTTP(httptest.NewRecorder(), r, next)
	if !errors.Is(err, errDownstream) {
		t.Fatalf("expected downstream error to be propagated unchanged, got: %v", err)
	}

	// the downstream chain ran against the rewritten request; fragments
	// are never part of RequestURI, but they are on the URL
	if downstreamSaw != "/rewritten?new=1" {
		t.Errorf("downstream should have seen rewritten RequestURI, got %q", downstreamSaw)
	}

	// after the rollback, request metadata is back at the checkpoint
	if r.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", r.Method)
	}
	if r.Host != "orig.example" {
		t.Errorf("Host = %q, want orig.example", r.Host)
	}
	if r.URL.Path != "/orig" || r.URL.RawQuery != "keep=1" || r.URL.Fragment != "origfrag" {
		t.Errorf("URL not restored: %+v", r.URL)
	}
	if r.RequestURI != "/orig?keep=1" {
		t.Errorf("RequestURI = %q, want /orig?keep=1", r.RequestURI)
	}
	if r.Header.Get("X-Downstream") != "" {
		t.Errorf("downstream header survived rollback: %v", r.Header)
	}

	// restoration happens in place, pointers captured downstream stay valid
	if r.URL != urlPtr {
		t.Error("r.URL pointer changed during restore; it must be updated in place")
	}
	headerMap["Z-Probe"] = []string{"same-map"}
	if r.Header.Get("Z-Probe") != "same-map" {
		t.Error("r.Header map was replaced; it must be restored in place")
	}

	// placeholder state is restored too
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if _, ok := repl.Get("downstream_static"); ok {
		t.Error("static placeholder set downstream survived rollback")
	}
	if _, ok := repl.Get("downstream.mapped"); ok {
		t.Error("provider registered downstream survived rollback")
	}
	if v, _ := repl.GetString("txn.pre"); v != "pre" {
		t.Error("provider registered before the checkpoint did not survive rollback")
	}
	if v := caddyhttp.GetVar(r.Context(), "downstream_var"); v != nil {
		t.Errorf("variable set downstream survived rollback: %v", v)
	}

	// the body must never have been read or replaced
	if body.read {
		t.Error("request body was read while taking the checkpoint")
	}
	if r.Body != io.ReadCloser(body) {
		t.Error("request body was replaced")
	}
}

func TestTransactionCommitKeepsState(t *testing.T) {
	for _, tc := range []struct {
		name string
		txn  *Transaction
		err  error
	}{
		{
			name: "success always commits",
			txn:  &Transaction{OnError: transactionPolicyRollback, OnCancel: transactionPolicyRollback},
			err:  nil,
		},
		{
			name: "error commits by default",
			txn:  &Transaction{},
			err:  caddyhttp.Error(http.StatusBadGateway, errDownstream),
		},
		{
			name: "error commits when policy is commit",
			txn:  &Transaction{OnError: transactionPolicyCommit},
			err:  caddyhttp.Error(http.StatusBadGateway, errDownstream),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")
			rewr := newTxnRewrite("/rewritten", tc.txn)
			next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
				if err := mutateDownstream(w, req); err != nil {
					return err
				}
				return tc.err
			})

			err := rewr.ServeHTTP(httptest.NewRecorder(), r, next)
			if !errors.Is(err, tc.err) {
				t.Fatalf("expected propagated error %v, got %v", tc.err, err)
			}

			if r.Method != http.MethodDelete || r.URL.Path != "/downstream" {
				t.Errorf("commit should keep downstream state, got %s %s", r.Method, r.URL.Path)
			}
			if r.Header.Get("X-Downstream") != "seen" {
				t.Error("commit should keep downstream headers")
			}
			repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
			if v, _ := repl.GetString("downstream_static"); v != "set" {
				t.Error("commit should keep downstream placeholders")
			}
			if v := caddyhttp.GetVar(r.Context(), "downstream_var"); v != "set" {
				t.Error("commit should keep downstream variables")
			}
		})
	}
}

func TestTransactionCancelPrecedence(t *testing.T) {
	// canceled context AND an error wrapping context.Canceled, with
	// rollback_on_error configured but on_cancel left at commit: the
	// cancellation policy must win and the state is committed
	r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)

	rewr := newTxnRewrite("/rewritten", &Transaction{OnError: transactionPolicyRollback})
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		if err := mutateDownstream(w, req); err != nil {
			return err
		}
		return context.Canceled
	})

	err := rewr.ServeHTTP(httptest.NewRecorder(), r, next)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if r.URL.Path != "/downstream" {
		t.Errorf("cancel policy commit should win over rollback_on_error, got %s", r.URL.Path)
	}

	// now with rollback_on_cancel, the same cancellation restores the
	// checkpoint, even when the downstream chain returns no error
	r2 := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")
	ctx2, cancel2 := context.WithCancel(r2.Context())
	cancel2()
	r2 = r2.WithContext(ctx2)

	rewr2 := newTxnRewrite("/rewritten", &Transaction{OnCancel: transactionPolicyRollback})
	next2 := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		return mutateDownstream(w, req)
	})

	if err := rewr2.ServeHTTP(httptest.NewRecorder(), r2, next2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r2.URL.Path != "/orig" || r2.Header.Get("X-Downstream") != "" {
		t.Errorf("rollback_on_cancel should restore despite nil error, got %s %v", r2.URL.Path, r2.Header)
	}
}

func TestTransactionNested(t *testing.T) {
	r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")

	inner := newTxnRewrite("/c", &Transaction{OnError: transactionPolicyRollback})
	outer := newTxnRewrite("/b", &Transaction{OnError: transactionPolicyRollback})

	chain := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		return outer.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
			if req.URL.Path != "/b" {
				t.Errorf("inner checkpoint should be taken after outer rewrite, got %s", req.URL.Path)
			}
			return inner.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
				if err := mutateDownstream(w, req); err != nil {
					return err
				}
				return caddyhttp.Error(http.StatusInternalServerError, errDownstream)
			}))
		}))
	})

	err := chain.ServeHTTP(httptest.NewRecorder(), r)
	if !errors.Is(err, errDownstream) {
		t.Fatalf("expected error through nested transactions, got %v", err)
	}

	// inner restores to its checkpoint (/b), outer then to its own (/orig)
	if r.URL.Path != "/orig" {
		t.Errorf("nested rollback should restore the outer checkpoint, got %s", r.URL.Path)
	}
	if r.Header.Get("X-Downstream") != "" {
		t.Error("inner downstream header survived the outer rollback")
	}
	if v := caddyhttp.GetVar(r.Context(), "downstream_var"); v != nil {
		t.Errorf("inner downstream variable survived the outer rollback: %v", v)
	}
}

func TestTransactionNestedInnerCommitOuterRollback(t *testing.T) {
	r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")

	// the inner transaction commits even on error; the outer transaction
	// must still be able to roll back everything the inner chain committed
	inner := newTxnRewrite("/c", &Transaction{OnError: transactionPolicyCommit})
	outer := newTxnRewrite("/b", &Transaction{OnError: transactionPolicyRollback})

	chain := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		return outer.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
			return inner.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
				if err := mutateDownstream(w, req); err != nil {
					return err
				}
				return caddyhttp.Error(http.StatusInternalServerError, errDownstream)
			}))
		}))
	})

	if err := chain.ServeHTTP(httptest.NewRecorder(), r); !errors.Is(err, errDownstream) {
		t.Fatalf("expected error, got %v", err)
	}
	if r.URL.Path != "/orig" || r.Header.Get("X-Downstream") != "" {
		t.Errorf("outer rollback must undo an inner commit, got path=%s header=%v", r.URL.Path, r.Header)
	}
}

func TestTransactionRepeatedReentryIsIndependent(t *testing.T) {
	// a fresh local checkpoint must be taken on every invocation, so an
	// earlier rollback cannot influence a later request handling pass
	rewr := newTxnRewrite("/rewritten", &Transaction{OnError: transactionPolicyRollback})

	for range 2 {
		r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
			if err := mutateDownstream(w, req); err != nil {
				return err
			}
			return caddyhttp.Error(http.StatusInternalServerError, errDownstream)
		})
		if err := rewr.ServeHTTP(httptest.NewRecorder(), r, next); !errors.Is(err, errDownstream) {
			t.Fatalf("expected downstream error, got %v", err)
		}
		if r.URL.Path != "/orig" {
			t.Fatalf("each invocation must restore independently, got %s", r.URL.Path)
		}
	}
}

func TestTransactionJSONAndValidate(t *testing.T) {
	var rewr Rewrite
	err := json.Unmarshal([]byte(`{
		"uri": "/new",
		"transaction": {"on_error": "rollback", "on_cancel": "rollback"}
	}`), &rewr)
	if err != nil {
		t.Fatalf("unmarshaling transaction config: %v", err)
	}
	if rewr.Transaction == nil ||
		rewr.Transaction.OnError != transactionPolicyRollback ||
		rewr.Transaction.OnCancel != transactionPolicyRollback {
		t.Fatalf("transaction config not parsed: %+v", rewr.Transaction)
	}
	if err := rewr.Validate(); err != nil {
		t.Errorf("valid policies should pass validation: %v", err)
	}

	var badRewr Rewrite
	if err := json.Unmarshal([]byte(`{"transaction": {"on_error": "maybe"}}`), &badRewr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := badRewr.Validate(); err == nil {
		t.Error("expected validation error for unrecognized policy")
	}
}

func TestNoTransactionIsZeroOverhead(t *testing.T) {
	// when no transaction is configured, behavior is exactly as before:
	// rewrite runs, downstream state persists, error propagates
	r := newTxnTestRequest(t, http.MethodGet, "http://orig.example/orig")
	rewr := Rewrite{URI: "/rewritten", logger: zap.NewNop()}
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		req.Header.Set("X-Downstream", "seen")
		caddyhttp.SetVar(req.Context(), "downstream_var", "set")
		return caddyhttp.Error(http.StatusInternalServerError, errDownstream)
	})

	err := rewr.ServeHTTP(httptest.NewRecorder(), r, next)
	if !errors.Is(err, errDownstream) {
		t.Fatalf("expected error propagation, got %v", err)
	}
	if r.URL.Path != "/rewritten" || r.Header.Get("X-Downstream") != "seen" {
		t.Errorf("state changed without a transaction: path=%s header=%v", r.URL.Path, r.Header)
	}
	if v := caddyhttp.GetVar(r.Context(), "downstream_var"); v != "set" {
		t.Errorf("variable did not persist without a transaction: %v", v)
	}
}
