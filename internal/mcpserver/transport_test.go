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

// A real SDK handler answers ping; its response blocks on a pipe nobody
// reads, and the Conn stays not quiescent until those bytes are written.
func TestConnPendingUntilResponseWritten(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "s", Version: "0"}, nil)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	tr := NewTransport(inR, outW)
	c := tr.Conn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx, tr)
	out := bufio.NewReader(outR)
	inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"p","version":"1"}}}` + "\n"))
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	inW.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	inW.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"))
	waitUntil(t, "the ping response write to start", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.inflight == 1 })
	if c.Quiescent() {
		t.Fatal("quiescent while the handler's response is blocked on a full pipe")
	}
	if l, err := out.ReadString('\n'); err != nil || !strings.Contains(l, `"id":2`) {
		t.Fatalf("ping response %q: %v", l, err)
	}
	waitUntil(t, "quiescence after the response write returned", c.Quiescent)
}

// Close ends the input only: the output stays open, so an exec'd image
// writes on the same stdout.
func TestConnCloseClosesInputOnly(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outR.Close()
	defer outW.Close()
	c := NewConn(inR, outW)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := inR.Read(make([]byte, 1)); err == nil {
		t.Fatal("input still open after Close")
	}
	if _, err := outW.Write([]byte("x")); err != nil {
		t.Fatalf("output closed by Close: %v", err)
	}
	b := make([]byte, 1)
	if _, err := outR.Read(b); err != nil || b[0] != 'x' {
		t.Fatalf("output read %q: %v", b, err)
	}
}

// A last frame with no newline, read at EOF but not yet accepted by any Read,
// is not quiescent: its bytes are out of the pipe and only this process has
// them.
func TestConnFinalFrameAtEOFNotQuiescentUntilAccepted(t *testing.T) {
	pr, pw := io.Pipe()
	c := NewConn(pr, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, err := c.Read(ctx); got <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-got; err != context.Canceled {
		t.Fatal(err)
	}
	go func() { pw.Write([]byte(reqA)); pw.Close() }()
	waitUntil(t, "the frame to be read", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.partial })
	time.Sleep(50 * time.Millisecond)
	if c.Quiescent() {
		t.Fatal("quiescent with a final frame read at EOF that no Read has accepted")
	}
	m, err := c.Read(context.Background())
	if err != nil || m.(*jsonrpc.Request).Method != "ping" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestConnCapturesState(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "s", Version: "0"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}}})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	tr := NewTransport(inR, outW)
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

func TestConnBlankLinesAndFinalFrameAtEOF(t *testing.T) {
	c := NewConn(strings.NewReader("\n"+reqA+"\n\n"+reqB), io.Discard)
	for i := 0; i < 2; i++ {
		if _, err := c.Read(context.Background()); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if _, err := c.Read(context.Background()); err != io.EOF {
		t.Fatalf("third read = %v, want EOF", err)
	}
}

func TestConnCancelledReadKeepsItsFrame(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	c := NewConn(pr, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, err := c.Read(ctx); got <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-got; err != context.Canceled {
		t.Fatal(err)
	}
	go pw.Write([]byte(reqA + "\n"))
	waitUntil(t, "the frame to be read", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.partial })
	if c.Quiescent() {
		t.Fatal("quiescent with a complete frame no Read has accepted")
	}
	m, err := c.Read(context.Background())
	if err != nil || m.(*jsonrpc.Request).Method != "ping" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestConnNotificationAndFailedWriteBlockQuiescence(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	c := NewConn(inR, outW)
	n := &jsonrpc.Request{Method: "notifications/message", Params: json.RawMessage(`{}`)}
	wrote := make(chan error, 1)
	go func() { wrote <- c.Write(context.Background(), n) }()
	waitUntil(t, "the notification write to start", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.inflight == 1 })
	if c.Quiescent() {
		t.Fatal("quiescent while a notification is mid-write")
	}
	outR.Close()
	if err := <-wrote; err == nil {
		t.Fatal("write to a closed pipe succeeded")
	}
	if c.Quiescent() {
		t.Fatal("quiescent after a failed write")
	}
}

func TestConnLargeFrameThroughOSPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	frame := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 1<<20) + `"}}` + "\n"
	go func() { w.WriteString(frame); w.Close() }()
	c := NewConn(r, io.Discard)
	start := time.Now()
	if _, err := c.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("1 MiB frame: %v", time.Since(start))
	if time.Since(start) > 5*time.Second {
		t.Fatal("1 MiB frame took over 5s")
	}
}

// Pause interrupts a read waiting for the first byte of a frame without
// consuming anything: a frame written while paused stays in the pipe until
// Unpause, then the same Read returns it.
func TestConnPauseConsumesNoByte(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	c := NewConn(r, io.Discard)
	got := make(chan jsonrpc.Message, 1)
	go func() { m, _ := c.Read(context.Background()); got <- m }()
	waitUntil(t, "the read to wait at a frame boundary", func() bool { c.smu.Lock(); defer c.smu.Unlock(); return c.reading })
	if err := c.Pause(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitPaused(ctx); err != nil {
		t.Fatalf("not paused: %v", err)
	}
	w.WriteString(reqA + "\n")
	time.Sleep(50 * time.Millisecond)
	select {
	case m := <-got:
		t.Fatalf("read %v while paused", m)
	default:
	}
	c.smu.Lock()
	partial := c.partial
	c.smu.Unlock()
	if partial {
		t.Fatal("a byte was consumed while paused")
	}
	c.Unpause()
	select {
	case m := <-got:
		if m == nil || m.(*jsonrpc.Request).Method != "ping" {
			t.Fatalf("after Unpause: %v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame after Unpause")
	}
}

func listenAck(id string) *jsonrpc.Request {
	return &jsonrpc.Request{Method: "notifications/subscriptions/acknowledged", Params: json.RawMessage(`{"_meta":{"io.modelcontextprotocol/subscriptionId":` + id + `},"notifications":{"toolsListChanged":true}}`)}
}

func TestConnOpenListenIsCarriedNotPending(t *testing.T) {
	for _, id := range []string{`3`, `"a&b"`} {
		t.Run(id, func(t *testing.T) {
			listen := `{"jsonrpc":"2.0","id":` + id + `,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true}}}`
			c := NewConn(strings.NewReader(listen+"\n"), io.Discard)
			m, err := c.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			lid := m.(*jsonrpc.Request).ID
			if c.Quiescent() {
				t.Fatal("quiescent before the listen's acknowledgement was written")
			}
			if err := c.Write(context.Background(), listenAck(id)); err != nil {
				t.Fatal(err)
			}
			if !c.Quiescent() {
				t.Fatal("an acknowledged subscriptions/listen blocks quiescence; the server answers it only when the subscription ends")
			}
			st := c.State()
			if len(st.Listens) != 1 {
				t.Fatalf("carried listens = %d, want 1", len(st.Listens))
			}

			var out strings.Builder
			n := NewConn(strings.NewReader(""), &out)
			n.Restore(st)
			m, err = n.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if req, ok := m.(*jsonrpc.Request); !ok || req.Method != "subscriptions/listen" || req.ID != lid {
				t.Fatalf("first message after Restore = %#v, want the carried listen", m)
			}
			if err := n.Write(context.Background(), listenAck(id)); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatalf("the replayed listen's acknowledgement was written again: %q", out.String())
			}
			if !n.Quiescent() {
				t.Fatal("a replayed listen blocks quiescence")
			}
			if err := n.Write(context.Background(), listenAck(id)); err != nil || out.Len() == 0 {
				t.Fatalf("a second acknowledgement was dropped too (err %v)", err)
			}
			if len(n.State().Listens) != 1 {
				t.Fatal("the restored listen is not carried into the next handoff")
			}
			if err := n.Write(context.Background(), &jsonrpc.Response{ID: lid, Result: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if len(n.State().Listens) != 0 {
				t.Fatal("a listen whose response was written is still carried")
			}
		})
	}
}

// A cancellation read for an open listen means its final response is owed:
// the handoff waits for it rather than carry a cancelled stream.
func TestConnCancelledListenBlocksQuiescence(t *testing.T) {
	const listen = `{"jsonrpc":"2.0","id":3,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true}}}`
	const cancel = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`
	c := NewConn(strings.NewReader(listen+"\n"+cancel+"\n"), io.Discard)
	for range 2 {
		if _, err := c.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Write(context.Background(), listenAck(`3`)); err != nil {
		t.Fatal(err)
	}
	if c.Quiescent() {
		t.Fatal("quiescent with a cancelled listen whose response is not written")
	}
	if err := c.Write(context.Background(), okResp(t, 3)); err != nil {
		t.Fatal(err)
	}
	if !c.Quiescent() || len(c.State().Listens) != 0 {
		t.Fatal("the cancelled listen's response was written, but it still blocks or is carried")
	}
}

// A batch holding an open listen keeps the handoff waiting: a sibling's
// response is buffered until the whole batch is written.
func TestConnBatchWithListenBlocksQuiescence(t *testing.T) {
	const b = `[{"jsonrpc":"2.0","id":3,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true}}},{"jsonrpc":"2.0","id":4,"method":"ping"}]`
	c := NewConn(strings.NewReader(b+"\n"), io.Discard)
	for range 2 {
		if _, err := c.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	c.Write(context.Background(), listenAck(`3`))
	c.Write(context.Background(), okResp(t, 4))
	if c.Quiescent() {
		t.Fatal("quiescent while a batch still waits for its listen's response")
	}
}
