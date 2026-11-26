package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakePiHost replaces the Pi host check in a child shim: "yes" accepts the
// parent; any other non-empty value is a marker file written by a check that
// then never returns.
func fakePiHost(mode string) {
	switch mode {
	case "":
	case "yes":
		piHost = func(int) bool { return true }
	default:
		piHost = func(int) bool {
			os.WriteFile(mode, nil, 0o600)
			time.Sleep(time.Hour)
			return false
		}
	}
}

func piHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "ctpi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

// startPiTestShim starts a child shim whose harnesses can reach nothing
// outside home.
func startPiTestShim(t *testing.T, cmd *exec.Cmd, home string, env ...string) *mcpProc {
	t.Helper()
	return startShimCmd(t, cmd, home, append([]string{
		"CROSSTALK_TEST_MAIN=1", "CROSSTALK_NO_AUTO_REFRESH=1", "CROSSTALK_CODEX_ENDPOINTS=",
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
	}, env...)...)
}

// M5: Pi waits 10 s for a direct server's initialize, so the production
// wiring must answer it while a claim source's startup work is still held.
func TestMCPInitializeNotHeldByPiHostCheck(t *testing.T) {
	home := piHome(t)
	held := filepath.Join(home, "held")
	cmd := exec.Command(os.Args[0], "mcp")
	p := startPiTestShim(t, cmd, home, "CROSSTALK_TEST_PI_HOST="+held)
	// The held claim source never returns, so the shim cannot exit by itself.
	t.Cleanup(func() { cmd.Process.Kill() })
	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"pi","version":"1"}}}`)
	line := p.expect(`"id":1`, 8*time.Second)
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &init); err != nil || init.Result.ProtocolVersion != "2025-06-18" || init.Result.Capabilities.Tools == nil {
		t.Fatalf("initialize did not succeed: %v %s", err, line)
	}
	if !within(8*time.Second, func() bool { _, err := os.Stat(held); return err == nil }) {
		t.Fatal("the Pi host check never ran, so nothing was held")
	}
}

// A Pi session is listed to other agents as soon as its extension says hello,
// before it calls any tool.
func TestMCPListsPiSessionBeforeAnyToolCall(t *testing.T) {
	home := piHome(t)
	const token = "tok-pi-wire"
	owner := startPiTestShim(t, exec.Command(os.Args[0], "mcp"), home, "CROSSTALK_TEST_PI_HOST=yes", "CROSSTALK_PI_TOKEN="+token)
	owner.handshake()

	sock := filepath.Join(home, ".local", "state", "crosstalk", "sock", "pi", strconv.Itoa(os.Getpid())+".sock")
	var c net.Conn
	if !within(10*time.Second, func() bool {
		var err error
		c, err = net.Dial("unix", sock)
		return err == nil
	}) {
		t.Fatalf("no Pi listener at %s", sock)
	}
	defer c.Close()
	hello, _ := json.Marshal(map[string]any{"proto": 1, "token": token, "ext_version": "0.3.0", "pi_version": "1.6.0", "session_id": "wire-1", "session_file": "", "cwd": "/tmp", "name": "pi-wired"})
	if _, err := c.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || !strings.Contains(line, `"hello":"ok"`) {
		t.Fatalf("hello reply %q %v", line, err)
	}

	other := startPiTestShim(t, exec.Command(os.Args[0], "mcp"), home)
	other.handshake()
	other.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}`)
	var res struct {
		Result struct {
			StructuredContent struct {
				Agents []struct {
					Name      string `json:"name"`
					Harness   string `json:"harness"`
					Reachable bool   `json:"reachable"`
					Crosstalk bool   `json:"crosstalk"`
				} `json:"agents"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	l := other.expect(`"id":2`, 15*time.Second)
	if err := json.Unmarshal([]byte(l), &res); err != nil {
		t.Fatal(err)
	}
	for _, a := range res.Result.StructuredContent.Agents {
		if a.Harness == "pi" {
			if a.Name != "pi-wired" || !a.Reachable || !a.Crosstalk {
				t.Fatalf("Pi row %+v", a)
			}
			return
		}
	}
	t.Fatalf("list_agents has no Pi session: %s", l)
}
