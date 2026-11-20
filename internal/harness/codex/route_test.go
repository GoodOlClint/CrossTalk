package codex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// routeThreads is an app-server stand-in: Inject is a scripted adapter.Fake
// and the thread is loaded on as many endpoints as hosts says.
type routeThreads struct {
	*adapter.Fake
	mu    sync.Mutex
	hosts int
}

func (r *routeThreads) setHosts(n int) { r.mu.Lock(); r.hosts = n; r.mu.Unlock() }

func (r *routeThreads) LoadedThreads(context.Context, ...string) []LoadedThread {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LoadedThread, r.hosts)
	for i := range out {
		out[i] = LoadedThread{ID: wThread, Endpoint: fmt.Sprint("ep", i)}
	}
	return out
}

type routeEnv struct {
	*wakeEnv
	t       *testing.T
	sys     *fakeSys
	claims  *Claims
	threads *routeThreads
	h       *Harness
	state   string
	dir     string
	j       *journal.Journal
	r       *receiver.Receiver
}

var (
	verifiedPeer   = receiver.Sender{Verified: true, AgentID: "a1a1a1a1a1a1a1a1", Name: "alice", Role: registry.RolePrimary}
	unverifiedPeer = receiver.Sender{PID: 31337}
)

// newRouteEnv is a shim whose parent is a --no-daemon Codex process, with
// the setting on, an owner-proven claim for wThread when claimed, and a
// receiver over a fresh journal for the thread's agent.
func newRouteEnv(t *testing.T, claimed bool) *routeEnv {
	t.Helper()
	e := &routeEnv{wakeEnv: newWakeEnv(t), t: t, sys: newFakeSys(), threads: &routeThreads{Fake: adapter.NewFake(adapter.Codex)}, state: t.TempDir(), dir: t.TempDir()}
	e.sys.own(wThread)
	e.claims = newTestClaims(t, e.sys, &fakeRegistrar{})
	if claimed {
		if err := e.claims.AcceptDurable(context.Background(), claim(wThread)); err != nil {
			t.Fatal(err)
		}
	}
	e.h = NewHarness(HarnessOptions{Adapter: e.threads})
	e.setting(true)
	rt := newWakeRoute(e.state, e.claims, func() *QueueGate {
		g := testGate(e.queue)
		if e.gate != nil {
			e.gate(g)
		}
		return g
	})
	rt.wakes.Now, rt.wakes.Bound = e.clk.now, time.Minute
	e.h.setWakeRoute(rt)
	e.open(e.h)
	return e
}

func (e *routeEnv) setting(on bool) {
	p := filepath.Join(e.state, SettingFile)
	if on {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	os.Remove(p)
}

func (e *routeEnv) open(ad adapter.Adapter) {
	if e.j != nil {
		e.j.Close()
	}
	j, _, err := journal.Open(e.dir, "c0c0c0c0c0c0c0c0", journal.Binding{InstanceID: "i1", SessionRef: wThread}, journal.Options{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { j.Close() })
	e.j = j
	self := receiver.Recipient{AgentID: "c0c0c0c0c0c0c0c0", Name: "codex-a", Role: registry.RolePrimary, Binding: adapter.Binding{Harness: adapter.Codex, SessionRef: wThread}}
	e.r = receiver.New(receiver.Config{Journal: j, Adapter: ad, Self: func() receiver.Recipient { return self }})
}

func routeMsg(kind msg.Kind, mode msg.Delivery) msg.Message {
	return msg.Message{MessageID: msg.NewUUIDv7(), ContextID: "ctx", To: "codex-a", Kind: kind, Delivery: mode, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: "hello"}}}
}

func (e *routeEnv) send(s receiver.Sender, m msg.Message) receiver.Result {
	ing := receiver.A2A
	if !s.Verified {
		ing = receiver.UDS
	}
	return e.r.Receive(context.Background(), receiver.Inbound{Ingress: ing, Message: m, Sender: s})
}

func (e *routeEnv) pulled() map[string]string {
	e.t.Helper()
	es, _, err := e.r.Pull("", 100)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, en := range es {
		out[en.MessageID] = en.State
	}
	return out
}

func (e *routeEnv) appServerCalls() int { return len(e.threads.Writes()) }

func (e *routeEnv) wantRoute(m msg.Message, d adapter.Delivery, want Route) {
	e.t.Helper()
	if got, err := e.h.selectRoute(context.Background(), m, d); got != want {
		e.t.Fatalf("route %d (%v), want %d", got, err, want)
	}
}

func (e *routeEnv) wantRows(n int) {
	e.t.Helper()
	if _, err := os.Stat(e.queue); errors.Is(err, os.ErrNotExist) && n == 0 {
		return
	}
	if got := len(e.wakeRows()); got != n {
		e.t.Fatalf("wake rows %d, want %d", got, n)
	}
}

func TestRoute_Row1_PriorUnknownWithQueueRouteNowPresent_ExpectUnknownNoResendNoRow(t *testing.T) {
	e := newRouteEnv(t, false)
	e.threads.setHosts(1)
	e.threads.ScriptInject(adapter.Outcome{State: adapter.Unknown, Err: errors.New("ambiguous")})
	m := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m); res.State != adapter.Unknown {
		t.Fatalf("first attempt %+v", res)
	}
	e.threads.setHosts(0)
	if err := e.claims.AcceptDurable(context.Background(), claim(wThread)); err != nil {
		t.Fatal(err)
	}
	if res := e.send(verifiedPeer, m); res.State != adapter.Unknown || !res.Duplicate {
		t.Fatalf("resend %+v, want unknown duplicate", res)
	}
	e.threads.setHosts(1)
	if res := e.send(verifiedPeer, m); res.State != adapter.Unknown || !res.Duplicate {
		t.Fatalf("resend with a host %+v, want unknown duplicate", res)
	}
	if n := e.appServerCalls(); n != 1 {
		t.Fatalf("app-server injects %d, want the first attempt only", n)
	}
	e.wantRows(0)
	e.threads.setHosts(0)
	if res := e.send(verifiedPeer, routeMsg(msg.Inform, msg.Immediate)); res.State != adapter.Mailboxed {
		t.Fatalf("a new message %+v, want the queue route live", res)
	}
}

func TestRoute_Row2_OneHostNoClaim_ExpectAppServerUnchanged(t *testing.T) {
	e := newRouteEnv(t, false)
	e.threads.setHosts(1)
	e.threads.SetDefaultInject(adapter.Outcome{State: adapter.Accepted})
	m := routeMsg(msg.Inform, msg.Immediate)
	e.wantRoute(m, delivery(msg.Immediate, true), RouteAppServer)
	if res := e.send(verifiedPeer, m); res.State != adapter.Accepted {
		t.Fatalf("%+v", res)
	}
	if n := e.appServerCalls(); n != 1 {
		t.Fatalf("app-server injects %d", n)
	}
	e.wantRows(0)
}

func TestRoute_Row3_NoHostCapabilityAbsent_ExpectUnreachableAskNoEndpointNoRow(t *testing.T) {
	cases := []struct {
		name    string
		claimed bool
		break_  func(e *routeEnv)
	}{
		{"no owner-proven claim", false, func(*routeEnv) {}},
		{"owner proof fails at write time", true, func(e *routeEnv) { e.sys.set(wThread, false, nil) }},
		{"gate fails", true, func(e *routeEnv) {
			e.gate = func(g *QueueGate) {
				g.Version = func(context.Context, ownerproof.Proc) (string, error) { return "0.168.0", nil }
			}
		}},
		{"queue file missing", true, func(e *routeEnv) { os.Remove(e.queue) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRouteEnv(t, c.claimed)
			c.break_(e)
			for _, k := range []struct {
				s    receiver.Sender
				mode msg.Delivery
			}{{verifiedPeer, msg.Immediate}, {verifiedPeer, msg.NextTurn}, {unverifiedPeer, msg.Immediate}} {
				m := routeMsg(msg.Inform, k.mode)
				d := delivery(k.mode, k.s.Verified)
				e.wantRoute(m, d, RouteNone)
				st, err := e.h.Inject(context.Background(), m, d)
				if st != adapter.Unreachable || !errors.Is(err, adapter.ErrNoEndpoint) {
					t.Fatalf("%v verified=%v: %s %v", k.mode, k.s.Verified, st, err)
				}
				if res := e.send(k.s, m); res.State != adapter.Unreachable {
					t.Fatalf("%v verified=%v: %+v", k.mode, k.s.Verified, res)
				}
			}
			if res := e.send(verifiedPeer, routeMsg(msg.Ask, msg.Immediate)); res.State != adapter.Rejected || res.Reason != receiver.ReasonNoEndpoint {
				t.Fatalf("ask %+v", res)
			}
			for id, st := range e.pulled() {
				if st == string(adapter.Mailboxed) || st == journal.StateDelivered {
					t.Fatalf("entry %s %s: a message without capability was mailboxed", id, st)
				}
			}
			e.wantRows(0)
			if n := e.appServerCalls(); n != 0 {
				t.Fatalf("app-server injects %d", n)
			}
			if c.name == "queue file missing" {
				if _, err := os.Stat(e.queue); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("queue file created: %v", err)
				}
			}
		})
	}
}

func TestRoute_Row4_NextTurnOrUnverifiedWithCapability_ExpectMailboxedNoWake(t *testing.T) {
	e := newRouteEnv(t, true)
	nt := routeMsg(msg.Inform, msg.NextTurn)
	e.wantRoute(nt, delivery(msg.NextTurn, true), RouteMailbox)
	uv := routeMsg(msg.Inform, msg.Immediate)
	e.wantRoute(uv, delivery(msg.Immediate, false), RouteMailbox)
	for _, c := range []struct {
		s receiver.Sender
		m msg.Message
	}{{verifiedPeer, nt}, {unverifiedPeer, uv}} {
		if res := e.send(c.s, c.m); res.State != adapter.Mailboxed {
			t.Fatalf("%+v", res)
		}
	}
	e.wantRows(0)
	got := e.pulled()
	if got[nt.MessageID] != journal.StateDelivered || got[uv.MessageID] != journal.StateDelivered {
		t.Fatalf("pulled %v", got)
	}
	if n := e.appServerCalls(); n != 0 {
		t.Fatalf("app-server injects %d", n)
	}
}

func TestRoute_Row5_ImmediateVerifiedWithCapability_ExpectMailboxedAndOneOpenWake(t *testing.T) {
	e := newRouteEnv(t, true)
	m1, m2 := routeMsg(msg.Inform, msg.Immediate), routeMsg(msg.Inform, msg.Immediate)
	e.wantRoute(m1, delivery(msg.Immediate, true), RouteWake)
	for _, m := range []msg.Message{m1, m2} {
		if res := e.send(verifiedPeer, m); res.State != adapter.Mailboxed {
			t.Fatalf("%+v", res)
		}
	}
	e.wantRows(1)
	wantState(t, e.h.wakeRoute().wakes, 1, GenQueued)
	got := e.pulled()
	if got[m1.MessageID] != journal.StateDelivered || got[m2.MessageID] != journal.StateDelivered {
		t.Fatalf("pulled %v", got)
	}
}

func TestRoute_Row6_AmbiguousHostsOrOwnership_ExpectUnreachableAskNoEndpointNoWake(t *testing.T) {
	cases := []struct {
		name    string
		claimed bool
		hosts   int
	}{
		{"claim and one host", true, 1},
		{"claim and two hosts", true, 2},
		{"two hosts, no claim", false, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRouteEnv(t, c.claimed)
			e.threads.setHosts(c.hosts)
			m := routeMsg(msg.Inform, msg.Immediate)
			e.wantRoute(m, delivery(msg.Immediate, true), RouteAmbiguous)
			if res := e.send(verifiedPeer, m); res.State != adapter.Unreachable {
				t.Fatalf("%+v", res)
			}
			if res := e.send(verifiedPeer, routeMsg(msg.Ask, msg.Immediate)); res.State != adapter.Rejected || res.Reason != receiver.ReasonNoEndpoint {
				t.Fatalf("ask %+v", res)
			}
			e.wantRows(0)
			if n := e.appServerCalls(); n != 0 {
				t.Fatalf("app-server injects %d", n)
			}
		})
	}
}

func TestRoute_Row6_ClaimAndHostProbeFails_ExpectUnreachableFailClosed(t *testing.T) {
	old := EndpointProbeTimeout
	EndpointProbeTimeout = 200 * time.Millisecond
	defer func() { EndpointProbeTimeout = old }()
	sock := filepath.Join(shortSockDir(t), "silent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	e := newRouteEnv(t, true)
	e.h.Threads = &Adapter{ControlSocket: sock}
	m := routeMsg(msg.Inform, msg.Immediate)
	e.wantRoute(m, delivery(msg.Immediate, true), RouteAmbiguous)
	if res := e.send(verifiedPeer, m); res.State != adapter.Unreachable {
		t.Fatalf("%+v", res)
	}
	e.wantRows(0)
}

// TestRoute_NoDeliveryMatchesTwoRows checks the selector against the plan's
// row conditions, written out separately, over every combination of facts.
func TestRoute_NoDeliveryMatchesTwoRows(t *testing.T) {
	probeErr := errors.New("probe")
	for hosts := range 3 {
		for _, hostErr := range []error{nil, probeErr} {
			for _, claimed := range []bool{false, true} {
				if hostErr != nil && !claimed {
					continue
				}
				for _, capOK := range []bool{false, true} {
					for _, ask := range []bool{false, true} {
						for _, wake := range []bool{false, true} {
							unambiguous := !(claimed && (hosts > 0 || hostErr != nil))
							present := claimed && capOK && !ask
							noHost := hosts == 0 && hostErr == nil
							rows := map[Route]bool{
								RouteAppServer: hosts == 1 && !claimed,
								RouteNone:      noHost && unambiguous && !present,
								RouteMailbox:   noHost && unambiguous && present && !wake,
								RouteWake:      noHost && unambiguous && present && wake,
								RouteAmbiguous: hosts >= 2 || !unambiguous,
							}
							var match []Route
							for r, ok := range rows {
								if ok {
									match = append(match, r)
								}
							}
							name := fmt.Sprintf("hosts=%d err=%v claimed=%v cap=%v ask=%v wake=%v", hosts, hostErr != nil, claimed, capOK, ask, wake)
							if len(match) != 1 {
								t.Fatalf("%s: rows %v", name, match)
							}
							capability := func() error {
								if capOK {
									return nil
								}
								return ErrQueueGate
							}
							got, _ := pickRoute(routeFacts{hosts: hosts, hostErr: hostErr, claimed: claimed, capability: capability, ask: ask, wake: wake})
							if got != match[0] {
								t.Fatalf("%s: selector %d, plan row %d", name, got, match[0])
							}
						}
					}
				}
			}
		}
	}
}

// TestRoute_SettingOff_ExpectExactlyThePreStepAdapterOutputs runs each
// delivery through the Harness with the setting off and through the bare
// Adapter (the pre-step Inject), against a real app-server rig, and then
// shows the setting is the only switch.
func TestRoute_SettingOff_ExpectExactlyThePreStepAdapterOutputs(t *testing.T) {
	type out struct {
		st  adapter.State
		err string
	}
	run := func(f func(m msg.Message, d adapter.Delivery) (adapter.State, error), m msg.Message, d adapter.Delivery) out {
		st, err := f(m, d)
		o := out{st: st}
		if err != nil {
			o.err = err.Error()
		}
		return o
	}
	for _, loaded := range []bool{false, true} {
		for _, kind := range []msg.Kind{msg.Inform, msg.Ask} {
			for _, mode := range []msg.Delivery{msg.Immediate, msg.NextTurn} {
				for _, verified := range []bool{true, false} {
					name := fmt.Sprintf("loaded=%v %s %s verified=%v", loaded, kind, mode, verified)
					rig := func() (*Adapter, *appservertest.Server) {
						s := appservertest.New(t)
						th := &appservertest.Thread{ID: wThread, Cwd: "/work", Loaded: loaded}
						s.AddThread(th)
						return &Adapter{ControlSocket: s.Endpoint()}, s
					}
					m := routeMsg(kind, mode)
					d := delivery(mode, verified)
					bare, _ := rig()
					want := run(func(m msg.Message, d adapter.Delivery) (adapter.State, error) {
						return bare.Inject(context.Background(), m, d)
					}, m, d)

					e := newRouteEnv(t, true)
					a, _ := rig()
					e.h.Threads = a
					e.setting(false)
					got := run(func(m msg.Message, d adapter.Delivery) (adapter.State, error) {
						return e.h.Inject(context.Background(), m, d)
					}, m, d)
					if got != want {
						t.Fatalf("%s: setting off %+v, pre-step %+v", name, got, want)
					}
					e.wantRows(0)

					if !loaded && kind == msg.Inform {
						e.setting(true)
						if st, err := e.h.Inject(context.Background(), m, d); st != adapter.Mailboxed {
							t.Fatalf("%s: setting on %s %v, want mailboxed", name, st, err)
						}
					}
				}
			}
		}
	}
}

func TestRoute_AskWithCapabilityPresent_ExpectRejectedNoEndpointNoRowNoEntry(t *testing.T) {
	e := newRouteEnv(t, true)
	if res := e.send(verifiedPeer, routeMsg(msg.Inform, msg.Immediate)); res.State != adapter.Mailboxed {
		t.Fatalf("inform %+v: capability should be present", res)
	}
	e.wantRows(1)
	e.take()
	ask := routeMsg(msg.Ask, msg.Immediate)
	e.wantRoute(ask, delivery(msg.Immediate, true), RouteNone)
	if res := e.send(verifiedPeer, ask); res.State != adapter.Rejected || res.Reason != receiver.ReasonNoEndpoint {
		t.Fatalf("ask %+v", res)
	}
	if st, ok := e.pulled()[ask.MessageID]; ok && (st == journal.StateMailboxed || st == journal.StateDelivered) {
		t.Fatalf("ask entry %s", st)
	}
	e.wantRows(0)
	if e.r.RouteOpen(ask.MessageID) {
		t.Fatal("ask route left open")
	}
}

// noSignal hides MailSignal: the process stops between the mailboxed record
// and the signal.
type noSignal struct{ adapter.Adapter }

func TestRoute_CrashAfterMailboxedBeforeSignal_ExpectPullableNoGenerationNextArrivalOpensOne(t *testing.T) {
	e := newRouteEnv(t, true)
	e.open(noSignal{e.h})
	m1 := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m1); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	e.wantRows(0)
	if g := gen(t, e.h.wakeRoute().wakes); g.N != 0 {
		t.Fatalf("generation %+v before any signal", g)
	}
	e.open(e.h)
	m2 := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m2); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	e.wantRows(1)
	got := e.pulled()
	if got[m1.MessageID] != journal.StateDelivered || got[m2.MessageID] != journal.StateDelivered {
		t.Fatalf("pulled %v", got)
	}
}

func TestRoute_ThreadResumedOnAppServer_ExpectQueueRouteStopsMailboxedStaysPullableNotResent(t *testing.T) {
	e := newRouteEnv(t, true)
	m1 := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m1); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	e.wantRows(1)
	e.take()

	e.threads.setHosts(1)
	m2 := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m2); res.State != adapter.Unreachable {
		t.Fatalf("claim and host: %+v, want unreachable", res)
	}

	e.sys.set(wThread, true, []int{4343})
	if revoked := e.claims.Recheck(); len(revoked) != 1 {
		t.Fatalf("revoked %v", revoked)
	}
	m3 := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m3); res.State != adapter.Accepted {
		t.Fatalf("after the switch: %+v, want the app-server route", res)
	}
	calls := e.threads.Writes()
	if len(calls) != 1 || calls[0].Message.MessageID != m3.MessageID {
		t.Fatalf("app-server injects %+v, want m3 only", calls)
	}
	e.wantRows(0)
	if st := e.pulled()[m1.MessageID]; st != journal.StateDelivered {
		t.Fatalf("m1 %q, want returned by the pull (marked delivered on first return)", st)
	}
}

type failingDirectory struct{}

func (failingDirectory) List() ([]registry.Descriptor, error) {
	return nil, errors.New("descriptor directory unreadable")
}

func TestRoute_ClaimAndEndpointListFails_ExpectUnreachableNoRow(t *testing.T) {
	e := newRouteEnv(t, true)
	e.h.Threads = &Adapter{ControlSocket: filepath.Join(shortSockDir(t), "none.sock"), Directory: failingDirectory{}}
	m := routeMsg(msg.Inform, msg.Immediate)
	e.wantRoute(m, delivery(msg.Immediate, true), RouteAmbiguous)
	if res := e.send(verifiedPeer, m); res.State != adapter.Unreachable {
		t.Fatalf("%+v", res)
	}
	if err := e.h.MailSignal(context.Background(), m, delivery(msg.Immediate, true)); !errors.Is(err, ErrHostProbe) {
		t.Fatalf("signal err %v, want ErrHostProbe", err)
	}
	e.wantRows(0)
}

func TestRoute_SettingOffWhileWriteWaits_ExpectNoRow(t *testing.T) {
	e := newRouteEnv(t, true)
	gates := 0
	e.gate = func(g *QueueGate) {
		if gates++; gates == 2 {
			g.afterBegin = func() { e.setting(false) }
		}
	}
	m := routeMsg(msg.Inform, msg.Immediate)
	if res := e.send(verifiedPeer, m); res.State != adapter.Mailboxed {
		t.Fatalf("%+v", res)
	}
	e.wantRows(0)
	if g := gen(t, e.h.wakeRoute().wakes); g.State != GenFailed {
		t.Fatalf("generation %+v, want failed", g)
	}
}

func TestRoute_RemoteSenderOrInboxCopy_ExpectMailboxedNoWake(t *testing.T) {
	fp := strings.Repeat("a", 64)
	remotePeer := receiver.Sender{Verified: true, Host: fp, HostAlias: "desk", AgentID: "e1e1e1e1e1e1e1e1", Name: "alice@desk", Role: registry.RolePrimary, Principal: receiver.RemoteKey(fp, "e1e1e1e1e1e1e1e1")}
	cases := []struct {
		name string
		in   func(m msg.Message) receiver.Inbound
		d    adapter.Delivery
	}{
		{"remote sender", func(m msg.Message) receiver.Inbound {
			return receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: remotePeer}
		}, adapter.Delivery{Mode: msg.Immediate, VerifiedSender: true, SenderRemote: true, Target: adapter.Binding{Harness: adapter.Codex, SessionRef: wThread}}},
		{"inbox copy", func(m msg.Message) receiver.Inbound {
			return receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: verifiedPeer, Inbox: "ops", InboxCopy: true}
		}, adapter.Delivery{Mode: msg.Immediate, VerifiedSender: true, Inbox: "ops", InboxCopy: true, Target: adapter.Binding{Harness: adapter.Codex, SessionRef: wThread}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRouteEnv(t, true)
			m := routeMsg(msg.Inform, msg.Immediate)
			e.wantRoute(m, c.d, RouteMailbox)
			if res := e.r.Receive(context.Background(), c.in(m)); res.State != adapter.Mailboxed {
				t.Fatalf("%+v", res)
			}
			e.wantRows(0)
			if st := e.pulled()[m.MessageID]; st != journal.StateDelivered {
				t.Fatalf("entry %q, want pullable", st)
			}
		})
	}
}

func TestRoute_SenderSideInjectIntoUnboundSession_ExpectNeverMailboxed(t *testing.T) {
	e := newRouteEnv(t, true)
	m := routeMsg(msg.Inform, msg.Immediate)
	d := delivery(msg.Immediate, true)
	d.SenderAgentID = "a1a1a1a1a1a1a1a1"
	st, err := e.h.Inject(context.Background(), m, d)
	if st == adapter.Mailboxed {
		t.Fatalf("sender-side inject returned mailboxed (%v) with no entry in the recipient's journal", err)
	}
	if n := e.appServerCalls(); n != 1 {
		t.Fatalf("app-server injects %d, want the pre-step adapter's Inject", n)
	}
	e.wantRows(0)
}
