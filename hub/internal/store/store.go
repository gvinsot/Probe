// Package store persists hub state: accounts, tracked repositories, webhook
// and badge routes, and a bounded report history.
//
// Files keeps it as JSON files under one data directory. The hub tracks a few
// hundred repositories per user and a cache of reports; a directory of
// atomically replaced files keeps an on-premise install free of any database
// dependency. Postgres keeps the same state in a database, so the hub is no
// longer bound to the node holding its volume. Keys are derived, never taken
// from user input, and every key is validated before it reaches the
// filesystem or a query.
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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gvinsot/Probe/hub/internal/report"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// MaxHistory is the maximum public history listing size.
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
	// StatusCancelled marks a queued attempt withdrawn before it started. It
	// is only reported live and never stored as a result.
	StatusCancelled = "cancelled"
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
	// AgentTokens authenticate coding agents on the MCP endpoint and the CLI
	// on the LLM gateway. Only the SHA-256 of each token is kept; the token
	// itself is shown once.
	AgentTokens []AgentToken `json:"agent_tokens,omitempty"`
	// LLMUsage is what the account consumed through the LLM gateway on its
	// last day of use.
	LLMUsage *LLMUsage `json:"llm_usage,omitempty"`
	// ReportLanguage is the language the AI reviewer writes this account's
	// reports in, one of ReportLanguages; empty is English.
	ReportLanguage string `json:"report_language,omitempty"`
}

// ReportLanguages are the languages a report can be written in, as the CLI's
// --report-language takes them. English, the default, is stored empty.
var ReportLanguages = []string{"English", "French", "German", "Spanish", "Italian", "Portuguese", "Dutch", "Polish", "Japanese", "Chinese", "Korean"}

// ValidReportLanguage reports whether language can be stored.
func ValidReportLanguage(language string) bool {
	return language == "" || slices.Contains(ReportLanguages, language)
}

// Agent token scopes: read lists repositories and reads reports; write also
// queues, reruns and cancels analyses and changes review settings. An llm
// token reaches only the LLM gateway, and the gateway accepts nothing else.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeLLM   = "llm"
)

// LLMUsage counts the gateway traffic of one account over one UTC day.
type LLMUsage struct {
	Day      string `json:"day"`
	Tokens   int64  `json:"tokens"`
	Requests int64  `json:"requests"`
}

// Today returns the tokens counted on day (YYYY-MM-DD), or 0 when the record
// belongs to an earlier day.
func (u *LLMUsage) Today(day string) int64 {
	if u == nil || u.Day != day {
		return 0
	}
	return u.Tokens
}

// AgentToken is one credential of an account: an MCP agent or a CLI login.
type AgentToken struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Scope      string    `json:"scope"`
	Hash       string    `json:"hash"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
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
	// Mode is the actual analysis mode: lint, review-read-only, review or plan.
	// Full review additionally requires operator validation of the base policy.
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
	BadgeKey string `json:"badge_key,omitempty"`
	// CodingRules are the owner's team coding rules, given to the reviewer
	// of every review of this repository.
	CodingRules string `json:"coding_rules,omitempty"`
	// Learning from team feedback is on unless the owner switched it off.
	// Feedback holds the latest human reactions to findings (bounded by
	// MaxFeedback); Outcomes counts, per kind of finding, whether the next
	// analyzed commit changed the file a finding was about, and
	// OutcomeBases the analyzed commits already compared, so that a rerun
	// counts nothing twice.
	LearningOff  bool                     `json:"learning_off,omitempty"`
	Feedback     []FeedbackEntry          `json:"feedback,omitempty"`
	Outcomes     map[string]OutcomeCounts `json:"outcomes,omitempty"`
	OutcomeBases []string                 `json:"outcome_bases,omitempty"`
	Latest       *Run                     `json:"latest,omitempty"`
	UpdatedAt    time.Time                `json:"updated_at"`
}

// Bounds of the feedback kept per repository.
const (
	MaxFeedback     = 300
	MaxOutcomeBases = 200
)

// Feedback votes.
const (
	VoteUp   = "up"
	VoteDown = "down"
)

// FeedbackEntry is one human reaction to a finding of a stored report: a
// vote, a comment, or both. A reply answers another entry. Topic, Path and
// Title are copied from the report when the reaction is recorded.
type FeedbackEntry struct {
	ID      string    `json:"id"`
	Commit  string    `json:"commit"`
	AlertID string    `json:"alert_id"`
	Topic   string    `json:"topic"`
	Path    string    `json:"path,omitempty"`
	Title   string    `json:"title,omitempty"`
	Vote    string    `json:"vote,omitempty"`
	Comment string    `json:"comment,omitempty"`
	ReplyTo string    `json:"reply_to,omitempty"`
	Author  string    `json:"author"`
	At      time.Time `json:"at"`
}

// OutcomeCounts records what developers did after findings of one kind.
type OutcomeCounts struct {
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
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
	CodingRules   string `json:"coding_rules,omitempty"`
	// Learning tells whether team feedback adapts future reviews, and
	// FeedbackCount how many reactions are kept.
	Learning      bool `json:"learning"`
	FeedbackCount int  `json:"feedback_count,omitempty"`
	// HookOutdated flags a monitored repository whose webhook predates the
	// installation token: the forge still delivers, the hub refuses, and the
	// owner has to reinstall the hook to get pushes and a badge back.
	HookOutdated bool       `json:"hook_outdated,omitempty"`
	Latest       *RecentRun `json:"latest,omitempty"`
	// Recent lists the normal analyses queued within RecentWindow, newest
	// first, so the dashboard can show the most severe status of a period.
	Recent           []RecentRun `json:"recent,omitempty"`
	RecentIncomplete bool        `json:"recent_incomplete,omitempty"`
	UpdatedAt        time.Time   `json:"updated_at"`
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
		CodingRules: r.CodingRules, Learning: !r.LearningOff, FeedbackCount: len(r.Feedback),
	}
	if r.Latest != nil {
		run := projectRecent(r.Latest)
		p.Latest = &run
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

// Files is a concurrency-safe directory of JSON records.
type Files struct {
	dir       string
	mu        sync.RWMutex
	indexesMu sync.Mutex
	indexes   map[string]*recordIndex
}

// Open prepares the data directory of a Files store.
func Open(dir string) (*Files, error) {
	for _, sub := range []string{"users", "repos", "reports", "hooks", "badges"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("data directory: %w", err)
		}
	}
	s := &Files{dir: dir, indexes: make(map[string]*recordIndex)}
	// Histories migrate on first access under a per-repository lock. Startup
	// never scans artifacts, so a legacy store cannot delay the HTTP listener.
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

func (s *Files) path(parts ...string) (string, error) {
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
func (s *Files) PutUser(u *User) error {
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
func (s *Files) User(key string) (*User, error) {
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
func (s *Files) PutRepo(userKey string, r *Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putRepoLocked(userKey, r)
}

func (s *Files) putRepoLocked(userKey string, r *Repo) error {
	path, err := s.path("repos", userKey, r.Key+".json")
	if err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	return writeJSON(path, r)
}

// Repo loads one repository.
func (s *Files) Repo(userKey, repoKey string) (*Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repoLocked(userKey, repoKey)
}

func (s *Files) repoLocked(userKey, repoKey string) (*Repo, error) {
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
func (s *Files) UpdateRepo(userKey, repoKey string, mutate func(*Repo) error) (*Repo, error) {
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
func (s *Files) Repos(userKey string) ([]*Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
func (s *Files) PutHook(hookKey string, route HookRoute) error {
	return s.putRoute("hooks", hookKey, route)
}

// Hook resolves a webhook routing key.
func (s *Files) Hook(hookKey string) (HookRoute, error) {
	return s.route("hooks", hookKey)
}

// DeleteHook forgets a webhook routing key.
func (s *Files) DeleteHook(hookKey string) error {
	return s.deleteRoute("hooks", hookKey)
}

// PutBadge registers the public key of a repository badge.
func (s *Files) PutBadge(badgeKey string, route HookRoute) error {
	return s.putRoute("badges", badgeKey, route)
}

// Badge resolves a badge key.
func (s *Files) Badge(badgeKey string) (HookRoute, error) {
	return s.route("badges", badgeKey)
}

// DeleteBadge forgets a badge key.
func (s *Files) DeleteBadge(badgeKey string) error {
	return s.deleteRoute("badges", badgeKey)
}

func (s *Files) putRoute(kind, key string, route HookRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(kind, key+".json")
	if err != nil {
		return err
	}
	return writeJSON(path, route)
}

func (s *Files) route(kind, key string) (HookRoute, error) {
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

func (s *Files) deleteRoute(kind, key string) error {
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
func (s *Files) OutdatedHooks() (int, error) {
	return outdatedHooks(s)
}

// UserKeys lists every stored account, for maintenance such as a key rewrap.
func (s *Files) UserKeys() ([]string, error) {
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
func (s *Files) UpdateUser(key string, mutate func(*User) error) error {
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

// Record loads one stored report.
func (s *Files) Record(userKey, repoKey, commit string) (*Record, error) {
	return s.RecordVariant(userKey, repoKey, commit, "normal")
}

// RecordVariant keeps plan artifacts separate from normal confidence reports.
func (s *Files) RecordVariant(userKey, repoKey, commit, variant string) (*Record, error) {
	if variant != "" && variant != "normal" && variant != "plan" {
		return nil, fmt.Errorf("invalid analysis variant")
	}
	path, err := s.path("reports", userKey, repoKey, recordName(commit, variant))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := readJSON(path, &rec); err != nil {
		return nil, err
	}
	rec.Error = SafeError(rec.Error)
	return &rec, nil
}

func recordName(commit, variant string) string {
	if variant == "plan" {
		return commit + ".plan.json"
	}
	return commit + ".json"
}
