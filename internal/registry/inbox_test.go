package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/identity"
)

type inboxEnv struct {
	t   *testing.T
	r   *Registry
	now time.Time
	mu  sync.Mutex
}

func newInboxEnv(t *testing.T) *inboxEnv {
	e := &inboxEnv{t: t, r: New(t.TempDir()), now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	e.r.Now = func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.now = e.now.Add(time.Second)
		return e.now
	}
	return e
}

func (e *inboxEnv) agent(ref string) *Registration {
	e.t.Helper()
	g, err := e.r.Acquire(req(ref, ref))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { g.Close() })
	if err := g.Publish(Descriptor{SessionRef: ref}); err != nil {
		e.t.Fatal(err)
	}
	return g
}

func id(ref string) string { return identity.AgentID("codex", ref) }

func noHook(InboxEvent) error { return nil }

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err %v, want %v", err, want)
	}
}

func (e *inboxEnv) file(name string) InboxFile {
	e.t.Helper()
	f, ok, err := e.r.ReadInbox(name)
	if err != nil || !ok {
		e.t.Fatalf("read inbox %s: ok=%v err=%v", name, ok, err)
	}
	return f
}

func TestAttachCreatesInboxOwnedByCaller(t *testing.T) {
	e := newInboxEnv(t)
	a := e.agent("a")
	st, err := e.r.AttachInbox(a, "coord", noHook)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Created || st.Owner != id("a") || len(st.Members) != 1 || st.Members[0].AgentID != id("a") {
		t.Fatalf("state %+v", st)
	}
	f := e.file("coord")
	if f.Name != "coord" || f.Owner != id("a") || len(f.Grants) != 0 {
		t.Fatalf("file %+v", f)
	}
	path := filepath.Join(e.r.Dir, "inboxes", "coord.json")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v err %v", fi, err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v err %v", di, err)
	}
	if got := a.Descriptor().Inboxes; len(got) != 1 || got[0].Name != "coord" || got[0].AttachedAt.IsZero() {
		t.Fatalf("descriptor inboxes %+v", got)
	}
	again, err := e.r.AttachInbox(a, "coord", noHook)
	if err != nil || again.Created {
		t.Fatalf("re-attach %+v %v", again, err)
	}
}

func TestAttachOwnedInboxNeedsGrant(t *testing.T) {
	e := newInboxEnv(t)
	a, b := e.agent("a"), e.agent("b")
	if _, err := e.r.AttachInbox(a, "coord", noHook); err != nil {
		t.Fatal(err)
	}
	_, err := e.r.AttachInbox(b, "coord", noHook)
	wantErr(t, err, ErrNotGranted)
	if len(b.Descriptor().Inboxes) != 0 {
		t.Fatal("b joined without a grant")
	}
	if _, err := e.r.GrantInbox(a, "coord", id("b"), false); err != nil {
		t.Fatal(err)
	}
	st, err := e.r.AttachInbox(b, "coord", noHook)
	if err != nil || len(st.Members) != 2 || st.Members[0].AgentID != id("b") {
		t.Fatalf("after grant %+v %v", st, err)
	}
}

func TestGrantOnlyByOwner(t *testing.T) {
	e := newInboxEnv(t)
	a, b, c := e.agent("a"), e.agent("b"), e.agent("c")
	e.r.AttachInbox(a, "coord", noHook)
	e.r.GrantInbox(a, "coord", id("b"), false)
	e.r.AttachInbox(b, "coord", noHook)
	_, err := e.r.GrantInbox(b, "coord", id("c"), false)
	wantErr(t, err, ErrNotOwner)
	_, err = e.r.GrantInbox(c, "coord", id("c"), false)
	wantErr(t, err, ErrNotOwner)
	if f := e.file("coord"); slices.Contains(f.Grants, id("c")) {
		t.Fatalf("grants %v", f.Grants)
	}
	_, err = e.r.GrantInbox(a, "nope", id("c"), false)
	wantErr(t, err, ErrNoSuchInbox)
}

func TestGrantOwnerHandsOverInOneWrite(t *testing.T) {
	e := newInboxEnv(t)
	a, b, c := e.agent("a"), e.agent("b"), e.agent("c")
	e.r.AttachInbox(a, "coord", noHook)
	_, err := e.r.GrantInbox(a, "coord", id("b"), true)
	wantErr(t, err, ErrNotMember)
	if f := e.file("coord"); f.Owner != id("a") {
		t.Fatalf("owner moved to a non-member: %+v", f)
	}
	e.r.GrantInbox(a, "coord", id("b"), false)
	e.r.AttachInbox(b, "coord", noHook)
	st, err := e.r.GrantInbox(a, "coord", id("b"), true)
	if err != nil {
		t.Fatal(err)
	}
	f := e.file("coord")
	if f.Owner != id("b") || !slices.Contains(f.Grants, id("a")) || st.Owner != id("b") {
		t.Fatalf("file %+v state %+v", f, st)
	}
	if len(a.Descriptor().Inboxes) != 1 {
		t.Fatal("old owner left the inbox on hand-over")
	}
	_, err = e.r.GrantInbox(a, "coord", id("c"), false)
	wantErr(t, err, ErrNotOwner)
	_, err = e.r.AttachInbox(c, "coord", noHook)
	wantErr(t, err, ErrNotGranted)
	if _, err := e.r.AttachInbox(a, "coord", noHook); err != nil {
		t.Fatalf("old owner re-attach: %v", err)
	}
}

func TestOwnerDetachLeavesInboxOwnerlessAndKeepsGrants(t *testing.T) {
	e := newInboxEnv(t)
	a, b := e.agent("a"), e.agent("b")
	e.r.AttachInbox(a, "coord", noHook)
	e.r.GrantInbox(a, "coord", id("b"), false)
	e.r.AttachInbox(b, "coord", noHook)
	var events []InboxEvent
	st, err := e.r.DetachInbox(a, "coord", func(ev InboxEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	f := e.file("coord")
	if f.Owner != "" || !slices.Contains(f.Grants, id("a")) || !slices.Contains(f.Grants, id("b")) {
		t.Fatalf("file %+v", f)
	}
	if st.Owner != "" || len(st.Members) != 1 || st.Members[0].AgentID != id("b") {
		t.Fatalf("state %+v", st)
	}
	if len(events) != 1 || events[0].Attached || events[0].Inbox != "coord" {
		t.Fatalf("hook events %+v", events)
	}
	if len(a.Descriptor().Inboxes) != 0 {
		t.Fatal("a still lists the inbox")
	}
	if _, err := e.r.DetachInbox(a, "coord", noHook); err != nil {
		t.Fatalf("second detach: %v", err)
	}
	if _, err := e.r.AttachInbox(a, "coord", noHook); err != nil {
		t.Fatalf("granted ex-owner re-attach: %v", err)
	}
}

func TestClaimOwnerlessInbox(t *testing.T) {
	e := newInboxEnv(t)
	a, b := e.agent("a"), e.agent("b")
	e.r.AttachInbox(a, "coord", noHook)
	_, err := e.r.ClaimInbox(b, "coord", noHook)
	wantErr(t, err, ErrInboxOwned)
	_, err = e.r.ClaimInbox(b, "missing", noHook)
	wantErr(t, err, ErrNoSuchInbox)
	e.r.DetachInbox(a, "coord", noHook)
	st, err := e.r.ClaimInbox(b, "coord", noHook)
	if err != nil {
		t.Fatal(err)
	}
	if st.Owner != id("b") || len(st.Members) != 1 || e.file("coord").Owner != id("b") {
		t.Fatalf("state %+v file %+v", st, e.file("coord"))
	}
	if len(b.Descriptor().Inboxes) != 1 {
		t.Fatal("claimer is not a member")
	}
	if _, err := e.r.ClaimInbox(b, "coord", noHook); err != nil {
		t.Fatalf("owner re-claim: %v", err)
	}
}

func TestAttachOwnerlessRejectedWithClaimHint(t *testing.T) {
	e := newInboxEnv(t)
	a, b := e.agent("a"), e.agent("b")
	e.r.AttachInbox(a, "coord", noHook)
	e.r.DetachInbox(a, "coord", noHook)
	_, err := e.r.AttachInbox(b, "coord", noHook)
	wantErr(t, err, ErrOwnerless)
	if errors.Is(err, ErrNotGranted) {
		t.Fatal("ownerless must not report not_granted")
	}
}

func TestClaimerInheritsGrants(t *testing.T) {
	e := newInboxEnv(t)
	a, b, c := e.agent("a"), e.agent("b"), e.agent("c")
	e.r.AttachInbox(a, "coord", noHook)
	e.r.GrantInbox(a, "coord", id("c"), false)
	e.r.DetachInbox(a, "coord", noHook)
	if _, err := e.r.ClaimInbox(b, "coord", noHook); err != nil {
		t.Fatal(err)
	}
	if f := e.file("coord"); !slices.Contains(f.Grants, id("c")) || f.Owner != id("b") {
		t.Fatalf("file %+v", f)
	}
	if _, err := e.r.AttachInbox(c, "coord", noHook); err != nil {
		t.Fatalf("granted member under new owner: %v", err)
	}
}

func TestInboxConcurrentClaimAndResume(t *testing.T) {
	for i := 0; i < 25; i++ {
		e := newInboxEnv(t)
		a, b := e.agent("a"), e.agent("b")
		st, err := e.r.AttachInbox(a, "coord", noHook)
		if err != nil {
			t.Fatal(err)
		}
		held := InboxMembership{Name: "coord", AttachedAt: st.Members[0].AttachedAt}
		a.Close()
		a2, err := e.r.Acquire(req("a", "a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := a2.Publish(Descriptor{SessionRef: "a"}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var claimErr, resumeErr error
		var kept bool
		wg.Add(2)
		go func() { defer wg.Done(); _, claimErr = e.r.ClaimInbox(b, "coord", noHook) }()
		go func() { defer wg.Done(); kept, resumeErr = e.r.ResumeInbox(a2, held, noHook) }()
		wg.Wait()
		if resumeErr != nil {
			t.Fatal(resumeErr)
		}
		f := e.file("coord")
		switch {
		case claimErr == nil && !kept:
			if f.Owner != id("b") {
				t.Fatalf("claim won but file %+v", f)
			}
		case kept && errors.Is(claimErr, ErrInboxOwned):
			if f.Owner != id("a") {
				t.Fatalf("resume won but file %+v", f)
			}
		default:
			t.Fatalf("no single winner: claim %v kept %v file %+v", claimErr, kept, f)
		}
		if got := len(a2.Descriptor().Inboxes) == 1; got != kept {
			t.Fatalf("resumed descriptor lists inbox=%v, kept=%v", got, kept)
		}
		a2.Close()
		b.Close()
	}
}

func TestInboxCrashBetweenFileAndDescriptor(t *testing.T) {
	e := newInboxEnv(t)
	a, b, c := e.agent("a"), e.agent("b"), e.agent("c")
	crash := errors.New("crash")
	var events []InboxEvent
	_, err := e.r.AttachInbox(a, "coord", func(ev InboxEvent) error { events = append(events, ev); return crash })
	wantErr(t, err, crash)
	if len(a.Descriptor().Inboxes) != 0 {
		t.Fatal("descriptor published after the hook failed")
	}
	if f := e.file("coord"); f.Owner != id("a") {
		t.Fatalf("file %+v", f)
	}
	if _, err := e.r.AttachInbox(c, "coord", noHook); !errors.Is(err, ErrOwnerless) {
		t.Fatalf("attach during crash window: %v", err)
	}
	if _, err := e.r.ClaimInbox(b, "coord", noHook); err != nil {
		t.Fatalf("claim of the crashed inbox: %v", err)
	}
	kept, err := e.r.ResumeInbox(a, InboxMembership{Name: "coord", AttachedAt: events[0].At}, noHook)
	if err != nil || kept {
		t.Fatalf("displaced owner resumed: kept=%v err=%v", kept, err)
	}
	if f := e.file("coord"); f.Owner != id("b") {
		t.Fatalf("displaced owner regained authority: %+v", f)
	}
	if _, err := e.r.GrantInbox(a, "coord", id("c"), false); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("displaced owner grant: %v", err)
	}

	_, err = e.r.DetachInbox(b, "coord", noHook)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.r.ClaimInbox(c, "coord", func(InboxEvent) error { return crash })
	wantErr(t, err, crash)
	if _, err := e.r.ClaimInbox(a, "coord", noHook); err != nil {
		t.Fatalf("claim after a crashed claim: %v", err)
	}
}

func TestResumeKeepsOwnerGrantedAndDropsOthers(t *testing.T) {
	e := newInboxEnv(t)
	a, b, c := e.agent("a"), e.agent("b"), e.agent("c")
	e.r.AttachInbox(a, "coord", noHook)
	e.r.GrantInbox(a, "coord", id("b"), false)
	at := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	m := InboxMembership{Name: "coord", AttachedAt: at}

	kept, err := e.r.ResumeInbox(b, m, noHook)
	if err != nil || !kept {
		t.Fatalf("granted resume kept=%v err=%v", kept, err)
	}
	if got := b.Descriptor().Inboxes; len(got) != 1 || !got[0].AttachedAt.Equal(at) {
		t.Fatalf("resumed inboxes %+v", got)
	}
	var dropped []InboxEvent
	kept, err = e.r.ResumeInbox(c, m, func(ev InboxEvent) error { dropped = append(dropped, ev); return nil })
	if err != nil || kept || len(dropped) != 1 || dropped[0].Attached {
		t.Fatalf("stranger resume kept=%v err=%v events=%+v", kept, err, dropped)
	}
	kept, err = e.r.ResumeInbox(c, InboxMembership{Name: "gone", AttachedAt: at}, noHook)
	if err != nil || kept {
		t.Fatalf("missing-file resume kept=%v err=%v", kept, err)
	}
}

func TestDescriptorWithInboxesReadByOldShape(t *testing.T) {
	d := Descriptor{AgentID: "x", Name: "n", Inboxes: []InboxMembership{{Name: "coord", AttachedAt: time.Now()}}}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		AgentID string `json:"agent_id"`
		Name    string `json:"name"`
		Role    string `json:"role"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.AgentID != "x" || old.Name != "n" {
		t.Fatalf("old shape decoded %+v err %v", old, err)
	}
	var back Descriptor
	if err := json.Unmarshal(b, &back); err != nil || len(back.Inboxes) != 1 || back.Inboxes[0].Name != "coord" {
		t.Fatalf("round trip %+v err %v", back, err)
	}
	b, _ = json.Marshal(Descriptor{})
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["inboxes"]; ok {
		t.Fatal("empty inboxes serialized")
	}
}

func TestListSkipsSupersededMemberAndDeadMember(t *testing.T) {
	e := newInboxEnv(t)
	a, b, c, d := e.agent("a"), e.agent("b"), e.agent("c"), e.agent("d")
	e.r.AttachInbox(a, "coord", noHook)
	for _, g := range []*Registration{b, c, d} {
		e.r.GrantInbox(a, "coord", g.Descriptor().AgentID, false)
		if _, err := e.r.AttachInbox(g, "coord", noHook); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(t, e.r, "coord"); !slices.Equal(got, []string{id("d"), id("c"), id("b"), id("a")}) {
		t.Fatalf("responder-first order %v", got)
	}

	e.r.Superseded = func(x Descriptor) bool { return x.AgentID == id("c") }
	dead := d.Descriptor()
	dead.PID, dead.AgentID = 1<<30, id("ghost")
	dead.Inboxes = []InboxMembership{{Name: "coord", AttachedAt: e.r.now().Add(time.Hour)}}
	writeAtomic(e.r.DescriptorPath(dead.AgentID), dead)
	if got := ids(t, e.r, "coord"); !slices.Equal(got, []string{id("d"), id("b"), id("a")}) {
		t.Fatalf("members after superseded and dead %v", got)
	}
	e.r.Superseded = nil
	d.Close()
	if got := ids(t, e.r, "coord"); !slices.Equal(got, []string{id("c"), id("b"), id("a")}) {
		t.Fatalf("members after close %v", got)
	}
}

func TestInboxMembersTieBreaksOnLowerAgentID(t *testing.T) {
	e := newInboxEnv(t)
	a, b := e.agent("a"), e.agent("b")
	at := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	for _, g := range []*Registration{a, b} {
		if err := g.setInboxes([]InboxMembership{{Name: "coord", AttachedAt: at}}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{id("a"), id("b")}
	slices.Sort(want)
	if got := ids(t, e.r, "coord"); !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestInboxNameValidation(t *testing.T) {
	e := newInboxEnv(t)
	a := e.agent("a")
	for _, n := range []string{"", "a@b", "a/b", "a\nb", "a\x00b", "inbox:x", "INBOX:x"} {
		if _, err := e.r.AttachInbox(a, n, noHook); !errors.Is(err, ErrInvalidInbox) {
			t.Errorf("attach %q: %v", n, err)
		}
	}
	if _, err := e.r.AttachInbox(a, "ok name.v2", noHook); err != nil {
		t.Fatal(err)
	}
}

func ids(t *testing.T, r *Registry, name string) []string {
	t.Helper()
	ds, err := r.InboxMembers(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range ds {
		out = append(out, d.AgentID)
	}
	return out
}

func TestInboxNamesCaseInsensitive(t *testing.T) {
	e := newInboxEnv(t)
	a, b := e.agent("a"), e.agent("b")
	if _, err := e.r.AttachInbox(a, "Coord", noHook); err != nil {
		t.Fatal(err)
	}
	_, err := e.r.AttachInbox(b, "coord", noHook)
	wantErr(t, err, ErrNotGranted)
	if f := e.file("COORD"); f.Name != "coord" || f.Owner != id("a") {
		t.Fatalf("file %+v", f)
	}
	entries, err := os.ReadDir(filepath.Join(e.r.Dir, "inboxes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if en.Name() != "coord.json" && en.Name() != "coord.lock" {
			t.Fatalf("unexpected file %s", en.Name())
		}
	}
	if got := ids(t, e.r, "cOORD"); !slices.Equal(got, []string{id("a")}) {
		t.Fatalf("members %v", got)
	}
	if _, err := e.r.GrantInbox(a, "COORD", id("b"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.AttachInbox(b, "cOord", noHook); err != nil {
		t.Fatal(err)
	}
}
