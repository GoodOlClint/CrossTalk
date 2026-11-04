package appserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func dialInit(t *testing.T, ep string, opt appserver.Options) *appserver.Client {
	t.Helper()
	c, err := appserver.Dial(ctx(t), ep, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Initialize(ctx(t), appserver.ClientInfo{Name: "crosstalk", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNormalizeEndpoint(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := map[string]string{
		"/tmp/a.sock":            "unix:///tmp/a.sock",
		"unix:///tmp/./a.sock":   "unix:///tmp/a.sock",
		"~/x.sock":               "unix://" + filepath.Join(home, "x.sock"),
		"ws://127.0.0.1:4500":    "ws://127.0.0.1:4500",
		"ws://127.0.0.1:4500/rp": "ws://127.0.0.1:4500/rp",
	}
	for in, want := range cases {
		got, err := appserver.NormalizeEndpoint(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"rel.sock", "http://x", "wss://x", "ws://"} {
		if _, err := appserver.NormalizeEndpoint(bad); !errors.Is(err, appserver.ErrBadEndpoint) {
			t.Errorf("NormalizeEndpoint(%q) err = %v, want ErrBadEndpoint", bad, err)
		}
	}
}

func TestParseServerVersion(t *testing.T) {
	cases := map[string]string{
		"codex-tui/0.166.0 (Mac OS 27.2.0; arm64) Apple_Terminal/488.9 (x; 0.1)": "0.166.0",
		"codex_app_server/1.2.3-alpha.1 (linux)":                                 "1.2.3-alpha.1",
	}
	for ua, want := range cases {
		if got, ok := appserver.ParseServerVersion(ua); !ok || got != want {
			t.Errorf("ParseServerVersion(%q) = %q, %v", ua, got, ok)
		}
	}
	if _, ok := appserver.ParseServerVersion("codex"); ok {
		t.Error("ParseServerVersion(codex) ok")
	}
}

func TestHandshakeOrderAndVersion(t *testing.T) {
	s := appservertest.New(t)
	c := dialInit(t, s.Endpoint(), appserver.Options{})
	if c.ServerVersion() != "0.166.0" {
		t.Fatalf("ServerVersion = %q", c.ServerVersion())
	}
	if _, err := c.LoadedThreads(ctx(t)); err != nil {
		t.Fatal(err)
	}
	calls := s.Calls()
	if len(calls) != 3 || calls[0].Method != "initialize" || calls[1].Method != "initialized" || calls[2].Method != "thread/loaded/list" {
		t.Fatalf("calls = %+v", calls)
	}
	var p struct {
		ClientInfo   appserver.ClientInfo `json:"clientInfo"`
		Capabilities map[string]any       `json:"capabilities"`
	}
	if err := json.Unmarshal(calls[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.ClientInfo.Name != "crosstalk" || p.Capabilities["experimentalApi"] != false {
		t.Fatalf("initialize params = %s", calls[0].Params)
	}
}

func TestStandardOnSendLenientOnReceive(t *testing.T) {
	for _, include := range []bool{false, true} {
		s := appservertest.New(t)
		s.IncludeJSONRPC = include
		var out, in [][]byte
		c := dialInit(t, s.Endpoint(), appserver.Options{OnFrame: func(dir string, raw []byte) {
			if dir == appserver.DirOut {
				out = append(out, bytes.Clone(raw))
			} else {
				in = append(in, bytes.Clone(raw))
			}
		}})
		if _, err := c.LoadedThreads(ctx(t)); err != nil {
			t.Fatalf("include=%v: %v", include, err)
		}
		for _, f := range out {
			if !bytes.Contains(f, []byte(`"jsonrpc":"2.0"`)) {
				t.Errorf("sent frame without jsonrpc header: %s", f)
			}
		}
		for _, f := range in {
			if got := bytes.Contains(f, []byte(`"jsonrpc"`)); got != include {
				t.Errorf("include=%v: received %s", include, f)
			}
		}
	}
}

func TestWSURLOverTCP(t *testing.T) {
	s, err := appservertest.StartTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := dialInit(t, s.Endpoint(), appserver.Options{})
	if !strings.HasPrefix(c.Endpoint(), "ws://127.0.0.1:") {
		t.Fatalf("endpoint %q", c.Endpoint())
	}
}

func TestRPCErrorAndTypedCalls(t *testing.T) {
	s := appservertest.New(t)
	yes := true
	s.AddThread(&appservertest.Thread{ID: "t1", Name: "pane", Cwd: "/w", Loaded: true, CanAcceptDirectInput: &yes,
		Turns: []appserver.Turn{{ID: "a", Status: "completed"}, {ID: "b", Status: "inProgress"}}})
	c := dialInit(t, s.Endpoint(), appserver.Options{})
	th, err := c.ReadThread(ctx(t), "t1")
	if err != nil || th.Status.Type != appserver.ThreadActive || th.Name == nil || *th.Name != "pane" || th.Cwd != "/w" || th.CanAcceptDirectInput == nil || !*th.CanAcceptDirectInput {
		t.Fatalf("ReadThread = %+v, %v", th, err)
	}
	act, err := c.ActiveTurn(ctx(t), "t1")
	if err != nil || act != "b" {
		t.Fatalf("ActiveTurn = %q, %v", act, err)
	}
	_, err = c.ReadThread(ctx(t), "nope")
	var rpc *appserver.RPCError
	if !errors.As(err, &rpc) || rpc.Code != appservertest.CodeInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.McpServerStatus(ctx(t), "t1", ""); err != nil {
		t.Fatal(err)
	}
}

func TestDropAfterWriteIsAmbiguous(t *testing.T) {
	s := appservertest.New(t)
	s.Hook = func(_ int, method string, _ json.RawMessage) appservertest.Reply {
		if method != "thread/loaded/list" {
			return appservertest.Reply{}
		}
		return appservertest.Reply{Handled: true, Drop: true}
	}
	c := dialInit(t, s.Endpoint(), appserver.Options{})
	_, err := c.LoadedThreads(ctx(t))
	if !errors.Is(err, appserver.ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	<-c.Done()
	if _, err := c.LoadedThreads(ctx(t)); !errors.Is(err, appserver.ErrNotSent) {
		t.Fatalf("after close err = %v, want ErrNotSent", err)
	}
}

func TestTimeoutIsAmbiguous(t *testing.T) {
	s := appservertest.New(t)
	release := make(chan struct{})
	defer close(release)
	s.Hook = func(_ int, method string, _ json.RawMessage) appservertest.Reply {
		if method == "thread/loaded/list" {
			<-release
		}
		return appservertest.Reply{}
	}
	c := dialInit(t, s.Endpoint(), appserver.Options{})
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.LoadedThreads(short); !errors.Is(err, appserver.ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
}

func TestNotInitializedRefused(t *testing.T) {
	s := appservertest.New(t)
	c, err := appserver.Dial(ctx(t), s.Endpoint(), appserver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var rpc *appserver.RPCError
	if _, err := c.LoadedThreads(ctx(t)); !errors.As(err, &rpc) || rpc.Code != appservertest.CodeNotInitialized {
		t.Fatalf("err = %v", err)
	}
}

func TestNotificationsAndServerRequests(t *testing.T) {
	s := appservertest.New(t)
	s.Greeting = []json.RawMessage{
		json.RawMessage(`{"method":"thread/status/changed","params":{"threadId":"t1","_meta":{"k":1}}}`),
		json.RawMessage(`{"id":0,"method":"mcpServer/elicitation/request","params":{}}`),
	}
	got := make(chan string, 4)
	c := dialInit(t, s.Endpoint(), appserver.Options{OnNotification: func(m string, _ json.RawMessage) { got <- m }})
	select {
	case m := <-got:
		if m != "thread/status/changed" {
			t.Fatalf("notification %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification")
	}
	if _, err := c.LoadedThreads(ctx(t)); err != nil {
		t.Fatal(err)
	}
}

func TestFramingFragmentsPingAndLimits(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		w := func(hdr0 byte, p string) {
			_ = appserver.WriteFrame(&rawWriter{a, hdr0}, 0, []byte(p), false)
		}
		w(0x01, `{"id":1,`)    // text, not final
		w(0x89, "hi")          // ping, final
		w(0x00, `"result":{}`) // continuation, not final
		w(0x80, `}`)           // continuation, final
		appserver.WriteFrame(a, appserver.OpClose, nil, false)
	}()
	var pings []string
	op, msg, err := appserver.ReadMessage(b, func(op byte, p []byte) error {
		if op == appserver.OpPing {
			pings = append(pings, string(p))
		}
		return nil
	})
	if err != nil || op != appserver.OpText || string(msg) != `{"id":1,"result":{}}` || len(pings) != 1 {
		t.Fatalf("op=%d msg=%q err=%v pings=%v", op, msg, err, pings)
	}
	if _, _, err := appserver.ReadMessage(b, nil); !errors.Is(err, appserver.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestMaskedRoundTripLengths(t *testing.T) {
	for _, n := range []int{0, 125, 126, 65535, 65536, 70000} {
		var buf bytes.Buffer
		p := bytes.Repeat([]byte("x"), n)
		if err := appserver.WriteFrame(&buf, appserver.OpText, p, true); err != nil {
			t.Fatal(err)
		}
		if buf.Bytes()[1]&0x80 == 0 {
			t.Fatalf("n=%d: mask bit not set", n)
		}
		_, got, err := appserver.ReadMessage(&buf, nil)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatalf("n=%d: err=%v len=%d", n, err, len(got))
		}
	}
}

// rawWriter rewrites the first header byte so tests can emit non-final and
// continuation frames.
type rawWriter struct {
	w    net.Conn
	hdr0 byte
}

func (r *rawWriter) Write(p []byte) (int, error) {
	q := bytes.Clone(p)
	q[0] = r.hdr0
	return r.w.Write(q)
}

func TestWriteDeadlineWhenPeerNeverReads(t *testing.T) {
	old := appserver.WriteTimeout
	appserver.WriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { appserver.WriteTimeout = old })

	dir, err := os.MkdirTemp("/tmp", "ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		key := ""
		for _, l := range strings.Split(string(buf[:n]), "\r\n") {
			if v, ok := strings.CutPrefix(l, "Sec-WebSocket-Key: "); ok {
				key = v
			}
		}
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + appserver.AcceptKey(key) + "\r\n\r\n"))
		time.Sleep(10 * time.Second)
	}()

	c, err := appserver.Dial(ctx(t), sock, appserver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	err = c.Notify("x", strings.Repeat("a", 32<<20))
	if !errors.Is(err, appserver.ErrNotSent) {
		t.Fatalf("Notify err = %v, want ErrNotSent", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("write blocked %v past a %v deadline", el, appserver.WriteTimeout)
	}
}
