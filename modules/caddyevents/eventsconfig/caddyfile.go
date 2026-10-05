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

// Package eventsconfig is for configuring caddyevents.App with the
// Caddyfile. This code can't be in the caddyevents package because
// the httpcaddyfile package imports caddyhttp, which imports
// caddyevents: hence, it creates an import cycle.
package eventsconfig

import (
	"encoding/json"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("events", parseApp)
}

// parseApp configures the "events" global option from Caddyfile to set up the events app.
// Syntax:
//
//	events {
//		on <event> <handler_module...>
//		async <event> <handler_module...> {
//			queue_capacity <events>
//			overflow_policy <drop_newest|drop_oldest>
//			shutdown_grace <duration>
//		}
//	}
//
// If <event> is *, then it will bind to all events. The "on"
// directive binds handlers synchronously, as usual; "async"
// binds them through a bounded asynchronous queue instead.
// The "async" block is optional: when omitted, the default
// queue settings are used. An asynchronous handler cannot
// take its own block in the Caddyfile; use JSON for that.
func parseApp(d *caddyfile.Dispenser, _ any) (any, error) {
	d.Next() // consume option name
	app := new(caddyevents.App)
	for d.NextBlock(0) {
		switch d.Val() {
		case "on":
			sub, err := parseSubscription(d, false)
			if err != nil {
				return nil, err
			}
			app.Subscriptions = append(app.Subscriptions, sub)

		case "async":
			sub, err := parseSubscription(d, true)
			if err != nil {
				return nil, err
			}
			app.Subscriptions = append(app.Subscriptions, sub)

		default:
			return nil, d.ArgErr()
		}
	}

	return httpcaddyfile.App{
		Name:  "events",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

// parseSubscription parses one "on" or "async" directive,
// with d currently positioned on the directive name.
func parseSubscription(d *caddyfile.Dispenser, async bool) (*caddyevents.Subscription, error) {
	if !d.NextArg() {
		return nil, d.ArgErr()
	}
	eventName := d.Val()
	if eventName == "*" {
		eventName = ""
	}

	if !d.NextArg() {
		return nil, d.ArgErr()
	}
	handlerName := d.Val()
	modID := "events.handlers." + handlerName

	var asyncCfg *caddyevents.Async
	var unm caddyfile.Unmarshaler
	var err error

	if !async {
		// the handler segment may include arguments and a
		// block of its own, all consumed by the module
		unm, err = caddyfile.UnmarshalModule(d, modID)
		if err != nil {
			return nil, err
		}
	} else {
		// collect the handler name and its same-line arguments
		// verbatim; for an async subscription the block on this
		// line holds queue settings rather than handler config
		handlerTokens := []caddyfile.Token{d.Token()}
		for d.NextArg() {
			handlerTokens = append(handlerTokens, d.Token())
		}

		asyncCfg = new(caddyevents.Async)
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			switch d.Val() {
			case "queue_capacity":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				capacity, convErr := strconv.Atoi(d.Val())
				if convErr != nil || capacity <= 0 {
					return nil, d.Errf("queue_capacity must be a positive integer, got %q", d.Val())
				}
				asyncCfg.QueueCapacity = capacity

			case "overflow_policy":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				switch d.Val() {
				case "drop_newest", "drop_oldest":
				default:
					return nil, d.Errf("overflow_policy must be drop_newest or drop_oldest, got %q", d.Val())
				}
				asyncCfg.OverflowPolicy = d.Val()

			case "shutdown_grace":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				grace, parseErr := caddy.ParseDuration(d.Val())
				if parseErr != nil || grace <= 0 {
					return nil, d.Errf("shutdown_grace must be a positive duration, got %q", d.Val())
				}
				asyncCfg.ShutdownGrace = caddy.Duration(grace)

			default:
				return nil, d.Errf("unrecognized asynchronous events subdirective: %s", d.Val())
			}
		}

		unm, err = caddyfile.UnmarshalModule(caddyfile.NewDispenser(handlerTokens), modID)
		if err != nil {
			return nil, err
		}
	}

	return &caddyevents.Subscription{
		Events: []string{eventName},
		Async:  asyncCfg,
		HandlersRaw: []json.RawMessage{
			caddyconfig.JSONModuleObject(unm, "handler", handlerName, nil),
		},
	}, nil
}
