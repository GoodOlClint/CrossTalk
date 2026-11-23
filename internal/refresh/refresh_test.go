package refresh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
)

type rec struct {
	stats, probes, quiesces, undos, execs int
	probed                                []uint64
}

func (r *rec) watcher(path string, id mcpserver.BinaryIdentity, stat func(string) (mcpserver.BinaryIdentity, error)) *Watcher {
	return &Watcher{
		Path: path,
		ID:   id,
		Stat: func(p string) (mcpserver.BinaryIdentity, error) { r.stats++; return stat(p) },
		Probe: func(_ context.Context, p string) (Probed, error) {
			r.probes++
			cur, _ := mcpserver.IdentityOf(p)
			r.probed = append(r.probed, cur.Ino)
			return Probed{}, nil
		},
		Quiesce: func(context.Context) (func(), error) { r.quiesces++; return func() { r.undos++ }, nil },
		Exec:    func(mcpserver.BinaryIdentity, Probed) error { r.execs++; return nil },
	}
}

// An unchanged binary costs one stat a tick: no command runs (no lsof, no
// probe) and nothing is paused. A fake lsof on PATH records any run.
func TestRefreshNoLsofOnTick(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "lsof-ran")
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	id, err := mcpserver.IdentityOf(bin)
	if err != nil {
		t.Fatal(err)
	}
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	for range 5 {
		w.Tick(context.Background())
	}
	if r.stats != 5 || r.probes != 0 || r.quiesces != 0 || r.execs != 0 {
		t.Fatalf("5 ticks on an unchanged binary: %+v, want 5 stats and nothing else", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("lsof ran on a tick")
	}
}

// A file replaced again between the probe and the exec is not exec'd; the
// next tick probes the newer file.
func TestRefreshInodeChangedAfterProbe(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	write := func(body string) {
		tmp := bin + ".new"
		if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		os.Link(bin, bin+".was-"+body)
		if err := os.Rename(tmp, bin); err != nil {
			t.Fatal(err)
		}
	}
	write("v1")
	id, _ := mcpserver.IdentityOf(bin)
	write("v2")
	v2, _ := mcpserver.IdentityOf(bin)
	var r rec
	stats := 0
	w := r.watcher(bin, id, func(p string) (mcpserver.BinaryIdentity, error) {
		stats++
		if stats == 3 {
			write("v3")
		}
		return mcpserver.IdentityOf(p)
	})
	w.Tick(context.Background())
	if r.execs != 0 || r.probes != 1 || r.undos != 1 {
		t.Fatalf("first tick: %+v, want one probe, undone, no exec", r)
	}
	v3, _ := mcpserver.IdentityOf(bin)
	w.Tick(context.Background())
	if r.probes != 2 || r.probed[0] != v2.Ino || r.probed[1] != v3.Ino || r.execs != 1 {
		t.Fatalf("second tick: %+v, want the newer file (ino %d after %d) probed and exec'd", r, v3.Ino, v2.Ino)
	}
}

// A handoff error before exec is tried again on the next tick; an exec
// failure rules the file out until the path changes.
func TestRefreshRetriesAfterTransientExecError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	errs := []error{errors.New("disk full"), ErrExecFailed}
	w.Exec = func(mcpserver.BinaryIdentity, Probed) error { r.execs++; e := errs[0]; errs = errs[1:]; return e }
	for range 3 {
		w.Tick(context.Background())
	}
	if r.execs != 2 || r.probes != 2 {
		t.Fatalf("%+v, want a retry after the transient error and none after the exec failure", r)
	}
}

// A failed Exec undoes the quiesce exactly once, so the gate admits work
// again and this shim keeps serving on its current binary.
func TestRefreshUndoesQuiesceAfterFailedExec(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	w.Quiesce = func(context.Context) (func(), error) {
		r.quiesces++
		if !quiesce.Pause() {
			t.Fatal("pause in progress")
		}
		return func() { r.undos++; quiesce.Unpause() }, nil
	}
	w.Exec = func(mcpserver.BinaryIdentity, Probed) error { r.execs++; return ErrExecFailed }
	w.Tick(context.Background())
	if r.execs != 1 || r.undos != 1 {
		t.Fatalf("%+v, want one exec and one undo", r)
	}
	done, ok := quiesce.TryEnter()
	if !ok {
		quiesce.Unpause()
		t.Fatal("the gate still refuses work after a failed exec")
	}
	done()
}

// A file that never quiesces is tried QuiesceTries times, then left until the
// path changes again, and GaveUp is told once; a newer file is tried afresh.
func TestRefreshStopsAfterBoundedQuiesceTries(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.Link(bin, bin+".was-v1")
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	w.QuiesceTries = 3
	w.Quiesce = func(context.Context) (func(), error) { r.quiesces++; return nil, context.DeadlineExceeded }
	var gaveUp []string
	w.GaveUp = func(_, why string) { gaveUp = append(gaveUp, why) }
	for range 6 {
		w.Tick(context.Background())
	}
	if r.quiesces != 3 || r.execs != 0 || len(gaveUp) != 1 || !strings.Contains(gaveUp[0], "3 attempts") {
		t.Fatalf("%+v, gave up %v: want 3 quiesce attempts, no exec, one give-up after 3", r, gaveUp)
	}
	os.Link(bin, bin+".was-v2")
	os.WriteFile(bin+".n", []byte("v3"), 0o755)
	os.Rename(bin+".n", bin)
	w.Tick(context.Background())
	if r.quiesces != 4 {
		t.Fatalf("quiesces = %d after a newer file, want it tried again", r.quiesces)
	}
}

// A pre-exec failure after quiesce (a handoff too large to write) counts
// toward the same bound, so ingress is not closed every tick forever.
func TestRefreshStopsAfterBoundedExecErrors(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	w.QuiesceTries = 2
	w.Exec = func(mcpserver.BinaryIdentity, Probed) error { r.execs++; return errors.New("handoff too large") }
	gaveUp := 0
	w.GaveUp = func(string, string) { gaveUp++ }
	for range 4 {
		w.Tick(context.Background())
	}
	if r.quiesces != 2 || gaveUp != 1 {
		t.Fatalf("%+v, gave up %d times: want 2 attempts then one give-up", r, gaveUp)
	}
}

func TestProbeReadsRefreshListens(t *testing.T) {
	dir := t.TempDir()
	for name, out := range map[string]string{"old": `{"version":"0.2.4","refresh_handoff":1}`, "new": `{"version":"0.2.5","refresh_handoff":1,"refresh_listens":true}`} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("#!/bin/sh\necho '"+out+"'\n"), 0o755)
		got, err := Probe(context.Background(), p)
		if err != nil || got.ReplaysListens != (name == "new") {
			t.Fatalf("%s: Probe = %+v, %v", name, got, err)
		}
	}
}

// With a listen open, a file that cannot replay it is refused before any
// quiesce, so ingress is never closed for it, and GaveUp says why; a file
// that can is probed once, before quiesce, and the probe result reaches Exec.
func TestRefreshListenCapabilityDecidedBeforeQuiesce(t *testing.T) {
	for _, replays := range []bool{false, true} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "crosstalk")
		os.WriteFile(bin, []byte("v1"), 0o755)
		id, _ := mcpserver.IdentityOf(bin)
		os.WriteFile(bin+".n", []byte("v2"), 0o755)
		os.Rename(bin+".n", bin)
		var events []string
		var gaveUp []string
		w := &Watcher{
			Path: bin, ID: id,
			Probe: func(context.Context, string) (Probed, error) {
				events = append(events, "probe")
				return Probed{ReplaysListens: replays}, nil
			},
			Listening: func() (int, int) { return 1, 0 },
			Quiesce:   func(context.Context) (func(), error) { events = append(events, "quiesce"); return func() {}, nil },
			Exec: func(_ mcpserver.BinaryIdentity, p Probed) error {
				events = append(events, "exec")
				if !p.ReplaysListens {
					t.Error("Exec got a probe result without listen replay")
				}
				return errors.New("transient")
			},
			GaveUp: func(_, why string) { gaveUp = append(gaveUp, why) },
		}
		w.Tick(context.Background())
		w.Tick(context.Background())
		if !replays {
			if len(events) != 1 || events[0] != "probe" || len(gaveUp) != 1 || !strings.Contains(gaveUp[0], "subscriptions/listen") {
				t.Fatalf("no replay: events %v, gave up %v: want one probe, no quiesce, one give-up naming the listen", events, gaveUp)
			}
			continue
		}
		if want := []string{"probe", "quiesce", "exec", "probe", "quiesce", "exec"}; !slices.Equal(events, want) {
			t.Fatalf("replay: events %v, want %v (no probe inside the quiesce window)", events, want)
		}
	}
}

// A listen read but not yet acknowledged may still fail validation, so the
// decision waits: nothing is refused and nothing is quiesced until it settles.
func TestRefreshDefersWhileAListenSettles(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	settling := 1
	w.Listening = func() (int, int) { return 0, settling }
	gaveUp := 0
	w.GaveUp = func(string, string) { gaveUp++ }
	w.Tick(context.Background())
	if r.quiesces != 0 || gaveUp != 0 {
		t.Fatalf("%+v, gave up %d while a listen settles: want neither", r, gaveUp)
	}
	settling = 0
	w.Tick(context.Background())
	if r.quiesces != 1 || r.execs != 1 || gaveUp != 0 {
		t.Fatalf("%+v, gave up %d after the listen failed validation: want the file taken", r, gaveUp)
	}
}

// An exec refused or failed (ErrExecFailed) reaches GaveUp, so the session
// is told.
func TestRefreshExecFailedGivesNotice(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	w.Exec = func(mcpserver.BinaryIdentity, Probed) error { r.execs++; return ErrExecFailed }
	gaveUp := 0
	w.GaveUp = func(string, string) { gaveUp++ }
	w.Tick(context.Background())
	w.Tick(context.Background())
	if r.execs != 1 || gaveUp != 1 {
		t.Fatalf("%+v, gave up %d: want one exec and one give-up", r, gaveUp)
	}
}

// ErrBusy from Quiesce defers the tick without using up the bounded tries.
func TestRefreshBusyDoesNotCountAsFailedTry(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	os.WriteFile(bin, []byte("v1"), 0o755)
	id, _ := mcpserver.IdentityOf(bin)
	os.Link(bin, bin+".was-v1")
	os.WriteFile(bin+".n", []byte("v2"), 0o755)
	os.Rename(bin+".n", bin)
	var r rec
	w := r.watcher(bin, id, mcpserver.IdentityOf)
	w.QuiesceTries = 2
	busy := true
	quiet := w.Quiesce
	w.Quiesce = func(ctx context.Context) (func(), error) {
		if busy {
			r.quiesces++
			return nil, ErrBusy
		}
		return quiet(ctx)
	}
	var gaveUp int
	w.GaveUp = func(string, string) { gaveUp++ }
	for range 5 {
		w.Tick(context.Background())
	}
	busy = false
	w.Tick(context.Background())
	if gaveUp != 0 || r.execs != 1 {
		t.Fatalf("gave up %d times, execs %d: want a busy tick uncounted and an exec once quiet", gaveUp, r.execs)
	}
}
