package upgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const ver = "9.9.9"

func counting(name string, runs *atomic.Int32, verify error) Target {
	return Target{
		Name:   name,
		Run:    func(context.Context, io.Writer) error { runs.Add(1); return nil },
		Verify: func(context.Context) error { return verify },
	}
}

func TestUpgradePaths(t *testing.T) {
	st := "/s"
	if got := LockPath(st); got != "/s/upgrade.lock" {
		t.Errorf("LockPath = %q", got)
	}
	if got := MarkerPath(st, ver, "claude"); got != "/s/upgrade/9.9.9/claude.done" {
		t.Errorf("MarkerPath claude = %q", got)
	}
	if got := MarkerPath(st, ver, "gateway"); got != "/s/upgrade/9.9.9/gateway.done" {
		t.Errorf("MarkerPath gateway = %q", got)
	}
	if got := LogPath(st, ver); got != "/s/upgrade/9.9.9/follow-up.log" {
		t.Errorf("LogPath = %q", got)
	}
}

func TestUpgradeMarkerRoundTrip(t *testing.T) {
	st := t.TempDir()
	if Done(st, ver, "claude") {
		t.Fatal("Done before MarkDone")
	}
	if err := MarkDone(st, ver, "claude"); err != nil {
		t.Fatal(err)
	}
	if !Done(st, ver, "claude") {
		t.Fatal("not Done after MarkDone")
	}
	if _, err := os.Stat(MarkerPath(st, ver, "claude")); err != nil {
		t.Fatalf("marker file: %v", err)
	}
	if Done(st, ver, "codex") || Done(st, "9.9.8", "claude") {
		t.Fatal("a marker counts for another target or version")
	}
}

func TestUpgradeLockExcludes(t *testing.T) {
	st := t.TempDir()
	rel, ok, err := TryLock(st)
	if err != nil || !ok {
		t.Fatalf("first TryLock: ok=%v err=%v", ok, err)
	}
	if _, ok2, err := TryLock(st); err != nil || ok2 {
		t.Fatalf("second TryLock while held: ok=%v err=%v", ok2, err)
	}
	var waited atomic.Int32
	got := make(chan struct{})
	go func() {
		r, err := Lock(context.Background(), st, func() { waited.Add(1) })
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r()
	}()
	select {
	case <-got:
		t.Fatal("blocking Lock returned while held")
	case <-time.After(100 * time.Millisecond):
	}
	rel()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking Lock did not return after release")
	}
	if waited.Load() != 1 {
		t.Fatalf("waiting called %d times, want 1", waited.Load())
	}
	if _, err := os.Stat(LockPath(st)); err != nil {
		t.Fatalf("lock file: %v", err)
	}
}

func TestUpgradeLockCancel(t *testing.T) {
	st := t.TempDir()
	rel, _, _ := TryLock(st)
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, st, func() {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock on cancel: %v", err)
	}
}

func TestFollowUpRunsOnce(t *testing.T) {
	st := t.TempDir()
	var runs atomic.Int32
	inRun, finish := make(chan struct{}), make(chan struct{})
	slow := Target{
		Name: "claude",
		Run: func(context.Context, io.Writer) error {
			if runs.Add(1) == 1 {
				close(inRun)
				<-finish
			}
			return nil
		},
		Verify: func(context.Context) error { return nil },
	}
	a := &Runner{StateDir: st, Version: ver, Targets: []Target{slow}}
	b := &Runner{StateDir: st, Version: ver, Targets: []Target{slow}}
	var wg sync.WaitGroup
	wg.Go(func() { a.Run(context.Background()) })
	select {
	case <-inRun:
	case <-time.After(5 * time.Second):
		t.Fatal("first runner never ran the step")
	}
	b.Run(context.Background())
	close(finish)
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("step ran %d times, want 1", n)
	}
	if !Done(st, ver, "claude") {
		t.Fatal("no marker after the run")
	}
}

func TestFollowUpRechecksMarkerUnderLock(t *testing.T) {
	st := t.TempDir()
	var runs atomic.Int32
	var notices []string
	r := &Runner{StateDir: st, Version: ver, Targets: []Target{counting("claude", &runs, nil)},
		Notice: func(n string) { notices = append(notices, n) }}
	r.beforeLock = func() {
		if err := MarkDone(st, ver, "claude"); err != nil {
			t.Error(err)
		}
	}
	r.Run(context.Background())
	if n := runs.Load(); n != 0 {
		t.Fatalf("step ran %d times after another process wrote the marker", n)
	}
	if len(notices) != 0 {
		t.Fatalf("notice for a run that did nothing: %q", notices)
	}
}

func TestFollowUpNoMarkerWhenVerifyFails(t *testing.T) {
	st := t.TempDir()
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Targets: []Target{counting("claude", &runs, errors.New("plugin 0.1.0, binary 9.9.9"))}}
	r.Run(context.Background())
	if Done(st, ver, "claude") {
		t.Fatal("marker written for a target whose verification failed")
	}
	rel, ok, _ := TryLock(st)
	if !ok {
		t.Fatal("lock still held after the run")
	}
	rel()
	r2 := &Runner{StateDir: st, Version: ver, Targets: []Target{counting("claude", &runs, nil)}}
	r2.Run(context.Background())
	if runs.Load() != 2 || !Done(st, ver, "claude") {
		t.Fatalf("next start did not retry: runs=%d done=%v", runs.Load(), Done(st, ver, "claude"))
	}
}

func TestFollowUpNoMarkerWhenStepFails(t *testing.T) {
	st := t.TempDir()
	verified := false
	r := &Runner{StateDir: st, Version: ver, Targets: []Target{{
		Name:   "codex",
		Run:    func(context.Context, io.Writer) error { return errors.New("offline") },
		Verify: func(context.Context) error { verified = true; return nil },
	}}}
	r.Run(context.Background())
	if Done(st, ver, "codex") || verified {
		t.Fatalf("failed step: done=%v verified=%v", Done(st, ver, "codex"), verified)
	}
}

func TestFollowUpPanicReleasesLock(t *testing.T) {
	st := t.TempDir()
	var runs atomic.Int32
	var notices []string
	r := &Runner{StateDir: st, Version: ver, Notice: func(n string) { notices = append(notices, n) }, Targets: []Target{
		{Name: "claude", Run: func(context.Context, io.Writer) error { panic("boom") }, Verify: func(context.Context) error { return nil }},
		counting("codex", &runs, nil),
	}}
	r.Run(context.Background())
	if Done(st, ver, "claude") || !Done(st, ver, "codex") || runs.Load() != 1 {
		t.Fatalf("after a panicking target: claude=%v codex=%v runs=%d", Done(st, ver, "claude"), Done(st, ver, "codex"), runs.Load())
	}
	rel, ok, _ := TryLock(st)
	if !ok {
		t.Fatal("lock still held after a panicking target")
	}
	rel()
	if len(notices) != 1 || !strings.Contains(notices[0], "claude") {
		t.Fatalf("notices = %q", notices)
	}
}

func TestFollowUpPerTargetMarkers(t *testing.T) {
	st := t.TempDir()
	var cl, cx atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Targets: []Target{counting("claude", &cl, nil), counting("codex", &cx, errors.New("old"))}}
	r.Run(context.Background())
	if !Done(st, ver, "claude") || Done(st, ver, "codex") {
		t.Fatalf("claude=%v codex=%v", Done(st, ver, "claude"), Done(st, ver, "codex"))
	}
	r2 := &Runner{StateDir: st, Version: ver, Targets: []Target{counting("claude", &cl, nil), counting("codex", &cx, nil)}}
	r2.Run(context.Background())
	if cl.Load() != 1 || cx.Load() != 2 {
		t.Fatalf("second start reran a done target: claude=%d codex=%d", cl.Load(), cx.Load())
	}
}

func TestFollowUpNoticeText(t *testing.T) {
	st := t.TempDir()
	var cl, cx atomic.Int32
	var notices []string
	r := &Runner{StateDir: st, Version: ver, Notice: func(n string) { notices = append(notices, n) },
		Targets: []Target{counting("claude", &cl, nil), counting("codex", &cx, errors.New("manifest has 0.1.0"))}}
	r.Run(context.Background())
	if len(notices) != 1 {
		t.Fatalf("got %d notices, want 1: %q", len(notices), notices)
	}
	n := notices[0]
	for _, want := range []string{ver, "claude", "codex", "manifest has 0.1.0", "run `crosstalk install`"} {
		if !strings.Contains(n, want) {
			t.Errorf("notice %q lacks %q", n, want)
		}
	}
}

// fakeClock's After fires at once and moves its Now forward by the wait.
func fakeClock() (func(time.Duration) <-chan time.Time, func() time.Time, *[]time.Duration) {
	var waits []time.Duration
	var mu sync.Mutex
	t0 := time.Unix(0, 0)
	return func(d time.Duration) <-chan time.Time {
			mu.Lock()
			waits = append(waits, d)
			t0 = t0.Add(d)
			mu.Unlock()
			c := make(chan time.Time, 1)
			c <- time.Time{}
			return c
		}, func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return t0
		}, &waits
}

func TestFollowUpRetryBound(t *testing.T) {
	st := t.TempDir()
	after, now, waits := fakeClock()
	var bad, good atomic.Int32
	passOnThird := Target{
		Name: "codex",
		Run:  func(context.Context, io.Writer) error { good.Add(1); return nil },
		Verify: func(context.Context) error {
			if good.Load() < 3 {
				return errors.New("stale cache")
			}
			return nil
		},
	}
	var notices []string
	r := &Runner{StateDir: st, Version: ver, Interval: 15 * time.Minute, Bound: 6 * time.Hour, After: after, Now: now,
		Notice:  func(n string) { notices = append(notices, n) },
		Targets: []Target{counting("claude", &bad, errors.New("old")), passOnThird}}
	done := make(chan struct{})
	go func() { r.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("retry loop did not stop at its bound")
	}
	if bad.Load() != 25 {
		t.Errorf("failing target ran %d times, want 25 (start plus 24 retries)", bad.Load())
	}
	if good.Load() != 3 || !Done(st, ver, "codex") {
		t.Errorf("target that passed on the third run: runs=%d done=%v", good.Load(), Done(st, ver, "codex"))
	}
	if len(*waits) != 24 || (*waits)[0] != 15*time.Minute {
		t.Errorf("waits = %v, want 24 of 15m", *waits)
	}
	if len(notices) != 2 {
		t.Errorf("notices = %q, want the first run's and the one when codex passed", notices)
	}
}

func TestFollowUpRetryStopsWhenAllPass(t *testing.T) {
	st := t.TempDir()
	after, now, waits := fakeClock()
	var runs atomic.Int32
	second := Target{
		Name: "claude",
		Run:  func(context.Context, io.Writer) error { runs.Add(1); return nil },
		Verify: func(context.Context) error {
			if runs.Load() < 2 {
				return errors.New("stale cache")
			}
			return nil
		},
	}
	r := &Runner{StateDir: st, Version: ver, Interval: time.Minute, Bound: time.Hour, After: after, Now: now, Targets: []Target{second}}
	r.Run(context.Background())
	if runs.Load() != 2 || len(*waits) != 1 {
		t.Fatalf("runs=%d waits=%d, want 2 runs and 1 wait", runs.Load(), len(*waits))
	}
}

func TestFollowUpRetryStopsWhenAnotherProcessFinished(t *testing.T) {
	st := t.TempDir()
	var waits int
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Interval: time.Minute, Bound: time.Hour,
		Targets: []Target{counting("claude", &runs, errors.New("old"))}}
	r.After = func(time.Duration) <-chan time.Time {
		waits++
		MarkDone(st, ver, "claude")
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	r.Run(context.Background())
	if runs.Load() != 1 || waits != 1 {
		t.Fatalf("runs=%d waits=%d, want 1 and 1", runs.Load(), waits)
	}
}

func TestFollowUpRetryAfterBusyLock(t *testing.T) {
	st := t.TempDir()
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Interval: time.Minute, Bound: time.Hour,
		Targets: []Target{{Name: "claude", Run: func(context.Context, io.Writer) error { runs.Add(1); return nil },
			Verify: func(context.Context) error {
				if runs.Load() < 2 {
					return errors.New("old")
				}
				return nil
			}}}}
	var waits int
	var rel func()
	r.After = func(time.Duration) <-chan time.Time {
		waits++
		switch waits {
		case 1:
			rel, _, _ = TryLock(st)
		case 2:
			rel()
		}
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	r.Run(context.Background())
	if runs.Load() != 2 || !Done(st, ver, "claude") {
		t.Fatalf("a retry that met a busy lock stopped retrying: runs=%d waits=%d", runs.Load(), waits)
	}
}

func TestFollowUpRetryBoundCountsAttemptTime(t *testing.T) {
	st := t.TempDir()
	after, now, _ := fakeClock()
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Interval: 15 * time.Minute, Bound: 6 * time.Hour, After: after, Now: now}
	r.Targets = []Target{{Name: "claude",
		Run: func(context.Context, io.Writer) error {
			runs.Add(1)
			after(15 * time.Minute)
			return nil
		},
		Verify: func(context.Context) error { return errors.New("old") }}}
	r.Run(context.Background())
	if n := runs.Load(); n != 13 {
		t.Fatalf("%d attempts of 15 minutes each with 15-minute waits, want 13 within 6 hours", n)
	}
}

func TestFollowUpEligibleRecheckedEachAttempt(t *testing.T) {
	st := t.TempDir()
	after, now, _ := fakeClock()
	var runs atomic.Int32
	var setUp atomic.Bool
	setUp.Store(true)
	tg := counting("codex", &runs, errors.New("old"))
	tg.Eligible = setUp.Load
	r := &Runner{StateDir: st, Version: ver, Interval: time.Minute, Bound: time.Hour, Now: now, Targets: []Target{tg}}
	r.After = func(d time.Duration) <-chan time.Time {
		setUp.Store(false)
		return after(d)
	}
	r.Run(context.Background())
	if runs.Load() != 1 {
		t.Fatalf("a target no longer set up was run again: runs=%d", runs.Load())
	}
}

func TestFollowUpTimeoutCoversVerify(t *testing.T) {
	st := t.TempDir()
	r := &Runner{StateDir: st, Version: ver, TargetTimeout: 50 * time.Millisecond, Targets: []Target{{Name: "claude",
		Run:    func(context.Context, io.Writer) error { return nil },
		Verify: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}}}
	done := make(chan struct{})
	go func() { r.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung Verify held the lock past the target timeout")
	}
	if Done(st, ver, "claude") {
		t.Fatal("marker after a timed-out Verify")
	}
}

func TestFollowUpEnterCountsAttempt(t *testing.T) {
	st := t.TempDir()
	var inside, entered, left atomic.Int32
	var noticedInside bool
	r := &Runner{StateDir: st, Version: ver, Targets: []Target{{Name: "claude",
		Run:    func(context.Context, io.Writer) error { inside.Store(entered.Load() - left.Load()); return nil },
		Verify: func(context.Context) error { return nil }}}}
	r.Enter = func(context.Context) (func(), bool) { entered.Add(1); return func() { left.Add(1) }, true }
	r.Notice = func(string) { noticedInside = entered.Load()-left.Load() == 1 }
	r.Run(context.Background())
	if inside.Load() != 1 || !noticedInside || left.Load() != 1 {
		t.Fatalf("attempt not counted through its notice: inside=%d noticedInside=%v left=%d", inside.Load(), noticedInside, left.Load())
	}
	var runs atomic.Int32
	r2 := &Runner{StateDir: t.TempDir(), Version: ver, Targets: []Target{counting("claude", &runs, nil)},
		Enter: func(context.Context) (func(), bool) { return nil, false }}
	r2.Run(context.Background())
	if runs.Load() != 0 {
		t.Fatal("ran an attempt Enter refused")
	}
}

func TestFollowUpNoticeKeepsErrorShort(t *testing.T) {
	st := t.TempDir()
	var notices []string
	long := errors.New("first line\nsecond line with https://user:secret@example.invalid/repo")
	r := &Runner{StateDir: st, Version: ver, Notice: func(n string) { notices = append(notices, n) },
		Targets: []Target{{Name: "claude", Run: func(context.Context, io.Writer) error { return long }, Verify: func(context.Context) error { return nil }}}}
	r.Run(context.Background())
	if len(notices) != 1 || strings.Contains(notices[0], "secret") || !strings.Contains(notices[0], "first line") || !strings.Contains(notices[0], LogPath(st, ver)) {
		t.Fatalf("notice = %q", notices)
	}
}

func TestFollowUpRetryOnlyByLockHolder(t *testing.T) {
	st := t.TempDir()
	rel, _, _ := TryLock(st)
	after, now, waits := fakeClock()
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Interval: time.Minute, Bound: time.Hour, After: after, Now: now,
		Targets: []Target{counting("claude", &runs, errors.New("old"))}}
	r.Run(context.Background())
	rel()
	if runs.Load() != 0 || len(*waits) != 0 {
		t.Fatalf("a shim that did not get the lock retried: runs=%d waits=%d", runs.Load(), len(*waits))
	}
}

func TestFollowUpRetryStopsOnCancel(t *testing.T) {
	st := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Interval: time.Hour, Bound: 6 * time.Hour,
		Targets: []Target{counting("claude", &runs, errors.New("old"))}}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestFollowUpWritesLog(t *testing.T) {
	st := t.TempDir()
	var runs atomic.Int32
	r := &Runner{StateDir: st, Version: ver, Targets: []Target{
		{Name: "gateway", Run: func(_ context.Context, w io.Writer) error { io.WriteString(w, "restarted the gateway\n"); return nil }, Verify: func(context.Context) error { return nil }},
		counting("claude", &runs, errors.New("plugin 0.1.0")),
	}}
	r.Run(context.Background())
	b, err := os.ReadFile(filepath.Join(st, "upgrade", ver, "follow-up.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gateway", "restarted the gateway", "claude", "plugin 0.1.0"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log lacks %q:\n%s", want, b)
		}
	}
}
