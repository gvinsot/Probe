package accounts

import (
	"testing"

	"github.com/gvinsot/SwiftProof/hub/internal/secrets"
	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

func TestRewrapMovesCredentialsToTheCurrentKey(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := make([]byte, 32), make([]byte, 32)
	newKey[31] = 7
	old, _ := secrets.New(oldKey)
	token, _ := old.Seal("gho_token")
	hookSecret, _ := old.Seal("hook-secret")
	if err := st.PutUser(&store.User{Key: "u1", Provider: "github", Token: token}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRepo("u1", &store.Repo{Key: "r1", HookSecret: hookSecret}); err != nil {
		t.Fatal(err)
	}

	keys, _ := secrets.New(newKey)
	if err := keys.WithPrevious([][]byte{oldKey}); err != nil {
		t.Fatal(err)
	}
	moved, err := Rewrap(st, keys)
	if err != nil || moved != 2 {
		t.Fatalf("Rewrap = %d, %v; want 2 values moved", moved, err)
	}
	current, _ := secrets.New(newKey)
	u, _ := st.User("u1")
	if got, err := current.Open(u.Token); err != nil || got != "gho_token" {
		t.Errorf("token = %q, %v", got, err)
	}
	r, _ := st.Repo("u1", "r1")
	if got, err := current.Open(r.HookSecret); err != nil || got != "hook-secret" {
		t.Errorf("hook secret = %q, %v", got, err)
	}
	if moved, _ := Rewrap(st, keys); moved != 0 {
		t.Errorf("a second rewrap must be a no-op, moved %d", moved)
	}
}
