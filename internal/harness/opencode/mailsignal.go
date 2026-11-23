package opencode

import (
	"context"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// WakeText is the whole prompt MailSignal sends; it carries no peer words
// (ADR 0033 D15, ADR 0007's fixed-text wake).
const WakeText = "A CrossTalk message is waiting: call receive_message"

func (h *Harness) MailSignal(ctx context.Context, _ msg.Message, d adapter.Delivery) error {
	_, err := h.prompt(ctx, d.Target, WakeText, "steer")
	return err
}
