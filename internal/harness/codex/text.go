package codex

import (
	"encoding/json"

	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// Preamble is the notice a Codex tool output carries for class c: a fixed
// opening, because a tool output otherwise reads as the model's own call,
// then the shared class text.
func Preamble(c msg.NoticeClass, fromHarness string) string {
	return "This tool output is a message from another agent session, delivered by CrossTalk. " + msg.PeerNotice(c, fromHarness)
}

// Render is RenderIn for a direct message.
func Render(m msg.Message, verifiedSender bool, fromHarness, fromAgentID string, class msg.NoticeClass) string {
	return RenderIn(m, verifiedSender, fromHarness, fromAgentID, "", false, class)
}

// RenderIn is Render for a message that arrived through the role inbox named
// inbox; "" names none.
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
	}{Preamble(class, fromHarness), m.From, fromHarness, fromAgentID, verifiedSender, m.MessageID, m.InReplyTo, kind, inbox, inboxCopy, m.Parts}
	out, _ := json.Marshal(data)
	return string(out)
}
