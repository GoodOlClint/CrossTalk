package journal

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var bind = Binding{InstanceID: "inst-1", HarnessEndpoint: "/codex.sock", SessionRef: "thread-1"}

const agent = "0123456789abcdef"

func open(t *testing.T, dir string, opt Options) (*Journal, Replay) {
	t.Helper()
	j, rp, err := Open(dir, agent, bind, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, rp
}

// Test 20 core: after a restart, a retry of the same message_id returns the
// prior state and the dispatch cannot be claimed again.
func TestRestartRetryReturnsPriorState(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	if _, dup, err := j.Accept("alice", "m1", "submitted"); dup || err != nil {
		t.Fatalf("first accept: dup=%v err=%v", dup, err)
	}
	if ok, _ := j.Claim("m1"); !ok {
		t.Fatal("first claim refused")
	}
	if err := j.Resolve("m1", "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := j.SetDedupeState("alice", "m1", "accepted"); err != nil {
		t.Fatal(err)
	}
	j.Close()

	j2, rp := open(t, dir, Options{})
	if len(rp.Dispatching) != 0 {
		t.Fatalf("resolved dispatch replayed as dispatching: %+v", rp.Dispatching)
	}
	prior, dup, err := j2.Accept("alice", "m1", "submitted")
	if err != nil || !dup || prior != "accepted" {
		t.Fatalf("retry: prior=%q dup=%v err=%v, want accepted true", prior, dup, err)
	}
	if ok, _ := j2.Claim("m1"); ok {
		t.Fatal("retry claimed an already-dispatched message")
	}
	if _, dup, _ := j2.Accept("bob", "m1", "submitted"); dup {
		t.Fatal("dedupe key must include the sender")
	}
}

func TestRecordsCarryBindingAndAreFsyncedLines(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	for _, r := range []Record{
		{Type: TypeTask, TaskID: "t1", Principal: "local-user", Kind: "ask", State: StateSubmitted},
		{Type: TypeRoute, Session: "thread-1", MessageID: "m2", Sender: "alice", State: StateOpen},
		{Type: TypeObligation, MessageID: "r1", InReplyTo: "m2", To: "alice", State: StatePending},
		{Type: TypeClaim, Owner: "fedcba9876543210", Claimed: true},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(Path(dir, agent))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}
	for _, l := range lines {
		for _, want := range []string{`"instance_id":"inst-1"`, `"harness_endpoint":"/codex.sock"`, `"session_ref":"thread-1"`} {
			if !strings.Contains(l, want) {
				t.Errorf("line %s missing %s", l, want)
			}
		}
	}
	fi, _ := os.Stat(Path(dir, agent))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestPartialTrailingLineDiscarded(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	_, _, _ = j.Accept("alice", "m1", "accepted")
	j.Close()
	f, _ := os.OpenFile(Path(dir, agent), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"dedupe","sender":"alice","message_id":"m2","sta`)
	f.Close()

	j2, rp := open(t, dir, Options{})
	if !rp.DiscardedPartial {
		t.Fatal("partial line not reported")
	}
	if _, ok := j2.Dedupe("alice", "m2"); ok {
		t.Fatal("partial record replayed")
	}
	if s, ok := j2.Dedupe("alice", "m1"); !ok || s != "accepted" {
		t.Fatalf("complete record lost: %q %v", s, ok)
	}
	if _, _, err := j2.Accept("alice", "m3", "accepted"); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	j3, rp3 := open(t, dir, Options{})
	if rp3.DiscardedPartial {
		t.Fatal("append after a discarded partial produced another partial")
	}
	if _, ok := j3.Dedupe("alice", "m3"); !ok {
		t.Fatal("record appended after partial discard was lost")
	}
}

func TestCorruptCompleteLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/journal", 0o700)
	os.WriteFile(Path(dir, agent), []byte("{not json}\n"), 0o600)
	if _, _, err := Open(dir, agent, bind, Options{}); err == nil {
		t.Fatal("corrupt complete line accepted")
	}
}

func TestUnresolvedDispatchingReturnedNotResolved(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	j.Claim("m1")
	j.Claim("m2")
	j.Resolve("m2", "accepted")
	j.Close()

	for range 2 {
		j2, rp := open(t, dir, Options{})
		if len(rp.Dispatching) != 1 || rp.Dispatching[0].MessageID != "m1" || rp.Dispatching[0].Binding != bind {
			t.Fatalf("dispatching = %+v", rp.Dispatching)
		}
		if r, _ := j2.DispatchState("m1"); r.Type != TypeDispatch {
			t.Fatalf("replay auto-resolved m1: %+v", r)
		}
		if ok, _ := j2.Claim("m1"); ok {
			t.Fatal("unresolved dispatch re-claimable after restart")
		}
		j2.Close()
	}
}

func TestRetentionKeepsUnresolved(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	j, _ := open(t, dir, Options{Now: clock})
	j.Accept("alice", "old-done", "accepted")
	j.Accept("alice", "old-sub", StateSubmitted)
	j.Claim("old-disp")
	j.Claim("old-unk")
	j.Resolve("old-unk", StateUnknown)
	j.Claim("old-ok")
	j.Resolve("old-ok", "accepted")
	j.Append(Record{Type: TypeTask, TaskID: "ask-open", Kind: "ask", State: StateWorking})
	j.Append(Record{Type: TypeTask, TaskID: "ask-done", Kind: "ask", State: "completed"})
	j.Append(Record{Type: TypeObligation, MessageID: "reply-pending", InReplyTo: "x", To: "alice", State: StatePending})
	j.Append(Record{Type: TypeObligation, MessageID: "reply-done", State: StatePending})
	j.Append(Record{Type: TypeObligation, MessageID: "reply-done", State: StateDelivered})
	j.Append(Record{Type: TypeClaim, Owner: "fedcba9876543210", Claimed: true})
	j.Close()

	now = now.Add(23 * time.Hour)
	j1, rp1 := open(t, dir, Options{Now: clock})
	if rp1.Truncated != 0 {
		t.Fatalf("truncated %d records inside the window", rp1.Truncated)
	}
	j1.Accept("alice", "fresh", "accepted")
	j1.Close()

	now = now.Add(2 * time.Hour)
	j2, rp := open(t, dir, Options{Now: clock})
	if _, ok := j2.Dedupe("alice", "old-done"); ok {
		t.Error("resolved dedupe older than 24h kept")
	}
	if _, ok := j2.Dedupe("alice", "fresh"); !ok {
		t.Error("record inside the window truncated")
	}
	if s, _ := j2.Dedupe("alice", "old-sub"); s != StateSubmitted {
		t.Error("submitted record truncated")
	}
	if r, _ := j2.DispatchState("old-disp"); r.Type != TypeDispatch {
		t.Error("dispatching record truncated")
	}
	if r, _ := j2.DispatchState("old-unk"); r.State != StateUnknown {
		t.Error("unknown outcome truncated")
	}
	if _, ok := j2.DispatchState("old-ok"); ok {
		t.Error("resolved dispatch older than 24h kept")
	}
	if ok, _ := j2.Claim("old-unk"); ok {
		t.Error("unknown dispatch re-claimable after retention pass")
	}
	if _, ok := j2.Latest(Record{Type: TypeTask, TaskID: "ask-open"}); !ok {
		t.Error("open ask truncated")
	}
	if _, ok := j2.Latest(Record{Type: TypeTask, TaskID: "ask-done"}); ok {
		t.Error("completed task older than 24h kept")
	}
	if _, ok := j2.Latest(Record{Type: TypeObligation, MessageID: "reply-pending"}); !ok {
		t.Error("undelivered reply truncated")
	}
	if _, ok := j2.Latest(Record{Type: TypeObligation, MessageID: "reply-done"}); ok {
		t.Error("delivered reply older than 24h kept")
	}
	if c, ok := j2.Latest(Record{Type: TypeClaim}); !ok || !c.Claimed {
		t.Error("active claim truncated")
	}
	if rp.Truncated == 0 {
		t.Error("nothing truncated")
	}
	if len(rp.Dispatching) != 1 || rp.Dispatching[0].MessageID != "old-disp" {
		t.Errorf("dispatching = %+v", rp.Dispatching)
	}
}

// C5: Claim and Cancel on one message are serialized; exactly one wins.
func TestCancelClaimRace(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	for i := range 200 {
		id := "m" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		var claimed, canceled atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 4 {
			wg.Go(func() {
				<-start
				if ok, err := j.Claim(id); err != nil {
					t.Error(err)
				} else if ok {
					claimed.Add(1)
				}
			})
			wg.Go(func() {
				<-start
				if ok, err := j.Cancel(id); err != nil {
					t.Error(err)
				} else if ok {
					canceled.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		if claimed.Load()+canceled.Load() != 1 {
			t.Fatalf("%s: claimed=%d canceled=%d, want exactly one winner", id, claimed.Load(), canceled.Load())
		}
	}
	j.Close()
	j2, _ := open(t, dir, Options{})
	if ok, _ := j2.Claim("ma"); ok {
		t.Fatal("claim/cancel outcome not durable across restart")
	}
	if ok, _ := j2.Cancel("ma"); ok {
		t.Fatal("claim/cancel outcome not durable across restart")
	}
}

func TestConcurrentDuplicateAcceptOnce(t *testing.T) {
	j, _ := open(t, t.TempDir(), Options{})
	var fresh atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, dup, err := j.Accept("alice", "m1", "submitted"); err == nil && !dup {
				fresh.Add(1)
			}
		})
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Fatalf("%d concurrent duplicates accepted as new", fresh.Load())
	}
}

func TestResolveRequiresClaim(t *testing.T) {
	j, _ := open(t, t.TempDir(), Options{})
	if err := j.Resolve("m1", "accepted"); err == nil {
		t.Fatal("resolve without claim succeeded")
	}
}

func TestClosedJournalRefusesWrites(t *testing.T) {
	j, _ := open(t, t.TempDir(), Options{})
	j.Close()
	if _, err := j.Claim("m1"); err != ErrClosed {
		t.Fatalf("err = %v", err)
	}
}

func TestSameConversation(t *testing.T) {
	other := bind
	other.InstanceID = "inst-2"
	if !bind.SameConversation(other) {
		t.Fatal("instance_id must not affect conversation identity")
	}
	other.SessionRef = "thread-2"
	if bind.SameConversation(other) {
		t.Fatal("different session_ref treated as same conversation")
	}
}

func TestAcceptRecordCarriesMessageAndRecordsListsInOrder(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	if _, dup, err := j.AcceptRecord(Record{Sender: "a", MessageID: "m1", State: "submitted", Message: []byte(`{"x":1}`)}); dup || err != nil {
		t.Fatal(dup, err)
	}
	if prior, dup, _ := j.AcceptRecord(Record{Sender: "a", MessageID: "m1", State: "submitted"}); !dup || prior != "submitted" {
		t.Fatal("second AcceptRecord not deduped")
	}
	j.SetDedupeState("a", "m1", "accepted")
	rs, err := j.Records()
	if err != nil || len(rs) != 2 || string(rs[0].Message) != `{"x":1}` || rs[0].Type != TypeDedupe || rs[1].State != "accepted" {
		t.Fatalf("%+v %v", rs, err)
	}
	j.Close()
	if _, err := j.Records(); err != ErrClosed {
		t.Fatal(err)
	}
}

func reopenAfter(t *testing.T, dir string, now time.Time, d time.Duration) (*Journal, Replay) {
	t.Helper()
	return open(t, dir, Options{Now: func() time.Time { return now.Add(d) }})
}

func TestRetentionKeepsOpenAskOriginal(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
	body := json.RawMessage(`{"message_id":"ask","kind":"ask"}`)
	j.Append(Record{Type: TypeDedupe, Sender: "sender", MessageID: "ask", State: "accepted", Message: body})
	j.Append(Record{Type: TypeRoute, MessageID: "ask", Sender: "sender", Session: "session", State: StateOpen})
	j.Append(Record{Type: TypeTask, MessageID: "ask", TaskID: "task", State: StateWorking})
	j.Claim("ask")
	j.Resolve("ask", "accepted")
	j.Accept("alice", "done", "accepted")
	j.Close()

	j2, _ := reopenAfter(t, dir, now, 25*time.Hour)
	rs, err := j2.Records()
	if err != nil {
		t.Fatal(err)
	}
	var kept []Type
	for _, r := range rs {
		kept = append(kept, r.Type)
		if r.Type == TypeDedupe && r.MessageID == "done" {
			t.Error("fully resolved message older than retention kept")
		}
	}
	for _, want := range []Type{TypeDedupe, TypeDispatch, TypeOutcome, TypeRoute, TypeTask} {
		found := false
		for _, r := range rs {
			found = found || r.Type == want && r.MessageID == "ask" && (want != TypeDedupe || len(r.Message) > 0)
		}
		if !found {
			t.Errorf("open ask lost %s record: kept %v", want, kept)
		}
	}
}

func TestRetentionKeepsUnansweredOutboundAsk(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
	j.Append(Record{Type: TypeOutbound, MessageID: "out", To: "bob", Kind: "ask", State: "accepted", TaskID: "far", Message: json.RawMessage(`{"deadline_seconds":0}`)})
	j.Append(Record{Type: TypeOutbound, MessageID: "out-done", To: "bob", Kind: "inform", State: "accepted"})
	j.Close()

	j2, _ := reopenAfter(t, dir, now, 25*time.Hour)
	if r, ok := j2.Latest(Record{Type: TypeOutbound, MessageID: "out"}); !ok || len(r.Message) == 0 {
		t.Errorf("unanswered outbound ask lost its record or body: %+v", r)
	}
	if _, ok := j2.Latest(Record{Type: TypeOutbound, MessageID: "out-done"}); ok {
		t.Error("resolved outbound older than retention kept")
	}

	j2.Append(Record{Type: TypeDedupe, Sender: "bob", MessageID: "answer", State: "accepted", Message: json.RawMessage(`{"message_id":"answer","in_reply_to":"out","kind":"inform"}`)})
	j2.Append(Record{Type: TypeReceipt, MessageID: "answer", InReplyTo: "out", Sender: "bob"})
	j2.Close()
	j3, _ := reopenAfter(t, dir, now, 50*time.Hour)
	if _, ok := j3.Latest(Record{Type: TypeOutbound, MessageID: "out"}); ok {
		t.Error("outbound ask kept after it was answered")
	}
}

func weekAsk(j *Journal) {
	j.Append(Record{Type: TypeOutbound, MessageID: "ask", To: "peer", Kind: "ask", State: "accepted", ContextID: "ask", Message: json.RawMessage(`{"message_id":"ask","context_id":"ask","kind":"ask","deadline_seconds":604800}`)})
}

func TestRetentionIgnoresReplyBodiesWithoutReceipt(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
	weekAsk(j)
	j.Append(Record{Type: TypeDedupe, MessageID: "wrong", Sender: "other-peer", State: "rejected", Message: json.RawMessage(`{"message_id":"wrong","in_reply_to":"ask","context_id":"ask"}`)})
	j.Append(Record{Type: TypeDedupe, MessageID: "legacy", Sender: "peer", State: "accepted", Message: json.RawMessage(`{"message_id":"legacy","in_reply_to":"ask","context_id":"ask"}`)})
	j.Append(Record{Type: TypeObligation, MessageID: "own-reply", InReplyTo: "ask", To: "peer", State: StateDelivered})
	j.Append(Record{Type: TypeReceipt, MessageID: "spoof", InReplyTo: "ask", Sender: "other-peer", ContextID: "ask"})
	j.Close()

	j2, _ := reopenAfter(t, dir, now, 48*time.Hour)
	if _, ok := j2.Latest(Record{Type: TypeOutbound, MessageID: "ask"}); !ok {
		t.Fatal("unanswered week-long ask pruned on reply bodies or a receipt from another peer")
	}
}

func TestRetentionPrunesAskAfterReceipt(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
	weekAsk(j)
	j.Append(Record{Type: TypeReceipt, MessageID: "answer", InReplyTo: "ask", Sender: "peer", ContextID: "ask"})
	j.Close()

	j2, _ := reopenAfter(t, dir, now, 12*time.Hour)
	if _, ok := j2.Latest(Record{Type: TypeOutbound, MessageID: "ask"}); !ok {
		t.Fatal("answered ask pruned inside the retention window")
	}
	j2.Close()
	j3, _ := reopenAfter(t, dir, now, 48*time.Hour)
	if _, ok := j3.Latest(Record{Type: TypeOutbound, MessageID: "ask"}); ok {
		t.Error("ask with a receipt kept past retention")
	}
}

func TestRetentionPrunesAskAfterAdmittedExpiryNotice(t *testing.T) {
	for _, tc := range []struct {
		state string
		kept  bool
	}{{"accepted", false}, {"queued", false}, {"rejected", true}, {StateSubmitted, true}} {
		t.Run(tc.state, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
			weekAsk(j)
			j.Append(Record{Type: TypeDedupe, MessageID: "notice", Sender: PrincipalCrossTalk, State: StateSubmitted, Message: json.RawMessage(`{"message_id":"notice","in_reply_to":"ask","kind":"inform"}`)})
			if tc.state != StateSubmitted {
				j.SetDedupeState(PrincipalCrossTalk, "notice", tc.state)
			}
			j.Close()

			j2, _ := reopenAfter(t, dir, now, 48*time.Hour)
			if _, ok := j2.Latest(Record{Type: TypeOutbound, MessageID: "ask"}); ok != tc.kept {
				t.Errorf("ask kept = %t, want %t", ok, tc.kept)
			}
		})
	}
}

func TestRetentionTreatsExpiredInformRouteAsClosed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
	for _, id := range []string{"fyi", "ask", "legacy"} {
		kind := map[string]string{"fyi": "inform", "ask": "ask", "legacy": ""}[id]
		j.Append(Record{Type: TypeDedupe, Sender: "alice", MessageID: id, State: "accepted", Message: json.RawMessage(`{"message_id":"` + id + `"}`)})
		j.Append(Record{Type: TypeRoute, Session: "s", MessageID: id, Sender: "alice", Kind: kind, State: StateOpen})
	}
	j.Close()

	j2, rp := reopenAfter(t, dir, now, 25*time.Hour)
	for _, id := range []string{"ask", "legacy"} {
		if _, ok := j2.Latest(Record{Type: TypeDedupe, Sender: "alice", MessageID: id}); !ok {
			t.Errorf("open %s route lost its message", id)
		}
	}
	if _, ok := j2.Latest(Record{Type: TypeRoute, Session: "s", MessageID: "fyi", Sender: "alice"}); ok {
		t.Error("inform route past its TTL kept")
	}
	if _, ok := j2.Latest(Record{Type: TypeDedupe, Sender: "alice", MessageID: "fyi"}); ok {
		t.Error("expired inform route's message kept as a dependency")
	}
	for _, u := range rp.Unresolved {
		if u.MessageID == "fyi" {
			t.Errorf("expired inform route replayed: %+v", u)
		}
	}
}

func TestRetentionKeepsInformRouteInsideTTL(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	j, _ := open(t, dir, Options{Now: func() time.Time { return now }})
	j.Append(Record{Type: TypeDedupe, Sender: "alice", MessageID: "fyi", State: "accepted", Message: json.RawMessage(`{"message_id":"fyi"}`)})
	j.Append(Record{Type: TypeRoute, Session: "s", MessageID: "fyi", Sender: "alice", Kind: "inform", State: StateOpen})
	j.Close()

	j2, _ := open(t, dir, Options{Now: func() time.Time { return now.Add(30 * time.Minute) }, Retention: 10 * time.Minute})
	if _, ok := j2.Latest(Record{Type: TypeDedupe, Sender: "alice", MessageID: "fyi"}); !ok {
		t.Error("open inform route inside its TTL lost its message")
	}
}

func TestAppendRefusesOverLimitRecord(t *testing.T) {
	j, _ := open(t, t.TempDir(), Options{})
	err := j.Append(Record{Type: TypeDedupe, Sender: "s", MessageID: "m", State: strings.Repeat("x", MaxRecordBytes)})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("Append over limit = %v, want ErrRecordTooLarge", err)
	}
	if err := j.Append(Record{Type: TypeDedupe, Sender: "s", MessageID: "m2", State: "x"}); err != nil {
		t.Fatalf("journal unusable after refusal: %v", err)
	}
}

func TestNudgeRecordReplaysAndIsKept(t *testing.T) {
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	if err := j.Append(Record{Type: TypeNudge, To: "peer-1", Count: 30}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Record{Type: TypeNudge, To: "peer-1", Count: 60}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j2, _ := open(t, dir, Options{})
	rs, err := j2.Records()
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, r := range rs {
		if r.Type == TypeNudge && r.To == "peer-1" {
			got = append(got, r.Count)
		}
	}
	if len(got) != 2 || got[0] != 30 || got[1] != 60 {
		t.Fatalf("replayed nudges %v", got)
	}
}
