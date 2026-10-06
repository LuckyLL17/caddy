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
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Resource plans select which resources are pushed and bound how
// much pushing is allowed per request.
const (
	// sourceConfigured pushes only the resources listed in the
	// handler configuration.
	sourceConfigured = "configured"

	// sourceLink pushes only resources found in Link response
	// header fields (excluding nopush resources).
	sourceLink = "link"

	// sourceBoth combines configured and Link resources.
	sourceBoth = "both"

	// orderConfiguredFirst commits configured resources before
	// Link resources.
	orderConfiguredFirst = "configured_first"

	// orderLinkFirst commits Link resources before configured
	// resources.
	orderLinkFirst = "link_first"

	// errorAbort stops pushing remaining candidates as soon as
	// a push attempt fails.
	errorAbort = "abort"

	// errorContinue logs a failed push attempt and continues
	// with the remaining candidates.
	errorContinue = "continue"
)

// ResourcePlan describes how push candidates are gathered, ordered,
// de-duplicated, bounded and committed for a single request. A plan
// is optional: when unset, the push handler behaves exactly as it
// does without one.
type ResourcePlan struct {
	// Source selects where push candidates come from: "configured"
	// (resources in the handler config), "link" (Link response
	// header fields), or "both". Default: "both".
	Source string `json:"source,omitempty"`

	// Order determines the candidate order when Source is "both":
	// "configured_first" or "link_first". Default:
	// "configured_first".
	Order string `json:"order,omitempty"`

	// MaxResources is the maximum number of resources pushed per
	// request, counted after de-duplication. Zero means unlimited.
	MaxResources int `json:"max_resources,omitempty"`

	// Budget is the cumulative byte budget per request, measured as
	// the combined length of the committed push target URIs (path
	// and query). A candidate that would exceed the budget is not
	// pushed. Zero means unlimited.
	Budget int64 `json:"budget,omitempty"`

	// OnError determines what happens when a push attempt fails:
	// "abort" stops pushing remaining candidates; "continue" logs
	// the failure and keeps going. Default: "abort".
	OnError string `json:"on_error,omitempty"`
}

// normalize fills in defaults on the plan.
func (p *ResourcePlan) normalize() {
	if p.Source == "" {
		p.Source = sourceBoth
	}
	if p.Order == "" {
		p.Order = orderConfiguredFirst
	}
	if p.OnError == "" {
		p.OnError = errorAbort
	}
}

// Validate ensures the plan configuration is usable.
func (p ResourcePlan) Validate() error {
	switch p.Source {
	case sourceConfigured, sourceLink, sourceBoth:
	default:
		return fmt.Errorf("resource plan source must be one of 'configured', 'link', or 'both', got: %s", p.Source)
	}
	switch p.Order {
	case orderConfiguredFirst, orderLinkFirst:
	default:
		return fmt.Errorf("resource plan order must be 'configured_first' or 'link_first', got: %s", p.Order)
	}
	switch p.OnError {
	case errorAbort, errorContinue:
	default:
		return fmt.Errorf("resource plan on_error must be 'abort' or 'continue', got: %s", p.OnError)
	}
	if p.MaxResources < 0 {
		return fmt.Errorf("resource plan max_resources must be non-negative, got: %d", p.MaxResources)
	}
	if p.Budget < 0 {
		return fmt.Errorf("resource plan budget must be non-negative, got: %d", p.Budget)
	}
	return nil
}

func (p ResourcePlan) includes(source string) bool {
	return p.Source == source || p.Source == sourceBoth
}

// pushCandidate is a resource that may be committed by the planner.
// Configured candidates undergo placeholder replacement; candidates
// parsed from Link headers do not.
type pushCandidate struct {
	method string
	target string
	linked bool
}

// Reasons a candidate is not pushed, used in debug logs.
const (
	skipInvalidTarget = "invalid_target"
	skipExternal      = "external"
	skipDuplicate     = "duplicate"
	skipResourceLimit = "max_resources"
	skipBudget        = "budget"
	skipMethod        = "invalid_method"
)

// pushPlanner maintains the independent state of one request's push
// plan: the admitted (de-duplicated) resources, the counters, and a
// snapshot of the synthetic push request headers.
type pushPlanner struct {
	plan      ResourcePlan
	resources []Resource
	pusher    http.Pusher
	header    http.Header
	request   *http.Request
	repl      *caddy.Replacer
	logger    *zap.Logger

	shouldLogCredentials bool

	seen      map[string]struct{}
	count     int
	spent     int64
	stopped   bool
	linksDone bool
}

func newPushPlanner(h Handler, plan ResourcePlan, pusher http.Pusher, hdr http.Header, r *http.Request, repl *caddy.Replacer, shouldLogCredentials bool) *pushPlanner {
	plan.normalize()
	return &pushPlanner{
		plan:                 plan,
		resources:            h.Resources,
		pusher:               pusher,
		header:               hdr,
		request:              r,
		repl:                 repl,
		logger:               h.logger,
		shouldLogCredentials: shouldLogCredentials,
		seen:                 make(map[string]struct{}),
	}
}

// commit admits and pushes candidates in order. It stops early when
// the request context is canceled or, depending on the error policy,
// a push attempt fails.
func (p *pushPlanner) commit(candidates []pushCandidate) {
	for _, cand := range candidates {
		if p.stopped {
			return
		}
		if err := p.request.Context().Err(); err != nil {
			p.stopped = true
			if c := p.logger.Check(zapcore.DebugLevel, "stopping push plan, request done"); c != nil {
				c.Write(zap.Error(err))
			}
			return
		}

		target, key, method, reason := p.admit(cand)
		if reason != "" {
			if c := p.logger.Check(zapcore.DebugLevel, "skipping push resource"); c != nil {
				c.Write(
					zap.String("uri", p.request.RequestURI),
					zap.String("push_target", cand.target),
					zap.String("reason", reason),
				)
			}
			continue
		}

		// reserve the resource before the push attempt so a
		// failed attempt is not retried via a later declaration
		p.seen[key] = struct{}{}
		p.count++
		p.spent += int64(len(target))

		if c := p.logger.Check(zapcore.DebugLevel, "pushing resource"); c != nil {
			c.Write(
				zap.String("uri", p.request.RequestURI),
				zap.String("push_method", method),
				zap.String("push_target", target),
				zap.Int("plan_count", p.count),
				zap.Int64("plan_spent", p.spent),
				zap.Object("push_headers", caddyhttp.LoggableHTTPHeader{
					Header:               p.header,
					ShouldLogCredentials: p.shouldLogCredentials,
				}),
			)
		}

		err := p.pusher.Push(target, &http.PushOptions{
			Method: method,
			Header: p.header,
		})
		if err != nil {
			p.logger.Error("could not push resource",
				zap.String("uri", p.request.RequestURI),
				zap.String("push_target", target),
				zap.Error(err),
			)
			if p.plan.OnError == errorAbort {
				p.stopped = true
				return
			}
		}
	}
}

// admit resolves and canonicalizes the candidate and checks it
// against the de-duplication set and the plan bounds. On success it
// returns the push target, the de-duplication key and the method. On
// rejection it returns a non-empty reason.
func (p *pushPlanner) admit(cand pushCandidate) (target, key, method, reason string) {
	method = cand.method
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return "", "", "", skipMethod
	}

	raw := cand.target
	if !cand.linked {
		raw = p.repl.ReplaceAll(raw, ".")
	}

	target, key, skipReason := canonicalizeTarget(raw, p.request.URL, p.request.Host)
	if skipReason != "" {
		return "", "", "", skipReason
	}

	if _, ok := p.seen[key]; ok {
		return "", "", "", skipDuplicate
	}
	if p.plan.MaxResources > 0 && p.count >= p.plan.MaxResources {
		return "", "", "", skipResourceLimit
	}
	if p.plan.Budget > 0 && p.spent+int64(len(target)) > p.plan.Budget {
		return "", "", "", skipBudget
	}

	return target, key, method, ""
}

// configuredCandidates builds candidates from the handler config in
// configuration order.
func (p *pushPlanner) configuredCandidates() []pushCandidate {
	candidates := make([]pushCandidate, 0, len(p.resources))
	for _, res := range p.resources {
		candidates = append(candidates, pushCandidate{
			method: res.Method,
			target: res.Target,
		})
	}
	return candidates
}

// linkCandidates parses a snapshot of Link header field values and
// builds candidates in header occurrence order, excluding nopush
// resources.
func linkCandidates(linkHeaders []string) []pushCandidate {
	var candidates []pushCandidate
	for _, field := range linkHeaders {
		for _, resource := range parseLinkHeader(field) {
			if _, ok := resource.params["nopush"]; ok {
				continue
			}
			candidates = append(candidates, pushCandidate{
				method: http.MethodGet,
				target: resource.uri,
				linked: true,
			})
		}
	}
	return candidates
}

// commitLinkPhase performs the at-most-once Link response header
// phase, honoring the cross-handler pushedLink guard and the
// configured candidate ordering. In link-first order it is also
// where configured resources commit, so it must run even when no
// Link header fields are present.
func (p *pushPlanner) commitLinkPhase(linkHeaders []string) {
	if p.linksDone {
		return
	}
	p.linksDone = true

	var links []pushCandidate
	if p.plan.includes(sourceLink) && len(linkHeaders) > 0 {
		// don't push Link resources if another push handler already did
		if caddyhttp.GetVar(p.request.Context(), pushedLink) == nil {
			caddyhttp.SetVar(p.request.Context(), pushedLink, true)
			if c := p.logger.Check(zapcore.DebugLevel, "pushing Link resources"); c != nil {
				c.Write(zap.Strings("linked", linkHeaders))
			}
			links = linkCandidates(linkHeaders)
		}
	}

	if p.plan.Order == orderLinkFirst && p.plan.includes(sourceConfigured) {
		p.commit(links)
		p.commit(p.configuredCandidates())
		return
	}
	p.commit(links)
}

// commitConfiguredPhase performs the pre-response configured
// resource phase. In link-first order configured resources are
// deferred to the Link phase, so this is a no-op there.
func (p *pushPlanner) commitConfiguredPhase() {
	if !p.plan.includes(sourceConfigured) {
		return
	}
	if p.plan.includes(sourceLink) && p.plan.Order == orderLinkFirst {
		return
	}
	p.commit(p.configuredCandidates())
}

// canonicalizeTarget resolves raw against the request URL and
// returns the push target and the de-duplication key. The fragment
// is dropped and the path is cleaned. Absolute and
// protocol-relative URIs are admitted only when their authority
// matches the request; anything external, malformed, or not an
// origin-relative path is rejected, returning a non-empty reason.
func canonicalizeTarget(raw string, reqURL *url.URL, reqHost string) (target, key, reason string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", skipInvalidTarget
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", skipInvalidTarget
	}
	if u.Scheme != "" &&
		!strings.EqualFold(u.Scheme, httpScheme) &&
		!strings.EqualFold(u.Scheme, httpsScheme) {
		return "", "", skipExternal
	}
	if u.Opaque != "" {
		return "", "", skipInvalidTarget
	}

	var p, rawQuery string
	if u.Host != "" {
		if !sameAuthority(u.Host, firstNonEmpty(reqHost, reqURL.Host)) {
			return "", "", skipExternal
		}
		p, rawQuery = u.Path, u.RawQuery
	} else {
		resolved := reqURL.ResolveReference(u)
		p, rawQuery = resolved.Path, resolved.RawQuery
	}

	p = cleanPushPath(p)
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "", "", skipInvalidTarget
	}

	target = p
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return target, target, ""
}

// cleanPushPath cleans p while preserving a trailing slash.
func cleanPushPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// sameAuthority reports whether host (from an absolute or
// protocol-relative URI) identifies the same authority as the
// request. An omitted port matches either default/implicit port.
func sameAuthority(host, reqHost string) bool {
	if strings.EqualFold(host, reqHost) {
		return true
	}
	name, port := splitHostPort(host)
	reqName, reqPort := splitHostPort(reqHost)
	if !strings.EqualFold(name, reqName) || name == "" {
		return false
	}
	return port == reqPort || port == "" || reqPort == ""
}

func splitHostPort(hostport string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.ToLower(hostport), ""
	}
	return strings.ToLower(host), port
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

const (
	httpScheme  = "http"
	httpsScheme = "https"
)
