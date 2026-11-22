package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/upgrade"
)

// probeHome is a home where Claude is set up with the plugin at an old
// version and registered to run a copy of the built binary, with a fake
// claude CLI on PATH that records being started.
func probeHome(t *testing.T) (home, bin string, env []string) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "ct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	bin = filepath.Join(home, "bin", "crosstalk")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	b, err := os.ReadFile(installBuild(t, "."))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"plugins":{"crosstalk@crosstalk":[]}}`), 0o644)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"crosstalk":{"type":"stdio","command":"`+bin+`","args":["mcp"]}}}`), 0o644)
	fake := filepath.Join(home, "fakebin")
	os.MkdirAll(fake, 0o755)
	os.WriteFile(filepath.Join(fake, "claude"), []byte("#!/bin/sh\necho \"$@\" >> "+filepath.Join(home, "started")+"\nexit 0\n"), 0o755)
	env = append(os.Environ(), "HOME="+home, "PATH="+fake+":/usr/bin:/bin:/usr/sbin:/sbin", "CROSSTALK_NO_AUTO_REFRESH=")
	return home, bin, env
}

func probeStarted(home string) bool {
	_, err := os.Stat(filepath.Join(home, "started"))
	return err == nil
}

// A shim started by `claude mcp get` or `claude mcp list` answers initialize
// and tools/list, binds no session and exits; it starts no follow-up (#264).
func TestMCPProbeShimStartsNoFollowUp(t *testing.T) {
	home, bin, env := probeHome(t)
	cmd := exec.Command(bin, "mcp")
	cmd.Env = env
	cmd.Stdout = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(in, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`)
	fmt.Fprintln(in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	fmt.Fprintln(in, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	time.Sleep(700 * time.Millisecond)
	in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("probe shim did not exit after stdin closed")
	}
	state := filepath.Join(home, ".local", "state", "crosstalk")
	if probeStarted(home) {
		b, _ := os.ReadFile(filepath.Join(home, "started"))
		t.Errorf("a probe shim ran the claude CLI:\n%s", b)
	}
	if _, err := os.Stat(upgrade.LockPath(state)); err == nil {
		t.Error("a probe shim took upgrade.lock")
	}
	if m, _ := filepath.Glob(filepath.Join(state, "upgrade", "*", "follow-up.log")); len(m) != 0 {
		t.Errorf("a probe shim wrote %v", m)
	}
}

// The same shim serving a Claude session registered at start runs the
// follow-up with no tool call made.
func TestMCPSessionShimRunsFollowUp(t *testing.T) {
	home, bin, env := probeHome(t)
	sessions := filepath.Join(home, ".claude", "sessions")
	os.MkdirAll(sessions, 0o755)
	pid := os.Getpid()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "11111111-aaaa-bbbb-cccc-333333333333", "name": "probe-e2e", "cwd": "/tmp"})
	os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), b, 0o600)
	cmd := exec.Command(bin, "mcp")
	cmd.Env = env
	cmd.Stdout = io.Discard
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
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	if !within(15*time.Second, func() bool { return probeStarted(home) }) {
		t.Fatal("a shim serving a registered Claude session never started the follow-up")
	}
}
