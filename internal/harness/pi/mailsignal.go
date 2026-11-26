package pi

import (
	"context"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// WakeText is the whole text MailSignal sends; it carries no peer words
// (ADR 0037 point 4).
const WakeText = "A CrossTalk message is waiting: call receive_message"

// MailSignal writes the fixed wake with trigger true whatever the delivery
// mode, so it can start a paid turn. It waits for no answer: the receiver
// calls it under the pull lock, and a busy session admits the wake only at a
// turn boundary that a pending receive_message would hold back.
func (h *Harness) MailSignal(ctx context.Context, _ msg.Message, d adapter.Delivery) error {
	if err := h.served(d.Target); err != nil {
		return err
	}
	_, err := h.deliver(ctx, WakeText, true, false)
	return err
}
