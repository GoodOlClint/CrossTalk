package codex

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var (
	ErrQueueStart  = errors.New("codex: codex queue could not be started")
	ErrQueueFailed = errors.New("codex: codex queue did not report success")
	ErrRefused     = errors.New("codex: app-server refused delivery")
)

// Inject delivers m into the bound thread over d.Target.HarnessEndpoint
// (§6). States: accepted on a turn/steer or turn/start success response;
// queued only on `codex queue` exit 0; unknown when a write may have
// reached the server without a response (never resent); unreachable when
// nothing was delivered.
func (a *Adapter) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	ep, thread := d.Target.HarnessEndpoint, d.Target.SessionRef
	if thread == "" {
		return adapter.Unreachable, ErrNoThread
	}
	if ep == "" {
		return adapter.Unreachable, ErrNoEndpoint
	}
	text := Render(m, d.VerifiedSender)
	mode := d.Mode
	if mode == "" {
		mode = m.Delivery
	}
	if mode == msg.NextTurn {
		return a.queue(ctx, ep, thread, text)
	}

	c, err := a.connect(ctx, ep)
	if err != nil {
		return adapter.Unreachable, err
	}
	defer c.Close()
	if _, err := CheckVersion(c.ServerVersion()); err != nil {
		return adapter.Unreachable, err
	}
	turn, err := c.ActiveTurn(ctx, thread)
	if err != nil {
		return adapter.Unreachable, err
	}
	if turn == "" {
		if !d.VerifiedSender {
			return a.queue(ctx, ep, thread, text)
		}
		return a.start(ctx, c, ep, thread, text)
	}
	return a.steer(ctx, c, ep, thread, turn, text)
}

type outcome int

const (
	oAccepted outcome = iota
	oAmbiguous
	oNotSteerable
	oRefused
)

func classify(res []byte, err error) outcome {
	if _, ok := NotSteerable(res, err); ok {
		return oNotSteerable
	}
	var rpc *appserver.RPCError
	switch {
	case err == nil:
		return oAccepted
	case errors.As(err, &rpc), errors.Is(err, appserver.ErrNotSent):
		return oRefused
	default:
		return oAmbiguous
	}
}

// steer implements §6 F1: activeTurnNotSteerable → next_turn; on any other
// refusal, refresh the turn list once: a different active turn is an
// expectedTurnId mismatch and is retried once, then next_turn; no active
// turn (active→idle race) → next_turn; the same turn means the refusal was
// not a mismatch and nothing is retried.
func (a *Adapter) steer(ctx context.Context, c *appserver.Client, ep, thread, turn, text string) (adapter.State, error) {
	res, err := c.SteerTurn(ctx, thread, turn, text)
	switch classify(res, err) {
	case oAccepted:
		return adapter.Accepted, nil
	case oAmbiguous:
		return adapter.Unknown, err
	case oNotSteerable:
		return a.queue(ctx, ep, thread, text)
	}
	if errors.Is(err, appserver.ErrNotSent) {
		return adapter.Unreachable, err
	}
	fresh, rerr := c.ActiveTurn(ctx, thread)
	switch {
	case rerr != nil:
		return adapter.Unreachable, errors.Join(err, rerr)
	case fresh == "":
		return a.queue(ctx, ep, thread, text)
	case fresh == turn:
		return adapter.Unreachable, fmt.Errorf("%w: turn/steer: %w", ErrRefused, err)
	}
	res, err = c.SteerTurn(ctx, thread, fresh, text)
	switch classify(res, err) {
	case oAccepted:
		return adapter.Accepted, nil
	case oAmbiguous:
		return adapter.Unknown, err
	}
	if errors.Is(err, appserver.ErrNotSent) {
		return adapter.Unreachable, err
	}
	return a.queue(ctx, ep, thread, text)
}

// start sends turn/start with threadId and input only (verified senders;
// §6, §7 V12).
func (a *Adapter) start(ctx context.Context, c *appserver.Client, ep, thread, text string) (adapter.State, error) {
	res, err := c.StartTurn(ctx, thread, text)
	switch classify(res, err) {
	case oAccepted:
		return adapter.Accepted, nil
	case oAmbiguous:
		return adapter.Unknown, err
	case oNotSteerable:
		return a.queue(ctx, ep, thread, text)
	}
	if errors.Is(err, appserver.ErrNotSent) {
		return adapter.Unreachable, err
	}
	return adapter.Unreachable, fmt.Errorf("%w: turn/start: %w", ErrRefused, err)
}

// QueueArgs is the exact `codex queue` argument list (§6 F17): the recorded
// endpoint and exact thread id, never a name, never --resume.
func QueueArgs(endpoint, threadID, text string) []string {
	return []string{"queue", "--remote", endpoint, "--thread", threadID, "--message", text}
}

func (a *Adapter) queue(ctx context.Context, ep, thread, text string) (adapter.State, error) {
	run, bin := a.Run, a.CodexBin
	if run == nil {
		run = ExecRunner
	}
	if bin == "" {
		bin = "codex"
	}
	code, out, err := run(ctx, bin, QueueArgs(ep, thread, text)...)
	if err != nil {
		return adapter.Unreachable, fmt.Errorf("%w: %v", ErrQueueStart, err)
	}
	if code != 0 {
		return adapter.Unknown, fmt.Errorf("%w: exit %d: %s", ErrQueueFailed, code, truncate(out, 512))
	}
	return adapter.Queued, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
