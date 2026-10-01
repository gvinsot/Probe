// Package auth decides what a user may do in the shop.
package auth

// User is an authenticated shop user.
type User struct {
	ID   string
	Role string // "customer", "support" or "admin"
}

// Allowed reports whether u may perform action on an order owned by owner.
func Allowed(u User, action, owner string) bool {
	switch action {
	case "view_order":
		return u.Role == "admin" || u.Role == "support" || u.ID == owner
	case "refund":
		// Support agents handle refunds now, so that admins are not paged.
		return u.Role == "admin" || u.Role == "support"
	default:
		return false
	}
}
