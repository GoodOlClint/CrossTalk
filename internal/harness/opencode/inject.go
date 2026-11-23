package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// rawSender is D15 (ADR 0033): only a verified registered agent of this host
// gets its text into the prompt; every other class is left in the journal.
func rawSender(d adapter.Delivery) bool {
	return d.VerifiedSender && !d.SenderRemote && !d.InboxCopy &&
		(d.Notice == msg.NoticeTeammate || d.Notice == msg.NoticeTeammateAgain)
}

// Inject sends one plain user prompt and never retries it (ADR 0033). Mode
// alone picks steer or queue; the message kind is not an input.
func (h *Harness) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	if !rawSender(d) {
		return adapter.Mailboxed, nil
	}
	mode := d.Mode
	if mode == "" {
		mode = m.Delivery
	}
	delivery := "steer"
	if mode == msg.NextTurn {
		delivery = "queue"
	}
	text := RenderIn(m, d.VerifiedSender, d.SenderHarness, d.SenderAgentID, d.Inbox, d.InboxCopy, d.Notice)
	return h.prompt(ctx, d.Target, text, delivery)
}

// prompt is unreachable when nothing was sent or the service refused before
// admitting, and unknown when the request left and admission is unproven.
func (h *Harness) prompt(ctx context.Context, target adapter.Binding, text, delivery string) (adapter.State, error) {
	id := target.SessionRef
	if !sessionID.MatchString(id) {
		return adapter.Unreachable, errors.New("opencode: deliver: target is not an OpenCode session id; nothing sent")
	}
	_, c, _, err := h.service()
	if err != nil {
		return adapter.Unreachable, fmt.Errorf("opencode: deliver %s: %w", id, err)
	}
	if target.HarnessEndpoint == "" || c.PinOrigin != target.HarnessEndpoint {
		return adapter.Unreachable, fmt.Errorf("opencode: deliver %s: service origin differs from the bound endpoint; nothing sent", id)
	}
	if ctx.Err() != nil {
		return adapter.Unreachable, fmt.Errorf("opencode: deliver %s: canceled before send; nothing sent", id)
	}
	if d, err := getSession(ctx, c, id); err != nil || d.isArchived() {
		return adapter.Unreachable, fmt.Errorf("opencode: deliver %s: session state unreadable or archived; nothing sent", id)
	}
	req := struct {
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
		Resume   *bool  `json:"resume,omitempty"`
	}{Text: text, Delivery: delivery}
	if delivery == "queue" {
		resume := false
		req.Resume = &resume
	}
	body, _ := json.Marshal(req)
	if ctx.Err() != nil {
		return adapter.Unreachable, fmt.Errorf("opencode: deliver %s: canceled before send; nothing sent", id)
	}
	r, err := c.Prompt(ctx, id, body)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && (e.Kind == KindUnreachable || e.Kind == KindNotFound) {
			return adapter.Unreachable, fmt.Errorf("opencode: deliver %s: %w", id, err)
		}
		return adapter.Unknown, fmt.Errorf("opencode: deliver %s: %w", id, err)
	}
	var doc struct {
		Data struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Delivery  string `json:"delivery"`
		} `json:"data"`
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &doc) != nil || doc.Data.ID == "" || doc.Data.SessionID != id {
		return adapter.Unknown, fmt.Errorf("opencode: deliver %s: response does not admit a message to the session", id)
	}
	switch doc.Data.Delivery {
	case "steer":
		return adapter.Accepted, nil
	case "queue":
		return adapter.Queued, nil
	}
	return adapter.Unknown, fmt.Errorf("opencode: deliver %s: response names an unknown delivery", id)
}
