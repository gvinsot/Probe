// Package api is what the shop's HTTP handlers call.
package api

import (
	"errors"

	"example.com/shop/auth"
	"example.com/shop/payment"
)

// ErrForbidden is returned when the user may not perform the operation.
var ErrForbidden = errors.New("forbidden")

// RefundOrder refunds amount cents of an order owned by owner, on behalf of u.
func RefundOrder(u auth.User, o *payment.Order, owner string, amount int64) error {
	if !auth.Allowed(u, "refund", owner) {
		return ErrForbidden
	}
	return payment.Refund(o, amount)
}
