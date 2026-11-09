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

func TestListAgentsBackendField(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "T-cx", Name: "cx", Cwd: "/w/cx", Status: "loaded", Reachable: true, Backend: "unreachable"},
		Session{Harness: "claude", SessionRef: "S-cl", Name: "cl", Cwd: "/w/cl", Status: "idle", Reachable: true},
	)
	e := newEnv(t, 4242, fb)
	b, err := json.Marshal(e.mustOK(nil, "list_agents", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Agents) != 2 {
		t.Fatalf("%s: %v", b, err)
	}
	for _, r := range out.Agents {
		v, has := r["backend"]
		switch r["harness"] {
		case "codex":
			if v != "unreachable" {
				t.Errorf("codex row backend = %v", v)
			}
		case "claude":
			if has {
				t.Errorf("claude row has backend key: %s", b)
			}
		}
	}
}
