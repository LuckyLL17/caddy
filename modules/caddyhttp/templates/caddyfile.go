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

package templates

import (
	"strconv"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("templates", parseCaddyfile)
}

// parseCaddyfile sets up the handler from Caddyfile tokens. Syntax:
//
//	templates [<matcher>] {
//	    mime <types...>
//	    between <open_delim> <close_delim>
//	    root <path>
//	    include_graph {
//	        compiled_cache <true|false>
//	        cache_capacity <entries>
//	        max_file_size <size>
//	        max_include_depth <depth>
//	        max_expansion_bytes <size>
//	        file_change_check <duration>
//	        cache_ttl <duration>
//	        eviction_policy <lru|random>
//	    }
//	}
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	h.Next() // consume directive name
	t := new(Templates)
	var err error
	for h.NextBlock(0) {
		switch h.Val() {
		case "mime":
			t.MIMETypes = h.RemainingArgs()
			if len(t.MIMETypes) == 0 {
				return nil, h.ArgErr()
			}
		case "between":
			t.Delimiters = h.RemainingArgs()
			if len(t.Delimiters) != 2 {
				return nil, h.ArgErr()
			}
		case "root":
			if !h.Args(&t.FileRoot) {
				return nil, h.ArgErr()
			}
		case "include_graph":
			if t.IncludeGraph != nil {
				return nil, h.Err("include_graph already specified")
			}
			if h.NextArg() {
				return nil, h.ArgErr()
			}
			policy := new(IncludeGraphPolicy)
			for nesting := h.Nesting(); h.NextBlock(nesting); {
				switch h.Val() {
				case "compiled_cache":
					args := h.RemainingArgs()
					if len(args) > 1 {
						return nil, h.ArgErr()
					}
					enabled := true
					if len(args) == 1 {
						enabled, err = strconv.ParseBool(args[0])
						if err != nil {
							return nil, h.Errf("parsing compiled_cache: %v", err)
						}
					}
					policy.CompiledCache = &enabled
				case "cache_capacity":
					args := h.RemainingArgs()
					if len(args) != 1 {
						return nil, h.ArgErr()
					}
					policy.CacheCapacity, err = strconv.ParseInt(args[0], 10, 64)
					if err != nil {
						return nil, h.Errf("parsing cache_capacity: %v", err)
					}
				case "max_file_size":
					args := h.RemainingArgs()
					if len(args) != 1 {
						return nil, h.ArgErr()
					}
					policy.MaxFileSize, err = parseCaddyfileSize(args[0])
					if err != nil {
						return nil, h.Errf("parsing max_file_size: %v", err)
					}
				case "max_include_depth":
					args := h.RemainingArgs()
					if len(args) != 1 {
						return nil, h.ArgErr()
					}
					policy.MaxIncludeDepth, err = strconv.Atoi(args[0])
					if err != nil {
						return nil, h.Errf("parsing max_include_depth: %v", err)
					}
				case "max_expansion_bytes":
					args := h.RemainingArgs()
					if len(args) != 1 {
						return nil, h.ArgErr()
					}
					policy.MaxExpansionBytes, err = parseCaddyfileSize(args[0])
					if err != nil {
						return nil, h.Errf("parsing max_expansion_bytes: %v", err)
					}
				case "file_change_check":
					args := h.RemainingArgs()
					if len(args) != 1 {
						return nil, h.ArgErr()
					}
					var duration time.Duration
					if args[0] != "-1" {
						duration, err = caddy.ParseDuration(args[0])
						if err != nil {
							return nil, h.Errf("parsing file_change_check: %v", err)
						}
					} else {
						duration = -1
					}
					policy.FileChangeCheck = caddy.Duration(duration)
				case "cache_ttl":
					args := h.RemainingArgs()
					if len(args) != 1 {
						return nil, h.ArgErr()
					}
					duration, err := caddy.ParseDuration(args[0])
					if err != nil {
						return nil, h.Errf("parsing cache_ttl: %v", err)
					}
					policy.CacheTTL = caddy.Duration(duration)
				case "eviction_policy":
					if !h.Args(&policy.EvictionPolicy) {
						return nil, h.ArgErr()
					}
				default:
					return nil, h.Errf("unrecognized include_graph subdirective: %s", h.Val())
				}
			}
			t.IncludeGraph = policy
		case "extensions":
			if h.NextArg() {
				return nil, h.ArgErr()
			}
			if t.ExtensionsRaw != nil {
				return nil, h.Err("extensions already specified")
			}
			for nesting := h.Nesting(); h.NextBlock(nesting); {
				extensionModuleName := h.Val()
				modID := "http.handlers.templates.functions." + extensionModuleName
				unm, err := caddyfile.UnmarshalModule(h.Dispenser, modID)
				if err != nil {
					return nil, err
				}
				cf, ok := unm.(CustomFunctions)
				if !ok {
					return nil, h.Errf("module %s (%T) does not provide template functions", modID, unm)
				}
				if t.ExtensionsRaw == nil {
					t.ExtensionsRaw = make(caddy.ModuleMap)
				}
				t.ExtensionsRaw[extensionModuleName] = caddyconfig.JSON(cf, nil)
			}
		}
	}
	return t, nil
}

func parseCaddyfileSize(value string) (int64, error) {
	if value == "-1" {
		return -1, nil
	}
	size, err := humanize.ParseBytes(value)
	if err != nil {
		return 0, err
	}
	if size > 1<<63-1 {
		return 0, strconv.ErrRange
	}
	return int64(size), nil
}
