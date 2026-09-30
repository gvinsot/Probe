package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// importedKey marks, in the meta table, a completed import of a file store.
const importedKey = "files_imported_at"

// ImportStats counts what ImportFiles copied. Ran is false when there was
// nothing to import or the import had already completed.
type ImportStats struct {
	Ran     bool
	Users   int
	Repos   int
	Routes  int
	Runs    int
	Skipped int
}

// ImportFiles copies the file store in dir into the database once: accounts,
// repositories, webhook and badge routes, report histories and their
// retention watermarks. A row already in the database is never overwritten,
// so an interrupted import resumes where it stopped; the database records the
// completed import and later starts skip it. The files are left in place.
func (p *Postgres) ImportFiles(ctx context.Context, dir string) (ImportStats, error) {
	var stats ImportStats
	if _, err := os.Stat(filepath.Join(dir, "users")); os.IsNotExist(err) {
		return stats, nil
	} else if err != nil {
		return stats, err
	}
	var done bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM meta WHERE key = $1)`, importedKey).Scan(&done); err != nil || done {
		return stats, err
	}
	src, err := Open(dir)
	if err != nil {
		return stats, err
	}
	stats.Ran = true

	userKeys, err := src.UserKeys()
	if err != nil {
		return stats, err
	}
	for _, userKey := range userKeys {
		u, err := src.User(userKey)
		if err != nil {
			stats.Skipped++
			continue
		}
		data, err := document(u)
		if err != nil {
			return stats, err
		}
		tag, err := p.pool.Exec(ctx, `INSERT INTO users (key, data) VALUES ($1, $2) ON CONFLICT DO NOTHING`, u.Key, data)
		if err != nil {
			return stats, fmt.Errorf("import account: %w", err)
		}
		stats.Users += int(tag.RowsAffected())
	}

	owners, err := src.children("repos")
	if err != nil {
		return stats, err
	}
	for _, userKey := range owners {
		repos, err := src.Repos(userKey)
		if err != nil {
			return stats, err
		}
		for _, r := range repos {
			data, err := document(r)
			if err != nil {
				return stats, err
			}
			tag, err := p.pool.Exec(ctx, `INSERT INTO repos (user_key, key, full_name, data) VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING`, userKey, r.Key, r.FullName, data)
			if err != nil {
				return stats, fmt.Errorf("import repository: %w", err)
			}
			stats.Repos += int(tag.RowsAffected())
		}
	}

	for _, kind := range []string{"hooks", "badges"} {
		routes, err := src.routes(kind)
		if err != nil {
			return stats, err
		}
		for key, route := range routes {
			data, err := document(route)
			if err != nil {
				return stats, err
			}
			tag, err := p.pool.Exec(ctx, `INSERT INTO routes (kind, key, data) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, kind, key, data)
			if err != nil {
				return stats, fmt.Errorf("import route: %w", err)
			}
			stats.Routes += int(tag.RowsAffected())
		}
	}

	reportOwners, err := src.children("reports")
	if err != nil {
		return stats, err
	}
	for _, userKey := range reportOwners {
		repoKeys, err := src.children("reports", userKey)
		if err != nil {
			return stats, err
		}
		for _, repoKey := range repoKeys {
			if err := p.importHistory(ctx, src, userKey, repoKey, &stats); err != nil {
				return stats, err
			}
		}
	}

	_, err = p.pool.Exec(ctx, `INSERT INTO meta (key, value) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		importedKey, time.Now().UTC().Format(time.RFC3339))
	return stats, err
}

// importHistory copies the retained results of one repository. A result that
// cannot be read is skipped and leaves the recent window incomplete, as an
// evicted one would.
func (p *Postgres) importHistory(ctx context.Context, src *Files, userKey, repoKey string, stats *ImportStats) error {
	runs, err := src.History(userKey, repoKey, 0)
	if err != nil {
		return err
	}
	through, incomplete, err := src.retention(userKey, repoKey)
	if err != nil {
		return err
	}
	for _, run := range runs {
		rec, err := src.RecordVariant(userKey, repoKey, run.Commit, run.Variant)
		if err != nil {
			stats.Skipped++
			if run.Variant != "plan" {
				incomplete = true
			}
			continue
		}
		rec.UserKey, rec.RepoKey = userKey, repoKey
		args, err := runRow(rec)
		if err != nil {
			return err
		}
		tag, err := p.pool.Exec(ctx, insertRun+` ON CONFLICT DO NOTHING`, args...)
		if err != nil {
			return fmt.Errorf("import analysis: %w", err)
		}
		stats.Runs += int(tag.RowsAffected())
	}
	if through.IsZero() && !incomplete {
		return nil
	}
	return putRetention(ctx, p.pool, userKey, repoKey, through, incomplete)
}

// children lists the valid keys naming the subdirectories of a store directory.
func (s *Files) children(parts ...string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(append([]string{s.dir}, parts...)...))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() && ValidKey(e.Name()) {
			keys = append(keys, e.Name())
		}
	}
	return keys, nil
}

// routes reads every webhook or badge route of a kind.
func (s *Files) routes(kind string) (map[string]HookRoute, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, kind))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	routes := make(map[string]HookRoute, len(entries))
	for _, e := range entries {
		key := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || key == e.Name() || !ValidKey(key) {
			continue
		}
		route, err := s.route(kind, key)
		if err != nil {
			continue
		}
		routes[key] = route
	}
	return routes, nil
}

// retention reads the eviction watermark of a repository history.
func (s *Files) retention(userKey, repoKey string) (time.Time, bool, error) {
	idx, dir, err := s.index(userKey, repoKey)
	if err != nil {
		return time.Time{}, true, err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.load(dir); err != nil {
		return time.Time{}, true, err
	}
	return idx.EvictedThrough, idx.Incomplete, nil
}
