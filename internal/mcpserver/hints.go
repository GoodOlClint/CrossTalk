package mcpserver

// Hints maps a stable reason or error code to the model's next step. Every
// entry names a real tool.
var Hints = map[string]string{
	"no_such_agent":   "No session has that name or agent_id. Run list_agents again and copy the name or agent_id from a row.",
	"self":            "That row is this session. Pick another row from list_agents; this session is the one with you: true.",
	"agent_held":      "Another CrossTalk server already serves this session, usually a second install. Tell the user to remove the manual crosstalk MCP entry (claude mcp remove --scope user crosstalk) and restart this session; list_agents still works meanwhile.",
	CodeUnbound:       "CrossTalk cannot tell which session this call came from, so only list_agents works. Tell the user.",
	CodeNotReady:      "This session is still registering. Retry the call once; if it repeats, call list_agents and tell the user.",
	"loop_cap":        "You have sent this session many messages in a short time, which looks like a loop. Stop sending to it and tell the user; receive_message shows anything it sent back.",
	"hop_repeat":      "This message would return to a session already in the chain that led to it, so CrossTalk stopped it. Do not forward it again; answer with reply to the original message, or tell the user.",
	"hop_overflow":    "This chain of forwards is too long, so CrossTalk stopped it. Do not forward it again; tell the user.",
	"no_such_message": "No reply route exists for that message_id: it was a first-contact inform that arrived before this session bound, or its route expired. Use send_message to the sender instead.",
}

func hintFor(reason string) string { return Hints[reason] }

func (e *Error) hint() string {
	if e.Hint != "" {
		return e.Hint
	}
	if h := hintFor(e.Reason); h != "" {
		return h
	}
	return hintFor(e.Code)
}

func describe(err error) string {
	out := err.Error()
	if h := toolError(err).hint(); h != "" {
		out += ". " + h
	}
	return out
}

func withHint(out any) any {
	switch r := out.(type) {
	case Result:
		if r.Hint == "" {
			r.Hint = hintFor(r.Reason)
		}
		return r
	case *Result:
		if r != nil && r.Hint == "" {
			r.Hint = hintFor(r.Reason)
		}
	}
	return out
}
