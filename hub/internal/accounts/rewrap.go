package accounts

import (
	"github.com/gvinsot/SwiftProof/hub/internal/secrets"
	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

// Rewrap reseals under the current deployment key every stored credential that
// only a previous key opens: forge tokens, webhook secrets and installation
// tokens. It runs at start-up after a rotation and returns how many values
// moved. A value no key opens is left untouched; the user signs in again.
func Rewrap(st *store.Store, keys *secrets.Keyring) (int, error) {
	userKeys, err := st.UserKeys()
	if err != nil {
		return 0, err
	}
	moved := 0
	reseal := func(value *string) {
		if fresh, changed, err := keys.Reseal(*value); err == nil && changed {
			*value = fresh
			moved++
		}
	}
	for _, userKey := range userKeys {
		if err := st.UpdateUser(userKey, func(u *store.User) error {
			reseal(&u.Token)
			reseal(&u.RefreshToken)
			return nil
		}); err != nil {
			return moved, err
		}
		repos, err := st.Repos(userKey)
		if err != nil {
			return moved, err
		}
		for _, repo := range repos {
			if repo.HookSecret == "" && repo.HookToken == "" {
				continue
			}
			if _, err := st.UpdateRepo(userKey, repo.Key, func(r *store.Repo) error {
				reseal(&r.HookSecret)
				reseal(&r.HookToken)
				return nil
			}); err != nil {
				return moved, err
			}
		}
	}
	return moved, nil
}
