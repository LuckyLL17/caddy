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

package eventsconfig

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
)

func init() {
	caddy.RegisterModule(testEventHandler{})
}

// testEventHandler is a minimal events.handlers module
// registered solely so the Caddyfile adapter has a real
// module to unmarshal in tests.
type testEventHandler struct {
	Args []string `json:"args,omitempty"`
}

func (testEventHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "events.handlers.test_event_handler",
		New: func() caddy.Module { return new(testEventHandler) },
	}
}

func (h *testEventHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextArg() {
			h.Args = append(h.Args, d.Val())
		}
	}
	return nil
}

func (testEventHandler) Handle(context.Context, caddy.Event) error { return nil }

func parseEvents(t *testing.T, input string) caddyevents.App {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	res, err := parseApp(d, nil)
	if err != nil {
		t.Fatalf("parsing events option: %v", err)
	}
	appCfg, ok := res.(httpcaddyfile.App)
	if !ok {
		t.Fatalf("expected httpcaddyfile.App, got %T", res)
	}
	var app caddyevents.App
	if err := json.Unmarshal(appCfg.Value, &app); err != nil {
		t.Fatalf("unmarshaling generated JSON: %v", err)
	}
	return app
}

func TestParseAppAsync(t *testing.T) {
	app := parseEvents(t, `
events {
	async foo test_event_handler --async-arg value {
		queue_capacity 10
		overflow_policy drop_oldest
		shutdown_grace 2s
	}
	on bar test_event_handler --sync-arg
	async * test_event_handler
}`)

	if len(app.Subscriptions) != 3 {
		t.Fatalf("expected 3 subscriptions, got %d", len(app.Subscriptions))
	}

	first := app.Subscriptions[0]
	if len(first.Events) != 1 || first.Events[0] != "foo" {
		t.Errorf("first subscription events = %v, want [foo]", first.Events)
	}
	if first.Async == nil {
		t.Fatal("first subscription should be asynchronous")
	}
	if first.Async.QueueCapacity != 10 {
		t.Errorf("queue_capacity = %d, want 10", first.Async.QueueCapacity)
	}
	if first.Async.OverflowPolicy != "drop_oldest" {
		t.Errorf("overflow_policy = %q, want drop_oldest", first.Async.OverflowPolicy)
	}
	if time.Duration(first.Async.ShutdownGrace) != 2*time.Second {
		t.Errorf("shutdown_grace = %s, want 2s", time.Duration(first.Async.ShutdownGrace))
	}

	var handler struct {
		Handler string   `json:"handler"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(first.HandlersRaw[0], &handler); err != nil {
		t.Fatal(err)
	}
	if handler.Handler != "test_event_handler" {
		t.Errorf("handler name = %q, want test_event_handler", handler.Handler)
	}
	if len(handler.Args) != 2 || handler.Args[0] != "--async-arg" || handler.Args[1] != "value" {
		t.Errorf("handler args = %v, want [--async-arg value]", handler.Args)
	}

	second := app.Subscriptions[1]
	if second.Async != nil {
		t.Error("on subscription should remain synchronous")
	}
	if len(second.Events) != 1 || second.Events[0] != "bar" {
		t.Errorf("second subscription events = %v, want [bar]", second.Events)
	}

	third := app.Subscriptions[2]
	if third.Async == nil {
		t.Error("* subscription should be asynchronous")
	}
	if len(third.Events) != 1 || third.Events[0] != "" {
		t.Errorf("* subscription events = %v, want [\"\"]", third.Events)
	}
}

func TestParseAppAsyncWithoutBlockUsesDefaults(t *testing.T) {
	app := parseEvents(t, `
events {
	async foo test_event_handler --arg
}`)
	if len(app.Subscriptions) != 1 || app.Subscriptions[0].Async == nil {
		t.Fatal("expected one asynchronous subscription")
	}
	// explicit defaults are applied by the app at Provision,
	// the adapter only needs to emit the async object
}

func TestParseAppAsyncErrors(t *testing.T) {
	for i, tc := range []struct {
		name  string
		input string
	}{
		{
			"non-positive capacity",
			`events {
				async foo test_event_handler {
					queue_capacity 0
				}
			}`,
		},
		{
			"bad overflow policy",
			`events {
				async foo test_event_handler {
					overflow_policy block
				}
			}`,
		},
		{
			"bad grace duration",
			`events {
				async foo test_event_handler {
					shutdown_grace nope
				}
			}`,
		},
		{
			"unknown subdirective",
			`events {
				async foo test_event_handler {
					queue 10
				}
			}`,
		},
		{
			"missing handler",
			`events {
				async foo
			}`,
		},
		{
			"unknown top-level directive",
			`events {
				whenever foo test_event_handler
			}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tc.input)
			if _, err := parseApp(d, nil); err == nil {
				t.Fatalf("case %d (%s): expected error", i, tc.name)
			}
		})
	}
}
