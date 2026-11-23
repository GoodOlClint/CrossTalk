// Package refresh re-execs a running shim when the binary at its trusted path
// is replaced (ADR 0030): a stat tick detects the change, `version --json`
// probes the new file, and the caller's quiesce and exec do the rest.
package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

// ErrExecFailed marks an Exec error from exec itself: the candidate file is
// not tried again until the path changes once more. Any other Exec error is
// tried again on the next tick.
var ErrExecFailed = errors.New("refresh: exec failed")

// ErrBusy from Quiesce: long background work is running; the tick ends without
// counting a failed try, and the next tick tries again.
var ErrBusy = errors.New("refresh: background work is running")

// OffEnv set to anything but "" or "0" turns refresh off.
const OffEnv = "CROSSTALK_NO_AUTO_REFRESH"

const (
	DefaultInterval     = 2 * time.Second
	DefaultProbeTimeout = 5 * time.Second
	DefaultQuiesceCap   = 10 * time.Second
	// DefaultQuiesceTries bounds the quiesce attempts for one new file; each
	// closes peer ingress for up to QuiesceCap.
	DefaultQuiesceTries = 5
)

// Off reports whether OffEnv turns refresh off.
func Off() bool {
	v := os.Getenv(OffEnv)
	return v != "" && v != "0"
}

// Trusted reports whether argv0 may be refreshed: it must be absolute and
// registered must say a harness registration runs it.
func Trusted(argv0 string, registered func(bin string) bool) bool {
	return filepath.IsAbs(argv0) && registered(argv0)
}

// Watcher checks Path every Interval against ID and execs a new binary.
type Watcher struct {
	Path string
	// ID is the identity recorded once for this process image.
	ID           mcpserver.BinaryIdentity
	Interval     time.Duration
	ProbeTimeout time.Duration
	QuiesceCap   time.Duration
	QuiesceTries int
	// Stat reads the identity of Path; nil means mcpserver.IdentityOf.
	Stat func(path string) (mcpserver.BinaryIdentity, error)
	// Probe checks that the file at path can take the handoff; nil means
	// Probe. It runs before Quiesce, so nothing is run while ingress is
	// closed.
	Probe func(ctx context.Context, path string) (Probed, error)
	// Listening counts the session's subscriptions/listen requests: open
	// ones only a file that replays listens may take, and settling ones not
	// acknowledged yet, which defer the decision to a later tick.
	Listening func() (open, settling int)
	// Busy, when set and true, ends a tick before the probe: long background
	// work runs, and a later tick looks again without a failed try.
	Busy func() bool
	// Quiesce stops new work and waits for running work until ctx ends; undo
	// restarts it.
	Quiesce func(ctx context.Context) (undo func(), err error)
	// Exec hands over to the file whose identity is id and whose probe gave
	// p; it returns only on failure.
	Exec func(id mcpserver.BinaryIdentity, p Probed) error
	// GaveUp, when set, is told why the file at Path will not be taken; it is
	// not tried again until the path changes.
	GaveUp func(path, why string)
	Log    *slog.Logger

	bad   mcpserver.BinaryIdentity
	tried mcpserver.BinaryIdentity
	tries int
}

// Run ticks until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	iv := w.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

func same(a, b mcpserver.BinaryIdentity) bool { return a.Dev == b.Dev && a.Ino == b.Ino }

// Tick is one check. A file that failed its probe is not probed again until
// the path changes once more.
func (w *Watcher) Tick(ctx context.Context) {
	stat := w.Stat
	if stat == nil {
		stat = mcpserver.IdentityOf
	}
	cur, err := stat(w.Path)
	if err != nil || same(cur, w.ID) || same(cur, w.bad) {
		return
	}
	if w.Busy != nil && w.Busy() {
		return
	}
	probe := w.Probe
	if probe == nil {
		probe = Probe
	}
	pctx, cancel := context.WithTimeout(ctx, orDefault(w.ProbeTimeout, DefaultProbeTimeout))
	p, err := probe(pctx, w.Path)
	cancel()
	if again, serr := stat(w.Path); serr != nil || !same(again, cur) {
		return
	}
	if err != nil {
		w.bad = cur
		w.log().Warn("refresh: new binary not taken", "path", w.Path, "err", err)
		return
	}
	if !p.ReplaysListens && w.Listening != nil {
		open, settling := w.Listening()
		if settling > 0 {
			return
		}
		if open > 0 {
			w.giveUp(cur, "it cannot take over the session's open subscriptions/listen", nil)
			return
		}
	}
	qctx, cancel := context.WithTimeout(ctx, orDefault(w.QuiesceCap, DefaultQuiesceCap))
	undo, err := w.Quiesce(qctx)
	cancel()
	if errors.Is(err, ErrBusy) {
		return
	}
	if err != nil {
		if !w.failed(cur, err) {
			w.log().Info("refresh: not quiescent; trying again later", "err", err)
		}
		return
	}
	if again, err := stat(w.Path); err != nil || !same(again, cur) {
		undo()
		return
	}
	err = w.Exec(cur, p)
	undo()
	if errors.Is(err, ErrExecFailed) {
		w.giveUp(cur, "it could not be exec'd", err)
		return
	}
	w.log().Warn("refresh: not exec'd; staying on this binary", "path", w.Path, "err", err)
	w.failed(cur, err)
}

// giveUp leaves cur until the path changes again and says why.
func (w *Watcher) giveUp(cur mcpserver.BinaryIdentity, why string, err error) {
	w.bad = cur
	w.log().Warn("refresh: staying on this binary until the path changes again", "path", w.Path, "why", why, "err", err)
	if w.GaveUp != nil {
		w.GaveUp(w.Path, why)
	}
}

// failed counts a quiesce or pre-exec failure for cur; at QuiesceTries in a
// row cur is left until the path changes again.
func (w *Watcher) failed(cur mcpserver.BinaryIdentity, err error) bool {
	if !same(cur, w.tried) {
		w.tried, w.tries = cur, 0
	}
	w.tries++
	if w.tries < orDefaultInt(w.QuiesceTries, DefaultQuiesceTries) {
		return false
	}
	w.giveUp(cur, fmt.Sprintf("it did not take the handoff in %d attempts", w.tries), err)
	return true
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func orDefaultInt(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

func (w *Watcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// Probed is what a probe learned about the new file.
type Probed struct {
	// ReplaysListens: the file takes over the open subscriptions/listen
	// requests a handoff carries (`refresh_listens` in `version --json`).
	ReplaysListens bool
}

// Probe runs `path version --json` and requires a version and the
// refresh_handoff format this binary writes.
func Probe(ctx context.Context, path string) (Probed, error) {
	out, err := exec.CommandContext(ctx, path, "version", "--json").Output()
	if err != nil {
		return Probed{}, fmt.Errorf("version --json: %w", err)
	}
	var v struct {
		Version        string `json:"version"`
		RefreshHandoff *int   `json:"refresh_handoff"`
		RefreshListens bool   `json:"refresh_listens"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Probed{}, fmt.Errorf("version --json: %w", err)
	}
	switch {
	case v.Version == "":
		return Probed{}, errors.New("version --json: no version")
	case v.RefreshHandoff == nil:
		return Probed{}, errors.New("version --json: no refresh_handoff")
	case *v.RefreshHandoff != mcpserver.HandoffFormat:
		return Probed{}, fmt.Errorf("version --json: refresh_handoff %d, this binary writes %d", *v.RefreshHandoff, mcpserver.HandoffFormat)
	}
	return Probed{ReplaysListens: v.RefreshListens}, nil
}
