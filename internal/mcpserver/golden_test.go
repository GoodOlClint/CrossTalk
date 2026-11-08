package mcpserver

import (
	"encoding/json"
	"testing"
)

const localListGolden = `{"agents":[{"agent_id":"e0c05562c6b2434a","crosstalk":false,"cwd":"/w/cx","harness":"codex","name":"cx","owner":null,"reachable":true,"status":"loaded"},{"agent_id":"b035bf4f6a447cdd","crosstalk":false,"cwd":"/w/p","harness":"codex","name":"plain-codex","owner":null,"reachable":false,"status":"loaded"}]}`

func TestLocalListAgentsRowsUnchanged(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "T-cx", Name: "cx", Cwd: "/w/cx", Status: "loaded", Reachable: true},
		Session{Harness: "codex", SessionRef: "T-plain", Name: "plain-codex", Cwd: "/w/p", Status: "loaded"},
	)
	e := newEnv(t, 4242, fb)
	b, err := json.Marshal(e.mustOK(nil, "list_agents", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != localListGolden {
		t.Fatalf("local list_agents JSON changed:\n got %s\nwant %s", b, localListGolden)
	}
}
