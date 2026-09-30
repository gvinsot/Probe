package store

import "time"

// Store is the hub state. Files keeps it in one data directory; Postgres keeps
// it in a database any node of a cluster can reach. Both behave alike: a
// missing record is ErrNotFound, an invalid key is refused before any I/O,
// and a mutation passed to UpdateUser or UpdateRepo runs under a lock that
// serializes it with every other writer of that record.
type Store interface {
	PutUser(u *User) error
	User(key string) (*User, error)
	UpdateUser(key string, mutate func(*User) error) error
	UserKeys() ([]string, error)

	PutRepo(userKey string, r *Repo) error
	Repo(userKey, repoKey string) (*Repo, error)
	UpdateRepo(userKey, repoKey string, mutate func(*Repo) error) (*Repo, error)
	Repos(userKey string) ([]*Repo, error)
	ReposWithRecent(userKey string, since time.Time) ([]PublicRepo, error)
	OutdatedHooks() (int, error)

	PutHook(hookKey string, route HookRoute) error
	Hook(hookKey string) (HookRoute, error)
	DeleteHook(hookKey string) error
	PutBadge(badgeKey string, route HookRoute) error
	Badge(badgeKey string) (HookRoute, error)
	DeleteBadge(badgeKey string) error

	PutRecord(rec *Record) error
	Record(userKey, repoKey, commit string) (*Record, error)
	RecordVariant(userKey, repoKey, commit, variant string) (*Record, error)
	History(userKey, repoKey string, limit int) ([]Run, error)
	Recent(userKey, repoKey string, since time.Time) ([]RecentRun, bool, error)
}

var (
	_ Store = (*Files)(nil)
	_ Store = (*Postgres)(nil)
)

// outdatedHooks counts the monitored repositories, across every account,
// whose webhook must be reinstalled (see Repo.HookOutdated).
func outdatedHooks(s Store) (int, error) {
	userKeys, err := s.UserKeys()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, userKey := range userKeys {
		repos, err := s.Repos(userKey)
		if err != nil {
			return count, err
		}
		for _, repo := range repos {
			if repo.HookOutdated() {
				count++
			}
		}
	}
	return count, nil
}

// reposWithRecent projects the repositories of an account with the normal
// analyses of each within the window.
func reposWithRecent(s Store, userKey string, since time.Time) ([]PublicRepo, error) {
	repos, err := s.Repos(userKey)
	if err != nil {
		return nil, err
	}
	out := make([]PublicRepo, 0, len(repos))
	for _, repo := range repos {
		public := repo.Public()
		recent, incomplete, err := s.Recent(userKey, repo.Key, since)
		if err != nil {
			return nil, err
		}
		public.Recent, public.RecentIncomplete = recent, incomplete
		out = append(out, public)
	}
	return out, nil
}
