package mcpserver

import (
	"strings"
	"testing"
)

func TestEveryHintNamesARealTool(t *testing.T) {
	tools := []string{"list_agents", "send_message", "ask", "reply", "receive_message", "claim", "release"}
	for _, reason := range []string{"no_such_agent", "self", "agent_held", CodeUnbound, CodeNotReady, "no_such_message"} {
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
