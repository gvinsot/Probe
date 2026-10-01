package auth

import "testing"

func TestAllowed(t *testing.T) {
	cases := []struct {
		user   User
		action string
		want   bool
	}{
		{User{ID: "u1", Role: "customer"}, "view_order", true},
		{User{ID: "u2", Role: "customer"}, "view_order", false},
		{User{ID: "s1", Role: "support"}, "view_order", true},
		{User{ID: "a1", Role: "admin"}, "refund", true},
		{User{ID: "s1", Role: "support"}, "refund", true},
		{User{ID: "u1", Role: "customer"}, "refund", false},
		{User{ID: "a1", Role: "admin"}, "delete_order", false},
	}
	for _, c := range cases {
		if got := Allowed(c.user, c.action, "u1"); got != c.want {
			t.Errorf("Allowed(%+v, %q) = %v, want %v", c.user, c.action, got, c.want)
		}
	}
}
