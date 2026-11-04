package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var ErrRefused = errors.New("codex: app-server refused delivery")

func (a *Adapter) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	ep, thread := d.Target.HarnessEndpoint, d.Target.SessionRef
	if thread == "" {
		return adapter.Unreachable, ErrNoThread
	}
	if ep == "" {
		return adapter.Unreachable, ErrNoEndpoint
	}
	c, err := a.connect(ctx, ep)
	if err != nil {
		return adapter.Unreachable, err
	}
	defer c.Close()
	if _, err := CheckVersion(c.ServerVersion()); err != nil {
		return adapter.Unreachable, err
	}
	th, err := c.ReadThread(ctx, thread)
	if err != nil {
		return adapter.Unreachable, err
	}
	if !statusOf(th).Reachable {
		return adapter.Unreachable, fmt.Errorf("%w: thread has no positive direct-input evidence", ErrRefused)
	}
	mode := d.Mode
	if mode == "" {
		mode = m.Delivery
	}
	text := Render(m, d.VerifiedSender)
	if mode == msg.NextTurn || !d.VerifiedSender {
		return appendOutput(ctx, c, thread, text)
	}
	res, err := c.StartToolOutput(ctx, thread, text)
	if _, refused := NotSteerable(res, err); refused {
		return appendOutput(ctx, c, thread, text)
	}
	if err != nil {
		return deliveryFailure(err)
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(res, &response) != nil || response.Turn.ID == "" {
		return adapter.Unknown, fmt.Errorf("%w: tool output response has no turn identifier", appserver.ErrAmbiguous)
	}
	return adapter.Accepted, nil
}

func appendOutput(ctx context.Context, c *appserver.Client, thread, text string) (adapter.State, error) {
	res, err := c.InjectToolOutput(ctx, thread, text)
	if err != nil {
		return deliveryFailure(err)
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(res, &response) != nil || response == nil || len(response) != 0 {
		return adapter.Unknown, fmt.Errorf("%w: history append response is not an empty object", appserver.ErrAmbiguous)
	}
	return adapter.Queued, nil
}

func deliveryFailure(err error) (adapter.State, error) {
	var rpc *appserver.RPCError
	if errors.As(err, &rpc) || errors.Is(err, appserver.ErrNotSent) {
		return adapter.Unreachable, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return adapter.Unknown, err
}
