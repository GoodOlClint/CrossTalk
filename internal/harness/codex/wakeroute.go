package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var (
	ErrNotClaimed = errors.New("codex wake: no owner-proven claim for the thread")
	ErrHosted     = errors.New("codex wake: an app-server hosts the thread")
	ErrWakeOff    = errors.New("codex wake: the --no-daemon setting is off")
	ErrHostProbe  = errors.New("codex wake: an app-server endpoint could not be asked whether it hosts the thread")
)

var (
	_ adapter.MailSignaler = (*Harness)(nil)
	_ adapter.PullSignaler = (*Harness)(nil)
)

// WakeDir holds one wake generation record per thread.
func WakeDir(stateDir string) string { return filepath.Join(stateDir, "codex-wakes") }

// wakeRoute is the queue wake of one shim whose parent is a --no-daemon
// Codex process; it exists only while ServeClaims runs.
type wakeRoute struct {
	stateDir string
	claims   *Claims
	wakes    *Wakes
}

func (h *Harness) setWakeRoute(rt *wakeRoute) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.route = rt
}

func (h *Harness) wakeRoute() *wakeRoute {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.route
}

// precheck holds at write time: the setting is on, the thread's claim is
// owner-proven now, and no app-server hosts it.
func (h *Harness) precheck(ctx context.Context, rt *wakeRoute, thread string) error {
	if !ClaimsEnabled(rt.stateDir) {
		return ErrWakeOff
	}
	if !rt.claims.Bound(thread) {
		return ErrNotClaimed
	}
	if err := rt.claims.proveTarget(thread); err != nil {
		return err
	}
	hosted, err := h.hosted(ctx, thread)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHostProbe, err)
	}
	if hosted {
		return ErrHosted
	}
	return nil
}

// hosted fails closed: an endpoint with nothing listening (no socket, or
// connection refused) hosts nothing, but one that accepts and then fails to
// answer is an error.
func (h *Harness) hosted(ctx context.Context, thread string) (bool, error) {
	if a, ok := h.Threads.(*Adapter); ok {
		return a.hostedStrict(ctx, thread)
	}
	return slices.ContainsFunc(h.LoadedThreads(ctx), func(t LoadedThread) bool { return t.ID == thread }), nil
}

func (a *Adapter) hostedStrict(ctx context.Context, thread string) (bool, error) {
	for _, ep := range a.KnownEndpoints() {
		ectx, cancel := context.WithTimeout(ctx, EndpointProbeTimeout)
		c, err := a.connect(ectx, ep)
		if err != nil {
			cancel()
			if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			return false, fmt.Errorf("%s: %w", ep, err)
		}
		ids, err := c.LoadedThreads(ectx)
		c.Close()
		cancel()
		if err != nil {
			return false, fmt.Errorf("%s: %w", ep, err)
		}
		if slices.Contains(ids, thread) {
			return true, nil
		}
	}
	return false, nil
}

// wakeEligible is plan 3.1 row 5: an immediate message from a verified
// sender. next_turn and unverified senders are mailbox only.
func wakeEligible(m msg.Message, d adapter.Delivery) bool {
	mode := d.Mode
	if mode == "" {
		mode = m.Delivery
	}
	return mode != msg.NextTurn && d.VerifiedSender
}

// MailSignal opens a wake generation for a mailboxed entry when the thread
// has none open. It is a no-op unless ServeClaims runs for this shim.
func (h *Harness) MailSignal(ctx context.Context, m msg.Message, d adapter.Delivery) error {
	rt := h.wakeRoute()
	if rt == nil {
		return nil
	}
	thread := d.Target.SessionRef
	err := rt.wakes.Signal(ctx, thread, m.MessageID, wakeEligible(m, d), func() error { return h.precheck(ctx, rt, thread) })
	if err != nil {
		h.log().Info("codex wake: no row", "thread", thread, "err", err)
	}
	return err
}

// MailPulled closes the thread's open generation and re-checks for
// wake-eligible entries that arrived after the pull's snapshot.
func (h *Harness) MailPulled(ctx context.Context, target adapter.Binding, more bool, mailboxed func() ([]string, error)) error {
	rt := h.wakeRoute()
	if rt == nil || target.SessionRef == "" {
		return nil
	}
	thread := target.SessionRef
	err := rt.wakes.Pulled(ctx, thread, more, mailboxed, func() error { return h.precheck(ctx, rt, thread) })
	if err != nil {
		h.log().Info("codex wake: pull re-check", "thread", thread, "err", err)
	}
	return err
}

func (h *Harness) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}
