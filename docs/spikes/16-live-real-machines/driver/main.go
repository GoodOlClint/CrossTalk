// Throwaway live-spike driver (S2, S5, S7). Not shipped. It runs the production
// shim, wire and MCP server over a Fake Codex harness so a host with no Claude
// or Codex session can take part in a remote exchange.
//
//	driver agent NAME [autoreply]   register NAME, stay up, print each injected message
//	driver send FROM TO N [TEXT]    N send_message calls, one JSON line per run
//	driver ask FROM TO TEXT         one ask, then wait for the reply as an inbound write
//	driver list FROM                one list_agents call
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/wire"
)

type fakeCodex struct {
	*adapter.Fake
	mu      sync.Mutex
	threads []codex.LoadedThread
}

func (f *fakeCodex) LoadedThreads(context.Context, ...string) []codex.LoadedThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codex.LoadedThread(nil), f.threads...)
}

type node struct {
	fc     *fakeCodex
	w      *wire.Wire
	srv    *mcpserver.Server
	cs     *mcp.ClientSession
	thread string
}

func newNode(name string) (*node, error) {
	state, err := registry.DefaultDir()
	if err != nil {
		return nil, err
	}
	n := &node{thread: "spike-" + name}
	n.fc = &fakeCodex{Fake: adapter.NewFake(adapter.Codex)}
	n.fc.threads = []codex.LoadedThread{{Endpoint: "fake://" + n.thread, ID: n.thread, Name: name, Cwd: "/spike", Status: "idle",
		Session: adapter.SessionStatus{Loaded: true, Reachable: true}}}
	hs := []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: n.fc, Directory: registry.New(state)})}
	n.w, err = wire.New(shim.Options{StateDir: state, Harnesses: hs})
	if err != nil {
		return nil, err
	}
	n.srv = mcpserver.New(n.w.Shim().MCPOptions())
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := n.srv.MCP().Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	n.cs, err = mcp.NewClient(&mcp.Implementation{Name: "spike", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, err
	}
	n.call("receive_message", map[string]any{})
	return n, nil
}

func (n *node) call(tool string, args any) (bool, map[string]any, time.Duration) {
	t0 := time.Now()
	res, err := n.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": n.thread}, Name: tool, Arguments: args})
	d := time.Since(t0)
	if err != nil {
		return true, map[string]any{"protocol_error": err.Error()}, d
	}
	out := map[string]any{}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return res.IsError, out, d
}

func (n *node) close() {
	n.cs.Close()
	n.srv.Close()
	n.w.Shim().Close()
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: driver agent|send|ask|list ...")
		os.Exit(2)
	}
	cmd := os.Args[1]
	name := os.Args[2]
	n, err := newNode(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "driver:", err)
		os.Exit(1)
	}
	defer n.close()
	switch cmd {
	case "agent":
		agent(n, len(os.Args) > 3 && os.Args[3] == "autoreply")
	case "send":
		cnt, _ := strconv.Atoi(os.Args[4])
		text := "spike ping"
		if len(os.Args) > 5 {
			text = os.Args[5]
		}
		for i := 1; i <= cnt; i++ {
			isErr, out, d := n.call("send_message", map[string]any{"to": os.Args[3], "text": fmt.Sprintf("%s %d", text, i)})
			line, _ := json.Marshal(map[string]any{"run": i, "ms": d.Milliseconds(), "is_error": isErr, "result": out})
			fmt.Println(string(line))
			time.Sleep(500 * time.Millisecond)
		}
	case "ask":
		isErr, out, d := n.call("ask", map[string]any{"to": os.Args[3], "text": os.Args[4], "deadline_seconds": 60})
		line, _ := json.Marshal(map[string]any{"ms": d.Milliseconds(), "is_error": isErr, "result": out})
		fmt.Println(string(line))
		deadline := time.Now().Add(40 * time.Second)
		seen := 0
		for time.Now().Before(deadline) {
			w := n.fc.Writes()
			for ; seen < len(w); seen++ {
				fmt.Printf("INBOUND kind=%s in_reply_to=%v text=%q\n", w[seen].Message.Kind, w[seen].Message.InReplyTo, w[seen].Message.Text())
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Println("INBOUND none within 40s")
	case "list":
		isErr, out, d := n.call("list_agents", map[string]any{})
		line, _ := json.MarshalIndent(map[string]any{"ms": d.Milliseconds(), "is_error": isErr, "result": out}, "", " ")
		fmt.Println(string(line))
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}

func agent(n *node, autoreply bool) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("agent up")
	seen := 0
	for ctx.Err() == nil {
		w := n.fc.Writes()
		for ; seen < len(w); seen++ {
			m := w[seen].Message
			fmt.Printf("%s INJECTED kind=%s from=%s text=%q\n", time.Now().UTC().Format("15:04:05.000"), m.Kind, m.From, m.Text())
			if autoreply && m.Kind == "ask" {
				isErr, out, _ := n.call("reply", map[string]any{"message_id": m.MessageID, "text": "spike pong"})
				b, _ := json.Marshal(out)
				fmt.Printf("REPLY is_error=%v %s\n", isErr, b)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
