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
	pid := os.Getpid()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "11111111-aaaa-bbbb-cccc-222222222222", "name": "claim-e2e", "cwd": "/tmp"})
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
	t.Cleanup(func() { in.Close(); cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, _ := filepath.Glob(filepath.Join(home, ".local", "state", "crosstalk", "agents", "*.json"))
		if len(m) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no descriptor within 10s of shim start, no tool call made: %v", m)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
