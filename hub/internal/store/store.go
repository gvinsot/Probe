// Package store persists hub state as JSON files under one data directory.
//
// The hub tracks a few hundred repositories per user and a cache of
// reports; a directory of atomically replaced files keeps the deployment free
// of any database dependency, which matters for an on-premise install. Keys
// are derived, never taken from user input, and every path is validated before
// it reaches the filesystem.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/SwiftProof/hub/internal/report"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// MaxHistory is the default history listing limit; cached results are retained.
const MaxHistory = 50

// RecentWindow bounds the runs sent with the repository list: the dashboard
// aggregates the most severe status over a period chosen up to this length.
const RecentWindow = 10 * 24 * time.Hour

// maxRecordBytes bounds a stored report; the CLI truncates its own outputs, so
// a larger file means something is wrong and must not be loaded into memory.
const maxRecordBytes = 32 << 20

// Run states.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// User is an authenticated forge account. Tokens are stored sealed.
type User struct {
	Key          string    `json:"key"`
	Provider     string    `json:"provider"`
	ID           string    `json:"id"`
	Login        string    `json:"login"`
	Name         string    `json:"name,omitempty"`
	AvatarURL    string    `json:"avatar_url,omitempty"`
	WebURL       string    `json:"web_url,omitempty"`
	Token        string    `json:"token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenExpiry  time.Time `json:"token_expiry,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Run is the state of one analysis, kept both on the repository (as the latest
// run) and in the report history.
type Run struct {
	Commit     string `json:"commit"`
	BaseCommit string `json:"base_commit,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Message    string `json:"message,omitempty"`
	Author     string `json:"author,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	// Mode is the analysis mode actually used: lint unless the operator
	// validated this repository's policy for review.
	Variant     string         `json:"variant,omitempty"`
	Intent      string         `json:"intent,omitempty"`
	Mode        string         `json:"mode,omitempty"`
	QueuedAt    time.Time      `json:"queued_at"`
	StartedAt   time.Time      `json:"started_at,omitempty"`
	FinishedAt  time.Time      `json:"finished_at,omitempty"`
	DurationMS  int64          `json:"duration_ms,omitempty"`
	Summary     report.Summary `json:"summary"`
	ToolVersion string         `json:"tool_version,omitempty"`
}

// Repo is one tracked repository of one user.
type Repo struct {
	Key           string    `json:"key"`
	Provider      string    `json:"provider"`
	ID            string    `json:"id"`
	FullName      string    `json:"full_name"`
	WebURL        string    `json:"web_url,omitempty"`
	CloneURL      string    `json:"clone_url,omitempty"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Admin         bool      `json:"admin"`
	HasPolicy     bool      `json:"has_policy"`
	PolicyAt      time.Time `json:"policy_at,omitempty"`
	Monitored     bool      `json:"monitored"`
	HookID        string    `json:"hook_id,omitempty"`
	HookKey       string    `json:"hook_key,omitempty"`
	HookSecret    string    `json:"hook_secret,omitempty"`
	// HookToken is the sealed installation secret carried in the webhook URL.
	// It is issued to the signed-in owner when monitoring is switched on and
	// is required on top of the forge signature.
	HookToken string `json:"hook_token,omitempty"`
	// BadgeKey addresses the public badge. It is distinct from HookKey so a
	// badge embedded in a README reveals nothing about the webhook.
	BadgeKey  string    `json:"badge_key,omitempty"`
	Latest    *Run      `json:"latest,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PublicRepo is the repository projection sent to a browser. It deliberately
// omits the webhook secret, token and routing key, which are credentials. The
// badge key is not one: it only reads the verdict the owner chose to publish.
type PublicRepo struct {
	Key           string `json:"key"`
	Provider      string `json:"provider"`
	FullName      string `json:"full_name"`
	WebURL        string `json:"web_url,omitempty"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Admin         bool   `json:"admin"`
	HasPolicy     bool   `json:"has_policy"`
	Monitored     bool   `json:"monitored"`
	BadgeKey      string `json:"badge_key,omitempty"`
	// HookOutdated flags a monitored repository whose webhook predates the
	// installation token: the forge still delivers, the hub refuses, and the
	// owner has to reinstall the hook to get pushes and a badge back.
	HookOutdated bool       `json:"hook_outdated,omitempty"`
	Latest       *RecentRun `json:"latest,omitempty"`
	// Recent lists compact normal results active within RecentWindow, plus
	// undated results which must not silently disappear from the dashboard.
	Recent    []RecentRun `json:"recent,omitempty"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// HookOutdated reports a monitored repository installed before webhooks
// carried an installation token and badges their own key. Every delivery of
// such a hook is refused until monitoring is switched on again.
func (r *Repo) HookOutdated() bool {
	return r.Monitored && (r.HookToken == "" || r.HookKey == "" || r.BadgeKey == "")
}

// Public projects a repository for the API.
func (r *Repo) Public() PublicRepo {
	p := PublicRepo{
		Key: r.Key, Provider: r.Provider, FullName: r.FullName, WebURL: r.WebURL,
		DefaultBranch: r.DefaultBranch, Private: r.Private, Admin: r.Admin,
		HasPolicy: r.HasPolicy, Monitored: r.Monitored, UpdatedAt: r.UpdatedAt,
	}
	if r.Latest != nil {
		latest := projectRecent(r.Latest)
		p.Latest = &latest
	}
	if r.Monitored {
		p.BadgeKey = r.BadgeKey
	}
	p.HookOutdated = r.HookOutdated()
	return p
}

// Record is a stored report: its run metadata plus the raw confidence report.
type Record struct {
	Run
	UserKey  string          `json:"user_key"`
	RepoKey  string          `json:"repo_key"`
	RepoName string          `json:"repo_name"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

// HookRoute maps a webhook routing key onto the repository it belongs to.
type HookRoute struct {
	UserKey  string `json:"user_key"`
	RepoKey  string `json:"repo_key"`
	Provider string `json:"provider"`
}

// Store is a concurrency-safe directory of JSON records.
type Store struct {
	dir    string
	mu     sync.RWMutex
	recent map[recentKey]map[string]RecentRun
}

// Open prepares the data directory.
func Open(dir string) (*Store, error) {
	for _, sub := range []string{"users", "repos", "reports", "hooks", "badges"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("data directory: %w", err)
		}
	}
	s := &Store{dir: dir, recent: make(map[recentKey]map[string]RecentRun)}
	if err := s.loadRecent(); err != nil {
		return nil, fmt.Errorf("recent index: %w", err)
	}
	return s, nil
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}$`)

// Key derives a filesystem-safe, collision-free key from an identity. The
// readable prefix helps an operator inspect the data directory; the digest
// suffix keeps distinct identities distinct.
func Key(parts ...string) string {
	raw := strings.Join(parts, "/")
	sum := sha256.Sum256([]byte(raw))
	var b strings.Builder
	for _, r := range raw {
		if len(b.String()) >= 48 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String() + "-" + hex.EncodeToString(sum[:6])
}

// ValidKey reports whether a key received from a request is safe to use as a
// path element. Anything else is rejected before touching the filesystem.
func ValidKey(key string) bool {
	return keyPattern.MatchString(key) && !strings.Contains(key, "..")
}

func (s *Store) path(parts ...string) (string, error) {
	for _, p := range parts[:len(parts)-1] {
		if !ValidKey(p) {
			return "", fmt.Errorf("invalid key %q", p)
		}
	}
	last := parts[len(parts)-1]
	if !ValidKey(strings.TrimSuffix(last, ".json")) {
		return "", fmt.Errorf("invalid key %q", last)
	}
	return filepath.Join(append([]string{s.dir}, parts...)...), nil
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Replace atomically: a crash mid-write must not leave a half record.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSON(path string, value any) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if info.Size() > maxRecordBytes {
		return fmt.Errorf("record %s exceeds %d bytes", filepath.Base(path), maxRecordBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	return json.Unmarshal(data, value)
}

// PutUser stores or refreshes an account.
func (s *Store) PutUser(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path("users", u.Key+".json")
	if err != nil {
		return err
	}
	u.UpdatedAt = time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = u.UpdatedAt
	}
	return writeJSON(path, u)
}

// User loads an account.
func (s *Store) User(key string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, err := s.path("users", key+".json")
	if err != nil {
		return nil, err
	}
	var u User
	if err := readJSON(path, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// PutRepo stores a repository of a user.
func (s *Store) PutRepo(userKey string, r *Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putRepoLocked(userKey, r)
}

func (s *Store) putRepoLocked(userKey string, r *Repo) error {
	path, err := s.path("repos", userKey, r.Key+".json")
	if err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	return writeJSON(path, r)
}

// Repo loads one repository.
func (s *Store) Repo(userKey, repoKey string) (*Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repoLocked(userKey, repoKey)
}

func (s *Store) repoLocked(userKey, repoKey string) (*Repo, error) {
	path, err := s.path("repos", userKey, repoKey+".json")
	if err != nil {
		return nil, err
	}
	var r Repo
	if err := readJSON(path, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRepo applies mutate to a stored repository under the store lock, so
// that concurrent webhook deliveries cannot lose an update.
func (s *Store) UpdateRepo(userKey, repoKey string, mutate func(*Repo) error) (*Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoLocked(userKey, repoKey)
	if err != nil {
		return nil, err
	}
	if err := mutate(r); err != nil {
		return nil, err
	}
	if err := s.putRepoLocked(userKey, r); err != nil {
		return nil, err
	}
	return r, nil
}

// Repos lists the repositories of a user, most recently updated first.
func (s *Store) Repos(userKey string) ([]*Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reposLocked(userKey)
}

func (s *Store) reposLocked(userKey string) ([]*Repo, error) {
	if !ValidKey(userKey) {
		return nil, fmt.Errorf("invalid key %q", userKey)
	}
	dir := filepath.Join(s.dir, "repos", userKey)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	repos := make([]*Repo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var r Repo
		if err := readJSON(filepath.Join(dir, e.Name()), &r); err != nil {
			continue
		}
		repos = append(repos, &r)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	return repos, nil
}

// PutHook registers the routing key of a repository webhook.
func (s *Store) PutHook(hookKey string, route HookRoute) error {
	return s.putRoute("hooks", hookKey, route)
}

// Hook resolves a webhook routing key.
func (s *Store) Hook(hookKey string) (HookRoute, error) {
	return s.route("hooks", hookKey)
}

// DeleteHook forgets a webhook routing key.
func (s *Store) DeleteHook(hookKey string) error {
	return s.deleteRoute("hooks", hookKey)
}

// PutBadge registers the public key of a repository badge.
func (s *Store) PutBadge(badgeKey string, route HookRoute) error {
	return s.putRoute("badges", badgeKey, route)
}

// Badge resolves a badge key.
func (s *Store) Badge(badgeKey string) (HookRoute, error) {
	return s.route("badges", badgeKey)
}

// DeleteBadge forgets a badge key.
func (s *Store) DeleteBadge(badgeKey string) error {
	return s.deleteRoute("badges", badgeKey)
}

func (s *Store) putRoute(kind, key string, route HookRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(kind, key+".json")
	if err != nil {
		return err
	}
	return writeJSON(path, route)
}

func (s *Store) route(kind, key string) (HookRoute, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, err := s.path(kind, key+".json")
	if err != nil {
		return HookRoute{}, err
	}
	var route HookRoute
	if err := readJSON(path, &route); err != nil {
		return HookRoute{}, err
	}
	return route, nil
}

func (s *Store) deleteRoute(kind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(kind, key+".json")
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// OutdatedHooks counts the monitored repositories, across every account,
// whose webhook must be reinstalled (see Repo.HookOutdated).
func (s *Store) OutdatedHooks() (int, error) {
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

// UserKeys lists every stored account, for maintenance such as a key rewrap.
func (s *Store) UserKeys() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(filepath.Join(s.dir, "users"))
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || name == e.Name() || !ValidKey(name) {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys, nil
}

// UpdateUser applies mutate to a stored account under the store lock.
func (s *Store) UpdateUser(key string, mutate func(*User) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path("users", key+".json")
	if err != nil {
		return err
	}
	var u User
	if err := readJSON(path, &u); err != nil {
		return err
	}
	if err := mutate(&u); err != nil {
		return err
	}
	u.UpdatedAt = time.Now().UTC()
	return writeJSON(path, &u)
}

// PutRecord retains the latest result per commit and variant without eviction.
func (s *Store) PutRecord(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Variant != "" && rec.Variant != "normal" && rec.Variant != "plan" {
		return fmt.Errorf("invalid analysis variant")
	}
	if !ValidKey(rec.Commit) {
		return fmt.Errorf("invalid commit %q", rec.Commit)
	}
	path, err := s.path("reports", rec.UserKey, rec.RepoKey, recordName(rec.Commit, rec.Variant))
	if err != nil {
		return err
	}
	if err := writeJSON(path, rec); err != nil {
		return err
	}
	since := time.Now().Add(-RecentWindow)
	for commit, run := range s.recent[recentKey{rec.UserKey, rec.RepoKey}] {
		if !run.inWindow(since) {
			delete(s.recent[recentKey{rec.UserKey, rec.RepoKey}], commit)
		}
	}
	s.indexRecent(rec.UserKey, rec.RepoKey, projectRecent(&rec.Run), since)
	return nil
}

// Record loads one stored report.
func (s *Store) Record(userKey, repoKey, commit string) (*Record, error) {
	return s.RecordVariant(userKey, repoKey, commit, "normal")
}

// RecordVariant keeps plan artifacts separate from normal confidence reports.
func (s *Store) RecordVariant(userKey, repoKey, commit, variant string) (*Record, error) {
	if variant != "" && variant != "normal" && variant != "plan" {
		return nil, fmt.Errorf("invalid analysis variant")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, err := s.path("reports", userKey, repoKey, recordName(commit, variant))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := readJSON(path, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// History lists the stored runs of a repository, newest first, without their
// raw reports.
func (s *Store) History(userKey, repoKey string, limit int) ([]Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !ValidKey(userKey) || !ValidKey(repoKey) {
		return nil, fmt.Errorf("invalid key")
	}
	dir := filepath.Join(s.dir, "reports", userKey, repoKey)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	runs := make([]Run, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var rec Record
		if err := readJSON(filepath.Join(dir, e.Name()), &rec); err != nil {
			continue
		}
		rec.Raw = nil
		runs = append(runs, rec.Run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].QueuedAt.After(runs[j].QueuedAt) })
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func recordName(commit, variant string) string {
	if variant == "plan" {
		return commit + ".plan.json"
	}
	return commit + ".json"
}
