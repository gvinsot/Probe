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

// Refund records a refund of amount cents on o, less the restocking fee
// when the goods come back to stock. It returns the amount refunded.
func Refund(o *Order, amount int64, restock bool) (int64, error) {
	if amount <= 0 {
		return 0, ErrInvalidAmount
	}
	refunded := amount
	if restock {
		refunded -= RestockingFee(amount)
	}
	o.Refunded += refunded
	return refunded, nil
}

// RestockingFee is 10% of amount, at most 15.00.
func RestockingFee(amount int64) int64 {
	fee := amount / 10
	if fee > 1500 {
		fee = 1500
	}
	return fee
}
