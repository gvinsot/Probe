package server

import (
	"sync"
	"time"
)

// maxLimiterKeys bounds the memory of a limiter. Once that many distinct keys
// were seen within one window, every further unseen key is refused until the
// window rolls: spraying random keys cannot grow the map or bypass the limit.
const maxLimiterKeys = 10000

// windowLimiter counts events per key in fixed windows. It is deliberately
// simple: it only has to keep one caller from flooding an endpoint, not to be
// a precise traffic shaper, and it resets entirely at every window.
type windowLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu     sync.Mutex
	start  time.Time
	counts map[string]int
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, now: time.Now, counts: map[string]int{}}
}

// Allow records one event for key and reports whether it is within the limit.
func (l *windowLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.start) >= l.window {
		l.start, l.counts = now, map[string]int{}
	}
	count, seen := l.counts[key]
	if !seen && len(l.counts) >= maxLimiterKeys {
		return false
	}
	if count >= l.limit {
		return false
	}
	l.counts[key] = count + 1
	return true
}
