package accounts

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/hub/internal/forge"
	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/store"
)

type failedRefresh struct{ forge.Provider }

func (failedRefresh) Refresh(_ context.Context, token forge.Token) (forge.Token, error) {
	return forge.Token{}, fmt.Errorf("refresh failed with %s and %s https://user:password@host/?token=hidden", token.AccessToken, token.RefreshToken)
}

func TestRefreshFailureRedactsKnownBareCredentials(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	manager := New(st, keys, map[string]forge.Provider{"github": failedRefresh{}})
	user := &store.User{Key: "user", Provider: "github"}
	if err := manager.Save(user, forge.Token{AccessToken: "short-opaque", RefreshToken: "other-opaque", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Token(context.Background(), user)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	for _, secret := range []string{"short-opaque", "other-opaque", "password", "hidden"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("refresh leaked %q: %s", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "refresh failed") {
		t.Fatalf("diagnostic lost: %s", err)
	}
}
