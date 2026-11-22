package refresh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		Probe: func(_ context.Context, p string) error {
			r.probes++
			cur, _ := mcpserver.IdentityOf(p)
			r.probed = append(r.probed, cur.Ino)
			return nil
		},
		Quiesce: func(context.Context) (func(), error) { r.quiesces++; return func() { r.undos++ }, nil },
		Exec:    func(mcpserver.BinaryIdentity) error { r.execs++; return nil },
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
		if stats == 2 {
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
	w.Exec = func(mcpserver.BinaryIdentity) error { r.execs++; e := errs[0]; errs = errs[1:]; return e }
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
	w.Exec = func(mcpserver.BinaryIdentity) error { r.execs++; return ErrExecFailed }
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
