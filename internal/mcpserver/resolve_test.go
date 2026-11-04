package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
)

func writeClaudeSession(t *testing.T, dir string, pid int, sessionID, name, cwd string) {
	t.Helper()
	b := []byte(`{"pid":` + itoa(pid) + `,"sessionId":"` + sessionID + `","name":"` + name + `","cwd":"` + cwd + `","status":"idle","messagingSocketPath":"` + filepath.Join(dir, "none.sock") + `"}`)
	if err := os.WriteFile(filepath.Join(dir, itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCaller(t *testing.T) {
	dir := t.TempDir()
	writeClaudeSession(t, dir, 4242, "claude-sess", "alpha", "/work/alpha")
	hs := testHarnesses(t, dir)

	cases := []struct {
		name    string
		meta    map[string]any
		ppid    int
		harness adapter.Harness
		ref     string
	}{
		{"threadId wins over ppid", map[string]any{"threadId": "t-1", "sessionId": "s-1"}, 4242, adapter.Codex, "t-1"},
		{"threadId without sessionId", map[string]any{"threadId": "t-2"}, 0, adapter.Codex, "t-2"},
		{"sessionId alone is not identity", map[string]any{"sessionId": "s-1"}, 0, "", ""},
		{"empty threadId", map[string]any{"threadId": ""}, 0, "", ""},
		{"non-string threadId", map[string]any{"threadId": 7}, 0, "", ""},
		{"claude parent", nil, 4242, adapter.Claude, "claude-sess"},
		{"sessionId alone falls to ppid", map[string]any{"sessionId": "s-1"}, 4242, adapter.Claude, "claude-sess"},
		{"no session file", nil, 999, "", ""},
		{"init parent", nil, 1, "", ""},
	}
	for _, c := range cases {
		got, err := resolveCaller(context.Background(), hs, harness.RequestContext{Meta: c.meta, ParentPID: c.ppid})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.harness != c.harness || got.sessionRef != c.ref {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, got.harness, got.sessionRef, c.harness, c.ref)
		}
	}
	got, _ := resolveCaller(context.Background(), hs, harness.RequestContext{ParentPID: 4242})
	if got.name != "alpha" || got.cwd != "/work/alpha" || got.harnessPID != 4242 {
		t.Errorf("claude caller fields: %+v", got)
	}
}
