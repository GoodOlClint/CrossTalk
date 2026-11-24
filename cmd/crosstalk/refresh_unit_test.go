package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/refresh"
)

// Arming takes only the exact absolute command a registration names: a
// relative command with a slash (which Registered counts as maybe ours) and
// a symlink to the same file do not arm.
func TestRefreshArmsOnlyForTheExactRegisteredCommand(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin", "crosstalk")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("x"), 0o755)
	link := filepath.Join(home, "link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	hs, err := harnesses(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		command string
		want    bool
	}{{bin, true}, {"./bin/crosstalk", false}, {link, false}} {
		reg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"crosstalk": map[string]any{"type": "stdio", "command": c.command, "args": []string{"mcp"}}}})
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), reg, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := refreshArmed(bin, home, hs); got != c.want {
			t.Errorf("registered %q, argv0 %q: armed=%v, want %v", c.command, bin, got, c.want)
		}
	}
}

// The transport reads a close-on-exec duplicate of fd 0, never a second
// wrapper of fd 0 whose finalizer could close stdin.
func TestPollableStdinIsADuplicate(t *testing.T) {
	saved, err := syscall.Dup(0)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := dup2(int(r.Fd()), 0); err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer func() { dup2(saved, 0); syscall.Close(saved); stdinFlags = -1 }()
	f, err := pollableStdin()
	if err != nil {
		t.Fatalf("pipe stdin not pollable: %v", err)
	}
	if f.Fd() == 0 {
		t.Fatal("the transport would own fd 0")
	}
	f.Close()
	runtime.GC()
	runtime.GC()
	var st syscall.Stat_t
	if err := syscall.Fstat(0, &st); err != nil {
		t.Fatalf("fd 0 closed: %v", err)
	}
}

// A file replaced again while the handoff is prepared is not exec'd, and
// the error is not one that rules the candidate out.
func TestExecHandoffRefusesAFileReplacedDuringPreparation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crosstalk")
	os.WriteFile(path, []byte("a"), 0o755)
	probed, _ := mcpserver.IdentityOf(path)
	os.WriteFile(path+".n", []byte("b"), 0o755)
	os.Rename(path+".n", path)
	srv := mcpserver.New(mcpserver.Options{})
	err := execHandoff(srv, mcpserver.NewConn(strings.NewReader(""), io.Discard), path, probed, refresh.Probed{})
	if !errors.Is(err, errChanged) || errors.Is(err, refresh.ErrExecFailed) {
		t.Fatalf("exec of a replaced file: %v, want errChanged", err)
	}
}

// A listen opened after the probe still stops the handoff to a file that
// cannot replay it, before any exec.
func TestExecHandoffRefusesOpenListenWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crosstalk")
	os.WriteFile(path, []byte("a"), 0o755)
	probed, _ := mcpserver.IdentityOf(path)
	conn := mcpserver.NewConn(strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true}}}`+"\n"), io.Discard)
	if _, err := conn.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	ack := &jsonrpc.Request{Method: "notifications/subscriptions/acknowledged", Params: json.RawMessage(`{"_meta":{"io.modelcontextprotocol/subscriptionId":3},"notifications":{"toolsListChanged":true}}`)}
	if err := conn.Write(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	srv := mcpserver.New(mcpserver.Options{})
	err := execHandoff(srv, conn, path, probed, refresh.Probed{})
	if !errors.Is(err, refresh.ErrExecFailed) || !strings.Contains(err.Error(), "subscriptions/listen") {
		t.Fatalf("handoff with an open listen to a file without replay: %v, want ErrExecFailed naming the listen", err)
	}
}
