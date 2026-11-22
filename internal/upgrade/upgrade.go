// Package upgrade runs the plugin and gateway steps of an upgrade once per
// version, under a lock shared with `crosstalk install` (ADR 0031).
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func LockPath(stateDir string) string { return filepath.Join(stateDir, "upgrade.lock") }

func dir(stateDir, version string) string { return filepath.Join(stateDir, "upgrade", version) }

func MarkerPath(stateDir, version, target string) string {
	return filepath.Join(dir(stateDir, version), target+".done")
}

func LogPath(stateDir, version string) string {
	return filepath.Join(dir(stateDir, version), "follow-up.log")
}

func Done(stateDir, version, target string) bool {
	_, err := os.Stat(MarkerPath(stateDir, version, target))
	return err == nil
}

func MarkDone(stateDir, version, target string) error {
	if err := os.MkdirAll(dir(stateDir, version), 0o700); err != nil {
		return err
	}
	return os.WriteFile(MarkerPath(stateDir, version, target), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func openLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(LockPath(stateDir), os.O_CREATE|os.O_RDWR, 0o600)
}

// TryLock takes upgrade.lock without waiting; ok is false when another
// process or another open of the file holds it. The descriptor is
// close-on-exec, so an exec or a child process never keeps the lock.
func TryLock(stateDir string) (release func(), ok bool, err error) {
	f, err := openLock(stateDir)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

// Lock takes upgrade.lock, calling waiting once if it has to wait.
func Lock(ctx context.Context, stateDir string, waiting func()) (release func(), err error) {
	for first := true; ; first = false {
		rel, ok, err := TryLock(stateDir)
		if err != nil || ok {
			return rel, err
		}
		if first && waiting != nil {
			waiting()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Target is one thing an upgrade brings up to date: Run is its install step,
// Verify the check that it is now at the binary's version.
type Target struct {
	Name   string
	Run    func(ctx context.Context, log io.Writer) error
	Verify func(ctx context.Context) error
}

// Runner is the follow-up of ADR 0031. Run makes one attempt; when that
// attempt held the lock and a target failed, it tries again every Interval
// until Bound has passed or every target is done.
type Runner struct {
	StateDir, Version string
	Targets           []Target
	// Notice receives one line after an attempt that ran a target.
	Notice          func(string)
	Interval, Bound time.Duration
	After           func(time.Duration) <-chan time.Time

	beforeLock func()
}

func (r *Runner) pending() []Target {
	var p []Target
	for _, t := range r.Targets {
		if !Done(r.StateDir, r.Version, t.Name) {
			p = append(p, t)
		}
	}
	return p
}

type outcome struct {
	name string
	err  error
}

func (r *Runner) Run(ctx context.Context) {
	took, pending := r.attempt(ctx, true)
	if !took || !pending || r.Interval <= 0 {
		return
	}
	after := r.After
	if after == nil {
		after = time.After
	}
	for waited := time.Duration(0); pending && waited+r.Interval <= r.Bound; waited += r.Interval {
		select {
		case <-ctx.Done():
			return
		case <-after(r.Interval):
		}
		_, pending = r.attempt(ctx, false)
	}
}

// attempt reports whether it held the lock and whether a target is still
// pending after it.
func (r *Runner) attempt(ctx context.Context, first bool) (took, pending bool) {
	before := r.pending()
	if ctx.Err() != nil || len(before) == 0 {
		return false, false
	}
	if r.beforeLock != nil {
		r.beforeLock()
	}
	release, ok, err := TryLock(r.StateDir)
	if err != nil || !ok {
		return false, true
	}
	defer release()
	todo := r.pending()
	if len(todo) == 0 {
		return true, false
	}
	if err := os.MkdirAll(dir(r.StateDir, r.Version), 0o700); err != nil {
		return true, true
	}
	log, err := os.OpenFile(LogPath(r.StateDir, r.Version), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log = nil
	}
	var w io.Writer = io.Discard
	if log != nil {
		defer log.Close()
		w = log
	}
	var outs []outcome
	for _, t := range todo {
		if ctx.Err() != nil {
			return true, true
		}
		fmt.Fprintf(w, "%s %s: start\n", time.Now().UTC().Format(time.RFC3339), t.Name)
		err := runTarget(ctx, t, w)
		if err == nil {
			err = MarkDone(r.StateDir, r.Version, t.Name)
		}
		if err != nil {
			pending = true
			fmt.Fprintf(w, "%s %s: failed: %v\n", time.Now().UTC().Format(time.RFC3339), t.Name, err)
		} else {
			fmt.Fprintf(w, "%s %s: done\n", time.Now().UTC().Format(time.RFC3339), t.Name)
		}
		outs = append(outs, outcome{t.Name, err})
	}
	if n := r.notice(outs, first); n != "" && r.Notice != nil {
		r.Notice(n)
	}
	return true, pending
}

func runTarget(ctx context.Context, t Target, w io.Writer) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	if err := t.Run(ctx, w); err != nil {
		return err
	}
	return t.Verify(ctx)
}

// notice is the line for the session: after a retry, only targets that now
// passed are worth telling.
func (r *Runner) notice(outs []outcome, first bool) string {
	var ok, bad []string
	for _, o := range outs {
		if o.err == nil {
			ok = append(ok, o.name)
		} else {
			bad = append(bad, o.name+" ("+o.err.Error()+")")
		}
	}
	if !first && len(ok) == 0 {
		return ""
	}
	var parts []string
	if len(ok) > 0 {
		parts = append(parts, "updated "+strings.Join(ok, ", "))
	}
	if first && len(bad) > 0 {
		parts = append(parts, "could not update "+strings.Join(bad, ", ")+"; run `crosstalk install`")
	}
	return "CrossTalk " + r.Version + " upgrade: " + strings.Join(parts, "; ")
}
