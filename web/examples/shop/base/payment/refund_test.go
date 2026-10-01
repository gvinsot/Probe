package payment

import (
	"errors"
	"testing"
)

func TestRefund(t *testing.T) {
	o := &Order{ID: "o1", Paid: 5000}
	if err := Refund(o, 2000); err != nil || o.Refunded != 2000 {
		t.Fatalf("partial refund: %v, refunded %d", err, o.Refunded)
	}
	if err := Refund(o, 0); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero refund: %v", err)
	}
	if err := Refund(o, 3500); !errors.Is(err, ErrExceedsPaid) {
		t.Fatalf("refund beyond the payment: %v", err)
	}
}
