// Package quiesce counts the work a refreshing shim waits for before it
// execs (ADR 0030): injects, claim proofs and wake transactions. Work already
// admitted enters with Enter; work a background loop starts on its own enters
// with Hold, which waits while a pause is in progress.
package quiesce

import (
	"context"
	"sync"
)

var g = struct {
	mu     sync.Mutex
	busy   int
	long   int
	paused bool
	change chan struct{}
}{change: make(chan struct{})}

func changed() {
	close(g.change)
	g.change = make(chan struct{})
}

// Enter counts one unit of work; call the returned func when it ends.
func Enter() func() {
	g.mu.Lock()
	g.busy++
	g.mu.Unlock()
	return leave()
}

// Inc counts one unit of work that ends with a matching Dec, for a counter
// that has no place to keep Enter's func.
func Inc() {
	g.mu.Lock()
	g.busy++
	g.mu.Unlock()
}

// Dec ends one unit counted by Inc.
func Dec() {
	g.mu.Lock()
	g.busy--
	changed()
	g.mu.Unlock()
}

// TryEnter is Enter that refuses while paused, for a new request at an
// ingress front door.
func TryEnter() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return nil, false
	}
	g.busy++
	return leave(), true
}

// Hold waits until no pause is in progress, then enters. It returns false
// when ctx ends first.
func Hold(ctx context.Context) (func(), bool) {
	for {
		g.mu.Lock()
		if !g.paused {
			g.busy++
			g.mu.Unlock()
			return leave(), true
		}
		ch := g.change
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// HoldLong is Hold for work that runs long (an upgrade follow-up attempt):
// it is counted by Long, and PauseUnlessLong refuses while it runs.
func HoldLong(ctx context.Context) (func(), bool) {
	for {
		g.mu.Lock()
		if !g.paused {
			g.busy++
			g.long++
			g.mu.Unlock()
			l := leave()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.long--
					g.mu.Unlock()
				})
				l()
			}, true
		}
		ch := g.change
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// Long reports the long work counted by HoldLong.
func Long() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.long
}

// PauseUnlessLong is Pause, except that it pauses nothing while long work
// runs; long reports that. Work cannot enter between the check and the pause.
func PauseUnlessLong() (paused, long bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.long > 0 {
		return false, true
	}
	if g.paused {
		return false, false
	}
	g.paused = true
	changed()
	return true, false
}

func leave() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.busy--
			changed()
			g.mu.Unlock()
		})
	}
}

// Pause makes TryEnter refuse and Hold wait. It reports false when another
// pause is already in progress.
func Pause() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return false
	}
	g.paused = true
	changed()
	return true
}

// Unpause ends the pause; held work starts.
func Unpause() {
	g.mu.Lock()
	g.paused = false
	changed()
	g.mu.Unlock()
}

// Wait blocks until no work is counted or ctx ends.
func Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		idle := g.busy == 0
		ch := g.change
		g.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Busy reports the counted work, for tests.
func Busy() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busy
}
