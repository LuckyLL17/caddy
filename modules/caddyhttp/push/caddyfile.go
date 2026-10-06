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
	"strconv"

	"github.com/dustin/go-humanize"

	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("push", parseCaddyfile)
}

// parseCaddyfile sets up the push handler. Syntax:
//
//	push [<matcher>] [<resource>] {
//	    [GET|HEAD] <resource>
//	    headers {
//	        [+]<field> [<value|regexp> [<replacement>]]
//	        -<field>
//	    }
//	    plan {
//	        source    <configured|link|both>
//	        order     <configured_first|link_first>
//	        max_resources <n>
//	        budget    <size>
//	        on_error  <abort|continue>
//	    }
//	}
//
// A single resource can be specified inline without opening a
// block for the most common/simple case. Or, a block can be
// opened and multiple resources can be specified, one per
// line, optionally preceded by the method. The headers
// subdirective can be used to customize the headers that
// are set on each (synthetic) push request, using the same
// syntax as the 'header' directive for request headers.
// The plan subdirective gathers configured and Link resources
// into one deterministic, de-duplicated and bounded plan for
// every request. Placeholders are accepted in resource and
// header field name and value and replacement tokens.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	h.Next() // consume directive name

	handler := new(Handler)

	// inline resources
	if h.NextArg() {
		handler.Resources = append(handler.Resources, Resource{Target: h.Val()})
	}

	// optional block
	for h.NextBlock(0) {
		switch h.Val() {
		case "headers":
			if h.NextArg() {
				return nil, h.ArgErr()
			}
			for nesting := h.Nesting(); h.NextBlock(nesting); {
				var err error

				// include current token, which we treat as an argument here
				// nolint:prealloc
				args := []string{h.Val()}
				args = append(args, h.RemainingArgs()...)

				if handler.Headers == nil {
					handler.Headers = new(HeaderConfig)
				}

				switch len(args) {
				case 1:
					err = headers.CaddyfileHeaderOp(&handler.Headers.HeaderOps, args[0], "", nil)
				case 2:
					err = headers.CaddyfileHeaderOp(&handler.Headers.HeaderOps, args[0], args[1], nil)
				case 3:
					err = headers.CaddyfileHeaderOp(&handler.Headers.HeaderOps, args[0], args[1], &args[2])
				default:
					return nil, h.ArgErr()
				}

				if err != nil {
					return nil, h.Err(err.Error())
				}
			}

		case "plan":
			if h.NextArg() {
				return nil, h.ArgErr()
			}
			if handler.Plan != nil {
				return nil, h.Err("plan already specified")
			}
			handler.Plan = new(ResourcePlan)
			if err := parsePlanCaddyfile(h, handler.Plan); err != nil {
				return nil, err
			}

		case "GET", "HEAD":
			method := h.Val()
			if !h.NextArg() {
				return nil, h.ArgErr()
			}
			target := h.Val()
			handler.Resources = append(handler.Resources, Resource{
				Method: method,
				Target: target,
			})

		default:
			handler.Resources = append(handler.Resources, Resource{Target: h.Val()})
		}
	}
	return handler, nil
}

// parsePlanCaddyfile parses the plan block:
//
//	plan {
//	    source        <configured|link|both>
//	    order         <configured_first|link_first>
//	    max_resources <n>
//	    budget        <size>
//	    on_error      <abort|continue>
//	}
func parsePlanCaddyfile(h httpcaddyfile.Helper, plan *ResourcePlan) error {
	for nesting := h.Nesting(); h.NextBlock(nesting); {
		switch h.Val() {
		case "source":
			if !h.NextArg() {
				return h.ArgErr()
			}
			if h.NextArg() {
				return h.ArgErr()
			}
			switch h.Val() {
			case sourceConfigured, sourceLink, sourceBoth:
				plan.Source = h.Val()
			default:
				return h.Errf("source must be one of %q, %q, or %q, got: %s",
					sourceConfigured, sourceLink, sourceBoth, h.Val())
			}

		case "order":
			if !h.NextArg() {
				return h.ArgErr()
			}
			if h.NextArg() {
				return h.ArgErr()
			}
			switch h.Val() {
			case orderConfiguredFirst, orderLinkFirst:
				plan.Order = h.Val()
			default:
				return h.Errf("order must be %q or %q, got: %s",
					orderConfiguredFirst, orderLinkFirst, h.Val())
			}

		case "max_resources":
			if !h.NextArg() {
				return h.ArgErr()
			}
			if h.NextArg() {
				return h.ArgErr()
			}
			maxResources, err := strconv.Atoi(h.Val())
			if err != nil || maxResources < 0 {
				return h.Errf("max_resources must be a non-negative integer, got: %s", h.Val())
			}
			plan.MaxResources = maxResources

		case "budget":
			if !h.NextArg() {
				return h.ArgErr()
			}
			if h.NextArg() {
				return h.ArgErr()
			}
			budget, err := humanize.ParseBytes(h.Val())
			if err != nil {
				return h.Errf("budget must be a byte size such as 1MB or 1048576, got: %s", h.Val())
			}
			plan.Budget = int64(budget)

		case "on_error":
			if !h.NextArg() {
				return h.ArgErr()
			}
			if h.NextArg() {
				return h.ArgErr()
			}
			switch h.Val() {
			case errorAbort, errorContinue:
				plan.OnError = h.Val()
			default:
				return h.Errf("on_error must be %q or %q, got: %s",
					errorAbort, errorContinue, h.Val())
			}

		default:
			return h.Errf("unrecognized plan subdirective: %s", h.Val())
		}
	}
	return nil
}
