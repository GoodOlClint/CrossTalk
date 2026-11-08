package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	senderAgent = "e1e1e1e1e1e1e1e1"
	targetAgent = "b0b0b0b0b0b0b0b0"
	otherAgent  = "c0c0c0c0c0c0c0c0"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func remoteFields() http.Header {
	h := http.Header{}
	h.Set(RemoteAgentIDHeader, senderAgent)
	h.Set(RemoteNameHeader, b64("Remote Alice"))
	h.Set(RemoteHarnessHeader, "codex")
	h.Set(RemoteRoleHeader, registry.RolePrimary)
	return h
}

type recorded struct {
	Path   string
	Query  string
	Host   string
	Header http.Header
	Body   []byte
}

type loopback struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

func newLoopback(t *testing.T) *loopback {
	t.Helper()
	l := &loopback{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.reqs = append(l.reqs, recorded{Path: r.URL.Path, Query: r.URL.RawQuery, Host: r.Host, Header: r.Header.Clone(), Body: b})
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *loopback) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

func (l *loopback) last() recorded {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		return recorded{}
	}
	return l.reqs[len(l.reqs)-1]
}

func agentsOf(m map[string]string) func(string) (string, bool) {
	return func(id string) (string, bool) { u, ok := m[id]; return u, ok }
}

func forwardGateway(t *testing.T, agents func(string) (string, bool)) *gwEnv {
	t.Helper()
	return startGateway(t, func(c *GatewayConfig) { c.Handler = NewForwarder(c.StateDir, agents) })
}

// pin enrols a fresh sender host under alias and returns its identity.
func (e *gwEnv) pin(alias string) Host {
	e.t.Helper()
	h, err := LoadOrCreateHost(newDir(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.peers.Upsert(Peer{Alias: alias, Address: "192.0.2.1:7745", Fingerprint: h.Fingerprint}); err != nil {
		e.t.Fatal(err)
	}
	return h
}

// senderClient is a test-only mTLS client with h's certificate that accepts
// only the gateway's own fingerprint.
func (e *gwEnv) senderClient(h Host) *http.Client {
	e.t.Helper()
	want := e.host.Fingerprint
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS13, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || Fingerprint(cs.PeerCertificates[0]) != want {
				return errors.New("gateway fingerprint mismatch")
			}
			return nil
		},
	}, MaxIdleConnsPerHost: 1}
	e.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func rpcBody(id any, method string, params any) []byte {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		panic(err)
	}
	return b
}

func sendParams(messageID, text string, meta map[string]any) map[string]any {
	m := map[string]any{"messageId": messageID, "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}}
	if meta != nil {
		m["metadata"] = map[string]any{"crosstalk": meta}
	}
	return map[string]any{"message": m, "configuration": map[string]any{"returnImmediately": true}}
}

type rpcResult struct {
	Status int
	Body   []byte
}

func (r rpcResult) errCode() (int, bool) {
	var v struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(r.Body, &v) != nil || v.Error == nil {
		return 0, false
	}
	return v.Error.Code, true
}

func (r rpcResult) hasReason(reason string) bool {
	return strings.Contains(string(r.Body), `"reason":"`+reason+`"`)
}

func (e *gwEnv) post(c *http.Client, path string, hdr http.Header, body []byte) rpcResult {
	e.t.Helper()
	req, err := http.NewRequest("POST", "https://"+e.addr()+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range hdr {
		req.Header[k] = append([]string(nil), vs...)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return rpcResult{Status: resp.StatusCode, Body: b}
}

func (e *gwEnv) token() string {
	e.t.Helper()
	tok, err := EnsureToken(e.stateDir)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

type agentEnv struct {
	fake *adapter.Fake
	srv  *a2a.Server
}

func startAgent(t *testing.T, stateDir, agentID string) *agentEnv {
	t.Helper()
	if _, err := EnsureToken(stateDir); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := adapter.NewFake(adapter.Claude)
	b := adapter.Binding{Harness: adapter.Claude, SessionRef: "sess-" + agentID}
	self := receiver.Recipient{AgentID: agentID, Name: "bob", Role: registry.RolePrimary, Binding: b}
	j, _, err := journal.Open(dir, agentID, journal.Binding{InstanceID: msg.NewUUIDv7(), SessionRef: b.SessionRef}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rcv := receiver.New(receiver.Config{Journal: j, Adapter: fake, Self: func() receiver.Recipient { return self }})
	srv := a2a.NewServer(a2a.Config{
		Receiver: rcv, Version: "test", PeerTokenPath: filepath.Join(dir, "peer.token"),
		GatewayTokenPath: filepath.Join(stateDir, "gateway.token"),
		Profile:          func() a2a.Profile { return a2a.Profile{Name: "bob", Harness: "claude", Role: registry.RolePrimary} },
	})
	if _, err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()
	t.Cleanup(func() { srv.Close(); <-done; j.Close() })
	return &agentEnv{fake: fake, srv: srv}
}

func injects(f *adapter.Fake, messageID string) int {
	n := 0
	for _, c := range f.Writes() {
		if c.Message.MessageID == messageID {
			n++
		}
	}
	return n
}

func TestTwoHostInform(t *testing.T) {
	g1 := startGateway(t, nil)
	g2 := newGateway(t, nil)
	bob := startAgent(t, g2.stateDir, targetAgent)
	g2.gw = NewGateway(GatewayConfig{
		StateDir: g2.stateDir, Host: g2.host, Bind: "127.0.0.1", Port: 0, Peers: g2.peers, Limits: DefaultLimits(), Log: g2.logs.logf,
		Handler: NewForwarder(g2.stateDir, agentsOf(map[string]string{targetAgent: bob.srv.URL()})),
	})
	g2.start()
	sender := g2.pin("desk1")
	if g1.host.Fingerprint == "" || g1.stateDir == g2.stateDir || g1.cfgDir == g2.cfgDir || g1.addr() == g2.addr() {
		t.Fatal("hosts are not independent")
	}
	cl := g2.senderClient(sender)
	id := msg.NewUUIDv7()
	body := rpcBody("1", "SendMessage", sendParams(id, "hello across hosts", nil))
	res := g2.post(cl, "/v1/a2a/"+targetAgent, remoteFields(), body)
	if _, bad := res.errCode(); bad || res.Status != 200 {
		t.Fatalf("inform: status %d body %s", res.Status, res.Body)
	}
	if n := injects(bob.fake, id); n != 1 {
		t.Fatalf("host 2 injected %d times, want 1", n)
	}
	w := bob.fake.Writes()
	if w[0].Message.From != "Remote Alice@desk1" || !w[0].Delivery.VerifiedSender {
		t.Fatalf("sender label %q verified %v, want %q from host 2's peers.json", w[0].Message.From, w[0].Delivery.VerifiedSender, "Remote Alice@desk1")
	}
	res = g2.post(cl, "/v1/a2a/"+targetAgent, remoteFields(), body)
	if res.Status != 200 {
		t.Fatalf("replay: status %d body %s", res.Status, res.Body)
	}
	if n := injects(bob.fake, id); n != 1 {
		t.Fatalf("after a replayed message_id host 2 injected %d times, want 1", n)
	}
}

func TestGatewayStripsInboundHeaders(t *testing.T) {
	lb := newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: lb.srv.URL}))
	sender := e.pin("desk1")
	cl := e.senderClient(sender)
	tok := e.token()
	want := map[string]string{
		"X-Crosstalk-Gw-Host":     sender.Fingerprint,
		"X-Crosstalk-Gw-Alias":    "desk1",
		"X-Crosstalk-Gw-Agent-Id": senderAgent,
		"X-Crosstalk-Gw-Name":     b64("Remote Alice"),
		"X-Crosstalk-Gw-Harness":  "codex",
		"X-Crosstalk-Gw-Role":     registry.RolePrimary,
	}
	forged := []struct{ key, val string }{
		{"x-crosstalk-gw-host", strings.Repeat("f", 64)},
		{"X-CROSSTALK-GW-ALIAS", "evil"},
		{"X-CrossTalk-GW-Agent-ID", "0000000000000000"},
		{"x-CrossTalk-gw-NAME", b64("evil")},
		{"X-CrossTalk-GW-Harness", "claude"},
		{"X-CrossTalk-GW-Role", registry.RoleSubagent},
		{"authorization", "Bearer nothing-valid"},
		{"X-CrossTalk-Extra", "1"},
		{"X-CrossTalk-Remote-Alias", "evil"},
	}
	check := func(t *testing.T, h http.Header) {
		t.Helper()
		before := lb.count()
		res := e.post(cl, "/v1/a2a/"+targetAgent, h, rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "x", nil)))
		if res.Status != 200 || lb.count() != before+1 {
			t.Fatalf("status %d, loopback requests %d -> %d, body %s", res.Status, before, lb.count(), res.Body)
		}
		got := lb.last().Header
		seen := map[string]bool{}
		for k, vs := range got {
			if !strings.HasPrefix(k, "X-Crosstalk-") {
				continue
			}
			w, ok := want[k]
			if !ok {
				t.Fatalf("loopback received %s: %q, a sender header that was not stripped", k, vs)
			}
			if len(vs) != 1 || vs[0] != w {
				t.Fatalf("loopback %s = %q, want exactly [%s]", k, vs, w)
			}
			seen[k] = true
		}
		if len(seen) != len(want) {
			t.Fatalf("loopback got %d gateway headers, want %d: %v", len(seen), len(want), got)
		}
		if a := got.Values("Authorization"); len(a) != 1 || a[0] != "Bearer "+tok {
			t.Fatalf("loopback Authorization %q, want exactly the gateway token", a)
		}
	}
	for _, f := range forged {
		f := f
		t.Run(f.key, func(t *testing.T) {
			h := remoteFields()
			h[f.key] = []string{f.val, f.val + "2"}
			check(t, h)
		})
	}
	t.Run("all at once", func(t *testing.T) {
		h := remoteFields()
		for _, f := range forged {
			h[f.key] = []string{f.val}
		}
		check(t, h)
	})
}

func TestGatewayRefusesOtherMethods(t *testing.T) {
	lb := newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: lb.srv.URL}))
	cl := e.senderClient(e.pin("desk1"))
	path := "/v1/a2a/" + targetAgent
	refused := map[string][]byte{
		"CancelTask":           rpcBody("1", "CancelTask", map[string]any{"id": "t"}),
		"ListTasks":            rpcBody("1", "ListTasks", map[string]any{}),
		"SendStreamingMessage": rpcBody("1", "SendStreamingMessage", sendParams("m", "x", nil)),
		"SubscribeToTask":      rpcBody("1", "SubscribeToTask", map[string]any{"id": "t"}),
		"GetExtendedAgentCard": rpcBody("1", "GetExtendedAgentCard", map[string]any{}),
		"unknown":              rpcBody("1", "NoSuchMethod", map[string]any{}),
		"lower case":           rpcBody("1", "sendmessage", sendParams("m", "x", nil)),
		"padded":               rpcBody("1", "SendMessage ", sendParams("m", "x", nil)),
		"empty":                rpcBody("1", "", map[string]any{}),
		"nested method":        rpcBody("1", "CancelTask", map[string]any{"method": "SendMessage", "id": "t"}),
		"duplicate method":     []byte(`{"jsonrpc":"2.0","id":"1","method":"SendMessage","method":"CancelTask","params":{"id":"t"}}`),
		"case-variant key":     []byte(`{"jsonrpc":"2.0","id":"1","method":"SendMessage","Method":"CancelTask","params":{"id":"t"}}`),
	}
	for name, body := range refused {
		body := body
		t.Run(name, func(t *testing.T) {
			res := e.post(cl, path, remoteFields(), body)
			if code, ok := res.errCode(); !ok || code != -32004 {
				t.Fatalf("status %d code %d ok %v body %s, want JSON-RPC UnsupportedOperation (-32004)", res.Status, code, ok, res.Body)
			}
		})
	}
	t.Run("batch", func(t *testing.T) {
		one := rpcBody("1", "SendMessage", sendParams("m", "x", nil))
		res := e.post(cl, path, remoteFields(), append(append([]byte("["), one...), ']'))
		if _, ok := res.errCode(); !ok || res.Status == 400 {
			t.Fatalf("batch: status %d body %s, want a JSON-RPC error that is not HTTP 400", res.Status, res.Body)
		}
	})
	if n := lb.count(); n != 0 {
		t.Fatalf("%d refused requests reached the loopback server, want 0", n)
	}
	sendBody := rpcBody("1", "SendMessage", sendParams("m1", "hello", nil))
	getBody := rpcBody("2", "GetTask", map[string]any{"id": "t"})
	for _, body := range [][]byte{sendBody, getBody} {
		before := lb.count()
		res := e.post(cl, path, remoteFields(), body)
		if res.Status != 200 || lb.count() != before+1 || !bytes.Equal(lb.last().Body, body) {
			t.Fatalf("allowed method: status %d, loopback %d -> %d, body forwarded verbatim: %v", res.Status, before, lb.count(), lb.count() > before && bytes.Equal(lb.last().Body, body))
		}
	}
}

func TestGatewayResolvesTargetLocally(t *testing.T) {
	a, b, c := newLoopback(t), newLoopback(t), newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: a.srv.URL, otherAgent: c.srv.URL}))
	cl := e.senderClient(e.pin("desk1"))
	evil := map[string]any{"url": b.srv.URL, "host": strings.TrimPrefix(b.srv.URL, "http://"), "agent_id": otherAgent, "a2a_url": b.srv.URL}
	body := rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "x", evil))
	hdr := remoteFields()
	hdr.Set("X-Forwarded-Host", strings.TrimPrefix(b.srv.URL, "http://"))
	hdr.Set("X-CrossTalk-Target", b.srv.URL)
	hdr.Set("Location", b.srv.URL)
	res := e.post(cl, "/v1/a2a/"+targetAgent+"?url="+b.srv.URL+"&agent="+otherAgent, hdr, body)
	if res.Status != 200 || a.count() != 1 || a.last().Path != "/" || a.last().Query != "" {
		t.Fatalf("status %d, registered target got %d requests (path %q query %q)", res.Status, a.count(), a.last().Path, a.last().Query)
	}
	for _, p := range []string{
		"/v1/a2a/../a2a/" + targetAgent,
		"/v1/a2a/%2e%2e/a2a/" + targetAgent,
		"/v1/a2a/" + targetAgent + "/../" + otherAgent,
		"/v1/a2a/" + targetAgent + "%2f..%2f" + otherAgent,
		"/v1/a2a/http:%2F%2F" + strings.TrimPrefix(b.srv.URL, "http://"),
		"/v1/a2a//" + targetAgent,
		"/v1/a2a/" + strings.ToUpper(targetAgent),
	} {
		before := a.count()
		e.post(cl, p, remoteFields(), body)
		if a.count() != before || b.count() != 0 || c.count() != 0 {
			t.Fatalf("path %q reached a loopback server: target %d->%d, other %d, third %d", p, before, a.count(), b.count(), c.count())
		}
	}
	if b.count() != 0 || c.count() != 0 {
		t.Fatalf("a listener the path did not name received requests: %d and %d, want 0", b.count(), c.count())
	}
}

func TestGatewayUnknownAgent(t *testing.T) {
	lb := newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: lb.srv.URL}))
	cl := e.senderClient(e.pin("desk1"))
	body := rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "x", nil))
	for name, id := range map[string]string{
		"unregistered id":      "d0d0d0d0d0d0d0d0",
		"native session id":    "5e5e5e5e5e5e5e5e",
		"not an agent_id":      "bob",
		"a native session uid": "0f0f0f0f-0f0f-0f0f-0f0f-0f0f0f0f0f0f",
	} {
		res := e.post(cl, "/v1/a2a/"+id, remoteFields(), body)
		if code, ok := res.errCode(); !ok || res.Status != 200 || !res.hasReason("no_such_agent") {
			t.Fatalf("%s: status %d code %d ok %v body %s, want rejected/no_such_agent as a JSON-RPC error", name, res.Status, code, ok, res.Body)
		}
	}
	if n := lb.count(); n != 0 {
		t.Fatalf("%d requests reached the loopback server, want 0", n)
	}
}

func TestGatewayMalformedRemoteFieldsRefused(t *testing.T) {
	lb := newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: lb.srv.URL}))
	cl := e.senderClient(e.pin("desk1"))
	body := rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "x", nil))
	with := func(k string, v ...string) http.Header {
		h := remoteFields()
		if v == nil {
			h.Del(k)
		} else {
			h[k] = v
		}
		return h
	}
	cases := map[string]http.Header{
		"agent_id missing":       with(RemoteAgentIDHeader),
		"agent_id empty":         with(RemoteAgentIDHeader, ""),
		"agent_id uppercase":     with(RemoteAgentIDHeader, strings.ToUpper(senderAgent)),
		"agent_id short":         with(RemoteAgentIDHeader, "e1e1"),
		"agent_id path":          with(RemoteAgentIDHeader, "../../etc/passwd"),
		"agent_id twice":         with(RemoteAgentIDHeader, senderAgent, senderAgent),
		"name missing":           with(RemoteNameHeader),
		"name empty":             with(RemoteNameHeader, ""),
		"name not base64url":     with(RemoteNameHeader, "Remote Alice!"),
		"name padded base64":     with(RemoteNameHeader, base64.URLEncoding.EncodeToString([]byte("a"))),
		"name 257 bytes":         with(RemoteNameHeader, b64(strings.Repeat("a", 257))),
		"name control character": with(RemoteNameHeader, b64("a\nb")),
		"name invalid utf8":      with(RemoteNameHeader, base64.RawURLEncoding.EncodeToString([]byte{0xff, 0xfe})),
		"name twice":             with(RemoteNameHeader, b64("a"), b64("b")),
		"harness missing":        with(RemoteHarnessHeader),
		"harness unknown":        with(RemoteHarnessHeader, "gemini"),
		"harness wrong case":     with(RemoteHarnessHeader, "Codex"),
		"harness twice":          with(RemoteHarnessHeader, "codex", "claude"),
		"role missing":           with(RemoteRoleHeader),
		"role unknown":           with(RemoteRoleHeader, "admin"),
		"role twice":             with(RemoteRoleHeader, "primary", "subagent"),
	}
	for name, h := range cases {
		h := h
		t.Run(name, func(t *testing.T) {
			res := e.post(cl, "/v1/a2a/"+targetAgent, h, body)
			if _, ok := res.errCode(); !ok || res.Status == http.StatusBadRequest || !res.hasReason(a2a.ReasonRemoteSender) {
				t.Fatalf("status %d body %s, want a JSON-RPC rejection %q and not HTTP 400", res.Status, res.Body, a2a.ReasonRemoteSender)
			}
		})
	}
	if n := lb.count(); n != 0 {
		t.Fatalf("%d malformed requests reached the loopback server, want 0", n)
	}
}

func TestGatewayRefusesClaimMetadata(t *testing.T) {
	lb := newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: lb.srv.URL}))
	cl := e.senderClient(e.pin("desk1"))
	for name, claim := range map[string]any{
		"claim":         map[string]any{"owner": "alice", "claimed": true},
		"release":       map[string]any{"owner": "alice", "claimed": false},
		"empty object":  map[string]any{},
		"null":          nil,
		"not an object": "alice",
	} {
		body := rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "x", map[string]any{"kind": "inform", "claim": claim}))
		res := e.post(cl, "/v1/a2a/"+targetAgent, remoteFields(), body)
		if _, ok := res.errCode(); !ok || res.Status != 200 || !res.hasReason(receiver.ReasonRemoteKind) {
			t.Fatalf("%s: status %d body %s, want rejected/%s", name, res.Status, res.Body, receiver.ReasonRemoteKind)
		}
	}
	if n := lb.count(); n != 0 {
		t.Fatalf("%d claim requests reached the loopback server, want 0", n)
	}
	plain := rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "claim", map[string]any{"kind": "inform"}))
	if res := e.post(cl, "/v1/a2a/"+targetAgent, remoteFields(), plain); res.Status != 200 || lb.count() != 1 {
		t.Fatalf("a message without claim metadata: status %d, loopback %d, want forwarded once", res.Status, lb.count())
	}
}

func TestGatewayForwardRequiresPinnedPeer(t *testing.T) {
	lb := newLoopback(t)
	e := forwardGateway(t, agentsOf(map[string]string{targetAgent: lb.srv.URL}))
	body := rpcBody("1", "SendMessage", sendParams(msg.NewUUIDv7(), "x", nil))

	pinned := e.pin("desk1")
	cl := e.senderClient(pinned)
	if res := e.post(cl, "/v1/a2a/"+targetAgent, remoteFields(), body); res.Status != 200 || lb.count() != 1 {
		t.Fatalf("pinned peer: status %d, loopback %d, want forwarded once", res.Status, lb.count())
	}

	stranger, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	writePending(t, e.stateDir, time.Now().Add(10*time.Minute))
	sc := e.senderClient(stranger)
	if res := e.post(sc, "/v1/a2a/"+targetAgent, remoteFields(), body); res.Status != http.StatusForbidden || lb.count() != 1 {
		t.Fatalf("unenrolled connection: status %d, loopback %d, want 403 and no new request", res.Status, lb.count())
	}

	if err := e.peers.Remove("desk1"); err != nil {
		t.Fatal(err)
	}
	if res := e.post(cl, "/v1/a2a/"+targetAgent, remoteFields(), body); res.Status != http.StatusForbidden || lb.count() != 1 {
		t.Fatalf("unpaired peer on its open connection: status %d, loopback %d, want 403 and no new request", res.Status, lb.count())
	}
}
