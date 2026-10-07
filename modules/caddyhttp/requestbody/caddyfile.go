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
	"github.com/dustin/go-humanize"

	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("request_body", parseCaddyfile)
}

// parseCaddyfile parses the request_body directive. Syntax:
//
//	request_body {
//	    max_size <size>
//	    set <body>
//	    replay {
//	        max_size <size>
//	        memory <size>
//	        spill_dir <path>
//	        allow <scope...>
//	        on_exceed <reject|degrade>
//	        on_cancel <delete|retain>
//	    }
//	}
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	h.Next() // consume directive name

	rb := new(RequestBody)

	// configuration should be in a block
	for h.NextBlock(0) {
		switch h.Val() {
		case "max_size":
			var sizeStr string
			if !h.AllArgs(&sizeStr) {
				return nil, h.ArgErr()
			}
			size, err := humanize.ParseBytes(sizeStr)
			if err != nil {
				return nil, h.Errf("parsing max_size: %v", err)
			}
			rb.MaxSize = int64(size)

		case "set":
			var setStr string
			if !h.AllArgs(&setStr) {
				return nil, h.ArgErr()
			}
			rb.Set = setStr

		case "replay":
			if rb.Replay == nil {
				rb.Replay = new(caddyhttp.BodyReplayConfig)
			}
			if err := parseReplayCaddyfile(h, rb.Replay); err != nil {
				return nil, err
			}

		default:
			return nil, h.Errf("unrecognized request_body subdirective '%s'", h.Val())
		}
	}

	return rb, nil
}

// parseReplayCaddyfile parses the replay block. The dispenser is
// positioned on the "replay" token.
func parseReplayCaddyfile(h httpcaddyfile.Helper, replay *caddyhttp.BodyReplayConfig) error {
	for nesting := h.Nesting(); h.NextBlock(nesting); {
		switch h.Val() {
		case "max_size":
			var sizeStr string
			if !h.AllArgs(&sizeStr) {
				return h.ArgErr()
			}
			size, err := humanize.ParseBytes(sizeStr)
			if err != nil {
				return h.Errf("parsing replay max_size: %v", err)
			}
			replay.MaxSize = int64(size)

		case "memory", "memory_max_size":
			var sizeStr string
			if !h.AllArgs(&sizeStr) {
				return h.ArgErr()
			}
			size, err := humanize.ParseBytes(sizeStr)
			if err != nil {
				return h.Errf("parsing replay memory: %v", err)
			}
			replay.MemoryMaxSize = int64(size)

		case "spill_dir":
			var dir string
			if !h.AllArgs(&dir) {
				return h.ArgErr()
			}
			replay.SpillDir = dir

		case "allow":
			scopes := h.RemainingArgs()
			if len(scopes) == 0 {
				return h.ArgErr()
			}
			for _, name := range scopes {
				if _, ok := caddyhttp.ParseBodyReplayScope(name); !ok {
					return h.Errf("unknown replay scope %q; expected one of %q, %q, %q",
						name,
						caddyhttp.BodyReplayScopeNestedRoutesName,
						caddyhttp.BodyReplayScopeErrorRoutesName,
						caddyhttp.BodyReplayScopeRetriesName)
				}
			}
			replay.Allow = append(replay.Allow, scopes...)

		case "on_exceed":
			var mode string
			if !h.AllArgs(&mode) {
				return h.ArgErr()
			}
			switch mode {
			case "reject", "degrade":
				replay.OnExceed = mode
			default:
				return h.Errf("unknown on_exceed %q; expected reject or degrade", mode)
			}

		case "on_cancel":
			var mode string
			if !h.AllArgs(&mode) {
				return h.ArgErr()
			}
			switch mode {
			case "delete", "retain":
				replay.OnCancel = mode
			default:
				return h.Errf("unknown on_cancel %q; expected delete or retain", mode)
			}

		default:
			return h.Errf("unrecognized replay subdirective '%s'", h.Val())
		}
	}
	return nil
}
