package wire

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/remote"
	"github.com/GoodOlClint/crosstalk/internal/shim"
)

func abortPosts(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if r.Method == http.MethodPost {
		panic(http.ErrAbortHandler)
	}
	return false
}

// timer is a fake clock source for the reply retry: every wait the shim asks
// for arrives on calls, and the test fires it.
type timer struct {
	off   atomic.Int64
	calls chan timerCall
}

type timerCall struct {
	d    time.Duration
	fire chan time.Time
}

func newTimer() *timer { return &timer{calls: make(chan timerCall, 64)} }

func (tm *timer) mod(so *shim.Options) {
	so.Now = func() time.Time { return time.Now().Add(time.Duration(tm.off.Load())) }
	so.ReplyRetry.After = func(d time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		tm.calls <- timerCall{d, c}
		return c
	}
}

func (tm *timer) next(t *testing.T) timerCall {
	t.Helper()
	select {
	case c := <-tm.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the shim armed no retry timer")
	}
	return timerCall{}
}

func (tm *timer) none(t *testing.T) {
	t.Helper()
	select {
	case c := <-tm.calls:
		t.Fatalf("the shim armed another retry (%v) after the obligation ended", c.d)
	case <-time.After(150 * time.Millisecond):
	}
}

func (tm *timer) advance(d time.Duration) { tm.off.Add(int64(d)) }

func twoHostsMod(t *testing.T, mod func(*shim.Options)) (h1, h2 *rhost, alice, bob *node) {
	return twoHostsMods(t, nil, mod)
}

func twoHostsMods(t *testing.T, mod1, mod2 func(*shim.Options)) (h1, h2 *rhost, alice, bob *node) {
	t.Helper()
	h1, h2 = newRHost(t), newRHost(t)
	h1.mod, h2.mod = mod1, mod2
	alice, bob = h1.agent("alice"), h2.agent("bob")
	h1.serve()
	h2.serve()
	pairHosts(t, h1, "lab", h2, "desk")
	return
}

func askBob(t *testing.T, alice *node, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"to": "bob@lab", "text": "what is 6*7?"}
	for k, v := range extra {
		args[k] = v
	}
	out := alice.ok("ask", args)
	if str(out, "state") != "accepted" {
		t.Fatalf("ask: %v", out)
	}
	return str(out, "message_id")
}

func obligation(t *testing.T, state, agentID, askID string) (st, reason, replyID string) {
	t.Helper()
	for _, r := range journalOf(t, state, agentID) {
		if r.Type == journal.TypeObligation && r.InReplyTo == askID {
			st, reason, replyID = r.State, r.Reason, r.MessageID
		}
	}
	return
}

func replyArgs(askID string) map[string]any {
	return map[string]any{"message_id": askID, "text": "42"}
}

func replyWrites(n *node) int {
	c := 0
	for _, s := range texts(n.codex.Writes()) {
		if strings.Contains(s, "42") {
			c++
		}
	}
	return c
}

func storedAsk(t *testing.T, state, agentID, askID string) msg.Message {
	t.Helper()
	for _, r := range journalOf(t, state, agentID) {
		if r.Type == journal.TypeOutbound && r.MessageID == askID && len(r.Message) > 0 {
			var m msg.Message
			if err := json.Unmarshal(r.Message, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no stored ask %s", askID)
	return msg.Message{}
}

func resendAsk(t *testing.T, h1, h2 *rhost, alice, bob *node, askID string) (adapter.State, error) {
	t.Helper()
	m := storedAsk(t, h1.state, alice.id(), askID)
	ps, err := h1.peers.List()
	if err != nil || len(ps) != 1 {
		t.Fatalf("peers %v %v", ps, err)
	}
	_, state, err := alice.w.SendRemote(context.Background(), descriptor(t, h1.state, alice.id()), h1.host, ps[0], bob.id(), m)
	return state, err
}

func TestTwoHostReplyAcrossDisconnect(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	var dropped atomic.Bool
	h2.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if r.Method != http.MethodPost || !dropped.CompareAndSwap(false, true) {
			return false
		}
		next.ServeHTTP(httptest.NewRecorder(), r)
		panic(http.ErrAbortHandler)
	})
	out := alice.ok("ask", map[string]any{"to": "bob@lab", "text": "what is 6*7?"})
	if str(out, "state") != "unknown" {
		t.Fatalf("ask across a dropped response: %v, want unknown", out)
	}
	askID := str(out, "message_id")
	waitFor(t, "the dropped ask's inject on host 2", func() bool { return len(bob.codex.Writes()) == 1 })
	if state, err := resendAsk(t, h1, h2, alice, bob, askID); state != adapter.Accepted {
		t.Fatalf("retry of the same message_id: %v %v", state, err)
	}
	if n := len(bob.codex.Writes()); n != 1 {
		t.Fatalf("host 2 injected %d times after the retry, want 1", n)
	}
	out = bob.ok("reply", replyArgs(askID))
	if str(out, "state") != "accepted" {
		t.Fatalf("reply: %v", out)
	}
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("host 1 injected the reply %d times, want 1", n)
	}
	if n := len(alice.codex.Writes()); n != 1 {
		t.Fatalf("host 1 injected %d times, want 1", n)
	}
	if st, _, _ := obligation(t, h2.state, bob.id(), askID); st != journal.StateDelivered {
		t.Fatalf("obligation %q, want delivered", st)
	}
}

func TestRemoteReplyPendingWhileGatewayDown(t *testing.T) {
	modes := map[string]struct {
		down  func(h1 *rhost)
		state adapter.State
	}{
		"stopped": {func(h1 *rhost) {
			h1.stop()
			waitFor(t, "gateway 1 stopped", func() bool { return !dials(h1.addr()) })
		}, adapter.Unreachable},
		"dropped": {func(h1 *rhost) { h1.setHook(abortPosts) }, adapter.Unknown},
	}
	for name, m := range modes {
		t.Run(name, func(t *testing.T) {
			h1, h2, alice, bob := twoHosts(t)
			askID := askBob(t, alice, nil)
			m.down(h1)
			out := bob.ok("reply", replyArgs(askID))
			if str(out, "state") != string(m.state) || str(out, "reason") != shim.ReasonRemoteReplyPending {
				t.Fatalf("reply with the asker's gateway down: %v, want %s/%s", out, m.state, shim.ReasonRemoteReplyPending)
			}
			if st, _, _ := obligation(t, h2.state, bob.id(), askID); st == journal.StateDelivered || st == journal.StateClosed || st == "" {
				t.Fatalf("obligation %q, want pending", st)
			}
			var sent bool
			for _, r := range journalOf(t, h2.state, bob.id()) {
				if r.Type == journal.TypeOutbound && r.MessageID == str(out, "message_id") && r.State == string(m.state) && r.To == receiver.RemoteKey(h1.host.Fingerprint, alice.id()) {
					sent = true
				}
			}
			if !sent {
				t.Fatalf("no %s outbound record keyed %s for the attempt", m.state, receiver.RemoteKey(h1.host.Fingerprint, alice.id()))
			}
			if n := len(alice.codex.Writes()); n != 0 {
				t.Fatalf("host 1 injected %d times, want 0", n)
			}
		})
	}
}

func TestRemoteReplyResumesAfterRestart(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	askID := askBob(t, alice, nil)
	h1.setHook(abortPosts)
	out := bob.ok("reply", replyArgs(askID))
	if s := str(out, "state"); s == "accepted" || s == "queued" {
		t.Fatalf("reply with the connection dropped: %v", out)
	}
	_, _, replyID := obligation(t, h2.state, bob.id(), askID)
	if replyID == "" || str(out, "message_id") != replyID {
		t.Fatalf("obligation reply id %q, result %v", replyID, out)
	}
	h1.setHook(nil)
	bob.restart()
	waitFor(t, "the resent reply on host 1", func() bool { return replyWrites(alice) == 1 })
	time.Sleep(150 * time.Millisecond)
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("host 1 injected the reply %d times, want 1", n)
	}
	waitFor(t, "obligation delivered", func() bool {
		st, _, id := obligation(t, h2.state, bob.id(), askID)
		return st == journal.StateDelivered && id == replyID
	})
	got := 0
	for _, r := range journalOf(t, h1.state, alice.id()) {
		if r.Type == journal.TypeDedupe && r.MessageID == replyID && len(r.Message) > 0 {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("host 1 holds %d dedupe records for reply %s, want 1", got, replyID)
	}
}

func TestRemoteAskReceiptSuppressesDeadline(t *testing.T) {
	h1, _, alice, bob := twoHosts(t)
	askID := askBob(t, alice, map[string]any{"deadline_seconds": 1})
	out := bob.ok("reply", replyArgs(askID))
	if str(out, "state") != "accepted" {
		t.Fatalf("reply: %v", out)
	}
	receipts := 0
	for _, r := range journalOf(t, h1.state, alice.id()) {
		if r.Type == journal.TypeReceipt && r.InReplyTo == askID {
			receipts++
		}
	}
	if receipts != 1 {
		t.Fatalf("host 1 recorded %d receipts, want 1", receipts)
	}
	time.Sleep(1500 * time.Millisecond)
	for _, s := range texts(alice.codex.Writes()) {
		if strings.Contains(s, "expired") {
			t.Fatalf("expiry notice after a delivered reply: %q", s)
		}
	}
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("reply injected %d times, want 1", n)
	}
}

func TestReplyNeverReachesNameReuser(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	askID := askBob(t, alice, nil)
	origID := alice.id()
	alice.shutdown()
	reuser := h1.agent("alice")
	if reuser.id() == origID {
		t.Fatal("reuser has the original agent_id")
	}
	out := bob.ok("reply", replyArgs(askID))
	if str(out, "state") != "rejected" || str(out, "reason") != shim.ReasonNoSuchAgent {
		t.Fatalf("reply to a gone asker: %v, want rejected/%s", out, shim.ReasonNoSuchAgent)
	}
	if n := len(reuser.codex.Writes()); n != 0 {
		t.Fatalf("name reuser got %d injects, want 0", n)
	}
	if n := h1.count("POST /v1/a2a/" + reuser.id()); n != 0 {
		t.Fatalf("%d POSTs to the reuser's path", n)
	}
	if n := h1.count("POST /v1/a2a/" + origID); n < 1 {
		t.Fatalf("no POST carried the original agent_id %s: %v", origID, h1.log())
	}
	if st, reason, _ := obligation(t, h2.state, bob.id(), askID); st != shim.ObligationRejected || reason != shim.ReasonNoSuchAgent {
		t.Fatalf("obligation %q/%q, want %s/%s", st, reason, shim.ObligationRejected, shim.ReasonNoSuchAgent)
	}
	posts := h1.count("POST /v1/a2a/")
	bob.restart()
	time.Sleep(200 * time.Millisecond)
	if n := h1.count("POST /v1/a2a/"); n != posts {
		t.Fatalf("a rejected obligation was retried: %d POSTs, was %d", n, posts)
	}
}

func TestReplyRetryAfterPeerWakes(t *testing.T) {
	h1, _, alice, bob := twoHostsMod(t, func(so *shim.Options) {
		so.ReplyRetry = shim.ReplyRetry{First: 50 * time.Millisecond, Cap: 100 * time.Millisecond}
	})
	_ = bob
	askID := askBob(t, alice, nil)
	h1.stop()
	waitFor(t, "gateway 1 stopped", func() bool { return !dials(h1.addr()) })
	out := bob.ok("reply", replyArgs(askID))
	if s := str(out, "state"); s == "accepted" || s == "queued" {
		t.Fatalf("reply with the gateway down: %v", out)
	}
	h1.restartGateway()
	waitFor(t, "the timer's retry to reach host 1", func() bool { return replyWrites(alice) == 1 })
	time.Sleep(400 * time.Millisecond)
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("host 1 injected the reply %d times, want 1", n)
	}
}

func TestReplyRouteSurvivesAliasChange(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	askID := askBob(t, alice, nil)
	if err := h2.peers.Upsert(remote.Peer{Alias: "renamed", Address: h1.addr(), Fingerprint: h1.host.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	ps, _ := h2.peers.List()
	if len(ps) != 1 || ps[0].Alias != "renamed" {
		t.Fatalf("peers after rename: %v", ps)
	}
	out := bob.ok("reply", replyArgs(askID))
	if str(out, "state") != "accepted" {
		t.Fatalf("reply after the alias changed: %v", out)
	}
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("host 1 injected the reply %d times, want 1", n)
	}
}

func TestRemoteReplyRetryBackoff(t *testing.T) {
	tm := newTimer()
	h1, _, alice, bob := twoHostsMod(t, tm.mod)
	askID := askBob(t, alice, nil)
	h1.setHook(abortPosts)
	bob.ok("reply", replyArgs(askID))
	want := []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second, 600 * time.Second, 600 * time.Second}
	for i, w := range want {
		c := tm.next(t)
		if c.d != w {
			t.Fatalf("wait %d is %v, want %v", i, c.d, w)
		}
		c.fire <- time.Now()
	}
	waitFor(t, "a POST per retry", func() bool { return h1.count("POST /v1/a2a/"+alice.id()) >= len(want)+1 })
}

func TestRemoteReplyRetryEndsUnreachable(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		limit time.Duration
	}{
		{"ask deadline on the replier's clock", map[string]any{"deadline_seconds": 300}, 300 * time.Second},
		{"24 h with no deadline", nil, 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tm := newTimer()
			h1, h2, alice, bob := twoHostsMod(t, tm.mod)
			askID := askBob(t, alice, c.extra)
			h1.setHook(abortPosts)
			bob.ok("reply", replyArgs(askID))
			first := tm.next(t)
			tm.advance(c.limit - time.Minute)
			first.fire <- time.Now()
			second := tm.next(t)
			posts := h1.count("POST /v1/a2a/")
			if posts != 2 {
				t.Fatalf("%d POSTs before the limit, want 2", posts)
			}
			tm.advance(2 * time.Minute)
			second.fire <- time.Now()
			waitFor(t, "obligation unreachable", func() bool {
				st, _, _ := obligation(t, h2.state, bob.id(), askID)
				return st == shim.ObligationUnreachable
			})
			tm.none(t)
			if n := h1.count("POST /v1/a2a/"); n != posts {
				t.Fatalf("%d POSTs after the limit, was %d", n, posts)
			}
			if n := len(alice.codex.Writes()); n != 0 {
				t.Fatalf("host 1 injected %d times", n)
			}
		})
	}
}

func TestAskRetryAfterTargetShimRestartDeduped(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	askID := askBob(t, alice, nil)
	if n := len(bob.codex.Writes()); n != 1 {
		t.Fatalf("host 2 injected %d times, want 1", n)
	}
	bob.restart()
	if state, err := resendAsk(t, h1, h2, alice, bob, askID); state != adapter.Accepted {
		t.Fatalf("retry after the target shim restarted: %v %v", state, err)
	}
	if n := len(bob.codex.Writes()); n != 0 {
		t.Fatalf("the restarted target injected %d times, want 0 (one inject in total)", n)
	}
}

func dials(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

func TestRemoteReplyResponseDroppedDedupes(t *testing.T) {
	h1, h2, alice, bob := twoHosts(t)
	askID := askBob(t, alice, nil)
	var dropped atomic.Bool
	h1.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if r.Method != http.MethodPost || !dropped.CompareAndSwap(false, true) {
			return false
		}
		next.ServeHTTP(httptest.NewRecorder(), r)
		panic(http.ErrAbortHandler)
	})
	out := bob.ok("reply", replyArgs(askID))
	if s := str(out, "state"); s != "unknown" || str(out, "reason") != shim.ReasonRemoteReplyPending {
		t.Fatalf("reply whose response was dropped: %v", out)
	}
	waitFor(t, "the dropped reply's inject on host 1", func() bool { return replyWrites(alice) == 1 })
	bob.restart()
	waitFor(t, "obligation delivered", func() bool {
		st, _, _ := obligation(t, h2.state, bob.id(), askID)
		return st == journal.StateDelivered
	})
	time.Sleep(150 * time.Millisecond)
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("host 1 injected the reply %d times, want 1", n)
	}
	if n := h1.count("POST /v1/a2a/" + alice.id()); n < 2 {
		t.Fatalf("%d POSTs: the reply was not resent", n)
	}
}

func TestReplyReachesOnlyTheAskersHost(t *testing.T) {
	h1, h2, h3 := newRHost(t), newRHost(t), newRHost(t)
	carol := h3.agent("carol")
	alice, bob := h1.agent("alice"), h2.agent("bob")
	h1.serve()
	h2.serve()
	h3.serve()
	pairHosts(t, h2, "third", h3, "desk3")
	pairHosts(t, h1, "lab", h2, "desk")
	ps, _ := h2.peers.List()
	if len(ps) != 2 || ps[0].Fingerprint != h3.host.Fingerprint {
		t.Fatalf("host 2 peers %v: the third host must come first", ps)
	}
	askID := askBob(t, alice, nil)
	out := bob.ok("reply", replyArgs(askID))
	if str(out, "state") != "accepted" {
		t.Fatalf("reply: %v", out)
	}
	if n := h3.count("POST "); n != 0 {
		t.Fatalf("the third host received %d POSTs: %v", n, h3.log())
	}
	if n := len(carol.codex.Writes()); n != 0 {
		t.Fatalf("the third host injected %d times", n)
	}
	if n := replyWrites(alice); n != 1 {
		t.Fatalf("the asker injected the reply %d times, want 1", n)
	}
}
