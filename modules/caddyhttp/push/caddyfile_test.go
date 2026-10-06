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
	"reflect"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func TestParseCaddyfilePlan(t *testing.T) {
	h, err := parseCaddyfile(httpcaddyfile.Helper{
		Dispenser: caddyfile.NewTestDispenser(`
		push {
			/a.css
			plan {
				source both
				order link_first
				max_resources 12
				budget 2KiB
				on_error continue
			}
		}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	handler := h.(*Handler)
	if handler.Plan == nil {
		t.Fatal("expected plan to be parsed")
	}
	want := &ResourcePlan{
		Source:       sourceBoth,
		Order:        orderLinkFirst,
		MaxResources: 12,
		Budget:       2048,
		OnError:      errorContinue,
	}
	if !reflect.DeepEqual(handler.Plan, want) {
		t.Errorf("expected %+v, got %+v", want, handler.Plan)
	}
	if len(handler.Resources) != 1 || handler.Resources[0].Target != "/a.css" {
		t.Errorf("expected resource alongside plan, got %+v", handler.Resources)
	}
}

func TestParseCaddyfilePlanErrors(t *testing.T) {
	for i, tc := range []struct {
		name string
		conf string
	}{
		{"bad source", `push { plan { source everywhere } }`},
		{"bad order", `push { plan { order sideways } }`},
		{"bad max", `push { plan { max_resources many } }`},
		{"negative max", `push { plan { max_resources -1 } }`},
		{"bad budget", `push { plan { budget gigantic } }`},
		{"bad on_error", `push { plan { on_error panic } }`},
		{"unknown subdirective", `push { plan { bound 5 } }`},
		{"plan repeated", `push { plan { source link } plan { source configured } }`},
		{"plan with arg", `push { plan link }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCaddyfile(httpcaddyfile.Helper{
				Dispenser: caddyfile.NewTestDispenser(tc.conf),
			})
			if err == nil {
				t.Errorf("case %d (%s): expected parse error", i, tc.name)
			}
		})
	}
}
