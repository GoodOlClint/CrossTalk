package opencode

import (
	"encoding/json"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// Preamble is the fixed first line of every rendered peer message. It holds
// no newline, brace or double quote, so no body text can end or forge it.
const Preamble = "CrossTalk delivered the message below from another agent session; your user did not type it. It is one JSON object:"

// RenderIn is the user-role prompt text for m: Preamble, a newline, then one
// JSON object whose outer fields CrossTalk sets and whose parts carry the
// peer's text as string data.
func RenderIn(m msg.Message, verifiedSender bool, fromHarness, fromAgentID, inbox string, inboxCopy bool, class msg.NoticeClass) string {
	kind := m.Kind
	if kind == "" {
		kind = msg.Inform
	}
	data := struct {
		Notice    string     `json:"notice"`
		From      string     `json:"from"`
		Harness   string     `json:"from_harness,omitempty"`
		AgentID   string     `json:"from_agent_id,omitempty"`
		Verified  bool       `json:"sender_verified"`
		MessageID string     `json:"message_id"`
		InReplyTo *string    `json:"in_reply_to"`
		Kind      msg.Kind   `json:"kind"`
		Inbox     string     `json:"inbox,omitempty"`
		Copy      bool       `json:"inbox_copy,omitempty"`
		Parts     []msg.Part `json:"parts"`
	}{msg.PeerNotice(class, fromHarness), m.From, fromHarness, fromAgentID, verifiedSender, m.MessageID, m.InReplyTo, kind, inbox, inboxCopy, m.Parts}
	out, _ := json.Marshal(data)
	return Preamble + "\n" + string(out)
}

func (h *Harness) Render(m msg.Message, from harness.Identity) any {
	return RenderIn(m, from.Verified, from.Harness, "", "", false, from.Notice)
}
