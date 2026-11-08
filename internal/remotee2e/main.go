// Test-only workload for scripts/remote-e2e: one Fake-backed Codex-shaped agent served by a shim, plus
// a loopback control port the scenario script drives with `call` and `writes`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/wire"
)

const control = "127.0.0.1:9000"

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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: remotee2e agent NAME | call TOOL JSON | writes")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "agent":
		err = runAgent(os.Args[2])
	case "call":
		err = client("POST", "/call/"+os.Args[2], os.Args[3])
	case "writes":
		err = client("GET", "/writes", "")
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "remotee2e:", err)
		os.Exit(1)
	}
}

func client(method, path, body string) error {
	req, err := http.NewRequest(method, "http://"+control+path, stringsReader(body))
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Println(string(b))
	if resp.StatusCode != 200 {
		return fmt.Errorf("control answered %d", resp.StatusCode)
	}
	return nil
}

func runAgent(name string) error {
	thread := "T-" + name
	fc := &fakeCodex{Fake: adapter.NewFake(adapter.Codex)}
	fc.threads = []codex.LoadedThread{{Endpoint: "fake://" + thread, ID: thread, Name: name, Cwd: "/work/" + name, Status: "idle",
		Session: adapter.SessionStatus{Loaded: true, Reachable: true}}}
	w, err := wire.New(shim.Options{PPID: os.Getpid(), Harnesses: []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: fc})}})
	if err != nil {
		return err
	}
	defer w.Shim().Close()
	srv := mcpserver.New(w.Shim().MCPOptions())
	defer srv.Close()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		return err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "remotee2e", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	defer cs.Close()
	call := func(tool string, args any) (map[string]any, bool, error) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Meta: map[string]any{"threadId": thread}, Name: tool, Arguments: args})
		if err != nil {
			return nil, false, err
		}
		out := map[string]any{}
		if res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			json.Unmarshal(b, &out)
		}
		return out, res.IsError, nil
	}
	if _, isErr, err := call("receive_message", map[string]any{}); err != nil || isErr {
		return fmt.Errorf("register: %v (tool error %v)", err, isErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /call/{tool}", func(rw http.ResponseWriter, r *http.Request) {
		var args map[string]any
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		out, isErr, err := call(r.PathValue("tool"), args)
		if err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		out["is_error"] = isErr
		json.NewEncoder(rw).Encode(out)
	})
	mux.HandleFunc("GET /writes", func(rw http.ResponseWriter, r *http.Request) {
		type row struct {
			MessageID string `json:"message_id"`
			From      string `json:"from"`
			Kind      string `json:"kind"`
			Text      string `json:"text"`
			InReplyTo string `json:"in_reply_to"`
		}
		rows := []row{}
		for _, c := range fc.Writes() {
			r := row{MessageID: c.Message.MessageID, From: c.Message.From, Kind: string(c.Message.Kind), Text: c.Message.Parts[0].Text}
			if c.Message.InReplyTo != nil {
				r.InReplyTo = *c.Message.InReplyTo
			}
			rows = append(rows, r)
		}
		json.NewEncoder(rw).Encode(rows)
	})
	hs := &http.Server{Addr: control, Handler: mux}
	go hs.ListenAndServe()
	fmt.Printf("agent %s ready as %s\n", name, thread)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return hs.Close()
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }
