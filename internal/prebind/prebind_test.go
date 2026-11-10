package prebind

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var t0 = time.Now().UTC().Truncate(time.Second)

const (
	senderA = "aaaaaaaaaaaaaaaa"
	senderB = "bbbbbbbbbbbbbbbb"
	target  = "cccccccccccccccc"
)

type rig struct {
	t     *testing.T
	dir   string
	store *Store
	now   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), now: t0}
	r.store = New(r.dir, Options{Now: func() time.Time { return r.now }})
	r.token(senderA)
	r.token(senderB)
	return r
}

func (r *rig) token(id string) {
	r.t.Helper()
	p := filepath.Join(r.dir, "tokens", id+".token")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("secret-"+id+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func body(id, text string) string {
	m := msg.Message{MessageID: id, ContextID: id, From: "alice", To: "bob", Kind: msg.Inform, Delivery: msg.Immediate, HopChain: []string{msg.Hop(senderA)}, SentAt: t0, Parts: []msg.Part{{Type: "text", Text: text}}}
	b, _ := json.Marshal(m)
	return string(b)
}

func entry(sender, id string) Entry {
	return Entry{AgentID: target, Harness: "claude", SessionRef: "ref-1", Sender: sender, Message: body(id, "hello"), DeliveredAt: t0, State: "accepted"}
}

var tgt = Target{AgentID: target, Harness: "claude", SessionRef: "ref-1"}

func (r *rig) drain(tg Target) []Entry {
	r.t.Helper()
	var got []Entry
	if err := r.store.Drain(tg, func(e Entry) error { got = append(got, e); return nil }); err != nil {
		r.t.Fatal(err)
	}
	return got
}

func (r *rig) dataPath(id string) string { return filepath.Join(r.dir, "prebind", id+".jsonl") }
func (r *rig) claimedPath(id string) string {
	return filepath.Join(r.dir, "prebind", id+".claimed")
}

func TestPreBindEntryRoundTrip(t *testing.T) {
	r := newRig(t)
	if err := r.store.Append(entry(senderA, "m1")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(r.dataPath(target))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("data file: %v %v", fi, err)
	}
	di, _ := os.Stat(filepath.Join(r.dir, "prebind"))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	got := r.drain(tgt)
	if len(got) != 1 || got[0].Sender != senderA || got[0].State != "accepted" || !got[0].DeliveredAt.Equal(t0) || !strings.Contains(got[0].Message, `"m1"`) {
		t.Fatalf("got %+v", got)
	}
	if _, err := os.Stat(r.dataPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data file survives a drain")
	}
	if _, err := os.Stat(r.claimedPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("claimed file survives a completed drain")
	}
	if got := r.drain(tgt); len(got) != 0 {
		t.Fatalf("second drain returned %d", len(got))
	}
}

func TestPreBindDropsBadMACWrongBindingModeSymlinkNoToken(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *rig)
		tg     Target
	}{
		{name: "bad mac", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.Message = body("m1", "forged text") })
		}},
		{name: "wrong sender claimed", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.Sender = senderB })
		}},
		{name: "wrong binding", tg: Target{AgentID: target, Harness: "claude", SessionRef: "ref-2"}},
		{name: "wrong harness", tg: Target{AgentID: target, Harness: "codex", SessionRef: "ref-1"}},
		{name: "wrong agent", tg: Target{AgentID: "dddddddddddddddd", Harness: "claude", SessionRef: "ref-1"}, mutate: func(r *rig) {
			os.Rename(r.dataPath(target), r.dataPath("dddddddddddddddd"))
		}},
		{name: "wrong mode", tg: tgt, mutate: func(r *rig) { os.Chmod(r.dataPath(target), 0o644) }},
		{name: "symlink", tg: tgt, mutate: func(r *rig) {
			real := filepath.Join(r.dir, "elsewhere.jsonl")
			os.Rename(r.dataPath(target), real)
			os.Symlink(real, r.dataPath(target))
		}},
		{name: "no token", tg: tgt, mutate: func(r *rig) { os.Remove(filepath.Join(r.dir, "tokens", senderA+".token")) }},
		{name: "token wrong mode", tg: tgt, mutate: func(r *rig) { os.Chmod(filepath.Join(r.dir, "tokens", senderA+".token"), 0o644) }},
		{name: "token changed", tg: tgt, mutate: func(r *rig) {
			os.WriteFile(filepath.Join(r.dir, "tokens", senderA+".token"), []byte("other\n"), 0o600)
		}},
		{name: "remote sender", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.Sender = "remote:abcd/" + senderA })
		}},
		{name: "local-user sender", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.Sender = "local-user" })
		}},
		{name: "traversal sender", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.Sender = "../peer" })
		}},
		{name: "unknown version", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.V = 2 })
		}},
		{name: "state not delivered", tg: tgt, mutate: func(r *rig) {
			rewrite(t, r.dataPath(target), func(e *Entry) { e.State = "rejected" })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			if err := r.store.Append(entry(senderA, "m1")); err != nil {
				t.Fatal(err)
			}
			if c.mutate != nil {
				c.mutate(r)
			}
			if got := r.drain(c.tg); len(got) != 0 {
				t.Fatalf("returned %+v", got)
			}
		})
	}
}

func rewrite(t *testing.T, path string, f func(*Entry)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	f(&e)
	out, _ := json.Marshal(e)
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreBindCapsSkipWrite(t *testing.T) {
	t.Run("entries per target", func(t *testing.T) {
		r := newRig(t)
		for i := range MaxEntries {
			if err := r.store.Append(entry(senderA, fmt.Sprintf("m%d", i))); err != nil {
				t.Fatalf("entry %d: %v", i, err)
			}
		}
		if err := r.store.Append(entry(senderA, "over")); !errors.Is(err, ErrCap) {
			t.Fatalf("65th entry: %v", err)
		}
		if got := r.drain(tgt); len(got) != MaxEntries {
			t.Fatalf("older entries lost: %d", len(got))
		}
	})
	t.Run("bytes per target", func(t *testing.T) {
		r := newRig(t)
		big := entry(senderA, "big")
		big.Message = body("big", strings.Repeat("x", 100<<10))
		n := 0
		for {
			big.Message = body(fmt.Sprintf("big%d", n), strings.Repeat("x", 100<<10))
			if err := r.store.Append(big); err != nil {
				if !errors.Is(err, ErrCap) {
					t.Fatal(err)
				}
				break
			}
			n++
			if n > MaxEntries {
				t.Fatal("byte cap never hit")
			}
		}
		fi, _ := os.Stat(r.dataPath(target))
		if n < 30 || fi.Size() > MaxTargetBytes {
			t.Fatalf("n=%d size=%d", n, fi.Size())
		}
	})
	t.Run("files per directory with claimed files counted", func(t *testing.T) {
		r := newRig(t)
		for i := range MaxFiles - 2 {
			id := fmt.Sprintf("%016x", i+1)
			e := entry(senderA, "m")
			e.AgentID = id
			if err := r.store.Append(e); err != nil {
				t.Fatalf("file %d: %v", i, err)
			}
		}
		for _, id := range []string{"eeeeeeeeeeeeee01", "eeeeeeeeeeeeee02"} {
			if err := os.WriteFile(filepath.Join(r.dir, "prebind", id+".claimed"), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := entry(senderA, "m")
		e.AgentID = "ffffffffffffffff"
		if err := r.store.Append(e); !errors.Is(err, ErrCap) {
			t.Fatalf("new file over the cap: %v", err)
		}
		e.AgentID = fmt.Sprintf("%016x", 1)
		e.Message = body("another", "x")
		if err := r.store.Append(e); err != nil {
			t.Fatalf("append to existing file at the file cap: %v", err)
		}
	})
	t.Run("bytes per directory with claimed files counted", func(t *testing.T) {
		r := newRig(t)
		if err := os.MkdirAll(filepath.Join(r.dir, "prebind"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, "prebind", "eeeeeeeeeeeeee01.claimed"), make([]byte, MaxDirBytes-100), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.store.Append(entry(senderA, "m1")); !errors.Is(err, ErrCap) {
			t.Fatalf("append past the directory bytes: %v", err)
		}
	})
}

func TestPreBindSweepsEntriesOlderThanTTL(t *testing.T) {
	r := newRig(t)
	old := entry(senderA, "old")
	old.AgentID = "eeeeeeeeeeeeee01"
	if err := r.store.Append(old); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(r.dir, "prebind", "eeeeeeeeeeeeee02.claimed")
	os.WriteFile(stale, []byte("x\n"), 0o600)
	oldTime := t0.Add(-2 * time.Hour)
	os.Chtimes(r.dataPath("eeeeeeeeeeeeee01"), oldTime, oldTime)
	os.Chtimes(stale, oldTime, oldTime)
	fresh := filepath.Join(r.dir, "prebind", "eeeeeeeeeeeeee03.claimed")
	os.WriteFile(fresh, []byte("x\n"), 0o600)
	os.Chtimes(fresh, t0, t0)

	if err := r.store.Append(entry(senderA, "m1")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{r.dataPath("eeeeeeeeeeeeee01"), stale} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survives a write", p)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh claimed file swept")
	}

	r.now = t0.Add(30 * time.Minute)
	other := entry(senderA, "m-old-delivery")
	other.DeliveredAt = t0.Add(-2 * time.Hour)
	if err := r.store.Append(other); err != nil {
		t.Fatal(err)
	}
	got := r.drain(tgt)
	if len(got) != 1 || !strings.Contains(got[0].Message, `"m1"`) {
		t.Fatalf("an entry delivered more than a TTL before the drain is replayed: %+v", got)
	}

	old2 := entry(senderA, "old2")
	old2.AgentID = "eeeeeeeeeeeeee04"
	r.store.Append(old2)
	os.Chtimes(r.dataPath("eeeeeeeeeeeeee04"), oldTime, oldTime)
	r.drain(tgt)
	if _, err := os.Stat(r.dataPath("eeeeeeeeeeeeee04")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a drain does not sweep")
	}
}

func TestPreBindDrainCrashPoints(t *testing.T) {
	boom := errors.New("crash")
	three := func(r *rig) {
		for _, id := range []string{"m1", "m2", "m3"} {
			if err := r.store.Append(entry(senderA, id)); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	ids := func(es []Entry) string {
		var out []string
		for _, e := range es {
			var m msg.Message
			json.Unmarshal([]byte(e.Message), &m)
			out = append(out, m.MessageID)
		}
		return strings.Join(out, ",")
	}

	t.Run("after the rename", func(t *testing.T) {
		r := newRig(t)
		three(r)
		if err := r.store.Drain(tgt, func(Entry) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
		if _, err := os.Stat(r.claimedPath(target)); err != nil {
			t.Fatal("no claimed file left")
		}
		if got := ids(r.drain(tgt)); got != "m1,m2,m3" {
			t.Fatalf("next drain: %s", got)
		}
		if _, err := os.Stat(r.claimedPath(target)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("claimed file survives the finishing drain")
		}
	})
	t.Run("partly consumed", func(t *testing.T) {
		r := newRig(t)
		three(r)
		n := 0
		r.store.Drain(tgt, func(Entry) error {
			n++
			if n == 2 {
				return boom
			}
			return nil
		})
		if got := ids(r.drain(tgt)); got != "m1,m2,m3" {
			t.Fatalf("next drain: %s", got)
		}
	})
	t.Run("leftover claimed file then a newer data file", func(t *testing.T) {
		r := newRig(t)
		three(r)
		r.store.Drain(tgt, func(Entry) error { return boom })
		if err := r.store.Append(entry(senderA, "m4")); err != nil {
			t.Fatal(err)
		}
		if got := ids(r.drain(tgt)); got != "m1,m2,m3,m4" {
			t.Fatalf("drain: %s", got)
		}
		if got := r.drain(tgt); len(got) != 0 {
			t.Fatal("entries replayed twice")
		}
	})
	t.Run("a writer after the rename loses nothing", func(t *testing.T) {
		r := newRig(t)
		three(r)
		var wrote bool
		got := ""
		r.store.Drain(tgt, func(e Entry) error {
			if !wrote {
				wrote = true
				if err := r.store.Append(entry(senderA, "late")); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		got = ids(r.drain(tgt))
		if got != "late" {
			t.Fatalf("late entry: %q", got)
		}
	})
}

func TestPreBindCollisionDropsConflictingSenders(t *testing.T) {
	r := newRig(t)
	for _, e := range []Entry{entry(senderA, "same"), entry(senderB, "same"), entry(senderA, "unique")} {
		if err := r.store.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	got := r.drain(tgt)
	if len(got) != 1 || !strings.Contains(got[0].Message, `"unique"`) {
		t.Fatalf("got %+v", got)
	}
}
