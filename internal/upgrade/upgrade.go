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
	"sync"
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

// key is the marker name of a target: Marker when set, else Name.
func (t Target) key() string {
	if t.Marker != "" {
		return t.Marker
	}
	return t.Name
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

// ClearMarkers removes target's marker, and every keyed marker "target-<key>",
// in every version directory. It does not follow a symlink at upgrade/ or at
// a version directory.
func ClearMarkers(stateDir, target string) error {
	root := filepath.Join(stateDir, "upgrade")
	if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	es, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		vd := filepath.Join(root, e.Name())
		if err := os.Remove(filepath.Join(vd, target+".done")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		keyed, err := filepath.Glob(filepath.Join(vd, target+"-*.done"))
		if err != nil {
			return err
		}
		for _, m := range keyed {
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
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
	f, err := tryLock(context.Background(), stateDir, nil)
	if f == nil {
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

func tryLock(ctx context.Context, stateDir string, waiting func()) (*os.File, error) {
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
	reapOrphan(ctx, ChildPath(stateDir), waiting)
	if ctx.Err() != nil {
		f.Close()
		return nil, ctx.Err()
	}
	return f, nil
}

// Lock takes upgrade.lock, calling waiting once if it has to wait, whether
// for the lock or for an orphaned command.
func Lock(ctx context.Context, stateDir string, waiting func()) (release func(), err error) {
	if waiting != nil {
		var once sync.Once
		w := waiting
		waiting = func() { once.Do(w) }
	}
	for first := true; ; first = false {
		f, err := tryLock(ctx, stateDir, waiting)
		if err != nil {
			return nil, err
		}
		if f != nil {
			return func() { f.Close() }, nil
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

// ErrNotRetryable marks a target error that retrying cannot fix: the Runner
// stops that target for the rest of its run, writes no marker, and tells the
// session once. The stop is in memory, so the next process runs the target
// once more.
var ErrNotRetryable = errors.New("not retryable")

// Target is one thing an upgrade brings up to date: Run is its install step,
// Verify the check that it is now at the binary's version.
type Target struct {
	Name string
	// Marker names the target's marker file; "" means Name. A target whose
	// state depends on more than the CrossTalk state directory sets it.
	Marker string
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
	stopped    map[string]bool
}

func (r *Runner) pending() []Target {
	var p []Target
	for _, t := range r.Targets {
		if !r.stopped[t.Name] && !Done(r.StateDir, r.Version, t.key()) && (t.Eligible == nil || t.Eligible()) {
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
	lock, err := tryLock(ctx, r.StateDir, nil)
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
			err = MarkDone(r.StateDir, r.Version, t.key())
		}
		if errors.Is(err, ErrNotRetryable) {
			if r.stopped == nil {
				r.stopped = map[string]bool{}
			}
			r.stopped[t.Name] = true
		} else if err != nil {
			pending = true
		}
		if err != nil {
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
	var ok, bad, manual []string
	for _, o := range outs {
		if o.err == nil {
			ok = append(ok, o.name)
		} else if errors.Is(o.err, ErrNotRetryable) {
			manual = append(manual, o.name+": "+strings.TrimPrefix(firstLine(o.err), ErrNotRetryable.Error()+": "))
		} else {
			bad = append(bad, o.name+" ("+brief(o.err)+")")
		}
	}
	if !first && len(ok) == 0 && len(manual) == 0 {
		return ""
	}
	var parts []string
	if len(ok) > 0 {
		parts = append(parts, "updated "+strings.Join(ok, ", "))
	}
	if first && len(bad) > 0 {
		parts = append(parts, "could not update "+strings.Join(bad, ", ")+"; run `crosstalk install` (log: "+LogPath(r.StateDir, r.Version)+")")
	}
	if len(manual) > 0 {
		parts = append(parts, "needs you, not retried: "+strings.Join(manual, "; "))
	}
	return "CrossTalk " + r.Version + " upgrade: " + strings.Join(parts, "; ")
}

// brief is the first line of err, at most 120 bytes; the log has the rest.
func brief(err error) string {
	s := firstLine(err)
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}
