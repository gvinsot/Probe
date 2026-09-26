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
	for i := 0; i < maxLimiterKeys; i++ {
		l.Allow(strconv.Itoa(i))
	}
	if l.Allow("fresh") {
		t.Fatal("a full limiter must refuse unseen keys instead of growing")
	}
}
