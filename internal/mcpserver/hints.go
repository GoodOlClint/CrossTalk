package mcpserver

// Hints maps a stable reason or error code to the model's next step. Every
// entry names a real tool.
var Hints = map[string]string{
	"no_such_agent":   "No session has that name or agent_id. Run list_agents again and copy the name or agent_id from a row.",
	"self":            "That row is this session. Pick another row from list_agents; this session is the one with you: true.",
	"agent_held":      "Another CrossTalk server already serves this session, usually a second install. Tell the user to remove the manual crosstalk MCP entry (claude mcp remove --scope user crosstalk) and restart this session; list_agents still works meanwhile.",
	CodeUnbound:       "CrossTalk cannot tell which session this call came from, so only list_agents works. Tell the user.",
	CodeNotReady:      "This session is still registering. Retry the call once; if it repeats, call list_agents and tell the user.",
	"loop_cap":        "The send cap for this peer is reached (20 sends in 10 minutes or 60 in an hour). This may be a message loop or simply a busy exchange. Stop sending to that peer and tell your user. The block lifts as older sends age out: within 10 minutes for the short cap, within an hour for the hourly one.",
	"no_such_message": "No reply route exists for that message_id. Use send_message to the sender instead.",
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
