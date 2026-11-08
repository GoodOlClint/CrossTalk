package remote

import (
	"sync"
	"time"
)

type stallLimiter struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	window time.Duration
	max    int
}

func newStallLimiter(window time.Duration, max int) *stallLimiter {
	return &stallLimiter{seen: map[string]time.Time{}, window: window, max: max}
}

func (s *stallLimiter) allow(addr string, now time.Time) bool { return true }

func (s *stallLimiter) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}
