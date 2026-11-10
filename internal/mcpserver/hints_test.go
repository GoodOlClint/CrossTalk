package mcpserver

import (
	"strings"
	"testing"
)

func TestEveryHintNamesARealTool(t *testing.T) {
	tools := []string{"list_agents", "send_message", "ask", "reply", "receive_message", "claim", "release", "rename", "attach", "detach", "grant"}
	for _, reason := range []string{"no_such_agent", "self", "agent_held", CodeUnbound, CodeNotReady, "no_such_message", "unknown_alias", "remote_native_unsupported", "unpaired_by_peer", "gateway_not_running", "tls_pin_mismatch", "invalid_name", "name_held", "invalid_inbox", "not_granted", "ownerless", "not_owner", "inbox_owned", "no_such_inbox", "not_member", "caller_not_primary", "remote_inbox_unsupported"} {
		h := hintFor(reason)
		named := false
		for _, tn := range tools {
			named = named || strings.Contains(h, tn)
		}
		if !named {
			t.Errorf("hint for %s names no tool: %q", reason, h)
		}
	}
}
