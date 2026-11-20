package codex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var (
	ErrRouteAmbiguous = fmt.Errorf("codex: thread has an owner-proven claim and an app-server host, or several hosts: %w", adapter.ErrNoEndpoint)
	ErrNoQueueRoute   = fmt.Errorf("codex: no app-server hosts the thread and the queue route is not available: %w", adapter.ErrNoEndpoint)
	ErrAskNotQueued   = fmt.Errorf("codex: asks are not accepted on the queue route: %w", adapter.ErrNoEndpoint)
)

// Route is a row of plan 3.1. Row 1, a prior attempt that ended unknown,
// never reaches Inject: the receiver's dedupe and dispatch claim return the
// recorded state without calling it.
type Route int

const (
	RouteAppServer   Route = 2
	RouteNone        Route = 3
	RouteMailbox     Route = 4
	RouteWake        Route = 5
	RouteAmbiguous   Route = 6
	routeUnevaluated Route = 0
)

// routeFacts are what one delivery's row depends on. capability is read only
// when the row needs it.
type routeFacts struct {
	hosts      int
	hostErr    error
	claimed    bool
	capability func() error
	ask        bool
	wake       bool
}

func pickRoute(f routeFacts) (Route, error) {
	if !f.claimed {
		switch f.hosts {
		case 0:
			return RouteNone, ErrNoQueueRoute
		case 1:
			return RouteAppServer, nil
		}
		return RouteAmbiguous, ErrRouteAmbiguous
	}
	if f.hostErr != nil {
		return RouteAmbiguous, fmt.Errorf("%w: %w", ErrRouteAmbiguous, f.hostErr)
	}
	if f.hosts > 0 {
		return RouteAmbiguous, ErrRouteAmbiguous
	}
	if err := f.capability(); err != nil {
		return RouteNone, fmt.Errorf("%w: %w", ErrNoQueueRoute, err)
	}
	if f.ask {
		return RouteNone, ErrAskNotQueued
	}
	if f.wake {
		return RouteWake, nil
	}
	return RouteMailbox, nil
}

// selectRoute picks the row for d, or routeUnevaluated when the queue route is not
// in play for this shim (never served claims, setting off, or a sender-side
// inject into an unbound session) and Inject behaves as before step 5.
func (h *Harness) selectRoute(ctx context.Context, m msg.Message, d adapter.Delivery) (Route, error) {
	rt, thread := h.wakeRoute(), d.Target.SessionRef
	if rt == nil || thread == "" || d.SenderAgentID != "" || !ClaimsEnabled(rt.stateDir) {
		return routeUnevaluated, nil
	}
	f := routeFacts{claimed: rt.claims.Bound(thread), ask: m.Kind == msg.Ask, wake: wakeEligible(m, d)}
	f.hosts, f.hostErr = h.hostCount(ctx, thread, f.claimed)
	f.capability = func() error { return h.capability(ctx, rt, thread) }
	return pickRoute(f)
}

// capability is the queue route's precondition at write time: the setting
// is on, the thread's claim is owner-proven now, and the gate passes.
func (h *Harness) capability(ctx context.Context, rt *wakeRoute, thread string) error {
	if !ClaimsEnabled(rt.stateDir) {
		return ErrWakeOff
	}
	if !rt.claims.Bound(thread) {
		return ErrNotClaimed
	}
	if err := rt.claims.proveTarget(thread); err != nil {
		return err
	}
	return rt.wakes.Gate().Tx(ctx, false, func(*sql.Conn) error { return nil })
}

// Inject delivers through the row route selects. A mailboxed result writes
// nothing outside the journal; the wake, for row 5 only, is MailSignal's.
func (h *Harness) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	r, err := h.selectRoute(ctx, m, d)
	switch r {
	case routeUnevaluated, RouteAppServer:
		return h.Threads.Inject(ctx, m, d)
	case RouteMailbox, RouteWake:
		return adapter.Mailboxed, nil
	}
	if err == nil {
		err = errors.New("codex: no route")
	}
	return adapter.Unreachable, err
}
