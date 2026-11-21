package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A Claude session whose parent session file exists is listed with a
// descriptor within seconds of the shim starting, with no CrossTalk call made
// (#145, #185).
func TestMCPRegistersClaudeSessionAtStart(t *testing.T) {
	bin := installBuild(t, ".")
	home, err := os.MkdirTemp("/tmp", "ct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "11111111-aaaa-bbbb-cccc-222222222222"
	pid := os.Getpid()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": sid, "name": "claim-e2e", "cwd": "/tmp"})
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "HOME="+home)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, _ := filepath.Glob(filepath.Join(home, ".local", "state", "crosstalk", "agents", "*.json"))
		if len(m) == 1 {
			var d struct{ Harness, Name, SessionRef string }
			raw, _ := os.ReadFile(m[0])
			var j map[string]any
			if json.Unmarshal(raw, &j) == nil {
				d.Harness, _ = j["harness"].(string)
				d.Name, _ = j["name"].(string)
				d.SessionRef, _ = j["session_ref"].(string)
			}
			if d.Harness != "claude" || d.Name != "claim-e2e" || d.SessionRef != sid {
				t.Fatalf("descriptor %s = %+v, want claude %q %q", m[0], d, "claim-e2e", sid)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no descriptor within 10s of shim start, no tool call made: %v", m)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
