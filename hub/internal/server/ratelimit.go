package server

import (
	"sync"
	"time"
)

// maxLimiterKeys bounds the memory of a limiter. Once that many distinct keys
// were seen within one window, an unseen key takes the place of the least
// active of a few sampled entries instead of growing the map. Refusing unseen
// keys outright would let anyone spraying random keys lock every legitimate
// caller out; evicting lets the flood only compete with itself, since its keys
// sit at the lowest counts while an active caller keeps its own budget.
const maxLimiterKeys = 10000

// evictionSample is how many entries a full limiter inspects to pick the one
// to forget. Map iteration order is random, so this is a random sample.
const evictionSample = 8

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
		l.evict()
	}
	if count >= l.limit {
		return false
	}
	l.counts[key] = count + 1
	return true
}

// evict forgets the least active of a random sample of keys. A forgotten key
// only regains its budget: no key ever loses budget to another one.
func (l *windowLimiter) evict() {
	victim, lowest, sampled := "", 0, 0
	for key, count := range l.counts {
		if sampled == 0 || count < lowest {
			victim, lowest = key, count
		}
		if sampled++; sampled == evictionSample {
			break
		}
	}
	delete(l.counts, victim)
}
