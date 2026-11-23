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
	f, err := tryLock(stateDir)
	if f == nil {
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

func tryLock(stateDir string) (*os.File, error) {
	f, err := openLock(stateDir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	reapOrphan(ChildPath(stateDir))
	return f, nil
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
	// Eligible, when set, is asked again before every attempt; a target it
	// rejects is skipped.
	Eligible func() bool
}

// Runner is the follow-up of ADR 0031. Run makes one attempt; while a target
// is still pending after it, because a target failed or another process held
// the lock, it tries again every Interval until Bound has passed or every
// target is done.
type Runner struct {
	StateDir, Version string
	Targets           []Target
	// Notice receives one line after an attempt that ran a target.
	Notice          func(string)
	Interval, Bound time.Duration
	// TargetTimeout bounds one target's Run and Verify together; 0 means none.
	TargetTimeout time.Duration
	After         func(time.Duration) <-chan time.Time
	Now           func() time.Time
	// Enter, when set, counts an attempt from before the lock until after its
	// notice; ok false skips the attempt.
	Enter func(ctx context.Context) (leave func(), ok bool)

	beforeLock func()
}

func (r *Runner) pending() []Target {
	var p []Target
	for _, t := range r.Targets {
		if !Done(r.StateDir, r.Version, t.Name) && (t.Eligible == nil || t.Eligible()) {
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
	ran, pending := r.attempt(ctx, true)
	if !pending || r.Interval <= 0 {
		return
	}
	after, now := r.After, r.Now
	if after == nil {
		after = time.After
	}
	if now == nil {
		now = time.Now
	}
	start := now()
	for pending && now().Sub(start)+r.Interval <= r.Bound {
		select {
		case <-ctx.Done():
			return
		case <-after(r.Interval):
		}
		var now bool
		now, pending = r.attempt(ctx, !ran)
		ran = ran || now
	}
}

// attempt reports whether it ran a target and whether a target is still
// pending after it.
func (r *Runner) attempt(ctx context.Context, first bool) (ran, pending bool) {
	before := r.pending()
	if ctx.Err() != nil || len(before) == 0 {
		return false, false
	}
	if r.Enter != nil {
		leave, ok := r.Enter(ctx)
		if !ok {
			return false, true
		}
		defer leave()
	}
	if r.beforeLock != nil {
		r.beforeLock()
	}
	lock, err := tryLock(r.StateDir)
	if lock == nil || err != nil {
		return false, true
	}
	defer lock.Close()
	ctx = context.WithValue(ctx, lockKey{}, lock)
	todo := r.pending()
	if len(todo) == 0 {
		return false, false
	}
	if err := os.MkdirAll(dir(r.StateDir, r.Version), 0o700); err != nil {
		return false, true
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
			return false, true
		}
		fmt.Fprintf(w, "%s %s: start\n", time.Now().UTC().Format(time.RFC3339), t.Name)
		err := runTarget(ctx, t, w, r.TargetTimeout)
		if err != nil && ctx.Err() != nil {
			fmt.Fprintf(w, "%s %s: cancelled: %v\n", time.Now().UTC().Format(time.RFC3339), t.Name, err)
			return false, true
		}
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

func runTarget(ctx context.Context, t Target, w io.Writer, timeout time.Duration) (err error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
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
			bad = append(bad, o.name+" ("+brief(o.err)+")")
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
		parts = append(parts, "could not update "+strings.Join(bad, ", ")+"; run `crosstalk install` (log: "+LogPath(r.StateDir, r.Version)+")")
	}
	return "CrossTalk " + r.Version + " upgrade: " + strings.Join(parts, "; ")
}

// brief is the first line of err, at most 120 bytes; the log has the rest.
func brief(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
