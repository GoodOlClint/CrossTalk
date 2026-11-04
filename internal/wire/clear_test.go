package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
)

// A session swapped by /clear and then /resume, in one process, rebinds its
// A2A listener and uds ingress each time and stays reachable by name.
func TestClearAndResumeRebindOverRealA2A(t *testing.T) {
	state, sessions := shortDir(t), shortDir(t)
	pid := os.Getpid()
	sock := filepath.Join(shortDir(t), "bob.sock")
	var mu sync.Mutex
	var got []string
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				r.ReadBytes('\n')
				l, _ := r.ReadBytes('\n')
				mu.Lock()
				got = append(got, string(l))
				mu.Unlock()
				c.Write([]byte(`{"type":"ack"}` + "\n"))
			}()
		}
	}()
	write := func(session string) {
		b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": session, "name": "bob", "cwd": "/w", "status": "idle", "messagingSocketPath": sock})
		if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".abc.key"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("sess-1")

	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: sessions, Ingress: udsingress.NewIngress})
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeCodex{Fake: adapter.NewFake(adapter.Codex)}
	w, err := New(shim.Options{StateDir: state, PPID: pid, Harnesses: []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: fc, Directory: registry.New(state)}), ch}})
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserver.New(w.Shim().MCPOptions())
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := w.Shim().Close(); err != nil {
			t.Error(err)
		}
	})
	bobCall := func() {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "receive_message", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("bound call: %v %+v", err, res)
		}
	}
	sender := newNode(t, state, 999998, "sender")
	expect := func(text string, n int) {
		t.Helper()
		if out := sender.ok("send_message", map[string]any{"to": "bob", "text": text}); out["state"] != "accepted" {
			t.Fatalf("%s: %v", text, out)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			ok := len(got) == n && strings.Contains(got[n-1], text)
			mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not delivered: %q", text, got)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	bobCall()
	expect("one", 1)
	write("sess-2")
	bobCall()
	expect("two", 2)
	write("sess-1")
	bobCall()
	expect("three", 3)
}
