package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

const selfID = "aaaaaaaaaaaaaaaa"

type env struct {
	t       *testing.T
	dir     string
	fake    *adapter.Fake
	srv     *Server
	url     string
	peerTok string
	stop    func()

	mu     sync.Mutex
	self   receiver.Recipient
	agents map[string]Peer
	toks   map[string]string
}

func newEnv(t *testing.T, lim receiver.Limits, alim Limits, hook func(*Server)) *env {
	t.Helper()
	return newEnvOn(t, t.TempDir(), nil, lim, alim, hook)
}

// newEnvOn builds the env on dir; tasks, when set, makes the task store from
// the env's journal.
func newEnvOn(t *testing.T, dir string, tasks func(*journal.Journal) TaskStore, lim receiver.Limits, alim Limits, hook func(*Server)) *env {
	t.Helper()
	e := &env{t: t, dir: dir, fake: adapter.NewFake(adapter.Claude), agents: map[string]Peer{}, toks: map[string]string{}}
	b := adapter.Binding{Harness: adapter.Claude, SessionRef: "sess-self"}
	e.self = receiver.Recipient{AgentID: selfID, Name: "self", Role: registry.RolePrimary, Binding: b}
	j, _, err := journal.Open(e.dir, selfID, journal.Binding{InstanceID: msg.NewUUIDv7(), SessionRef: b.SessionRef}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rcv := receiver.New(receiver.Config{Journal: j, Adapter: e.fake, Limits: lim, Self: func() receiver.Recipient {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.self
	}})
	peerPath := filepath.Join(e.dir, "peer.token")
	e.peerTok, err = udsingress.EnsureKey(peerPath)
	if err != nil {
		t.Fatal(err)
	}
	var ts TaskStore
	if tasks != nil {
		ts = tasks(j)
	}
	if alim.MaxPending == 0 {
		alim.MaxPending = lim.MaxPending
	}
	if alim.MaxMessageBytes == 0 {
		alim.MaxMessageBytes = lim.MaxMessageBytes
	}
	e.srv = NewServer(Config{
		Tasks:         ts,
		Receiver:      rcv,
		Version:       "test-1",
		PeerTokenPath: peerPath,
		Limits:        alim,
		Profile: func() Profile {
			e.mu.Lock()
			defer e.mu.Unlock()
			return Profile{Name: e.self.Name, Harness: "claude", Role: e.self.Role, Cwd: "/work/self"}
		},
		ResolvePeer: func(name string) (Peer, bool) {
			e.mu.Lock()
			defer e.mu.Unlock()
			p, ok := e.agents[name]
			return p, ok
		},
	})
	if hook != nil {
		hook(e.srv)
	}
	e.url, err = e.srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(context.Background()) }()
	var once sync.Once
	e.stop = func() {
		once.Do(func() {
			e.srv.Close()
			if err := <-done; err != nil {
				t.Errorf("Serve: %v", err)
			}
			j.Close()
		})
	}
	t.Cleanup(e.stop)
	return e
}

func (e *env) addAgent(name, agentID, role string) string {
	e.t.Helper()
	path := filepath.Join(e.dir, agentID+".token")
	tok, err := udsingress.EnsureKey(path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.mu.Lock()
	e.agents[name] = Peer{AgentID: agentID, Role: role, TokenPath: path}
	e.toks[name] = tok
	e.mu.Unlock()
	return tok
}

func (e *env) setSelf(role, owner string) {
	e.mu.Lock()
	e.self.Role, e.self.Owner = role, owner
	e.mu.Unlock()
}

// sdkClient is the helloworld client shape: resolve the card, build the
// client from it, attach the credential through the SDK AuthInterceptor.
func (e *env) sdkClient(token, name string) (*a2aclient.Client, context.Context) {
	e.t.Helper()
	ctx := context.Background()
	card, err := agentcard.DefaultResolver.Resolve(ctx, e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	creds := a2aclient.NewInMemoryCredentialsStore()
	creds.Set("s", SchemeBearer, a2aclient.AuthCredential(token))
	cl, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithCallInterceptors(&a2aclient.AuthInterceptor{Service: creds}))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cl.Destroy() })
	ctx = a2aclient.AttachSessionID(ctx, "s")
	if name != "" {
		ctx = a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{SenderHeader: {name}})
	}
	return cl, ctx
}

func (e *env) peer() (*a2aclient.Client, context.Context) { return e.sdkClient(e.peerTok, "") }

func textMsg(text string, meta map[string]any) *sdk.Message {
	m := &sdk.Message{ID: msg.NewUUIDv7(), Role: sdk.MessageRoleUser, Parts: sdk.ContentParts{sdk.NewTextPart(text)}}
	if meta != nil {
		m.Metadata = map[string]any{MetaKey: meta}
	}
	return m
}

func send(t *testing.T, cl *a2aclient.Client, ctx context.Context, m *sdk.Message, immediate bool) (*sdk.Task, error) {
	t.Helper()
	req := &sdk.SendMessageRequest{Message: m}
	if immediate {
		req.Config = &sdk.SendMessageConfig{ReturnImmediately: true}
	}
	res, err := cl.SendMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	task, ok := res.(*sdk.Task)
	if !ok {
		t.Fatalf("result is %T, want task", res)
	}
	return task, nil
}

func meta(t *sdk.Task) map[string]any {
	ct, _ := t.Metadata[MetaKey].(map[string]any)
	return ct
}

func waitState(t *testing.T, cl *a2aclient.Client, ctx context.Context, id sdk.TaskID, want sdk.TaskState) *sdk.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := cl.GetTask(ctx, &sdk.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if task.Status.State == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s state %s, want %s", id, task.Status.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writesFor(f *adapter.Fake, messageID string) int {
	n := 0
	for _, c := range f.Writes() {
		if c.Message.MessageID == messageID {
			n++
		}
	}
	return n
}

func rawCall(t *testing.T, url, token, method string, params any, accept string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "1", "method": method, "params": params})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sdk.SvcParamVersion, string(sdk.Version))
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v: %s", method, err, b)
	}
	return resp.StatusCode, out
}

func errCode(out map[string]any) float64 {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

// Test 17 (a).
func TestAgentCard(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	card, err := agentcard.DefaultResolver.Resolve(context.Background(), e.url)
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "self" || card.Version != "test-1" {
		t.Errorf("name/version = %q/%q", card.Name, card.Version)
	}
	if !strings.HasPrefix(card.Description, "claude session, role primary.") {
		t.Errorf("description %q", card.Description)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("interfaces %v", card.SupportedInterfaces)
	}
	if i := card.SupportedInterfaces[0]; i.URL != e.url || i.ProtocolBinding != sdk.TransportProtocolJSONRPC || i.ProtocolVersion != "1.0" {
		t.Errorf("interface %+v", i)
	}
	if card.Capabilities.Streaming || card.Capabilities.PushNotifications || card.Capabilities.ExtendedAgentCard {
		t.Errorf("capabilities %+v", card.Capabilities)
	}
	if len(card.DefaultInputModes) != 1 || card.DefaultInputModes[0] != "text/plain" || len(card.DefaultOutputModes) != 1 || card.DefaultOutputModes[0] != "text/plain" {
		t.Errorf("modes %v %v", card.DefaultInputModes, card.DefaultOutputModes)
	}
	sch, ok := card.SecuritySchemes[SchemeBearer].(sdk.HTTPAuthSecurityScheme)
	if !ok || !strings.EqualFold(sch.Scheme, "bearer") {
		t.Errorf("security scheme %#v", card.SecuritySchemes)
	}
	if len(card.SecurityRequirements) != 1 {
		t.Fatalf("requirements %v", card.SecurityRequirements)
	}
	if _, ok := card.SecurityRequirements[0][SchemeBearer]; !ok {
		t.Errorf("requirements %v", card.SecurityRequirements)
	}
	var ids []string
	for _, s := range card.Skills {
		if s.Name == "" || s.Description == "" || len(s.Tags) == 0 || len(s.Examples) == 0 {
			t.Errorf("skill %+v incomplete", s)
		}
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "inform,command,ask" {
		t.Errorf("skills %v", ids)
	}

	resp, err := http.Get(e.url + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var doc struct {
		Metadata map[string]map[string]string `json:"metadata"`
	}
	if strings.Contains(string(raw), "cwd") {
		t.Error("card carries cwd")
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Metadata[MetaKey]; got["role"] != "primary" || got["harness"] != "claude" {
		t.Errorf("metadata.crosstalk = %v", got)
	}
	if strings.Contains(string(raw), e.peerTok) || strings.Contains(card.Description, "/work/self") {
		t.Error("card leaks the token or puts cwd in the description")
	}
}

// Test 17 (a), ADR 0011.
func TestAgentCardExposure(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	port := strings.TrimPrefix(e.url, "http://127.0.0.1:")
	do := func(method, path, host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, e.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://evil.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	noCORS := func(what string, resp *http.Response) {
		t.Helper()
		for k := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") || strings.EqualFold(k, "Vary") {
				t.Errorf("%s: header %s present", what, k)
			}
		}
	}
	for _, path := range []string{"/.well-known/agent-card.json"} {
		for _, host := range []string{"", "127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port} {
			resp := do(http.MethodGet, path, host)
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s host %q = %d", path, host, resp.StatusCode)
			}
			noCORS("GET "+path, resp)
			if strings.Contains(string(raw), "cwd") {
				t.Errorf("GET %s carries cwd: %s", path, raw)
			}
			if !json.Valid(raw) {
				t.Errorf("GET %s body is not JSON", path)
			}
		}
		resp := do(http.MethodOptions, path, "")
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("OPTIONS %s = %d", path, resp.StatusCode)
		}
		noCORS("OPTIONS "+path, resp)
		for _, m := range []string{http.MethodGet, http.MethodOptions} {
			resp := do(m, path, "evil.example")
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("%s %s with foreign Host = %d", m, path, resp.StatusCode)
			}
			noCORS(m+" foreign host", resp)
		}
	}
	if resp := do(http.MethodGet, "/.well-known/agent.json", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("legacy card path = %d", resp.StatusCode)
	}
}

// Test 17 (b), (d).
func TestSendWaitsForCompletion(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	m := textMsg("hello from the sdk client", nil)
	task, err := send(t, cl, ctx, m, false)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != sdk.TaskStateCompleted || meta(task)["completion"] != "delivered" || meta(task)["message_id"] != m.ID {
		t.Fatalf("task %s meta %v", task.Status.State, meta(task))
	}
	w := e.fake.Writes()
	if len(w) != 1 || w[0].Message.Parts[0].Text != "hello from the sdk client" || w[0].Message.From != receiver.PrincipalLocalUser || w[0].Message.Kind != msg.Inform {
		t.Fatalf("writes %+v", w)
	}
	got, err := cl.GetTask(ctx, &sdk.GetTaskRequest{ID: task.ID})
	if err != nil || got.Status.State != sdk.TaskStateCompleted || meta(got)["completion"] != "delivered" {
		t.Fatalf("GetTask %v %v", got, err)
	}
}

// Test 17 (c).
func TestUnknownNeverCompletes(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Unknown})
	cl, ctx := e.peer()
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if task, err := send(t, cl, wctx, textMsg("maybe", nil), false); err == nil {
		t.Fatalf("default wait returned %s", task.Status.State)
	}
	list, err := cl.ListTasks(ctx, &sdk.ListTasksRequest{})
	if err != nil || len(list.Tasks) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if s := list.Tasks[0].Status.State; s != sdk.TaskStateWorking || meta(list.Tasks[0])["state"] != "unknown" {
		t.Fatalf("state %s meta %v", s, meta(list.Tasks[0]))
	}
}

// Test 17 (e) and the ask deadline.
func TestAskCompletesOnReply(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	m := textMsg("which branch?", map[string]any{"kind": "ask"})
	task, err := send(t, cl, ctx, m, true)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != sdk.TaskStateSubmitted {
		t.Fatalf("immediate state %s", task.Status.State)
	}
	waitState(t, cl, ctx, task.ID, sdk.TaskStateWorking)
	if err := e.srv.CompleteAsk(m.ID, "slice/s4"); err != nil {
		t.Fatal(err)
	}
	got := waitState(t, cl, ctx, task.ID, sdk.TaskStateCompleted)
	if got.Status.Message == nil || got.Status.Message.Parts[0].Text() != "slice/s4" || got.Status.Message.Role != sdk.MessageRoleAgent {
		t.Fatalf("final message %+v", got.Status.Message)
	}
	if meta(got)["completion"] != "delivered" {
		t.Fatalf("meta %v", meta(got))
	}
	if err := e.srv.CompleteAsk(m.ID, "again"); !errors.Is(err, ErrAskClosed) {
		t.Fatalf("second reply: %v", err)
	}
	if err := e.srv.CompleteAsk(msg.NewUUIDv7(), "x"); !errors.Is(err, ErrNoSuchAsk) {
		t.Fatalf("unknown ask: %v", err)
	}

	d := textMsg("anyone?", map[string]any{"kind": "ask", "deadline_seconds": 1})
	dt, err := send(t, cl, ctx, d, false)
	if err != nil {
		t.Fatal(err)
	}
	if dt.Status.State != sdk.TaskStateFailed || meta(dt)["reason"] != ReasonDeadline {
		t.Fatalf("deadline task %s %v", dt.Status.State, meta(dt))
	}
	if err := e.srv.CompleteAsk(d.ID, "late"); !errors.Is(err, ErrAskClosed) {
		t.Fatalf("late reply: %v", err)
	}
}

// Test 17 (f) and test 22 with the fake blocking between claim and write.
func TestCancel(t *testing.T) {
	release := make(chan struct{})
	held := make(chan string, 1)
	var hold sync.Once
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) {
		s.beforeReceive = func(id string) {
			hold.Do(func() { held <- id; <-release })
		}
	})
	cl, ctx := e.peer()

	m := textMsg("cancel me", nil)
	task, err := send(t, cl, ctx, m, true)
	if err != nil {
		t.Fatal(err)
	}
	<-held
	got, err := cl.CancelTask(ctx, &sdk.CancelTaskRequest{ID: task.ID})
	if err != nil || got.Status.State != sdk.TaskStateCanceled {
		t.Fatalf("cancel unclaimed: %v %v", got, err)
	}
	close(release)
	e.srv.Close()
	if n := writesFor(e.fake, m.ID); n != 0 {
		t.Fatalf("canceled task wrote %d times", n)
	}

	e2 := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl2, ctx2 := e2.peer()
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	e2.fake.ScriptInject(adapter.Outcome{State: adapter.Accepted, Entered: entered, Gate: gate})
	m2 := textMsg("claimed", nil)
	t2, err := send(t, cl2, ctx2, m2, true)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := cl2.CancelTask(ctx2, &sdk.CancelTaskRequest{ID: t2.ID}); !errors.Is(err, sdk.ErrTaskNotCancelable) {
		t.Fatalf("cancel claimed: %v", err)
	}
	if g, _ := cl2.GetTask(ctx2, &sdk.GetTaskRequest{ID: t2.ID}); g.Status.State != sdk.TaskStateWorking {
		t.Fatalf("claimed task state %s, want working", g.Status.State)
	}
	close(gate)
	waitState(t, cl2, ctx2, t2.ID, sdk.TaskStateCompleted)
	if n := writesFor(e2.fake, m2.ID); n != 1 {
		t.Fatalf("claimed task wrote %d times", n)
	}
	if _, err := cl2.CancelTask(ctx2, &sdk.CancelTaskRequest{ID: t2.ID}); !errors.Is(err, sdk.ErrTaskNotCancelable) {
		t.Fatalf("cancel terminal: %v", err)
	}
}

// Test 22: whatever the interleaving, canceled means zero writes and
// TaskNotCancelable means exactly one.
func TestCancelRace(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	type res struct {
		id       string
		canceled bool
	}
	var out []res
	for range 60 {
		m := textMsg("race", nil)
		task, err := send(t, cl, ctx, m, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cl.CancelTask(ctx, &sdk.CancelTaskRequest{ID: task.ID})
		switch {
		case err == nil:
			out = append(out, res{m.ID, true})
		case errors.Is(err, sdk.ErrTaskNotCancelable):
			out = append(out, res{m.ID, false})
		default:
			t.Fatal(err)
		}
	}
	e.srv.Close()
	var nc, nn int
	for _, r := range out {
		w := writesFor(e.fake, r.id)
		if r.canceled && w != 0 || !r.canceled && w != 1 {
			t.Fatalf("message %s canceled=%v writes=%d", r.id, r.canceled, w)
		}
		if r.canceled {
			nc++
		} else {
			nn++
		}
	}
	t.Logf("canceled %d, not cancelable %d", nc, nn)
}

// Test 17 (g).
func TestContinuationErrors(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	done, err := send(t, cl, ctx, textMsg("one", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	c := textMsg("two", nil)
	c.TaskID = done.ID
	if _, err := send(t, cl, ctx, c, false); !errors.Is(err, sdk.ErrUnsupportedOperation) {
		t.Fatalf("terminal continuation: %v", err)
	}
	u := textMsg("three", nil)
	u.TaskID = sdk.NewTaskID()
	if _, err := send(t, cl, ctx, u, false); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("unknown continuation: %v", err)
	}
	if n := len(e.fake.Writes()); n != 1 {
		t.Fatalf("writes %d", n)
	}
}

// Test 17 (h).
func TestListOwnTasksOnly(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	xTok := e.addAgent("x", "xxxxxxxxxxxxxxxx", registry.RolePrimary)
	pc, pctx := e.peer()
	xc, xctx := e.sdkClient(xTok, "x")
	for range 2 {
		if _, err := send(t, pc, pctx, textMsg("peer", nil), false); err != nil {
			t.Fatal(err)
		}
	}
	xt, err := send(t, xc, xctx, textMsg("x", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := pc.ListTasks(pctx, &sdk.ListTasksRequest{})
	if err != nil || len(pl.Tasks) != 2 || pl.TotalSize != 2 {
		t.Fatalf("peer list %v %v", pl, err)
	}
	for _, task := range pl.Tasks {
		if task.ID == xt.ID {
			t.Fatal("peer sees x's task")
		}
	}
	xl, err := xc.ListTasks(xctx, &sdk.ListTasksRequest{})
	if err != nil || len(xl.Tasks) != 1 || xl.Tasks[0].ID != xt.ID {
		t.Fatalf("x list %v %v", xl, err)
	}
	page, err := pc.ListTasks(pctx, &sdk.ListTasksRequest{PageSize: 1})
	if err != nil || len(page.Tasks) != 1 || page.NextPageToken == "" {
		t.Fatalf("page %v %v", page, err)
	}
	next, err := pc.ListTasks(pctx, &sdk.ListTasksRequest{PageSize: 1, PageToken: page.NextPageToken})
	if err != nil || len(next.Tasks) != 1 || next.NextPageToken != "" || next.Tasks[0].ID == page.Tasks[0].ID {
		t.Fatalf("next page %v %v", next, err)
	}
	if w := e.fake.Writes(); len(w) != 3 || w[2].Message.From != "x" {
		t.Fatalf("writes %+v", w)
	}
}

// Test 17 (i): refusal is the SDK's JSON-RPC UNAUTHENTICATED error
// (-31401) on HTTP 200.
func TestAuthRefused(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	xTok := e.addAgent("x", "xxxxxxxxxxxxxxxx", registry.RolePrimary)
	params := map[string]any{"message": map[string]any{"messageId": msg.NewUUIDv7(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}}
	for name, tok := range map[string]string{"none": "", "wrong": "not-the-token", "own token without name": xTok} {
		code, out := rawCall(t, e.url, tok, "SendMessage", params, "")
		if code != http.StatusOK || errCode(out) != -31401 {
			t.Errorf("%s: http %d body %v", name, code, out)
		}
	}
	for name, c := range map[string]struct{ tok, name string }{
		"wrong token for agent": {"not-the-token", "x"},
		"peer token as agent":   {e.peerTok, "x"},
		"unknown agent":         {xTok, "nobody"},
		"wrong token":           {"not-the-token", ""},
	} {
		cl, ctx := e.sdkClient(c.tok, c.name)
		if _, err := send(t, cl, ctx, textMsg("hi", nil), false); !errors.Is(err, sdk.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), xTok) || strings.Contains(err.Error(), e.peerTok) {
			t.Errorf("%s: error leaks a token", name)
		}
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("refused requests reached the adapter: %d calls", n)
	}
	if fi, err := os.Stat(filepath.Join(e.dir, "peer.token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("peer token mode %v %v", fi, err)
	}
}

// Test 17 (j).
func TestUnimplementedMethods(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	sendParams := map[string]any{"message": map[string]any{"messageId": msg.NewUUIDv7(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}}
	taskParams := map[string]any{"id": "t1", "taskId": "t1"}
	cases := []struct {
		method string
		params any
		code   float64
	}{
		{"SendStreamingMessage", sendParams, -32004},
		{"SubscribeToTask", taskParams, -32004},
		{"GetTaskPushNotificationConfig", taskParams, -32003},
		{"CreateTaskPushNotificationConfig", map[string]any{"taskId": "t1", "url": "http://127.0.0.1:1/x"}, -32003},
		{"ListTaskPushNotificationConfigs", taskParams, -32003},
		{"DeleteTaskPushNotificationConfig", taskParams, -32003},
		{"GetExtendedAgentCard", map[string]any{}, -32004},
	}
	for _, c := range cases {
		code, out := rawCall(t, e.url, e.peerTok, c.method, c.params, "application/json")
		if code != http.StatusOK || errCode(out) != c.code {
			t.Errorf("%s: http %d body %v, want code %v", c.method, code, out, c.code)
		}
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("unimplemented methods reached the adapter: %d calls", n)
	}
}

// Test 23, A2A authority parts.
func TestContinuationAuthority(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	xTok := e.addAgent("x", "xxxxxxxxxxxxxxxx", registry.RolePrimary)
	yTok := e.addAgent("y", "yyyyyyyyyyyyyyyy", registry.RolePrimary)
	xc, xctx := e.sdkClient(xTok, "x")
	yc, yctx := e.sdkClient(yTok, "y")
	e.setSelf(registry.RoleSubagent, "zzzzzzzzzzzzzzzz")

	ask := textMsg("question from y", map[string]any{"kind": "ask"})
	yt, err := send(t, yc, yctx, ask, true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, yc, yctx, yt.ID, sdk.TaskStateWorking)
	if w := e.fake.Writes(); len(w) != 1 || w[0].Message.Kind != msg.Ask {
		t.Fatalf("ask from non-owner not delivered as ask: %+v", w)
	}

	xm := textMsg("hijack", nil)
	xm.TaskID = yt.ID
	if _, err := send(t, xc, xctx, xm, true); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("x continuation: %v", err)
	}
	if _, err := xc.GetTask(xctx, &sdk.GetTaskRequest{ID: yt.ID}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("x GetTask: %v", err)
	}
	if _, err := xc.CancelTask(xctx, &sdk.CancelTaskRequest{ID: yt.ID}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("x CancelTask: %v", err)
	}

	cmd := textMsg("do it", map[string]any{"kind": "command"})
	cmd.TaskID = yt.ID
	if _, err := send(t, yc, yctx, cmd, true); !errors.Is(err, sdk.ErrUnauthorized) {
		t.Fatalf("y command continuation: %v", err)
	}
	inf := textMsg("more context", nil)
	inf.TaskID = yt.ID
	got, err := send(t, yc, yctx, inf, true)
	if err != nil || got.Status.State != sdk.TaskStateWorking || len(got.History) != 2 {
		t.Fatalf("y inform continuation: %v %v", got, err)
	}
	w := e.fake.Writes()
	if len(w) != 2 || w[1].Message.Kind != msg.Inform || w[1].Message.TaskID == nil || *w[1].Message.TaskID != string(yt.ID) || w[1].Message.ContextID != yt.ContextID {
		t.Fatalf("writes %+v", w)
	}

	ct, err := send(t, yc, yctx, textMsg("command from non-owner", map[string]any{"kind": "command"}), false)
	if err != nil || ct.Status.State != sdk.TaskStateFailed || meta(ct)["reason"] != receiver.ReasonPolicy {
		t.Fatalf("command task: %v %v", ct, err)
	}
	if n := len(e.fake.Writes()); n != 2 {
		t.Fatalf("rejected command was injected: %d writes", n)
	}
}

func TestMetadataAndClientIDs(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	xTok := e.addAgent("x", "xxxxxxxxxxxxxxxx", registry.RolePrimary)
	xc, xctx := e.sdkClient(xTok, "x")
	irt := msg.NewUUIDv7()
	m := textMsg("do the thing", map[string]any{"kind": "command", "in_reply_to": irt, "delivery": "next_turn", "hop_chain": []any{msg.Hop("elsewhere")}})
	m.ID = "client-1"
	task, err := send(t, xc, xctx, m, false)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := meta(task)["message_id"].(string)
	if task.Status.State != sdk.TaskStateCompleted || !msg.IsUUIDv7(id) {
		t.Fatalf("task %s message_id %q", task.Status.State, id)
	}
	hm, _ := task.History[0].Metadata[MetaKey].(map[string]any)
	if hm["client_message_id"] != "client-1" || task.History[0].ID != id {
		t.Fatalf("history %+v", task.History[0])
	}
	w := e.fake.Writes()
	if len(w) != 1 {
		t.Fatalf("writes %d", len(w))
	}
	g := w[0].Message
	if g.MessageID != id || g.Kind != msg.Command || g.Delivery != msg.NextTurn || g.InReplyTo == nil || *g.InReplyTo != irt || len(g.HopChain) != 1 || g.From != "x" {
		t.Fatalf("normalized %+v", g)
	}
	again, err := send(t, xc, xctx, m, false)
	if err != nil || again.ID != task.ID || len(e.fake.Writes()) != 1 {
		t.Fatalf("retry: %v %v writes %d", again, err, len(e.fake.Writes()))
	}
	bad := textMsg("x", map[string]any{"kind": 7})
	if _, err := send(t, xc, xctx, bad, false); !errors.Is(err, sdk.ErrInvalidParams) {
		t.Fatalf("bad metadata: %v", err)
	}
}

func TestAdmission(t *testing.T) {
	e := newEnv(t, receiver.Limits{MaxMessageBytes: 8}, Limits{MaxWaiting: 1, MaxOpenAsks: 1}, nil)
	cl, ctx := e.peer()
	if _, err := send(t, cl, ctx, textMsg("far too large", nil), false); !errors.Is(err, sdk.ErrServerError) {
		t.Fatalf("too large: %v", err)
	}
	if l, _ := cl.ListTasks(ctx, &sdk.ListTasksRequest{}); len(l.Tasks) != 0 {
		t.Fatalf("admission rejection left a task: %v", l.Tasks)
	}
	if _, err := send(t, cl, ctx, textMsg("q1", map[string]any{"kind": "ask"}), true); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, cl, ctx, textMsg("q2", map[string]any{"kind": "ask"}), true); !errors.Is(err, sdk.ErrServerError) {
		t.Fatalf("second open ask: %v", err)
	}
	entered := make(chan struct{}, 1)
	e.fake.ScriptInject(adapter.Outcome{State: adapter.Unknown, Entered: entered})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go cl.SendMessage(wctx, &sdk.SendMessageRequest{Message: textMsg("hang", nil)})
	<-entered
	if _, err := send(t, cl, ctx, textMsg("w2", nil), false); !errors.Is(err, sdk.ErrServerError) {
		t.Fatalf("second wait admitted: %v", err)
	}
}

type failingTaskStore struct {
	MemoryTaskStore
	fail bool
}

func (s *failingTaskStore) Save(r journal.Record) error {
	if s.fail {
		return errors.New("injected storage failure")
	}
	return s.MemoryTaskStore.Save(r)
}

func TestTerminalWriteFailureIsNotCompleted(t *testing.T) {
	ts := &failingTaskStore{}
	st := newStore(ts, slog.New(slog.DiscardHandler))
	m := msg.Message{MessageID: msg.NewUUIDv7(), ContextID: "ctx", Kind: msg.Ask}
	task, _, err := st.create("peer", m, encode(m), 32, 256)
	if err != nil {
		t.Fatal(err)
	}
	durable := len(ts.Load())
	ts.fail = true
	if err := st.completeAsk(m.MessageID, "answer"); err == nil {
		t.Fatal("completeAsk succeeded despite failed persistence")
	}
	if got := st.snapshot(task, nil); got.Status.State.Terminal() || len(got.History) != 1 {
		t.Fatalf("task moved despite failed persistence: state=%v history=%d", got.Status.State, len(got.History))
	}
	select {
	case <-task.done:
		t.Fatal("terminal notification closed despite failed persistence")
	default:
	}
	if len(ts.Load()) != durable {
		t.Fatalf("durable records changed: %d -> %d", durable, len(ts.Load()))
	}
	srv := &Server{tasks: st}
	srv.apply(task, receiver.Result{MessageID: m.MessageID, State: adapter.Accepted})
	got := st.snapshot(task, nil)
	ct, _ := got.Metadata[MetaKey].(map[string]any)
	if got.Status.State != sdk.TaskStateWorking || ct["state"] != string(adapter.Unknown) || ct["reason"] != receiver.ReasonJournal {
		t.Fatalf("outcome write failure not reported unknown/journal: %v %v", got.Status.State, ct)
	}
}
