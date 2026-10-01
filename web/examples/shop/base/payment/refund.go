// Package payment records the payments and refunds of orders.
package payment

import "errors"

// Order is a paid order. Amounts are in cents.
type Order struct {
	ID       string
	Paid     int64
	Refunded int64
}

var (
	ErrInvalidAmount = errors.New("refund amount must be positive")
	ErrExceedsPaid   = errors.New("refund exceeds what was paid")
)

// Refund records a refund of amount cents on o.
func Refund(o *Order, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	if o.Refunded+amount > o.Paid {
		return ErrExceedsPaid
	}
	o.Refunded += amount
	return nil
}
