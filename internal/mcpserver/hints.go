package mcpserver

// Hints maps a stable reason or error code to the model's next step. Every
// entry names a real tool.
var Hints = map[string]string{
	"no_such_agent":             "No session has that name or agent_id. Run list_agents again and copy the name or agent_id from a row.",
	"self":                      "That row is this session. Pick another row from list_agents; this session is the one with you: true.",
	"agent_held":                "Another CrossTalk server already serves this session, usually a second install. Tell the user to remove the manual crosstalk MCP entry (claude mcp remove --scope user crosstalk) and restart this session; list_agents still works meanwhile.",
	CodeUnbound:                 "CrossTalk cannot tell which session this call came from, so only list_agents works. Tell the user.",
	CodeNotReady:                "This session is still registering. Retry the call once; if it repeats, call list_agents and tell the user.",
	"unknown_alias":             "No paired host has that name. list_agents shows the paired hosts and their agents; address one as name@host from a row.",
	"remote_native_unsupported": "That session has no CrossTalk and cannot be messaged from another host. Pick a row from list_agents with crosstalk: true, or tell your user.",
	"unpaired_by_peer":          "That host no longer accepts this one. Tell your user to pair the two hosts again; list_agents shows the paired hosts.",
	"gateway_not_running":       "The other host's CrossTalk gateway is not running. Tell your user to start it there, then send_message again.",
	"tls_pin_mismatch":          "The host at that address is not the one that was paired. Tell your user and do not retry send_message.",
	"invalid_name":              "That name cannot be used: 1 to 64 bytes, no @, no control characters, not 16 hex digits, not starting with inbox:. Pick another and call rename again.",
	"name_held":                 "Another live session already has that name. Call list_agents to see who, pick a different name, or retry once if that session has just ended.",
	"no_such_message":           "No reply route exists for that message_id. Use send_message to the sender instead.",
}

// HintRemoteKind explains remote_kind to the model.
const HintRemoteKind = "command, claim and release reach only sessions on this host in this version. Use send_message with the default kind or ask to reach a session on a paired host."

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
