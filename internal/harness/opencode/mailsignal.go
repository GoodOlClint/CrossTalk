package opencode

import (
	"context"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// WakeText is the whole prompt MailSignal sends; it carries no peer words
// (ADR 0033 D15, ADR 0007's fixed-text wake).
const WakeText = "A CrossTalk message is waiting: call receive_message"

// wakeTimeout bounds MailSignal, which the receiver calls under its pull lock.
var wakeTimeout = 5 * time.Second

func (h *Harness) MailSignal(ctx context.Context, _ msg.Message, d adapter.Delivery) error {
	ctx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()
	_, err := h.prompt(ctx, d.Target, WakeText, "steer")
	return err
}
