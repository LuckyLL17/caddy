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
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Transaction outcome policies.
const (
	// transactionPolicyCommit keeps the rewritten request state when the
	// downstream handler chain finishes. It is the default policy.
	transactionPolicyCommit = "commit"

	// transactionPolicyRollback restores the checkpoint captured before
	// the rewrite was applied.
	transactionPolicyRollback = "rollback"
)

// Transaction configures an optional request-change transaction around
// a rewrite. When configured, a restorable checkpoint of the request
// method, URI (path, query string and fragment), host, headers, and the
// request-scoped placeholder state (the replacer's static values and
// registered providers, and the variable table behind
// {http.vars.*} placeholders) is taken immediately before the rewrite
// runs.
//
// The request body is never read, copied, or buffered: the snapshot
// only captures request metadata, so a downstream handler actively
// consuming the body is unaffected.
//
// The transaction is decided exactly once, when the handler chain
// directly following this rewrite returns:
//
//   - If the request context is canceled (the client disconnected, or
//     the downstream chain returns an error wrapping
//     context.Canceled), OnCancel applies. Cancellation always takes
//     precedence over OnError.
//   - Otherwise, if the downstream chain returns a non-nil error,
//     OnError applies. Retries performed inside downstream handlers
//     (for example by the reverse proxy) are internal to that chain
//     and do not interact with this boundary; only the final result is
//     evaluated.
//   - Otherwise the transaction commits.
//
// A commit leaves the request exactly as the downstream chain left it.
// A rollback restores every captured field in place on the same
// request, URL, and header map, and then returns the downstream error
// unchanged; error propagation is never altered.
//
// Transactions compose with nested routes: nested transactions are
// independent checkpoints that restore in stack order. An error route
// that contains its own rewrites opens fresh transactions and follows
// the same rules if error handling errors again; snapshots are local
// to a single request, so concurrent requests never share state.
type Transaction struct {
	// OnError decides the outcome when the handler chain downstream of
	// the rewrite returns an error that is not caused by request
	// cancellation. "commit" (the default) keeps the rewritten request;
	// "rollback" restores the checkpoint before the error continues up
	// the chain.
	OnError string `json:"on_error,omitempty"`

	// OnCancel decides the outcome when the request context is canceled
	// while the downstream handler chain runs, including when that
	// chain returns an error wrapping context.Canceled. "commit" (the
	// default) keeps the rewritten request; "rollback" restores the
	// checkpoint. Cancellation takes precedence over OnError.
	OnCancel string `json:"on_cancel,omitempty"`
}

// validate checks that the configured policies are recognized.
func (t Transaction) validate() error {
	for _, policy := range []struct{ field, value string }{
		{"on_error", t.OnError},
		{"on_cancel", t.OnCancel},
	} {
		switch policy.value {
		case "", transactionPolicyCommit, transactionPolicyRollback:
		default:
			return errors.New("transaction." + policy.field + ": unrecognized policy " + strconv.Quote(policy.value) + "; must be \"commit\" or \"rollback\"")
		}
	}
	return nil
}

// shouldRollback applies the deterministic decision rules documented on
// Transaction: cancellation first, then a returned error, otherwise
// commit.
func (t *Transaction) shouldRollback(r *http.Request, err error) bool {
	if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return t.OnCancel == transactionPolicyRollback
	}
	if err != nil {
		return t.OnError == transactionPolicyRollback
	}
	return false
}

// requestSnapshot is a per-request, restorable checkpoint of the
// request metadata that a rewrite (and handlers acting on the rewritten
// request) may mutate. It deliberately excludes the request body.
type requestSnapshot struct {
	method     string
	host       string
	requestURI string
	url        url.URL
	header     http.Header
	replacer   caddy.ReplacerSnapshot
	vars       caddyhttp.VarsSnapshot
}

// takeSnapshot captures a restorable checkpoint of r and repl. The
// repl is the request-scoped replacer obtained from r.
func takeSnapshot(r *http.Request, repl *caddy.Replacer) requestSnapshot {
	return requestSnapshot{
		method:     r.Method,
		host:       r.Host,
		requestURI: r.RequestURI,
		url:        cloneURL(r.URL),
		header:     r.Header.Clone(),
		replacer:   repl.Snapshot(),
		vars:       caddyhttp.SnapshotVars(r.Context()),
	}
}

// cloneURL returns a deep copy of u. All url.URL fields are value
// types except User, which holds a pointer to opaque userinfo.
func cloneURL(u *url.URL) url.URL {
	u2 := *u
	if u.User != nil {
		if password, ok := u.User.Password(); ok {
			u2.User = url.UserPassword(u.User.Username(), password)
		} else {
			u2.User = url.User(u.User.Username())
		}
	}
	return u2
}

// restore reverts r and repl to the checkpoint. Every field is written
// back in place: the request, URL, and header map pointers observed by
// downstream handlers stay valid.
func (s requestSnapshot) restore(r *http.Request, repl *caddy.Replacer) {
	r.Method = s.method
	r.Host = s.host
	r.RequestURI = s.requestURI

	*r.URL = s.url

	for key := range r.Header {
		delete(r.Header, key)
	}
	for key, vals := range s.header {
		r.Header[key] = append([]string(nil), vals...)
	}

	repl.Restore(s.replacer)
	s.vars.Restore(r.Context())
}
