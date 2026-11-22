package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const versionPkg = "github.com/GoodOlClint/crosstalk/internal/mcpserver.Version"

// buildVersion builds the crosstalk binary stamped with version into out.
func buildVersion(t *testing.T, out, version string) {
	t.Helper()
	b, err := exec.Command("go", "build", "-ldflags", "-X "+versionPkg+"="+version, "-o", out, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", version, err, b)
	}
}

// refreshHome is a short HOME whose Claude registration names bin and whose
// session file binds the test process as a Claude session.
func refreshHome(t *testing.T, bin string) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "ctr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	reg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"crosstalk": map[string]any{"type": "stdio", "command": bin, "args": []string{"mcp"}}}})
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	s, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "11111111-aaaa-bbbb-cccc-333333333333", "name": "refresh-e2e", "cwd": "/tmp"})
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), s, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

type mcpProc struct {
	t     *testing.T
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
	home  string
}

func startShim(t *testing.T, bin, home string, env ...string) *mcpProc {
	t.Helper()
	return startShimCmd(t, exec.Command(bin, "mcp"), home, env...)
}

// shimEnv is the test process's environment for a real shim under home,
// with PATH cut to the system directories so its upgrade follow-up never
// finds the real claude or codex CLI. exec keeps the last of duplicate keys.
func shimEnv(home string, env ...string) []string {
	return append(append(os.Environ(), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME="+home), env...)
}

func startShimCmd(t *testing.T, cmd *exec.Cmd, home string, env ...string) *mcpProc {
	t.Helper()
	cmd.Env = shimEnv(home, env...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &mcpProc{t: t, cmd: cmd, in: in, lines: make(chan string, 64), home: home}
	go func() {
		r := bufio.NewReader(out)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				close(p.lines)
				return
			}
			p.lines <- l
		}
	}()
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
	return p
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(b []byte) (int, error) {
	w.t.Logf("shim stderr: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func (p *mcpProc) send(frame string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, frame+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

// expect reads stdout lines until one contains want.
func (p *mcpProc) expect(want string, within time.Duration) string {
	p.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				p.t.Fatalf("stdout closed waiting for %q", want)
			}
			if strings.Contains(l, want) {
				return l
			}
		case <-deadline:
			p.t.Fatalf("no stdout line with %q within %v", want, within)
		}
	}
}

func (p *mcpProc) handshake() {
	p.t.Helper()
	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"refresh-test","version":"1"}}}`)
	p.expect(`"id":1`, 10*time.Second)
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

type descriptor struct {
	PID    int    `json:"pid"`
	A2AURL string `json:"a2a_url"`
}

// descriptorWhere waits for the one agent descriptor to satisfy ok.
func (p *mcpProc) descriptorWhere(what string, ok func(descriptor) bool) descriptor {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		m, _ := filepath.Glob(filepath.Join(p.home, ".local", "state", "crosstalk", "agents", "*.json"))
		if len(m) == 1 {
			var d descriptor
			if b, err := os.ReadFile(m[0]); err == nil && json.Unmarshal(b, &d) == nil && ok(d) {
				return d
			}
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("no descriptor %s within 15s: %v", what, m)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func cardVersion(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c.Version
}

var builds = struct {
	sync.Mutex
	dir  string
	bins map[string]string
}{bins: map[string]string{}}

// copyOf copies a binary stamped with version, built once per test run, to
// dst.
func copyOf(t *testing.T, dst, version string) {
	t.Helper()
	builds.Lock()
	src, ok := builds.bins[version]
	if !ok {
		if builds.dir == "" {
			d, err := os.MkdirTemp("", "ctv")
			if err != nil {
				builds.Unlock()
				t.Fatal(err)
			}
			builds.dir = d
		}
		src = filepath.Join(builds.dir, version)
		buildVersion(t, src, version)
		builds.bins[version] = src
	}
	builds.Unlock()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func replaceByRename(t *testing.T, dst, version string) {
	t.Helper()
	tmp := dst + ".new"
	copyOf(t, tmp, version)
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func replaceWithScript(t *testing.T, dst, script string) {
	t.Helper()
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

// noRefreshAfter replaces the binary and checks for three ticks that the
// first image keeps serving: no list_changed, the same A2A endpoint at the
// first version, and tool calls answered.
func noRefreshAfter(t *testing.T, p *mcpProc, replace func()) {
	t.Helper()
	p.handshake()
	before := p.descriptorWhere("for the first image", func(d descriptor) bool { return d.A2AURL != "" })
	replace()
	deadline := time.After(3*time.Second*2 + 500*time.Millisecond)
wait:
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatal("shim stdout closed after the replacement")
			}
			if strings.Contains(l, "list_changed") {
				t.Fatalf("refreshed: %s", l)
			}
		case <-deadline:
			break wait
		}
	}
	after := p.descriptorWhere("still the first image's", func(descriptor) bool { return true })
	if after.A2AURL != before.A2AURL {
		t.Fatalf("descriptor endpoint changed %s -> %s: a new image registered", before.A2AURL, after.A2AURL)
	}
	if v := cardVersion(t, after.A2AURL); v != "9.9.1" {
		t.Fatalf("card version = %q, want the first image's 9.9.1", v)
	}
	p.send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}`)
	if l := p.expect(`"id":8`, 10*time.Second); strings.Contains(l, `"error"`) {
		t.Fatalf("tools/call after the replacement: %s", l)
	}
}

func firstImage(t *testing.T) (bin, home string) {
	t.Helper()
	bin = filepath.Join(t.TempDir(), "crosstalk")
	copyOf(t, bin, "9.9.1")
	return bin, refreshHome(t, bin)
}

func TestRefreshNotArmedForRelativeArgv0(t *testing.T) {
	bin, home := firstImage(t)
	reg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"crosstalk": map[string]any{"type": "stdio", "command": "crosstalk", "args": []string{"mcp"}}}})
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Args[0], cmd.Dir = "crosstalk", filepath.Dir(bin)
	p := startShimCmd(t, cmd, home, "PATH="+filepath.Dir(bin)+":/usr/bin:/bin:/usr/sbin:/sbin")
	noRefreshAfter(t, p, func() { replaceByRename(t, bin, "9.9.2") })
}

func TestRefreshNotArmedForUnregisteredPath(t *testing.T) {
	bin, home := firstImage(t)
	reg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"crosstalk": map[string]any{"type": "stdio", "command": filepath.Join(home, "elsewhere", "crosstalk"), "args": []string{"mcp"}}}})
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	p := startShim(t, bin, home)
	noRefreshAfter(t, p, func() { replaceByRename(t, bin, "9.9.2") })
}

func TestRefreshOffByEnv(t *testing.T) {
	bin, home := firstImage(t)
	p := startShim(t, bin, home, "CROSSTALK_NO_AUTO_REFRESH=1")
	noRefreshAfter(t, p, func() { replaceByRename(t, bin, "9.9.2") })
}

func TestRefreshSkipsBadBinary(t *testing.T) {
	bin, home := firstImage(t)
	p := startShim(t, bin, home)
	noRefreshAfter(t, p, func() { replaceWithScript(t, bin, "exit 1") })
}

func TestRefreshSkipsBinaryWithoutHandoff(t *testing.T) {
	bin, home := firstImage(t)
	p := startShim(t, bin, home)
	noRefreshAfter(t, p, func() { replaceWithScript(t, bin, `echo '{"version":"0.1.0"}'`) })
}

func TestRefreshExecsOnInodeChange(t *testing.T) {
	bin, home := firstImage(t)
	p := startShim(t, bin, home)
	p.handshake()
	before := p.descriptorWhere("for the first image", func(d descriptor) bool { return d.A2AURL != "" })
	if v := cardVersion(t, before.A2AURL); v != "9.9.1" {
		t.Fatalf("first image card version = %q", v)
	}
	replaceByRename(t, bin, "9.9.2")
	p.expect(`notifications/tools/list_changed`, 15*time.Second)
	after := p.descriptorWhere("rewritten by the new image", func(d descriptor) bool { return d.A2AURL != "" && d.A2AURL != before.A2AURL })
	if after.PID != p.cmd.Process.Pid || before.PID != p.cmd.Process.Pid {
		t.Fatalf("descriptor pids %d -> %d, shim pid %d: not the same process", before.PID, after.PID, p.cmd.Process.Pid)
	}
	if v := cardVersion(t, after.A2AURL); v != "9.9.2" {
		t.Fatalf("after refresh card version = %q, want 9.9.2", v)
	}
	p.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}`)
	if l := p.expect(`"id":7`, 10*time.Second); strings.Contains(l, `"error"`) {
		t.Fatalf("tools/call after refresh, same pipe, no initialize: %s", l)
	}
}
