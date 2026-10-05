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

package caddyevents

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// asyncTestApp builds an app with the given subscriptions,
// applies async defaults, and starts it; it is stopped on
// test cleanup.
func asyncTestApp(tb testing.TB, subs ...*Subscription) (*App, caddy.Context, context.CancelFunc) {
	tb.Helper()
	app, ctx, cancel := testApp(tb)
	app.Subscriptions = subs
	app.provisionCtx = ctx
	for _, sub := range subs {
		if sub.Async != nil {
			if err := sub.Async.provision(); err != nil {
				tb.Fatalf("provision async options: %v", err)
			}
		}
	}
	if err := app.Start(); err != nil {
		tb.Fatalf("start: %v", err)
	}
	tb.Cleanup(func() {
		app.Stop()
		cancel()
	})
	return app, ctx, cancel
}

func waitSignal(tb testing.TB, ch <-chan struct{}, what string) {
	tb.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		tb.Fatalf("timed out waiting for %s", what)
	}
}

// gateHandler signals enter when Handle is invoked and
// blocks until proceed is closed.
type gateHandler struct {
	enter   chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func newGateHandler() *gateHandler {
	return &gateHandler{
		enter:   make(chan struct{}),
		proceed: make(chan struct{}),
	}
}

func (h *gateHandler) Handle(context.Context, caddy.Event) error {
	h.once.Do(func() { close(h.enter) })
	<-h.proceed
	return nil
}

func (h *gateHandler) release() { close(h.proceed) }

// recordHandler records the value of the "v" data key
// for each event it handles, in order.
type recordHandler struct {
	mu      sync.Mutex
	values  []string
	skipped int
}

func (h *recordHandler) Handle(_ context.Context, e caddy.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := e.Data["v"].(string); ok {
		h.values = append(h.values, v)
	} else {
		h.skipped++
	}
	return nil
}

func (h *recordHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.values...)
}

// A slow asynchronous handler must not block the emitter
// or handlers of other subscriptions.
func TestAsyncEmitterIsNotBlocked(t *testing.T) {
	slow := newGateHandler()
	syncH := &recordHandler{}

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"slow_event"},
			Handlers: []Handler{slow},
			Async:    &Async{QueueCapacity: 4, ShutdownGrace: caddy.Duration(time.Second)},
		},
		&Subscription{
			Events:   []string{"other_event"},
			Handlers: []Handler{syncH},
		},
	)
	defer cancel()

	emitted := make(chan struct{})
	go func() {
		app.Emit(ctx, "slow_event", map[string]any{"v": "x"})
		close(emitted)
	}()
	waitSignal(t, slow.enter, "slow handler to start")

	// the emitter returns even though the handler is stuck
	select {
	case <-emitted:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked on a slow asynchronous handler")
	}

	// other subscriptions and events are unaffected
	app.Emit(ctx, "other_event", map[string]any{"v": "y"})
	if got := syncH.snapshot(); len(got) != 1 || got[0] != "y" {
		t.Errorf("synchronous handler got %v, want [y]", got)
	}

	slow.release()
}

// With drop_newest, once the queue is full the incoming
// events are discarded; queued ones (and the in-flight
// event) are still handled in order.
func TestAsyncBoundedQueueDropNewest(t *testing.T) {
	h := newRecordHandlerWithGate()

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async: &Async{
				QueueCapacity:  1,
				OverflowPolicy: asyncOverflowDropNewest,
				ShutdownGrace:  caddy.Duration(time.Second),
			},
		},
	)
	defer cancel()

	app.Emit(ctx, "e", map[string]any{"v": "1"})
	waitSignal(t, h.enter, "handler to start on event 1")

	// event 2 fills the one-slot queue; event 3 is dropped
	app.Emit(ctx, "e", map[string]any{"v": "2"})
	app.Emit(ctx, "e", map[string]any{"v": "3"})

	h.release()

	dropped := app.asyncHandlers[0].dropped.Load()
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	if got := h.snapshot(); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Errorf("handled %v, want [1 2]", got)
	}
	if dropped != 1 {
		t.Errorf("dropped %d events, want 1", dropped)
	}
}

// With drop_oldest, overflow evicts the longest-queued
// event so the newest one is handled.
func TestAsyncBoundedQueueDropOldest(t *testing.T) {
	h := newRecordHandlerWithGate()

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async: &Async{
				QueueCapacity:  1,
				OverflowPolicy: asyncOverflowDropOldest,
				ShutdownGrace:  caddy.Duration(time.Second),
			},
		},
	)
	defer cancel()

	app.Emit(ctx, "e", map[string]any{"v": "1"})
	waitSignal(t, h.enter, "handler to start on event 1")

	app.Emit(ctx, "e", map[string]any{"v": "2"})
	app.Emit(ctx, "e", map[string]any{"v": "3"})

	h.release()

	dropped := app.asyncHandlers[0].dropped.Load()
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	if got := h.snapshot(); len(got) != 2 || got[0] != "1" || got[1] != "3" {
		t.Errorf("handled %v, want [1 3]", got)
	}
	if dropped != 1 {
		t.Errorf("dropped %d events, want 1", dropped)
	}
}

// The asynchronous handler must operate on an independent
// snapshot of the data: later mutation of the emitter's
// map cannot change what the handler sees, and vice versa.
func TestAsyncDataIsolation(t *testing.T) {
	h := &captureHandler{
		enter:   make(chan struct{}),
		proceed: make(chan struct{}),
	}

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async:    &Async{ShutdownGrace: caddy.Duration(time.Second)},
		},
	)
	defer cancel()

	data := map[string]any{"v": "original"}
	app.Emit(ctx, "e", data)
	waitSignal(t, h.enter, "handler to start")

	// mutate the emitter's map while the handler is still
	// holding its copy on another goroutine
	data["v"] = "changed"
	data["added_by_emitter"] = true
	close(h.proceed)

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	if h.got["v"] != "original" {
		t.Errorf("handler saw %q, want isolated snapshot %q", h.got["v"], "original")
	}
	if _, ok := h.got["added_by_emitter"]; ok {
		t.Error("handler's event data contains key added to the emitter's map after enqueue")
	}

	h.got["added_by_handler"] = true
	if _, ok := data["added_by_handler"]; ok {
		t.Error("handler mutation leaked into the emitter's map")
	}
}

// Concurrent emits must not race the event copy path;
// each copied event keeps its own data map.
func TestAsyncConcurrentEmits(t *testing.T) {
	h := &recordHandler{}

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async:    &Async{QueueCapacity: 512, ShutdownGrace: caddy.Duration(5 * time.Second)},
		},
	)
	defer cancel()

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			app.Emit(ctx, "e", map[string]any{"v": fmt.Sprintf("%d", i)})
		}(i)
	}
	wg.Wait()

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	if got := h.snapshot(); len(got) != n {
		t.Errorf("handled %d events, want %d", len(got), n)
	}
}

// A single worker delivers events to one subscription in
// emission order.
func TestAsyncDeliveryOrder(t *testing.T) {
	h := &recordHandler{}

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async:    &Async{QueueCapacity: 256, ShutdownGrace: caddy.Duration(5 * time.Second)},
		},
	)
	defer cancel()

	const n = 200
	for i := 0; i < n; i++ {
		app.Emit(ctx, "e", map[string]any{"v": fmt.Sprintf("%d", i)})
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	got := h.snapshot()
	if len(got) != n {
		t.Fatalf("handled %d events, want %d", len(got), n)
	}
	for i, v := range got {
		if v != fmt.Sprintf("%d", i) {
			t.Errorf("event %d delivered value %s, want %d", i, v, i)
			break
		}
	}
}

// An abort from an asynchronous handler cannot reach the
// emitter; it only skips the event's remaining handlers,
// and later events are still delivered.
func TestAsyncAbortIsScopedToEvent(t *testing.T) {
	first := &abortOnHandler{abortValue: "abort"}
	second := &recordHandler{}

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{first, second},
			Async:    &Async{ShutdownGrace: caddy.Duration(time.Second)},
		},
	)
	defer cancel()

	app.Emit(ctx, "e", map[string]any{"v": "before"})
	app.Emit(ctx, "e", map[string]any{"v": "abort"})
	app.Emit(ctx, "e", map[string]any{"v": "after"})

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	if got := first.snapshot(); len(got) != 2 || got[0] != "before" || got[1] != "after" {
		t.Errorf("first handler saw %v, want [before after]", got)
	}
	if got := second.snapshot(); len(got) != 2 || got[0] != "before" || got[1] != "after" {
		t.Errorf("second handler saw %v, want [before after] (aborted event skipped, others delivered)", got)
	}

	// the emitter never observes the abort
	ev := app.Emit(ctx, "e", map[string]any{"v": "post-stop"})
	if ev.Aborted != nil {
		t.Errorf("asynchronous abort leaked to emitter: %v", ev.Aborted)
	}
	app.Stop()
}

// Stop delivers events already queued, then the worker
// exits; emits during shutdown are dropped, never blocked.
func TestAsyncStopDrainsQueue(t *testing.T) {
	h := &recordHandler{}

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async:    &Async{QueueCapacity: 256, ShutdownGrace: caddy.Duration(5 * time.Second)},
		},
	)

	const n = 100
	for i := 0; i < n; i++ {
		app.Emit(ctx, "e", map[string]any{"v": fmt.Sprintf("%d", i)})
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}

	if got := len(h.snapshot()); got != n {
		t.Errorf("after Stop, handled %d events, want %d", got, n)
	}

	// an emit racing with shutdown must return immediately and not panic
	done := make(chan struct{})
	go func() {
		app.Emit(ctx, "e", map[string]any{"v": "late"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit after Stop blocked")
	}

	cancel()
	app.Stop() // must be safe to call again
}

// A handler that ignores cancellation cannot make Stop hang
// past the configured shutdown grace plus the cancel wait.
func TestAsyncStopDoesNotHangOnStuckHandler(t *testing.T) {
	h := newGateHandler()

	app, ctx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async: &Async{
				QueueCapacity: 1,
				ShutdownGrace: caddy.Duration(20 * time.Millisecond),
			},
		},
	)
	defer cancel()

	app.Emit(ctx, "e", map[string]any{"v": "x"})
	waitSignal(t, h.enter, "handler to start")

	start := time.Now()
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s with a stuck handler; wanted bounded wait", elapsed)
	}

	// let the detached worker goroutine finish after the test
	h.release()
}

// A configuration reload creates a new App: the old app's
// workers are fully stopped and must not receive events
// emitted through the new app.
func TestAsyncReloadIsolatesApps(t *testing.T) {
	oldH := &recordHandler{}
	oldApp, oldCtx, oldCancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{oldH},
			Async:    &Async{ShutdownGrace: caddy.Duration(time.Second)},
		},
	)

	oldApp.Emit(oldCtx, "e", map[string]any{"v": "old"})
	if err := oldApp.Stop(); err != nil {
		t.Fatal(err)
	}
	oldCancel()

	newH := &recordHandler{}
	newApp, newCtx, newCancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{newH},
			Async:    &Async{ShutdownGrace: caddy.Duration(time.Second)},
		},
	)
	defer newCancel()

	newApp.Emit(newCtx, "e", map[string]any{"v": "new"})
	if err := newApp.Stop(); err != nil {
		t.Fatal(err)
	}

	if got := oldH.snapshot(); len(got) != 1 || got[0] != "old" {
		t.Errorf("old app handled %v after reload, want [old]", got)
	}
	if got := newH.snapshot(); len(got) != 1 || got[0] != "new" {
		t.Errorf("new app handled %v, want [new]", got)
	}
}

type ctxMarkerKeyType struct{}

var ctxMarkerKey ctxMarkerKeyType

// Asynchronous handlers receive an isolated context: it
// exposes the event's own placeholders but none of the
// emitter's context values.
func TestAsyncHandlerContextIsolation(t *testing.T) {
	h := &contextInspectHandler{}

	app, emitCtx, cancel := asyncTestApp(t,
		&Subscription{
			Events:   []string{"e"},
			Handlers: []Handler{h},
			Async:    &Async{ShutdownGrace: caddy.Duration(time.Second)},
		},
	)
	defer cancel()

	emitterCtx := emitCtx.WithValue(ctxMarkerKey, "emitter-private")
	app.Emit(emitterCtx, "e", map[string]any{"v": "payload"})

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()

	if h.dataPlaceholder != "payload" {
		t.Errorf("event.data.v placeholder = %q, want %q", h.dataPlaceholder, "payload")
	}
	if h.emitterValue != nil {
		t.Errorf("handler context inherited emitter value %v; want isolated context", h.emitterValue)
	}
}

// Defaults and validation of Async settings.
func TestAsyncProvisionDefaultsAndValidation(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	t.Run("defaults", func(t *testing.T) {
		app := &App{
			Subscriptions: []*Subscription{{
				Handlers: []Handler{&recordHandler{}},
				Async:    &Async{},
			}},
		}
		if err := app.Provision(ctx); err != nil {
			t.Fatal(err)
		}
		a := app.Subscriptions[0].Async
		if a.QueueCapacity != defaultAsyncQueueCapacity {
			t.Errorf("default capacity = %d, want %d", a.QueueCapacity, defaultAsyncQueueCapacity)
		}
		if a.OverflowPolicy != asyncOverflowDropNewest {
			t.Errorf("default policy = %q, want %q", a.OverflowPolicy, asyncOverflowDropNewest)
		}
		if time.Duration(a.ShutdownGrace) != defaultAsyncShutdownGrace {
			t.Errorf("default grace = %s, want %s", time.Duration(a.ShutdownGrace), defaultAsyncShutdownGrace)
		}
	})

	for _, tc := range []struct {
		name string
		a    Async
	}{
		{"negative capacity", Async{QueueCapacity: -1}},
		{"unknown policy", Async{OverflowPolicy: "block"}},
		{"negative grace", Async{ShutdownGrace: caddy.Duration(-time.Second)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{
				Subscriptions: []*Subscription{{
					Handlers: []Handler{&recordHandler{}},
					Async:    &tc.a,
				}},
			}
			if err := app.Provision(ctx); err == nil {
				t.Fatal("provision should reject invalid async settings")
			}
		})
	}
}

// recordHandlerWithGate is a gateHandler that also records
// the "v" data key of each event.
type recordHandlerWithGate struct {
	enter   chan struct{}
	proceed chan struct{}
	once    sync.Once

	mu     sync.Mutex
	values []string
}

func newRecordHandlerWithGate() *recordHandlerWithGate {
	return &recordHandlerWithGate{
		enter:   make(chan struct{}),
		proceed: make(chan struct{}),
	}
}

func (h *recordHandlerWithGate) Handle(_ context.Context, e caddy.Event) error {
	h.once.Do(func() { close(h.enter) })
	<-h.proceed
	h.mu.Lock()
	h.values = append(h.values, e.Data["v"].(string))
	h.mu.Unlock()
	return nil
}

func (h *recordHandlerWithGate) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.values...)
}

func (h *recordHandlerWithGate) release() { close(h.proceed) }

type captureHandler struct {
	enter   chan struct{}
	proceed chan struct{}
	got     map[string]any
}

func (h *captureHandler) Handle(_ context.Context, e caddy.Event) error {
	h.enter <- struct{}{}
	<-h.proceed
	h.got = e.Data
	return nil
}

type abortOnHandler struct {
	recordHandler
	abortValue string
}

func (h *abortOnHandler) Handle(ctx context.Context, e caddy.Event) error {
	if e.Data["v"] == h.abortValue {
		return caddy.ErrEventAborted
	}
	return h.recordHandler.Handle(ctx, e)
}

type contextInspectHandler struct {
	dataPlaceholder string
	emitterValue    any
}

func (h *contextInspectHandler) Handle(ctx context.Context, e caddy.Event) error {
	if repl, ok := ctx.Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok && repl != nil {
		h.dataPlaceholder, _ = repl.GetString("event.data.v")
	}
	h.emitterValue = ctx.Value(ctxMarkerKey)
	return nil
}
