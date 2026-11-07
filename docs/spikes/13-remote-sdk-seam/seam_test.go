package sdkseam

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	selfID     = "b0b0b0b0b0b0b0b0"
	localAlice = "a1a1a1a1a1a1a1a1"
	remoteBob  = "e5e5e5e5e5e5e5e5"
	token      = "gateway-token-for-the-spike"
)

type rig struct {
	t        *testing.T
	a, b, c  Host
	gw       *Gateway
	be       *Backend
	fake     *adapter.Fake
	j        *journal.Journal
	rcv      *receiver.Receiver
	client   *http.Client
	endpoint string
}

func newRig(t *testing.T, shape SenderShape) *rig {
	t.Helper()
	r := &rig{t: t, fake: adapter.NewFake(adapter.Claude)}
	var err error
	for _, h := range []*Host{&r.a, &r.b, &r.c} {
		if *h, err = NewHost(); err != nil {
			t.Fatal(err)
		}
	}
	bind := adapter.Binding{Harness: adapter.Claude, SessionRef: "sess-b", HarnessEndpoint: "/tmp/cc-socks/1.sock", HarnessPID: 1}
	j, _, err := journal.Open(t.TempDir(), selfID, journal.Binding{InstanceID: "inst-1", HarnessEndpoint: bind.HarnessEndpoint, SessionRef: bind.SessionRef}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	r.j = j
	r.rcv = receiver.New(receiver.Config{Journal: j, Adapter: r.fake, Self: func() receiver.Recipient {
		return receiver.Recipient{AgentID: selfID, Name: "bob", Role: registry.RolePrimary, Binding: bind}
	}})
	if r.be, err = NewBackend(r.rcv, token, shape); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.be.Close)
	if r.gw, err = NewGateway(r.b, r.be.URL, token, map[string]string{r.a.FP: "host-a"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.gw.Close)
	r.client = r.clientFor(r.a, 0)
	r.endpoint = "https://" + r.gw.Addr + "/v1/a2a/" + selfID
	return r
}

func (r *rig) clientFor(h Host, maxVersion uint16) *http.Client {
	wantServer := r.b.FP
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{h.Cert}, MinVersion: tls.VersionTLS12, MaxVersion: maxVersion, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if fingerprint(&cs) != wantServer {
				return errors.New("server not pinned")
			}
			return nil
		},
	}}}
}

func good(agentID string) a2aclient.ServiceParams {
	return a2aclient.ServiceParams{
		HdrAgentID: {agentID}, HdrName: {EncodeName("alice")}, HdrHarness: {"claude"}, HdrRole: {"primary"},
	}
}

type outcome struct {
	state, reason string
	duplicate     bool
}

func send(ctx context.Context, hc *http.Client, endpoint string, params a2aclient.ServiceParams, kind, id string) (outcome, error) {
	cl, err := a2aclient.NewFromEndpoints(ctx, []*sdk.AgentInterface{sdk.NewAgentInterface(endpoint, sdk.TransportProtocolJSONRPC)},
		a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		return outcome{}, err
	}
	defer cl.Destroy()
	m := sdk.NewMessage(sdk.MessageRoleUser, sdk.NewTextPart("hello from the other host"))
	m.ID = id
	if kind != "" {
		m.Metadata = map[string]any{"crosstalk": map[string]any{"kind": kind}}
	}
	res, err := cl.SendMessage(a2aclient.AttachServiceParams(ctx, params), &sdk.SendMessageRequest{Message: m, Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		return outcome{}, err
	}
	t, ok := res.(*sdk.Task)
	if !ok {
		return outcome{}, errors.New("not a task")
	}
	ct, _ := t.Metadata["crosstalk"].(map[string]any)
	var o outcome
	o.state, _ = ct["state"].(string)
	o.reason, _ = ct["reason"].(string)
	o.duplicate, _ = ct["duplicate"].(bool)
	return o, nil
}

func (r *rig) mustSend(params a2aclient.ServiceParams, kind, id string) outcome {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o, err := send(ctx, r.client, r.endpoint, params, kind, id)
	if err != nil {
		r.t.Fatalf("send: %v", err)
	}
	return o
}

func (r *rig) lastSeen() Remote {
	r.t.Helper()
	s := r.be.Seen()
	if len(s) == 0 {
		r.t.Fatal("backend saw no request")
	}
	return s[len(s)-1]
}

func (r *rig) local(s receiver.Sender, kind msg.Kind, id string) receiver.Result {
	m := msg.Message{MessageID: id, ContextID: "ctx", From: "x", To: "bob", Kind: kind, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: "local"}}}
	return r.rcv.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: s})
}

var localPrimary = receiver.Sender{Verified: true, AgentID: localAlice, Name: "alice", Role: registry.RolePrimary}

func TestC0_UnpinnedClientRefusedBeforeHTTP(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := send(ctx, r.clientFor(r.c, 0), r.endpoint, good(remoteBob), "", msg.NewUUIDv7()); err == nil {
		t.Fatal("unpinned client was served")
	}
	if r.gw.Hits.Load() != 0 || r.be.Hits.Load() != 0 || len(r.fake.Writes()) != 0 {
		t.Fatalf("gateway hits %d, backend hits %d, writes %d", r.gw.Hits.Load(), r.be.Hits.Load(), len(r.fake.Writes()))
	}
}

func TestC0_TLS12Refused(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := send(ctx, r.clientFor(r.a, tls.VersionTLS12), r.endpoint, good(remoteBob), "", msg.NewUUIDv7())
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("want a protocol version failure, got %v", err)
	}
	if r.be.Hits.Load() != 0 {
		t.Fatal("backend reached")
	}
}

func TestC0_BackendRequiresGatewayToken(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	plain := &http.Client{}
	host := "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"[:64]
	full := good(remoteBob)
	full[HdrHost] = []string{host}
	full[HdrAlias] = []string{"host-a"}
	cases := map[string]a2aclient.ServiceParams{
		"no bearer":             full,
		"wrong bearer":          merge(full, a2aclient.ServiceParams{"Authorization": {"Bearer nope"}}),
		"right bearer no hdrs":  {"Authorization": {"Bearer " + token}},
		"right bearer bad host": merge(good(remoteBob), a2aclient.ServiceParams{"Authorization": {"Bearer " + token}, HdrHost: {"short"}}),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := send(ctx, plain, r.be.URL, p, "", msg.NewUUIDv7()); err == nil {
				t.Fatal("loopback accepted it")
			}
		})
	}
	if r.be.Calls.Load() != 0 || len(r.fake.Writes()) != 0 {
		t.Fatalf("calls %d writes %d", r.be.Calls.Load(), len(r.fake.Writes()))
	}
}

func merge(a, b a2aclient.ServiceParams) a2aclient.ServiceParams {
	o := a2aclient.ServiceParams{}
	for k, v := range a {
		o[k] = v
	}
	for k, v := range b {
		o[k] = v
	}
	return o
}

func TestC0_PinnedMessageDelivered(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	o := r.mustSend(good(remoteBob), "", msg.NewUUIDv7())
	if o.state != "accepted" || len(r.fake.Writes()) != 1 {
		t.Fatalf("%+v writes %d", o, len(r.fake.Writes()))
	}
}

func TestC1_SenderHostIsConnectionFingerprint(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	id := msg.NewUUIDv7()
	o := r.mustSend(good(remoteBob), "", id)
	if o.state != "accepted" {
		t.Fatalf("%+v", o)
	}
	got := r.lastSeen()
	if got.Host != r.a.FP || got.Alias != "host-a" || got.AgentID != remoteBob || got.Name != "alice" {
		t.Fatalf("%+v want host %s", got, r.a.FP)
	}
	if st, ok := r.j.Dedupe("remote:"+r.a.FP+"/"+remoteBob, id); !ok || st != "accepted" {
		t.Fatalf("dedupe under host-scoped key: %q %v", st, ok)
	}
	w := r.fake.Writes()
	if len(w) != 1 || w[0].Message.From != "remote:"+r.a.FP+"/"+remoteBob {
		t.Fatalf("writes %+v", w)
	}
}

func TestC2_SpoofedHeadersIgnored(t *testing.T) {
	third := strings.Repeat("ab", 32)
	cases := map[string]a2aclient.ServiceParams{
		"remote-host":            {HdrHost: {third}},
		"remote-host lower":      {strings.ToLower(HdrHost): {third}},
		"remote-host mixed case": {"x-CROSSTALK-remote-HOST": {third}},
		"two remote-host":        {HdrHost: {third, strings.Repeat("cd", 32)}},
		"remote-alias":           {HdrAlias: {"host-evil"}},
		"authorization":          {"Authorization": {"Bearer " + token}},
		"local agent header":     {"X-CrossTalk-Agent": {"alice"}},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, NamespacedPrincipal)
			o := r.mustSend(merge(good(remoteBob), extra), "", msg.NewUUIDv7())
			if o.state != "accepted" {
				t.Fatalf("%+v", o)
			}
			if g := r.lastSeen(); g.Host != r.a.FP || g.Alias != "host-a" {
				t.Fatalf("host %q alias %q, want %q host-a", g.Host, g.Alias, r.a.FP)
			}
		})
	}
}

func TestC3_MalformedAgentFieldsRefusedBeforeChain(t *testing.T) {
	mut := func(f func(p a2aclient.ServiceParams)) a2aclient.ServiceParams {
		p := good(remoteBob)
		f(p)
		return p
	}
	cases := map[string]a2aclient.ServiceParams{
		"agent_id uppercase":  mut(func(p a2aclient.ServiceParams) { p[HdrAgentID] = []string{"E5E5E5E5E5E5E5E5"} }),
		"agent_id 15 digits":  mut(func(p a2aclient.ServiceParams) { p[HdrAgentID] = []string{remoteBob[:15]} }),
		"agent_id 17 digits":  mut(func(p a2aclient.ServiceParams) { p[HdrAgentID] = []string{remoteBob + "e"} }),
		"agent_id non-hex":    mut(func(p a2aclient.ServiceParams) { p[HdrAgentID] = []string{"g5e5e5e5e5e5e5e5"} }),
		"agent_id absent":     mut(func(p a2aclient.ServiceParams) { delete(p, HdrAgentID) }),
		"agent_id twice":      mut(func(p a2aclient.ServiceParams) { p[HdrAgentID] = []string{remoteBob, localAlice} }),
		"name not base64url":  mut(func(p a2aclient.ServiceParams) { p[HdrName] = []string{"!!!"} }),
		"name over 256 bytes": mut(func(p a2aclient.ServiceParams) { p[HdrName] = []string{EncodeName(strings.Repeat("a", 257))} }),
		"name control char":   mut(func(p a2aclient.ServiceParams) { p[HdrName] = []string{EncodeName("a\x01b")} }),
		"name empty":          mut(func(p a2aclient.ServiceParams) { p[HdrName] = []string{""} }),
		"harness gemini":      mut(func(p a2aclient.ServiceParams) { p[HdrHarness] = []string{"gemini"} }),
		"harness absent":      mut(func(p a2aclient.ServiceParams) { delete(p, HdrHarness) }),
		"role admin":          mut(func(p a2aclient.ServiceParams) { p[HdrRole] = []string{"admin"} }),
		"role absent":         mut(func(p a2aclient.ServiceParams) { delete(p, HdrRole) }),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, NamespacedPrincipal)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := send(ctx, r.client, r.endpoint, p, "", msg.NewUUIDv7())
			if err == nil {
				t.Fatal("malformed request was accepted")
			}
			t.Logf("client error: %v", err)
			if r.gw.Hits.Load() != 0 || r.be.Hits.Load() != 0 || r.be.Calls.Load() != 0 || len(r.fake.Calls()) != 0 {
				t.Fatalf("reached past the gateway: gw %d backend %d calls %d adapter %d", r.gw.Hits.Load(), r.be.Hits.Load(), r.be.Calls.Load(), len(r.fake.Calls()))
			}
		})
	}
}

func TestC4_AttestedAgentIDStaysInHostNamespace(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	x, y := msg.NewUUIDv7(), msg.NewUUIDv7()
	if res := r.local(localPrimary, msg.Inform, x); res.State != adapter.Accepted {
		t.Fatalf("local %+v", res)
	}
	if o := r.mustSend(good(localAlice), "", y); o.state != "accepted" || len(r.fake.Writes()) != 2 {
		t.Fatalf("%+v writes %d", o, len(r.fake.Writes()))
	}
	if _, ok := r.j.Dedupe("remote:"+r.a.FP+"/"+localAlice, y); !ok {
		t.Fatal("no record under the host-scoped key")
	}
	if _, ok := r.j.Dedupe(localAlice, y); ok {
		t.Fatal("a remote message wrote a record under the bare local agent id")
	}
	if st, ok := r.j.Dedupe(localAlice, x); !ok || st != "accepted" {
		t.Fatalf("local alice's own record changed: %q %v", st, ok)
	}
}

func TestC4_SameMessageIDAsLocalIsRefusedNotInjected(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	x := msg.NewUUIDv7()
	if res := r.local(localPrimary, msg.Inform, x); res.State != adapter.Accepted {
		t.Fatalf("local %+v", res)
	}
	o := r.mustSend(good(localAlice), "", x)
	if o.state != "rejected" || o.reason != receiver.ReasonDuplicateDispatch || len(r.fake.Writes()) != 1 {
		t.Fatalf("finding: the dispatch claim is keyed by message_id alone: %+v writes %d", o, len(r.fake.Writes()))
	}
	if st, ok := r.j.Dedupe(localAlice, x); !ok || st != "accepted" {
		t.Fatalf("local alice's record changed: %q %v", st, ok)
	}
}

func TestC4_PlanShapedSenderNoLongerCollides(t *testing.T) {
	r := newRig(t, PlanShaped)
	x := msg.NewUUIDv7()
	if res := r.local(localPrimary, msg.Inform, x); res.State != adapter.Accepted {
		t.Fatalf("local %+v", res)
	}
	o := r.mustSend(good(localAlice), "", x)
	if o.duplicate || o.reason == receiver.ReasonDuplicateDispatch || len(r.fake.Writes()) != 2 {
		t.Fatalf("the remote message is a different sender's message and must be injected: %+v writes %d", o, len(r.fake.Writes()))
	}
}

func TestC5_RemotePrimaryCannotCommand(t *testing.T) {
	r := newRig(t, NamespacedPrincipal)
	p := good(remoteBob)
	if p[HdrRole][0] != "primary" {
		t.Fatal("test setup: role must be primary")
	}
	o := r.mustSend(p, "command", msg.NewUUIDv7())
	if o.state != "rejected" || o.reason != receiver.ReasonPolicy || len(r.fake.Writes()) != 0 {
		t.Fatalf("%+v writes %d", o, len(r.fake.Writes()))
	}
	if res := r.local(localPrimary, msg.Command, msg.NewUUIDv7()); res.State != adapter.Accepted || len(r.fake.Writes()) != 1 {
		t.Fatalf("control: a local registered primary must be able to command: %+v", res)
	}
}

func TestC5_PlanShapedSenderCannotCommandNow(t *testing.T) {
	r := newRig(t, PlanShaped)
	o := r.mustSend(good(remoteBob), "command", msg.NewUUIDv7())
	if o.state != "rejected" || o.reason != receiver.ReasonRemoteKind || len(r.fake.Writes()) != 0 {
		t.Fatalf("a plan-shaped remote primary must be refused remote_kind: %+v writes %d", o, len(r.fake.Writes()))
	}
}
