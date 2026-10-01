package payment

import (
	"errors"
	"testing"
)

func TestRefund(t *testing.T) {
	o := &Order{ID: "o1", Paid: 5000}
	if got, err := Refund(o, 2000, false); err != nil || got != 2000 || o.Refunded != 2000 {
		t.Fatalf("partial refund: %d, %v, refunded %d", got, err, o.Refunded)
	}
	if _, err := Refund(o, 0, false); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero refund: %v", err)
	}
}

func TestRestockingFee(t *testing.T) {
	o := &Order{ID: "o2", Paid: 40000}
	if got, err := Refund(o, 3000, true); err != nil || got != 2700 {
		t.Fatalf("restocked refund: %d, %v", got, err)
	}
	if fee := RestockingFee(40000); fee != 1500 {
		t.Fatalf("fee cap: %d", fee)
	}
}
