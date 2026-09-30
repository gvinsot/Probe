package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// queryTimeout bounds one store operation, so that an unreachable database
// fails a request instead of holding it.
const queryTimeout = 30 * time.Second

// migrationLock is the advisory lock key serializing schema migrations when
// two hub processes start against the same database.
const migrationLock int64 = 0x70726f6265 // "probe"

// Postgres keeps the hub state in a PostgreSQL database. Accounts and
// repositories are the documents the file store writes; a repository history
// is one row per analysis, bounded to MaxRecords like the file store's.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to the database and applies the pending migrations.
func OpenPostgres(ctx context.Context, conn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(conn)
	if err != nil {
		// Never echo the connection string: it holds the password.
		return nil, errors.New("database connection string: " + SafeError(err.Error()))
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %s", SafeError(err.Error()))
	}
	p := &Postgres{pool: pool}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %s", SafeError(err.Error()))
	}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database schema: %w", err)
	}
	return p, nil
}

// Close releases the connections.
func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLock)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("migration %s: no version prefix", entry.Name())
		}
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		script, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// Without arguments, Exec runs the whole script at once.
			if _, err := tx.Exec(ctx, string(script)); err != nil {
				return fmt.Errorf("migration %s: %w", entry.Name(), err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), queryTimeout)
}

// tx runs fn in a transaction, committed when fn succeeds.
func (p *Postgres) tx(fn func(context.Context, pgx.Tx) error) error {
	ctx, cancel := p.ctx()
	defer cancel()
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

// scanJSON decodes the single JSON column of row into dst.
func scanJSON(row pgx.Row, dst any) error {
	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return json.Unmarshal(data, dst)
}

// document encodes a record for a jsonb column.
func document(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}

func validKeys(keys ...string) error {
	for _, key := range keys {
		if !ValidKey(key) {
			return fmt.Errorf("invalid key %q", key)
		}
	}
	return nil
}

// PutUser stores or refreshes an account.
func (p *Postgres) PutUser(u *User) error {
	if err := validKeys(u.Key); err != nil {
		return err
	}
	u.UpdatedAt = time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = u.UpdatedAt
	}
	data, err := document(u)
	if err != nil {
		return err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	_, err = p.pool.Exec(ctx, `INSERT INTO users (key, data) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data`, u.Key, data)
	return err
}

// User loads an account.
func (p *Postgres) User(key string) (*User, error) {
	if err := validKeys(key); err != nil {
		return nil, err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	var u User
	if err := scanJSON(p.pool.QueryRow(ctx, `SELECT data FROM users WHERE key = $1`, key), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// UpdateUser applies mutate to a stored account with its row locked.
func (p *Postgres) UpdateUser(key string, mutate func(*User) error) error {
	if err := validKeys(key); err != nil {
		return err
	}
	return p.tx(func(ctx context.Context, tx pgx.Tx) error {
		var u User
		if err := scanJSON(tx.QueryRow(ctx, `SELECT data FROM users WHERE key = $1 FOR UPDATE`, key), &u); err != nil {
			return err
		}
		if err := mutate(&u); err != nil {
			return err
		}
		u.UpdatedAt = time.Now().UTC()
		data, err := document(&u)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET data = $2 WHERE key = $1`, key, data)
		return err
	})
}

// UserKeys lists every stored account, for maintenance such as a key rewrap.
func (p *Postgres) UserKeys() ([]string, error) {
	ctx, cancel := p.ctx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT key FROM users ORDER BY key COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PutRepo stores a repository of a user.
func (p *Postgres) PutRepo(userKey string, r *Repo) error {
	if err := validKeys(userKey, r.Key); err != nil {
		return err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	return putRepo(ctx, p.pool, userKey, r)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func putRepo(ctx context.Context, db execer, userKey string, r *Repo) error {
	r.UpdatedAt = time.Now().UTC()
	data, err := document(r)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO repos (user_key, key, full_name, data) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_key, key) DO UPDATE SET full_name = EXCLUDED.full_name, data = EXCLUDED.data`,
		userKey, r.Key, r.FullName, data)
	return err
}

// Repo loads one repository.
func (p *Postgres) Repo(userKey, repoKey string) (*Repo, error) {
	if err := validKeys(userKey, repoKey); err != nil {
		return nil, err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	var r Repo
	if err := scanJSON(p.pool.QueryRow(ctx, `SELECT data FROM repos WHERE user_key = $1 AND key = $2`, userKey, repoKey), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRepo applies mutate to a stored repository with its row locked, so
// that concurrent webhook deliveries cannot lose an update.
func (p *Postgres) UpdateRepo(userKey, repoKey string, mutate func(*Repo) error) (*Repo, error) {
	if err := validKeys(userKey, repoKey); err != nil {
		return nil, err
	}
	var r Repo
	err := p.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := scanJSON(tx.QueryRow(ctx, `SELECT data FROM repos WHERE user_key = $1 AND key = $2 FOR UPDATE`, userKey, repoKey), &r); err != nil {
			return err
		}
		if err := mutate(&r); err != nil {
			return err
		}
		return putRepo(ctx, tx, userKey, &r)
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Repos lists the repositories of a user, sorted by name.
func (p *Postgres) Repos(userKey string) ([]*Repo, error) {
	if err := validKeys(userKey); err != nil {
		return nil, err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT data FROM repos WHERE user_key = $1 ORDER BY full_name, key`, userKey)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Repo, error) {
		var r Repo
		return &r, scanJSON(row, &r)
	})
}

// ReposWithRecent projects the repositories of an account with their recent
// normal analyses.
func (p *Postgres) ReposWithRecent(userKey string, since time.Time) ([]PublicRepo, error) {
	return reposWithRecent(p, userKey, since)
}

// OutdatedHooks counts the monitored repositories, across every account,
// whose webhook must be reinstalled (see Repo.HookOutdated).
func (p *Postgres) OutdatedHooks() (int, error) {
	return outdatedHooks(p)
}

// PutHook registers the routing key of a repository webhook.
func (p *Postgres) PutHook(hookKey string, route HookRoute) error {
	return p.putRoute("hooks", hookKey, route)
}

// Hook resolves a webhook routing key.
func (p *Postgres) Hook(hookKey string) (HookRoute, error) {
	return p.route("hooks", hookKey)
}

// DeleteHook forgets a webhook routing key.
func (p *Postgres) DeleteHook(hookKey string) error {
	return p.deleteRoute("hooks", hookKey)
}

// PutBadge registers the public key of a repository badge.
func (p *Postgres) PutBadge(badgeKey string, route HookRoute) error {
	return p.putRoute("badges", badgeKey, route)
}

// Badge resolves a badge key.
func (p *Postgres) Badge(badgeKey string) (HookRoute, error) {
	return p.route("badges", badgeKey)
}

// DeleteBadge forgets a badge key.
func (p *Postgres) DeleteBadge(badgeKey string) error {
	return p.deleteRoute("badges", badgeKey)
}

func (p *Postgres) putRoute(kind, key string, route HookRoute) error {
	if err := validKeys(key); err != nil {
		return err
	}
	data, err := document(route)
	if err != nil {
		return err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	_, err = p.pool.Exec(ctx, `INSERT INTO routes (kind, key, data) VALUES ($1, $2, $3)
		ON CONFLICT (kind, key) DO UPDATE SET data = EXCLUDED.data`, kind, key, data)
	return err
}

func (p *Postgres) route(kind, key string) (HookRoute, error) {
	if err := validKeys(key); err != nil {
		return HookRoute{}, err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	var route HookRoute
	if err := scanJSON(p.pool.QueryRow(ctx, `SELECT data FROM routes WHERE kind = $1 AND key = $2`, kind, key), &route); err != nil {
		return HookRoute{}, err
	}
	return route, nil
}

func (p *Postgres) deleteRoute(kind, key string) error {
	if err := validKeys(key); err != nil {
		return err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	_, err := p.pool.Exec(ctx, `DELETE FROM routes WHERE kind = $1 AND key = $2`, kind, key)
	return err
}

const insertRun = `INSERT INTO runs (user_key, repo_key, name, commit_id, plan, status, queued_at, activity_at, record, raw)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// runRow is the column values of a record; the raw report travels apart
// from the metadata document so that listings never read it.
func runRow(rec *Record) ([]any, error) {
	meta := *rec
	meta.Raw = nil
	data, err := document(&meta)
	if err != nil {
		return nil, err
	}
	var activity *time.Time
	if at := projectRecent(&rec.Run).activityAt(); !at.IsZero() {
		activity = &at
	}
	var raw []byte
	if rec.Raw != nil {
		raw = []byte(rec.Raw)
	}
	return []any{rec.UserKey, rec.RepoKey, recordName(rec.Commit, rec.Variant), rec.Commit,
		rec.Variant == "plan", rec.Status, rec.QueuedAt, activity, data, raw}, nil
}

// PutRecord replaces the result of a commit and variant, then evicts the
// oldest results beyond MaxRecords, advancing the retention watermark first.
func (p *Postgres) PutRecord(rec *Record) error {
	if !validRun(rec.Run) {
		return fmt.Errorf("invalid commit or analysis variant")
	}
	if err := validKeys(rec.UserKey, rec.RepoKey); err != nil {
		return err
	}
	clean := *rec
	clean.Error = SafeError(clean.Error)
	args, err := runRow(&clean)
	if err != nil {
		return err
	}
	return p.tx(func(ctx context.Context, tx pgx.Tx) error {
		// Serialize the writers of one history, so that eviction sees every
		// result committed before it.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, clean.UserKey+"/"+clean.RepoKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insertRun+` ON CONFLICT (user_key, repo_key, name) DO UPDATE SET
			commit_id = EXCLUDED.commit_id, plan = EXCLUDED.plan, status = EXCLUDED.status,
			queued_at = EXCLUDED.queued_at, activity_at = EXCLUDED.activity_at,
			record = EXCLUDED.record, raw = EXCLUDED.raw`, args...); err != nil {
			return err
		}
		return prune(ctx, tx, clean.UserKey, clean.RepoKey)
	})
}

func prune(ctx context.Context, tx pgx.Tx, userKey, repoKey string) error {
	rows, err := tx.Query(ctx, `SELECT name, record FROM runs WHERE user_key = $1 AND repo_key = $2
		ORDER BY queued_at DESC, name OFFSET $3`, userKey, repoKey, MaxRecords)
	if err != nil {
		return err
	}
	var names []string
	var evicted []Run
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			rows.Close()
			return err
		}
		var run Run
		if err := json.Unmarshal(data, &run); err != nil {
			rows.Close()
			return err
		}
		names, evicted = append(names, name), append(evicted, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(names) == 0 {
		return err
	}
	through, incomplete, err := retention(ctx, tx, userKey, repoKey)
	if err != nil {
		return err
	}
	for _, run := range evicted {
		through, incomplete = evict(run, through, incomplete)
	}
	if err := putRetention(ctx, tx, userKey, repoKey, through, incomplete); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM runs WHERE user_key = $1 AND repo_key = $2 AND name = ANY($3)`, userKey, repoKey, names)
	return err
}

func retention(ctx context.Context, db pgx.Tx, userKey, repoKey string) (time.Time, bool, error) {
	var through *time.Time
	var incomplete bool
	err := db.QueryRow(ctx, `SELECT evicted_through, incomplete FROM run_retention WHERE user_key = $1 AND repo_key = $2`,
		userKey, repoKey).Scan(&through, &incomplete)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil || through == nil {
		return time.Time{}, incomplete, err
	}
	return through.UTC(), incomplete, nil
}

// putRetention merges a watermark: it only ever moves forward, and an
// incomplete history stays incomplete.
func putRetention(ctx context.Context, db execer, userKey, repoKey string, through time.Time, incomplete bool) error {
	var at *time.Time
	if !through.IsZero() {
		at = &through
	}
	_, err := db.Exec(ctx, `INSERT INTO run_retention (user_key, repo_key, evicted_through, incomplete) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_key, repo_key) DO UPDATE SET
			evicted_through = GREATEST(run_retention.evicted_through, EXCLUDED.evicted_through),
			incomplete = run_retention.incomplete OR EXCLUDED.incomplete`, userKey, repoKey, at, incomplete)
	return err
}

// Record loads one stored report.
func (p *Postgres) Record(userKey, repoKey, commit string) (*Record, error) {
	return p.RecordVariant(userKey, repoKey, commit, "normal")
}

// RecordVariant keeps plan artifacts separate from normal confidence reports.
func (p *Postgres) RecordVariant(userKey, repoKey, commit, variant string) (*Record, error) {
	if variant != "" && variant != "normal" && variant != "plan" {
		return nil, fmt.Errorf("invalid analysis variant")
	}
	name := recordName(commit, variant)
	if err := validKeys(userKey, repoKey, strings.TrimSuffix(name, ".json")); err != nil {
		return nil, err
	}
	ctx, cancel := p.ctx()
	defer cancel()
	var data, raw []byte
	var size *int64
	err := p.pool.QueryRow(ctx, `SELECT record, CASE WHEN octet_length(raw) > $4 THEN NULL ELSE raw END, octet_length(raw)
		FROM runs WHERE user_key = $1 AND repo_key = $2 AND name = $3`, userKey, repoKey, name, maxRecordBytes).Scan(&data, &raw, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if size != nil && *size > maxRecordBytes {
		return nil, fmt.Errorf("record %s exceeds %d bytes", name, maxRecordBytes)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if raw != nil {
		rec.Raw = json.RawMessage(raw)
	}
	rec.Error = SafeError(rec.Error)
	return &rec, nil
}

// History lists the retained results newest first, without their reports.
// Zero returns all retained metadata (at most MaxRecords).
func (p *Postgres) History(userKey, repoKey string, limit int) ([]Run, error) {
	if err := validKeys(userKey, repoKey); err != nil {
		return nil, err
	}
	var bound *int
	if limit > 0 {
		bound = &limit
	}
	ctx, cancel := p.ctx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT record FROM runs WHERE user_key = $1 AND repo_key = $2
		ORDER BY queued_at DESC, name LIMIT $3`, userKey, repoKey, bound)
	if err != nil {
		return nil, err
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Run, error) {
		var run Run
		err := scanJSON(row, &run)
		return indexedRun(run), err
	})
	if len(runs) == 0 {
		return nil, err
	}
	return runs, err
}

// Recent lists the normal analyses active within the window, pending and
// undated ones first, then by latest activity. The incomplete flag covers the
// MaxRecent bound and results evicted from the window.
func (p *Postgres) Recent(userKey, repoKey string, since time.Time) ([]RecentRun, bool, error) {
	if err := validKeys(userKey, repoKey); err != nil {
		return nil, true, err
	}
	var runs []RecentRun
	var incomplete bool
	err := p.tx(func(ctx context.Context, tx pgx.Tx) error {
		through, evictedPending, err := retention(ctx, tx, userKey, repoKey)
		if err != nil {
			return err
		}
		incomplete = windowIncomplete(through, evictedPending, since)
		rows, err := tx.Query(ctx, `SELECT record FROM runs
			WHERE user_key = $1 AND repo_key = $2 AND NOT plan
			  AND (status IN ('queued', 'running') OR activity_at IS NULL OR activity_at >= $3)
			ORDER BY (status IN ('queued', 'running') OR activity_at IS NULL) DESC, activity_at DESC NULLS LAST, commit_id
			LIMIT $4`, userKey, repoKey, since, MaxRecent+1)
		if err != nil {
			return err
		}
		runs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecentRun, error) {
			var run Run
			err := scanJSON(row, &run)
			return projectRecent(&run), err
		})
		return err
	})
	if err != nil {
		return nil, true, err
	}
	if len(runs) > MaxRecent {
		runs, incomplete = runs[:MaxRecent], true
	}
	if runs == nil {
		runs = []RecentRun{}
	}
	return runs, incomplete, nil
}
