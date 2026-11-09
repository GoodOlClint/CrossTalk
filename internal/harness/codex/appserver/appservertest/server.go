// Package appservertest is an in-process fake Codex app-server speaking
// websocket-framed JSON-RPC on a unix socket, for tests and fixtures only.
package appservertest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

const DefaultUserAgent = "codex-tui/0.166.0 (Fake OS; arm64) appservertest (crosstalk; test)"

// Error codes and messages used by the fake. They are the fake's own
// choices; live placement comes from `crosstalk capture` (§6 F1).
const (
	CodeInvalidRequest = -32600
	CodeNotInitialized = -32002
	CodeMethodNotFound = -32601
	MsgNoActiveTurn    = "no active turn to steer"
	MsgNotSteerable    = "active turn is not steerable"
)

type Thread struct {
	ID     string
	Name   string
	Cwd    string
	Loaded bool
	// Status overrides the derived idle/active status when set (e.g. systemError).
	Status               string
	CanAcceptDirectInput *bool
	// ModelProvider is thread/read's modelProvider; "" means "openai".
	ModelProvider string
	// Turns, oldest first.
	Turns []appserver.Turn
	// NotSteerable, when "review" or "compact", makes steer and start fail
	// with activeTurnNotSteerable while a turn is in progress.
	NotSteerable string
	Steered      []string
	Started      []string
	ToolOutputs  []appserver.ToolOutput
	Injected     []appserver.ToolOutput
}

type Call struct {
	Conn   int
	Method string
	Params json.RawMessage
}

// Reply is a Hook's decision for one request.
type Reply struct {
	Handled bool
	Result  any
	Err     *appserver.RPCError
	// Drop closes the connection without a response. With Apply the default
	// behaviour runs first (the server acted, the client never learns).
	Drop  bool
	Apply bool
}

type Server struct {
	Path           string
	UserAgent      string
	IncludeJSONRPC bool
	// Hook, if set, runs before the default handler for every request.
	Hook func(conn int, method string, params json.RawMessage) Reply
	// Greeting notifications are sent right after the initialize response.
	Greeting []json.RawMessage
	// Account is account/read's account type; "" means "chatgpt" and "none"
	// makes the account null.
	Account string
	// RateLimitsErr, when set, is the error account/rateLimits/read returns.
	RateLimitsErr *appserver.RPCError
	// RateLimitsHang makes account/rateLimits/read never answer.
	RateLimitsHang bool

	ln      net.Listener
	mu      sync.Mutex
	threads map[string]*Thread
	calls   []Call
	nconn   int
	turnSeq int
	wg      sync.WaitGroup
	conns   map[net.Conn]struct{}
	closed  bool
}

// Start listens on a fresh socket in dir.
func Start(dir string) (*Server, error) {
	p := filepath.Join(dir, "as.sock")
	_ = os.Remove(p)
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	return serveOn(ln, p), nil
}

// StartTCP listens on a loopback TCP port, for ws:// endpoints.
func StartTCP() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return serveOn(ln, ""), nil
}

// New starts a unix-socket server in a short temporary directory (macOS
// limits socket paths to 104 bytes) and closes it at test cleanup.
func New(tb testing.TB) *Server {
	tb.Helper()
	dir, err := os.MkdirTemp("", "ctas")
	if err != nil {
		tb.Fatal(err)
	}
	s, err := Start(dir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		s.Close()
		os.RemoveAll(dir)
	})
	return s
}

func serveOn(ln net.Listener, path string) *Server {
	s := &Server{Path: path, UserAgent: DefaultUserAgent, ln: ln, threads: map[string]*Thread{}, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s
}

// Endpoint is the unix:// endpoint for Path, or ws:// for a TCP server.
func (s *Server) Endpoint() string {
	if s.Path == "" {
		return "ws://" + s.ln.Addr().String() + "/"
	}
	return "unix://" + s.Path
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) AddThread(t *Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[t.ID] = t
}

// Thread returns a copy of the thread's current state.
func (s *Server) Thread(id string) Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.threads[id]
	if t == nil {
		return Thread{}
	}
	cp := *t
	cp.Turns = slices.Clone(t.Turns)
	cp.Steered = slices.Clone(t.Steered)
	cp.Started = slices.Clone(t.Started)
	cp.ToolOutputs = slices.Clone(t.ToolOutputs)
	cp.Injected = slices.Clone(t.Injected)
	return cp
}

// Mutate runs f on the thread under the server lock.
func (s *Server) Mutate(id string, f func(*Thread)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.threads[id]; t != nil {
		f(t)
	}
}

// NewTurnID returns the next deterministic turn id.
func (s *Server) NewTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newTurnIDLocked()
}

func (s *Server) newTurnIDLocked() string {
	s.turnSeq++
	return fmt.Sprintf("019a0000-0000-7000-8000-%012d", s.turnSeq)
}

func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// CallsTo returns the recorded calls with the given method.
func (s *Server) CallsTo(method string) []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.nconn++
		id := s.nconn
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(id, c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			c.Close()
		}()
	}
}

func handshake(c net.Conn) (*bufio.Reader, error) {
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, err
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || key == "" {
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return nil, errors.New("not a websocket upgrade")
	}
	_, err = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+appserver.AcceptKey(key)+"\r\n\r\n")
	return br, err
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *Server) serve(conn int, c net.Conn) {
	br, err := handshake(c)
	if err != nil {
		return
	}
	var wmu sync.Mutex
	send := func(m map[string]any) error {
		if s.IncludeJSONRPC {
			m["jsonrpc"] = "2.0"
		}
		raw, _ := json.Marshal(m)
		wmu.Lock()
		defer wmu.Unlock()
		return appserver.WriteFrame(c, appserver.OpText, raw, false)
	}
	initialized, initReq := false, false
	for {
		op, data, err := appserver.ReadMessage(br, nil)
		if err != nil {
			return
		}
		if op != appserver.OpText {
			continue
		}
		var r request
		if json.Unmarshal(data, &r) != nil {
			continue
		}
		s.mu.Lock()
		s.calls = append(s.calls, Call{Conn: conn, Method: r.Method, Params: r.Params})
		hang := r.Method == "account/rateLimits/read" && s.RateLimitsHang
		s.mu.Unlock()
		if len(r.ID) == 0 {
			if r.Method == "initialized" && initReq {
				initialized = true
			}
			continue
		}
		if hang && initialized {
			continue
		}
		var reply Reply
		if s.Hook != nil {
			reply = s.Hook(conn, r.Method, r.Params)
		}
		switch {
		case reply.Handled && !reply.Apply:
		case r.Method == "initialize":
			initReq = true
			reply.Result = map[string]any{"userAgent": s.UserAgent, "codexHome": "/fake/.codex", "platformFamily": "unix", "platformOs": "macos"}
		case !initialized:
			reply.Err = &appserver.RPCError{Code: CodeNotInitialized, Message: "Not initialized"}
		default:
			reply.Result, reply.Err = s.handle(r.Method, r.Params)
		}
		if reply.Drop {
			return
		}
		m := map[string]any{"id": r.ID}
		if reply.Err != nil {
			m["error"] = reply.Err
		} else {
			if reply.Result == nil {
				reply.Result = map[string]any{}
			}
			m["result"] = reply.Result
		}
		if send(m) != nil {
			return
		}
		if r.Method == "initialize" {
			for _, g := range s.Greeting {
				var n map[string]any
				if json.Unmarshal(g, &n) == nil && send(n) != nil {
					return
				}
			}
		}
	}
}

func invalid(msg string) *appserver.RPCError {
	return &appserver.RPCError{Code: CodeInvalidRequest, Message: msg}
}

// NotSteerableError is the fake's activeTurnNotSteerable error; it places
// the CodexErrorInfo under error.data.codexErrorInfo.
func NotSteerableError(kind string) *appserver.RPCError {
	data, _ := json.Marshal(map[string]any{"codexErrorInfo": map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": kind}}})
	return &appserver.RPCError{Code: CodeInvalidRequest, Message: MsgNotSteerable, Data: data}
}

// NotSteerableStartError is the refusal live codex-cli 0.166.0 returns for
// turn/start during a review or compact turn: no data, key only in message.
func NotSteerableStartError(kind string) *appserver.RPCError {
	k := strings.ToUpper(kind[:1]) + kind[1:]
	return &appserver.RPCError{Code: -32603, Message: "failed to submit turn input: ActiveTurnNotSteerable { turn_kind: " + k + " }"}
}

func activeTurn(t *Thread) string {
	for _, tr := range t.Turns {
		if tr.Status == appserver.TurnInProgress {
			return tr.ID
		}
	}
	return ""
}

func status(t *Thread) map[string]any {
	switch {
	case !t.Loaded:
		return map[string]any{"type": appserver.ThreadNotLoaded}
	case t.Status != "":
		if t.Status == appserver.ThreadActive {
			return map[string]any{"type": t.Status, "activeFlags": []string{}}
		}
		return map[string]any{"type": t.Status}
	case activeTurn(t) != "":
		return map[string]any{"type": appserver.ThreadActive, "activeFlags": []string{}}
	default:
		return map[string]any{"type": appserver.ThreadIdle}
	}
}

func (s *Server) handle(method string, raw json.RawMessage) (any, *appserver.RPCError) {
	var p struct {
		ThreadID       string                `json:"threadId"`
		ExpectedTurnID string                `json:"expectedTurnId"`
		Input          []appserver.UserInput `json:"input"`
		Limit          int                   `json:"limit"`
		ToolOutput     *appserver.ToolOutput `json:"toolOutput"`
		Items          []struct {
			Type string `json:"type"`
			appserver.ToolOutput
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalid("invalid request params")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case "thread/loaded/list":
		ids := []string{}
		for id, t := range s.threads {
			if t.Loaded {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return map[string]any{"data": ids, "nextCursor": nil}, nil
	case "mcpServerStatus/list":
		return map[string]any{"data": []any{}, "nextCursor": nil}, nil
	case "account/read":
		switch s.Account {
		case "none":
			return map[string]any{"account": nil, "requiresOpenaiAuth": true}, nil
		case "":
			return map[string]any{"account": map[string]any{"type": "chatgpt", "email": "a@example.com", "planType": "plus"}, "requiresOpenaiAuth": true}, nil
		}
		return map[string]any{"account": map[string]any{"type": s.Account}, "requiresOpenaiAuth": true}, nil
	case "account/rateLimits/read":
		if s.RateLimitsErr != nil {
			return nil, s.RateLimitsErr
		}
		return map[string]any{"rateLimits": map[string]any{"limitId": "codex"}}, nil
	}
	t := s.threads[p.ThreadID]
	if t == nil {
		return nil, invalid("thread not found: " + p.ThreadID)
	}
	text := ""
	if len(p.Input) > 0 {
		text = p.Input[0].Text
	}
	switch method {
	case "thread/read":
		th := map[string]any{"id": t.ID, "cwd": t.Cwd, "status": status(t), "name": nil, "canAcceptDirectInput": true, "modelProvider": "openai"}
		if t.ModelProvider != "" {
			th["modelProvider"] = t.ModelProvider
		}
		if t.Name != "" {
			th["name"] = t.Name
		}
		if t.CanAcceptDirectInput != nil {
			th["canAcceptDirectInput"] = *t.CanAcceptDirectInput
		}
		return map[string]any{"thread": th}, nil
	case "thread/turns/list":
		if !t.Loaded {
			return map[string]any{"data": []any{}}, nil
		}
		data := []any{}
		for i := len(t.Turns) - 1; i >= 0; i-- {
			if p.Limit > 0 && len(data) == p.Limit {
				break
			}
			data = append(data, map[string]any{"id": t.Turns[i].ID, "status": t.Turns[i].Status, "items": []any{}})
		}
		return map[string]any{"data": data, "nextCursor": nil}, nil
	case "turn/steer":
		if !t.Loaded {
			return nil, invalid("thread not loaded")
		}
		act := activeTurn(t)
		switch {
		case act == "":
			return nil, invalid(MsgNoActiveTurn)
		case p.ExpectedTurnID != act:
			return nil, invalid(fmt.Sprintf("expected active turn id %s but found %s", p.ExpectedTurnID, act))
		case t.NotSteerable != "":
			return nil, NotSteerableError(t.NotSteerable)
		}
		t.Steered = append(t.Steered, text)
		return map[string]any{"turnId": act}, nil
	case "thread/inject_items":
		if !t.Loaded || (t.CanAcceptDirectInput != nil && !*t.CanAcceptDirectInput) {
			return nil, invalid("thread does not accept direct input")
		}
		if len(p.Items) == 0 {
			return nil, invalid("items must not be empty")
		}
		for _, item := range p.Items {
			if item.Type != "function_call_output" || item.Name == "" || item.Namespace == "" || item.Output == "" {
				return nil, invalid("invalid named tool output item")
			}
		}
		for _, item := range p.Items {
			t.Injected = append(t.Injected, item.ToolOutput)
		}
		return map[string]any{}, nil
	case "turn/start":
		if !t.Loaded {
			return nil, invalid("thread not loaded")
		}
		if t.CanAcceptDirectInput != nil && !*t.CanAcceptDirectInput {
			return nil, invalid("thread does not accept direct input")
		}
		if p.ToolOutput != nil && (p.Input == nil || len(p.Input) != 0 || p.ToolOutput.Name == "" || p.ToolOutput.Namespace == "" || p.ToolOutput.Output == "") {
			return nil, invalid("invalid named tool output")
		}
		if activeTurn(t) != "" && t.NotSteerable != "" {
			return nil, NotSteerableStartError(t.NotSteerable)
		}
		id := activeTurn(t)
		if p.ToolOutput != nil {
			t.ToolOutputs = append(t.ToolOutputs, *p.ToolOutput)
		} else {
			t.Started = append(t.Started, text)
		}
		if id == "" || p.ToolOutput == nil {
			id = s.newTurnIDLocked()
			t.Turns = append(t.Turns, appserver.Turn{ID: id, Status: appserver.TurnInProgress})
		}
		return map[string]any{"turn": map[string]any{"id": id, "status": appserver.TurnInProgress, "items": []any{}}}, nil
	}
	return nil, &appserver.RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
}

// BackendDownError is the error live codex-cli 0.167.0 returns from
// account/rateLimits/read when the app-server cannot reach its backend.
func BackendDownError() *appserver.RPCError {
	return &appserver.RPCError{Code: -32603, Message: "failed to fetch codex rate limits: error sending request for url (https://chatgpt.com/backend-api/wham/usage)"}
}
