package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	reqA = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	reqB = `{"jsonrpc":"2.0","id":2,"method":"ping"}`
)

func okResp(t *testing.T, id int64) *jsonrpc.Response {
	t.Helper()
	i, err := jsonrpc.MakeID(float64(id))
	if err != nil {
		t.Fatal(err)
	}
	return &jsonrpc.Response{ID: i, Result: json.RawMessage(`{}`)}
}

func TestConnNoReadAhead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	c := NewConn(r, io.Discard)
	if _, err := w.WriteString(reqA + "\n" + reqB + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Close()
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != reqB+"\n" {
		t.Fatalf("bytes left in the pipe = %q, want the second request", rest)
	}
}

func TestConnFragmentedFrame(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	c := NewConn(pr, io.Discard)
	got := make(chan error, 1)
	go func() { _, err := c.Read(context.Background()); got <- err }()
	if _, err := pw.Write([]byte(reqA[:10])); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a partial frame", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.partial })
	if c.Quiescent() {
		t.Fatal("quiescent with half a frame buffered")
	}
	if _, err := pw.Write([]byte(reqA[10:] + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if c.Quiescent() {
		t.Fatal("quiescent with a request still owed a response")
	}
	if err := c.Write(context.Background(), okResp(t, 1)); err != nil {
		t.Fatal(err)
	}
	if !c.Quiescent() {
		t.Fatal("not quiescent after the response was written")
	}
}

func TestConnPendingUntilResponseWritten(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	c := NewConn(inR, outW)
	go inW.Write([]byte(reqA + "\n"))
	if _, err := c.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() { wrote <- c.Write(context.Background(), okResp(t, 1)) }()
	waitUntil(t, "the response write to start", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.inflight == 1 })
	if c.Quiescent() {
		t.Fatal("quiescent while the response is blocked on a full pipe")
	}
	if _, err := io.ReadAll(io.LimitReader(outR, int64(len(`{"id":1,"jsonrpc":"2.0","result":{}}`)+1))); err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if !c.Quiescent() {
		t.Fatal("not quiescent after the response write returned")
	}
}

func TestConnCapturesState(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "s", Version: "0"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}}})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	tr := &Transport{In: inR, Out: outW}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx, tr)
	out := bufio.NewReader(outR)
	send := func(frame string) {
		t.Helper()
		if _, err := inW.Write([]byte(frame + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"9"}}}`)
	line, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil || resp.Result.ProtocolVersion == "" {
		t.Fatalf("initialize response %q: %v", line, err)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"warning"}}`)
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	st := tr.Conn().State()
	if !strings.Contains(string(st.InitializeParams), `"probe"`) {
		t.Errorf("initialize params = %s", st.InitializeParams)
	}
	if st.ProtocolVersion != resp.Result.ProtocolVersion {
		t.Errorf("protocol version = %q, response said %q", st.ProtocolVersion, resp.Result.ProtocolVersion)
	}
	if !st.Initialized {
		t.Error("initialized not captured")
	}
	if !strings.Contains(string(st.SetLevelParams), `"warning"`) {
		t.Errorf("setLevel params = %s", st.SetLevelParams)
	}
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type blockingReader struct{ unblock chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.unblock; return 0, io.EOF }

func TestConnCloseUnblocksReadOfNonCloser(t *testing.T) {
	br := blockingReader{make(chan struct{})}
	defer close(br.unblock)
	c := NewConn(br, io.Discard)
	got := make(chan error, 1)
	go func() { _, err := c.Read(context.Background()); got <- err }()
	time.Sleep(20 * time.Millisecond)
	c.Close()
	select {
	case err := <-got:
		if err != io.EOF {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
}
