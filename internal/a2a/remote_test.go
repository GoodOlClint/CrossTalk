package a2a

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

var (
	hostA = strings.Repeat("a", 64)
	hostB = strings.Repeat("b", 64)
	hostC = strings.Repeat("c", 64)
)

const (
	remoteID = "e1e1e1e1e1e1e1e1"
	localID  = "a1a1a1a1a1a1a1a1"
)

type hdrs map[string][]string

func (h hdrs) with(k string, v ...string) hdrs {
	o := hdrs{}
	for kk, vv := range h {
		o[kk] = vv
	}
	if v == nil {
		delete(o, k)
	} else {
		o[k] = v
	}
	return o
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func gwHdrs(host, alias, agentID, name, role string) hdrs {
	return hdrs{
		GWHostHeader:    {host},
		GWAliasHeader:   {alias},
		GWAgentIDHeader: {agentID},
		GWNameHeader:    {b64(name)},
		GWHarnessHeader: {"codex"},
		GWRoleHeader:    {role},
	}
}

func goodRemote() hdrs { return gwHdrs(hostA, "desk", remoteID, "Remote Alice", registry.RolePrimary) }

func (e *env) clientWith(token string, h hdrs) (*sdkClient, context.Context) {
	e.t.Helper()
	cl, ctx := e.sdkClient(token, "")
	sp := a2aclient.ServiceParams{}
	for k, v := range h {
		sp[k] = v
	}
	return cl, a2aclient.AttachServiceParams(ctx, sp)
}

type sdkClient = a2aclient.Client

func (e *env) remote(h hdrs) (*sdkClient, context.Context) { return e.clientWith(e.gwTok, h) }

func dedupeSenders(t *testing.T, j *journal.Journal, messageID string) []string {
	t.Helper()
	rs, err := j.Records()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if r.Type == journal.TypeDedupe && r.MessageID == messageID && !seen[r.Sender] {
			seen[r.Sender] = true
			out = append(out, r.Sender)
		}
	}
	return out
}

func taskReasonOf(task *sdk.Task) (string, string) {
	ct := meta(task)
	state, _ := ct["state"].(string)
	reason, _ := ct["reason"].(string)
	return state, reason
}

func TestRemoteSenderKeyIsHostScoped(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.remote(goodRemote())
	m := textMsg("from afar", nil)
	task, err := send(t, cl, ctx, m, false)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != sdk.TaskStateCompleted {
		t.Fatalf("task %s meta %v", task.Status.State, meta(task))
	}
	want := receiver.RemoteKey(hostA, remoteID)
	if got := dedupeSenders(t, e.j, m.ID); len(got) != 1 || got[0] != want {
		t.Fatalf("dedupe senders %q, want [%s]", got, want)
	}
	w := e.fake.Writes()
	if len(w) != 1 || w[0].Message.From != "Remote Alice@desk" || !w[0].Delivery.VerifiedSender || w[0].Delivery.SenderHarness != "codex" {
		t.Fatalf("writes %+v", w)
	}
	snd := receiver.Sender{Verified: true, Host: hostA, HostAlias: "desk", AgentID: remoteID, Name: "Remote Alice@desk", Principal: want}
	if got := principal(snd); got != want {
		t.Fatalf("principal %q, want %q", got, want)
	}
}

func TestRemoteCannotPoseAsLocalAgent(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	tok := e.addAgent("alice", localID, registry.RolePrimary)

	lc, lctx := e.sdkClient(tok, "alice")
	lm := textMsg("local ask", map[string]any{"kind": "ask"})
	lt, err := send(t, lc, lctx, lm, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "local ask injected", func() bool { return writesFor(e.fake, lm.ID) == 1 })

	rc, rctx := e.remote(gwHdrs(hostA, "desk", localID, "alice", registry.RolePrimary))
	rm := textMsg("remote ask", map[string]any{"kind": "ask"})
	rt, err := send(t, rc, rctx, rm, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "remote ask injected", func() bool { return writesFor(e.fake, rm.ID) == 1 })

	remoteKey := receiver.RemoteKey(hostA, localID)
	if got := dedupeSenders(t, e.j, rm.ID); len(got) != 1 || got[0] != remoteKey {
		t.Fatalf("remote message dedupe senders %q, want [%s]", got, remoteKey)
	}
	if got := dedupeSenders(t, e.j, lm.ID); len(got) != 1 || got[0] != localID {
		t.Fatalf("local message dedupe senders %q, want [%s]", got, localID)
	}
	rs, _ := e.j.Records()
	routes := map[string]string{}
	for _, r := range rs {
		if r.Type == journal.TypeRoute && r.State == journal.StateOpen {
			routes[r.MessageID] = r.Sender
		}
	}
	if routes[lm.ID] != localID || routes[rm.ID] != remoteKey {
		t.Fatalf("routes %v, want local %s remote %s", routes, localID, remoteKey)
	}
	if _, err := lc.GetTask(lctx, &sdk.GetTaskRequest{ID: rt.ID}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("local alice read the remote task: %v", err)
	}
	if _, err := rc.GetTask(rctx, &sdk.GetTaskRequest{ID: lt.ID}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("remote read the local task: %v", err)
	}
}

func TestRemoteCannotPoseAsThirdHost(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	m := textMsg("from host A", nil)
	m2 := textMsg("from host B", nil)

	forged := goodRemote().with("X-CrossTalk-Remote-Host", hostC).with("X-CrossTalk-Remote-Alias", "third")
	ca, actx := e.remote(forged)
	if _, err := send(t, ca, actx, m, false); err != nil {
		t.Fatal(err)
	}
	cb, bctx := e.remote(gwHdrs(hostB, "lab", remoteID, "Remote Alice", registry.RolePrimary))
	if _, err := send(t, cb, bctx, m2, false); err != nil {
		t.Fatalf("second host with the same agent_id: %v", err)
	}
	wantA, wantB := receiver.RemoteKey(hostA, remoteID), receiver.RemoteKey(hostB, remoteID)
	if got := dedupeSenders(t, e.j, m.ID); len(got) != 1 || got[0] != wantA {
		t.Fatalf("host A message dedupe senders %q, want [%s]", got, wantA)
	}
	if got := dedupeSenders(t, e.j, m2.ID); len(got) != 1 || got[0] != wantB {
		t.Fatalf("host B message dedupe senders %q, want [%s]", got, wantB)
	}
	rs, _ := e.j.Records()
	for _, r := range rs {
		if strings.Contains(r.Sender, hostC) {
			t.Fatalf("forged third-host header reached a record: %+v", r)
		}
	}
	if n := len(e.fake.Writes()); n != 2 {
		t.Fatalf("injects %d, want 2", n)
	}
}

func TestGatewayHeadersWithoutCredentialIgnored(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	xTok := e.addAgent("x", "xxxxxxxxxxxxxxxx", registry.RolePrimary)
	h := goodRemote()

	cases := []struct {
		name  string
		tok   string
		agent string
		from  string
		key   string
		fail  bool
	}{
		{"peer token", e.peerTok, "", receiver.PrincipalLocalUser, receiver.PrincipalLocalUser, false},
		{"agent own token", xTok, "x", "x", "xxxxxxxxxxxxxxxx", false},
		{"wrong bearer", "not-the-token", "", "", "", true},
		{"gateway token with the local agent header", e.gwTok, "x", "", "", true},
		{"gateway token without host headers", e.gwTok, "", "", "", true},
	}
	rejectedCase := "gateway token without host headers"
	for _, c := range cases {
		hh := h
		if c.name == "gateway token without host headers" {
			hh = hdrs{}
		}
		cl, ctx := e.clientWith(c.tok, hh)
		if c.agent != "" {
			sp := a2aclient.ServiceParams{SenderHeader: {c.agent}}
			for k, v := range hh {
				sp[k] = v
			}
			ctx = a2aclient.AttachServiceParams(ctx, sp)
		}
		m := textMsg("hi "+c.name, nil)
		task, err := send(t, cl, ctx, m, false)
		if c.fail {
			var se *sdk.Error
			if c.name == rejectedCase {
				if !errors.As(err, &se) || se.Details[MetaKey] == nil {
					t.Errorf("%s: err %v, want a JSON-RPC rejection", c.name, err)
				}
			} else if !errors.Is(err, sdk.ErrUnauthenticated) {
				t.Errorf("%s: err %v, want unauthenticated", c.name, err)
			}
			if writesFor(e.fake, m.ID) != 0 {
				t.Errorf("%s: refused request injected", c.name)
			}
			continue
		}
		if err != nil || task.Status.State != sdk.TaskStateCompleted {
			t.Errorf("%s: %v %v", c.name, task, err)
			continue
		}
		if got := dedupeSenders(t, e.j, m.ID); len(got) != 1 || got[0] != c.key {
			t.Errorf("%s: dedupe senders %q, want [%s]", c.name, got, c.key)
		}
		for _, w := range e.fake.Writes() {
			if w.Message.MessageID == m.ID && (w.Message.From != c.from || strings.Contains(w.Message.From, "@desk")) {
				t.Errorf("%s: From %q, want %q", c.name, w.Message.From, c.from)
			}
		}
	}
}

func TestMalformedRemoteFieldsRefused(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	long := strings.Repeat("n", 257)
	cases := []struct {
		name string
		h    hdrs
	}{
		{"agent_id uppercase", goodRemote().with(GWAgentIDHeader, "E1E1E1E1E1E1E1E1")},
		{"agent_id 15 digits", goodRemote().with(GWAgentIDHeader, remoteID[:15])},
		{"agent_id 17 digits", goodRemote().with(GWAgentIDHeader, remoteID+"1")},
		{"agent_id non-hex", goodRemote().with(GWAgentIDHeader, "g1e1e1e1e1e1e1e1")},
		{"agent_id absent", goodRemote().with(GWAgentIDHeader)},
		{"agent_id twice", goodRemote().with(GWAgentIDHeader, remoteID, "f2f2f2f2f2f2f2f2")},
		{"name not base64url", goodRemote().with(GWNameHeader, "not base64!")},
		{"name over 256 bytes", goodRemote().with(GWNameHeader, b64(long))},
		{"name with control character", goodRemote().with(GWNameHeader, b64("bad\x07name"))},
		{"name empty", goodRemote().with(GWNameHeader, "")},
		{"name absent", goodRemote().with(GWNameHeader)},
		{"harness gemini", goodRemote().with(GWHarnessHeader, "gemini")},
		{"harness absent", goodRemote().with(GWHarnessHeader)},
		{"role admin", goodRemote().with(GWRoleHeader, "admin")},
		{"role absent", goodRemote().with(GWRoleHeader)},
		{"host uppercase", goodRemote().with(GWHostHeader, strings.ToUpper(hostA))},
		{"host short", goodRemote().with(GWHostHeader, hostA[:63])},
		{"host absent", goodRemote().with(GWHostHeader)},
		{"host twice", goodRemote().with(GWHostHeader, hostA, hostB)},
		{"alias absent", goodRemote().with(GWAliasHeader)},
		{"alias with at sign", goodRemote().with(GWAliasHeader, "a@b")},
		{"alias with space", goodRemote().with(GWAliasHeader, "my desk")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl, ctx := e.remote(c.h)
			_, err := send(t, cl, ctx, textMsg("must not land", nil), false)
			var se *sdk.Error
			if !errors.As(err, &se) {
				t.Fatalf("err %v is not a JSON-RPC error", err)
			}
			ct, _ := se.Details[MetaKey].(map[string]any)
			if ct["state"] != string(adapter.Rejected) {
				t.Fatalf("details %v, want state rejected", se.Details)
			}
			if n := len(e.fake.Calls()); n != 0 {
				t.Fatalf("refused request reached the adapter: %d calls", n)
			}
		})
	}
}

func TestRemoteCommandRejected(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cmd := map[string]any{"kind": "command"}
	for _, role := range []string{registry.RolePrimary, registry.RoleSubagent} {
		cl, ctx := e.remote(gwHdrs(hostA, "desk", remoteID, "Remote Alice", role))
		task, err := send(t, cl, ctx, textMsg("run it", cmd), false)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if st, reason := taskReasonOf(task); task.Status.State != sdk.TaskStateFailed || st != string(adapter.Rejected) || reason != receiver.ReasonRemoteKind {
			t.Fatalf("%s: task %s state %q reason %q", role, task.Status.State, st, reason)
		}
	}
	e.setSelf(registry.RoleSubagent, remoteID)
	cl, ctx := e.remote(goodRemote())
	task, err := send(t, cl, ctx, textMsg("owner command", cmd), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := taskReasonOf(task); task.Status.State != sdk.TaskStateFailed || reason != receiver.ReasonRemoteKind {
		t.Fatalf("remote agent_id equal to the owner: task %s reason %q", task.Status.State, reason)
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("remote command reached the adapter: %d calls", n)
	}
}

func TestRemotePrimaryCannotCommand(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cmd := map[string]any{"kind": "command"}

	pTok := e.addAgent("pri", "bbbbbbbbbbbbbbbb", registry.RolePrimary)
	lc, lctx := e.sdkClient(pTok, "pri")
	lm := textMsg("local primary command", cmd)
	lt, err := send(t, lc, lctx, lm, false)
	if err != nil || lt.Status.State != sdk.TaskStateCompleted || writesFor(e.fake, lm.ID) != 1 {
		t.Fatalf("control: a local primary's command was not accepted: %v %v", lt, err)
	}

	cl, ctx := e.remote(gwHdrs(hostA, "desk", remoteID, "Remote Primary", registry.RolePrimary))
	rm := textMsg("remote primary command", cmd)
	task, err := send(t, cl, ctx, rm, false)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != sdk.TaskStateFailed || writesFor(e.fake, rm.ID) != 0 {
		t.Fatalf("remote primary command: task %s, injects %d", task.Status.State, writesFor(e.fake, rm.ID))
	}
	if n := len(e.fake.Writes()); n != 1 {
		t.Fatalf("injects %d, want only the control's 1", n)
	}
}

func TestRemoteClaimRefused(t *testing.T) {
	var calls int
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) {
		s.cfg.Claim = func(context.Context, receiver.Sender, bool) error { calls++; return nil }
	})
	for _, claimed := range []bool{true, false} {
		cl, ctx := e.remote(goodRemote())
		m := &sdk.Message{ID: msg.NewUUIDv7(), Role: sdk.MessageRoleUser, Parts: sdk.ContentParts{sdk.NewTextPart("claim")},
			Metadata: map[string]any{MetaKey: map[string]any{"claim": map[string]any{"owner": "Remote Alice@desk", "claimed": claimed}}}}
		_, err := cl.SendMessage(ctx, &sdk.SendMessageRequest{Message: m, Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
		var se *sdk.Error
		if !errors.As(err, &se) {
			t.Fatalf("claimed=%v: err %v is not a JSON-RPC error", claimed, err)
		}
		ct, _ := se.Details[MetaKey].(map[string]any)
		if ct["state"] != string(adapter.Rejected) || ct["reason"] != receiver.ReasonRemoteKind {
			t.Fatalf("claimed=%v: details %v", claimed, se.Details)
		}
	}
	if calls != 0 {
		t.Fatalf("claim hook called %d times for a remote sender", calls)
	}
}

func TestDispatchClaimKeyedBySender(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	local := receiver.Sender{Verified: true, AgentID: localID, Name: "alice", Role: registry.RolePrimary}
	m := msg.Message{MessageID: msg.NewUUIDv7(), ContextID: "ctx", From: "alice", To: "self", Kind: msg.Inform, Delivery: msg.Immediate, Parts: []msg.Part{{Type: "text", Text: "local"}}}
	if res := e.rcv.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: local}); res.State != adapter.Accepted {
		t.Fatalf("local %+v", res)
	}

	cl, ctx := e.remote(gwHdrs(hostA, "desk", localID, "alice", registry.RolePrimary))
	rm := &sdk.Message{ID: m.MessageID, Role: sdk.MessageRoleUser, Parts: sdk.ContentParts{sdk.NewTextPart("remote, same message_id")}}
	task, err := send(t, cl, ctx, rm, false)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != sdk.TaskStateCompleted {
		_, reason := taskReasonOf(task)
		t.Fatalf("remote message with a local message_id: task %s reason %q", task.Status.State, reason)
	}
	if n := writesFor(e.fake, m.MessageID); n != 2 {
		t.Fatalf("injects %d, want 2 (one per sender)", n)
	}

	again, err := send(t, cl, ctx, rm, false)
	if err != nil || again.ID != task.ID {
		t.Fatalf("true duplicate from the same sender: %v %v", again, err)
	}
	if n := writesFor(e.fake, m.MessageID); n != 2 {
		t.Fatalf("duplicate was injected: %d writes", n)
	}
	var scopes []string
	rs, _ := e.j.Records()
	for _, r := range rs {
		if r.Type == journal.TypeDispatch && r.MessageID == m.MessageID {
			scopes = append(scopes, r.Sender)
		}
	}
	if len(scopes) != 2 || scopes[0] != "" || scopes[1] != receiver.RemoteKey(hostA, localID) {
		t.Fatalf("dispatch records scoped %q, want [\"\" %s]", scopes, receiver.RemoteKey(hostA, localID))
	}
}

func TestRemoteTaskResumeKeepsHostScope(t *testing.T) {
	st := &store{}
	key := receiver.RemoteKey(hostA, remoteID)
	m := textMsg("replayed", nil)
	tk := &task{principal: key, state: sdk.TaskStateSubmitted, history: []*sdk.Message{m}}
	in, ok := st.resumeInbound(tk, nil)
	if !ok || in == nil {
		t.Fatal("remote task not rebuilt")
	}
	if in.Sender.Host != hostA || in.Sender.AgentID != remoteID || principal(in.Sender) != key || in.Sender.Principal != key {
		t.Fatalf("sender %+v", in.Sender)
	}
}
