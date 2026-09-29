package instance

import (
	"errors"
	"testing"
)

func TestSecondAcquireFails(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Acquire = %v, want ErrRunning", err)
	}
	first.Release()
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	again.Release()
}
