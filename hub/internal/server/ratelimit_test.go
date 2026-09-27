package server

import (
	"strconv"
	"testing"
	"time"
)

func TestWindowLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newWindowLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("the first two events must pass")
	}
	if l.Allow("a") {
		t.Fatal("the third event within the window must be refused")
	}
	if !l.Allow("b") {
		t.Fatal("another key has its own budget")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("a new window restores the budget")
	}
}

// A flood of distinct keys must neither grow the limiter nor cost the keys
// already in use their budget, nor lock out a key never seen before.
func TestWindowLimiterSurvivesKeyFlood(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newWindowLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	l.Allow("active")
	l.Allow("active")
	for i := 0; i < 3*maxLimiterKeys; i++ {
		l.Allow("flood-" + strconv.Itoa(i))
	}
	if len(l.counts) > maxLimiterKeys {
		t.Fatalf("the limiter grew to %d keys", len(l.counts))
	}
	if !l.Allow("genuinehookkey") {
		t.Fatal("a full limiter must still admit an unseen key")
	}
	if !l.Allow("active") {
		t.Fatal("a key in use must keep the rest of its budget")
	}
	if l.Allow("active") {
		t.Fatal("a key in use must not regain its budget through the flood")
	}
}
