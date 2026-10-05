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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(App{})
}

// App implements a global eventing system within Caddy.
// Modules can emit and subscribe to events, providing
// hooks into deep parts of the code base that aren't
// otherwise accessible. Events provide information about
// what and when things are happening, and this facility
// allows handlers to take action when events occur,
// add information to the event's metadata, and even
// control program flow in some cases.
//
// Events are propagated in a DOM-like fashion. An event
// emitted from module `a.b.c` (the "origin") will first
// invoke handlers listening to `a.b.c`, then `a.b`,
// then `a`, then those listening regardless of origin.
// If a handler returns the special error Aborted, then
// propagation immediately stops and the event is marked
// as aborted. Emitters may optionally choose to adjust
// program flow based on an abort.
//
// Modules can subscribe to events by origin and/or name.
// A handler is invoked only if it is subscribed to the
// event by name and origin. Subscriptions should be
// registered during the provisioning phase, before apps
// are started.
//
// Event handlers are fired synchronously as part of the
// regular flow of the program. This allows event handlers
// to control the flow of the program if the origin permits
// it and also allows handlers to convey new information
// back into the origin module before it continues.
// In essence, event handlers are similar to HTTP
// middleware handlers.
//
// Event bindings/subscribers are unordered; i.e.
// event handlers are invoked in an arbitrary order.
// Event handlers should not rely on the logic of other
// handlers to succeed.
//
// Subscriptions are synchronous by default, as
// described above. A subscription may opt into
// asynchronous, bounded delivery by setting its
// Async field: matching events are then copied onto
// a per-subscription bounded queue and handled by a
// dedicated worker goroutine, so a slow handler does
// not block event emitters or other subscriptions.
// See the Async type for the semantics of that mode.
//
// The entirety of this app module is EXPERIMENTAL and
// subject to change. Pay attention to release notes.
type App struct {
	// Subscriptions bind handlers to one or more events
	// either globally or scoped to specific modules or module
	// namespaces.
	Subscriptions []*Subscription `json:"subscriptions,omitempty"`

	// Map of event name to map of module ID/namespace to handlers
	subscriptions map[string]map[caddy.ModuleID][]Handler

	// provisionCtx is the context in which the app was
	// provisioned; async handlers run within its lifetime.
	provisionCtx caddy.Context

	// asyncHandlers are the workers backing asynchronous
	// subscriptions; they are stopped on reload/shutdown.
	asyncHandlers []*asyncHandler

	logger  *zap.Logger
	started bool
}

// Subscription represents binding of one or more handlers to
// one or more events.
type Subscription struct {
	// The name(s) of the event(s) to bind to. Default: all events.
	Events []string `json:"events,omitempty"`

	// The ID or namespace of the module(s) from which events
	// originate to listen to for events. Default: all modules.
	//
	// Events propagate up, so events emitted by module "a.b.c"
	// will also trigger the event for "a.b" and "a". Thus, to
	// receive all events from "a.b.c" and "a.b.d", for example,
	// one can subscribe to either "a.b" or all of "a" entirely.
	Modules []caddy.ModuleID `json:"modules,omitempty"`

	// The event handler modules. These implement the actual
	// behavior to invoke when an event occurs. At least one
	// handler is required.
	HandlersRaw []json.RawMessage `json:"handlers,omitempty" caddy:"namespace=events.handlers inline_key=handler"`

	// The decoded handlers; Go code that is subscribing to
	// an event should set this field directly; HandlersRaw
	// is meant for JSON configuration to fill out this field.
	Handlers []Handler `json:"-"`

	// Async optionally enables asynchronous, bounded
	// delivery of matching events to this subscription's
	// handlers. When nil, events are delivered
	// synchronously by Emit, which is the default.
	Async *Async `json:"async,omitempty"`
}

// Async configures asynchronous, bounded delivery for a
// subscription. When set, each event matching the
// subscription is copied and handed to a single worker
// goroutine dedicated to the subscription, through a
// bounded in-memory queue. Emit never waits for the
// subscription's handlers, so a slow handler cannot
// block event emitters or any other subscription.
//
// Asynchronous delivery differs from the default
// synchronous mode in important ways:
//
//   - Handlers run after Emit has returned, so they
//     cannot abort an event, alter the event seen by
//     the emitter, or otherwise affect program flow.
//   - The emitter's context is not used; handlers
//     receive an isolated context that carries only
//     the event's own placeholders and data and
//     remains valid until the app is stopped.
//   - Each event gets an independent copy of the data
//     map, so the emitter and other handlers may use
//     the original event concurrently without data
//     races. Map values are copied shallowly, as with
//     synchronous delivery.
//   - Events are handled in emission order for the
//     subscription, one at a time; when the queue is
//     full, events are dropped per OverflowPolicy
//     rather than blocking the emitter.
//
// On configuration reload or shutdown, Stop waits up
// to ShutdownGrace for queued events to finish; events
// still pending afterwards are abandoned and logged.
type Async struct {
	// QueueCapacity is the maximum number of events
	// that may be buffered while waiting for the
	// subscription's handlers. It must be positive.
	// Default: 256.
	QueueCapacity int `json:"queue_capacity,omitempty"`

	// OverflowPolicy decides what happens to a newly
	// emitted event when the queue is full:
	//
	//   - "drop_newest" (default): the incoming event
	//     is dropped.
	//   - "drop_oldest": the oldest queued event is
	//     dropped to make room for the incoming one.
	//
	// Dropped events are counted and logged.
	OverflowPolicy string `json:"overflow_policy,omitempty"`

	// ShutdownGrace is how long Stop waits for queued
	// and in-flight events to finish processing on
	// configuration reload or shutdown. After it
	// elapses, the handler context is canceled and
	// any remaining queued events are abandoned.
	// Default: 30s.
	ShutdownGrace caddy.Duration `json:"shutdown_grace,omitempty"`
}

// Overflow policies for Async.
const (
	asyncOverflowDropNewest = "drop_newest"
	asyncOverflowDropOldest = "drop_oldest"

	defaultAsyncQueueCapacity = 256
	defaultAsyncShutdownGrace = 30 * time.Second

	// asyncCancelWait is the additional time Stop waits
	// for a handler that is still running after its
	// context was canceled past the shutdown grace,
	// before detaching the worker so a reload cannot
	// hang indefinitely.
	asyncCancelWait = time.Second
)

// provision fills in defaults and validates the async
// settings.
func (a *Async) provision() error {
	if a.QueueCapacity == 0 {
		a.QueueCapacity = defaultAsyncQueueCapacity
	}
	if a.QueueCapacity < 0 {
		return fmt.Errorf("async queue_capacity must be positive, got %d", a.QueueCapacity)
	}

	switch a.OverflowPolicy {
	case "":
		a.OverflowPolicy = asyncOverflowDropNewest
	case asyncOverflowDropNewest, asyncOverflowDropOldest:
	default:
		return fmt.Errorf("unknown async overflow_policy %q; valid values are %q and %q",
			a.OverflowPolicy, asyncOverflowDropNewest, asyncOverflowDropOldest)
	}

	if a.ShutdownGrace == 0 {
		a.ShutdownGrace = caddy.Duration(defaultAsyncShutdownGrace)
	}
	if a.ShutdownGrace < 0 {
		return fmt.Errorf("async shutdown_grace must be positive, got %s", time.Duration(a.ShutdownGrace))
	}

	return nil
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "events",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision sets up the app.
func (app *App) Provision(ctx caddy.Context) error {
	app.logger = ctx.Logger()
	app.provisionCtx = ctx
	app.subscriptions = make(map[string]map[caddy.ModuleID][]Handler)

	for _, sub := range app.Subscriptions {
		if sub.HandlersRaw == nil {
			continue
		}
		handlersIface, err := ctx.LoadModule(sub, "HandlersRaw")
		if err != nil {
			return fmt.Errorf("loading event subscriber modules: %v", err)
		}
		for _, h := range handlersIface.([]any) {
			sub.Handlers = append(sub.Handlers, h.(Handler))
		}
		if len(sub.Handlers) == 0 {
			// pointless to bind without any handlers
			return fmt.Errorf("no handlers defined")
		}
	}

	for _, sub := range app.Subscriptions {
		if sub.Async != nil {
			if err := sub.Async.provision(); err != nil {
				return err
			}
		}
	}

	return nil
}

// Start runs the app.
func (app *App) Start() error {
	for _, sub := range app.Subscriptions {
		// asynchronous subscriptions are registered through
		// a wrapper that enqueues a copy of each event; the
		// subscription's real handlers run on the worker.
		registered := sub
		if sub.Async != nil {
			ah := app.newAsyncHandler(sub)
			app.asyncHandlers = append(app.asyncHandlers, ah)

			subCopy := *sub
			subCopy.Handlers = []Handler{ah}
			registered = &subCopy
		}

		if err := app.Subscribe(registered); err != nil {
			app.stopAsyncHandlers()
			return err
		}
	}

	app.started = true

	return nil
}

// Stop gracefully shuts down the app, waiting for events
// queued by asynchronous subscriptions to finish within
// their configured grace periods.
func (app *App) Stop() error {
	app.stopAsyncHandlers()
	return nil
}

// Subscribe binds one or more event handlers to one or more events
// according to the subscription s. For now, subscriptions can only
// be created during the provision phase; new bindings cannot be
// created after the events app has started.
func (app *App) Subscribe(s *Subscription) error {
	if app.started {
		return fmt.Errorf("events already started; new subscriptions closed")
	}

	// handle special case of catch-alls (omission of event name or module space implies all)
	if len(s.Events) == 0 {
		s.Events = []string{""}
	}
	if len(s.Modules) == 0 {
		s.Modules = []caddy.ModuleID{""}
	}

	for _, eventName := range s.Events {
		if app.subscriptions[eventName] == nil {
			app.subscriptions[eventName] = make(map[caddy.ModuleID][]Handler)
		}
		for _, originModule := range s.Modules {
			app.subscriptions[eventName][originModule] = append(app.subscriptions[eventName][originModule], s.Handlers...)
		}
	}

	return nil
}

// On is syntactic sugar for Subscribe() that binds a single handler
// to a single event from any module. If the eventName is empty string,
// it counts for all events.
func (app *App) On(eventName string, handler Handler) error {
	return app.Subscribe(&Subscription{
		Events:   []string{eventName},
		Handlers: []Handler{handler},
	})
}

// Emit creates and dispatches an event named eventName to all relevant handlers with
// the metadata data. Events are emitted and propagated synchronously. The returned Event
// value will have any additional information from the invoked handlers.
//
// Note that the data map is not copied, for efficiency. After Emit() is called, the
// data passed in should not be changed in other goroutines.
func (app *App) Emit(ctx caddy.Context, eventName string, data map[string]any) caddy.Event {
	e, err := caddy.NewEvent(ctx, eventName, data)
	if err != nil {
		app.logger.Error("failed to create event",
			zap.String("name", eventName), zap.Error(err))
	}

	// A handler can only be reached through subscriptions to this event by
	// name or to all events, so if neither is bound, nothing can observe
	// this event and the only remaining output is the debug log below.
	// Bail out before deriving loggers and registering replacer values:
	// some events, such as tls_get_certificate, are emitted on every TLS
	// handshake, where that work is significant and always wasted.
	if app.subscriptions[eventName] == nil && app.subscriptions[""] == nil &&
		!app.logger.Core().Enabled(zapcore.DebugLevel) {
		return e
	}

	logger := app.logger.With(zap.String("name", eventName))

	var originModule caddy.ModuleInfo
	var originModuleID caddy.ModuleID
	var originModuleName string
	if origin := e.Origin(); origin != nil {
		originModule = origin.CaddyModule()
		originModuleID = originModule.ID
		originModuleName = originModule.String()
	}

	logger = logger.With(
		zap.String("id", e.ID().String()),
		zap.String("origin", originModuleName))

	// add event info to replacer, make sure it's in the context
	repl, ok := ctx.Context.Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if !ok {
		repl = caddy.NewReplacer()
		ctx.Context = context.WithValue(ctx.Context, caddy.ReplacerCtxKey, repl)
	}
	repl.Map(eventPlaceholders(e, originModuleID))

	logger = logger.WithLazy(zap.Any("data", e.Data))

	logger.Debug("event")

	// invoke handlers bound to the event by name and also all events; this for loop
	// iterates twice at most: once for the event name, once for "" (all events)
	for {
		moduleID := originModuleID

		// implement propagation up the module tree (i.e. start with "a.b.c" then "a.b" then "a" then "")
		for {
			if app.subscriptions[eventName] == nil {
				break // shortcut if event not bound at all
			}

			for _, handler := range app.subscriptions[eventName][moduleID] {
				select {
				case <-ctx.Done():
					logger.Error("context canceled; event handling stopped")
					return e
				default:
				}

				// this log can be a useful sanity check to ensure your handlers are in fact being invoked
				// (see https://github.com/mholt/caddy-events-exec/issues/6)
				logger.Debug("invoking subscribed handler",
					zap.String("subscribed_to", eventName),
					zap.Any("handler", handler))

				if err := handler.Handle(ctx, e); err != nil {
					aborted := errors.Is(err, caddy.ErrEventAborted)

					logger.Error("handler error",
						zap.Error(err),
						zap.Bool("aborted", aborted))

					if aborted {
						e.Aborted = err
						return e
					}
				}
			}

			if moduleID == "" {
				break
			}
			lastDot := strings.LastIndex(string(moduleID), ".")
			if lastDot < 0 {
				moduleID = "" // include handlers bound to events regardless of module
			} else {
				moduleID = moduleID[:lastDot]
			}
		}

		// include handlers listening to all events
		if eventName == "" {
			break
		}
		eventName = ""
	}

	return e
}

// Handler is a type that can handle events.
type Handler interface {
	Handle(context.Context, caddy.Event) error
}

// eventPlaceholders returns a replacer mapping that exposes
// an event's identity and data under the event.* placeholders.
func eventPlaceholders(e caddy.Event, originModuleID caddy.ModuleID) func(string) (any, bool) {
	return func(key string) (any, bool) {
		switch key {
		case "event":
			return e, true
		case "event.id":
			return e.ID(), true
		case "event.name":
			return e.Name(), true
		case "event.time":
			return e.Timestamp(), true
		case "event.time_unix":
			return e.Timestamp().UnixMilli(), true
		case "event.module":
			return originModuleID, true
		case "event.data":
			return e.Data, true
		}

		if after, ok := strings.CutPrefix(key, "event.data."); ok {
			if val, ok := e.Data[after]; ok {
				return val, true
			}
		}

		return nil, false
	}
}

// copyEvent returns an event value that is safe to use on
// another goroutine: the struct value (including id,
// timestamp, name, and origin) is copied and the Data map
// is shallow-copied, so later changes to the original event
// or its map by the emitter or other handlers cannot race
// the copy. Map values themselves are not deep-copied, the
// same shallow-sharing contract as synchronous delivery.
func copyEvent(e caddy.Event) caddy.Event {
	cp := e
	cp.Aborted = nil
	if e.Data != nil {
		cp.Data = make(map[string]any, len(e.Data))
		for k, v := range e.Data {
			cp.Data[k] = v
		}
	}
	return cp
}

// newAsyncHandler creates the bounded queue and worker for
// an asynchronous subscription and starts the worker.
func (app *App) newAsyncHandler(sub *Subscription) *asyncHandler {
	cfg := *sub.Async

	logger := app.logger
	if logger == nil {
		logger = zap.NewNop()
	}

	baseCtx := app.provisionCtx
	if baseCtx.Context == nil {
		baseCtx.Context = context.Background()
	}

	ah := &asyncHandler{
		handlers: sub.Handlers,
		cfg:      cfg,
		baseCtx:  baseCtx,
		queue:    make(chan caddy.Event, cfg.QueueCapacity),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		logger: logger.With(
			zap.Strings("events", sub.Events),
			zap.Any("modules", sub.Modules),
			zap.Int("queue_capacity", cfg.QueueCapacity),
			zap.String("overflow_policy", cfg.OverflowPolicy)),
		// log at most a few overflow warnings per second
		// to avoid flooding logs during sustained overload
		dropLogger: zap.New(zapcore.NewSamplerWithOptions(
			logger.Core(), time.Second, 3, 0)),
	}
	ah.start()
	return ah
}

// stopAsyncHandlers shuts down all async workers; it is
// safe to call when no workers exist and may be called
// more than once.
func (app *App) stopAsyncHandlers() {
	for _, ah := range app.asyncHandlers {
		ah.shutdown()
	}
	app.asyncHandlers = nil
}

// asyncHandler is the Handler registered for an
// asynchronous subscription. Emit invokes Handle inline,
// where it enqueues a copy of the event into a bounded
// queue; a dedicated worker goroutine runs the
// subscription's real handlers in emission order.
type asyncHandler struct {
	handlers []Handler
	cfg      Async
	baseCtx  caddy.Context

	queue chan caddy.Event

	// stopCh is closed once shutdown begins; doneCh is
	// closed when the worker has exited. The queue is
	// intentionally never closed so that emits racing
	// with shutdown never panic on a closed channel.
	stopCh chan struct{}
	doneCh chan struct{}

	rootCtx       context.Context
	cancelRootCtx context.CancelFunc
	shutdownOnce  sync.Once

	stopping          atomic.Bool
	enqueued, dropped atomic.Uint64

	logger     *zap.Logger
	dropLogger *zap.Logger
}

// Handle enqueues a copy of the event for asynchronous
// delivery. It never blocks: when the bounded queue is
// full, the configured overflow policy is applied. It
// also never returns an error, since an asynchronous
// handler cannot abort an event or otherwise influence
// the emitter's flow.
func (ah *asyncHandler) Handle(_ context.Context, e caddy.Event) error {
	if ah.stopping.Load() {
		ah.dropped.Add(1)
		ah.dropLogger.Warn("dropping event for asynchronous subscription during shutdown",
			zap.String("event", e.Name()))
		return nil
	}

	ev := copyEvent(e)
	enqueued := false
	switch ah.cfg.OverflowPolicy {
	case asyncOverflowDropOldest:
		select {
		case ah.queue <- ev:
			enqueued = true
		default:
			// make room by discarding the oldest event;
			// another publisher may win the race, in which
			// case fall back to dropping the newest one
			select {
			case <-ah.queue:
				ah.dropped.Add(1)
			default:
			}
			select {
			case ah.queue <- ev:
				enqueued = true
			default:
				ah.dropped.Add(1)
			}
			ah.logOverflow(ev)
		}
	default: // drop_newest
		select {
		case ah.queue <- ev:
			enqueued = true
		default:
			ah.dropped.Add(1)
			ah.logOverflow(ev)
		}
	}
	if enqueued {
		ah.enqueued.Add(1)
	}
	return nil
}

func (ah *asyncHandler) logOverflow(ev caddy.Event) {
	ah.dropLogger.Warn("asynchronous subscription queue is full; dropping event",
		zap.String("event", ev.Name()),
		zap.String("id", ev.ID().String()),
		zap.String("overflow_policy", ah.cfg.OverflowPolicy),
		zap.Uint64("dropped_total", ah.dropped.Load()))
}

// start launches the worker goroutine.
func (ah *asyncHandler) start() {
	ah.rootCtx, ah.cancelRootCtx = context.WithCancel(ah.baseCtx.Context)
	go ah.run()
}

func (ah *asyncHandler) run() {
	defer close(ah.doneCh)
	for {
		select {
		case ev := <-ah.queue:
			ah.dispatch(ev)
		case <-ah.stopCh:
			ah.drain()
			return
		}
	}
}

// drain delivers events still queued when shutdown began,
// until the queue is empty or the root context is
// canceled (grace period elapsed / config unloaded).
func (ah *asyncHandler) drain() {
	for {
		select {
		case ev := <-ah.queue:
			if ah.rootCtx.Err() != nil {
				remaining := uint64(1 + len(ah.queue))
				ah.dropped.Add(remaining)
				ah.logger.Error("abandoned queued events for asynchronous subscription during shutdown",
					zap.Uint64("abandoned", remaining))
				return
			}
			ah.dispatch(ev)
		default:
			return
		}
	}
}

// dispatch runs the subscription's handlers for a single
// event on an isolated context carrying only this
// event's placeholders.
func (ah *asyncHandler) dispatch(ev caddy.Event) {
	var originModuleID caddy.ModuleID
	if origin := ev.Origin(); origin != nil {
		originModuleID = origin.CaddyModule().ID
	}

	eventCtx := ah.baseCtx
	eventCtx.Context = ah.rootCtx
	repl := caddy.NewReplacer()
	repl.Map(eventPlaceholders(ev, originModuleID))
	ctx := eventCtx.WithValue(caddy.ReplacerCtxKey, repl)

	logger := ah.logger.With(
		zap.String("id", ev.ID().String()),
		zap.String("name", ev.Name()),
		zap.Any("data", ev.Data))

	for _, handler := range ah.handlers {
		select {
		case <-ah.rootCtx.Done():
			ah.logger.Error("context canceled; asynchronous event handling stopped",
				zap.String("id", ev.ID().String()))
			return
		default:
		}

		logger.Debug("invoking subscribed handler",
			zap.String("subscribed_to", ev.Name()),
			zap.Any("handler", handler))

		if err := handler.Handle(ctx, ev); err != nil {
			aborted := errors.Is(err, caddy.ErrEventAborted)

			logger.Error("handler error",
				zap.Error(err),
				zap.Bool("aborted", aborted))

			// an abort cannot reach the emitter anymore; it
			// only stops this event's remaining handlers,
			// not other queued events
			if aborted {
				return
			}
		}
	}
}

// shutdown stops accepting events, lets the worker drain
// within the configured grace period, and detaches a
// worker that still does not finish so reloads and
// shutdowns cannot hang indefinitely.
func (ah *asyncHandler) shutdown() {
	ah.shutdownOnce.Do(func() {
		ah.stopping.Store(true)
		close(ah.stopCh)

		timer := time.NewTimer(time.Duration(ah.cfg.ShutdownGrace))
		defer timer.Stop()

		select {
		case <-ah.doneCh:
		case <-timer.C:
			ah.logger.Warn("asynchronous subscription did not finish within shutdown grace period; canceling handler context",
				zap.Duration("grace", time.Duration(ah.cfg.ShutdownGrace)))
			ah.cancelRootCtx()

			select {
			case <-ah.doneCh:
			case <-time.After(asyncCancelWait):
				ah.logger.Error("asynchronous event handler is still running after context cancellation; detaching worker goroutine until handler returns")
				return
			}
		}

		if dropped := ah.dropped.Load(); dropped > 0 {
			ah.logger.Warn("asynchronous subscription stopped; some events were dropped",
				zap.Uint64("enqueued", ah.enqueued.Load()),
				zap.Uint64("dropped", dropped))
		}
	})
}

// Interface guards
var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
	_ Handler           = (*asyncHandler)(nil)
)
