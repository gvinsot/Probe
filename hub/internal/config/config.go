// Package config reads the deployment configuration of the SwiftProof hub.
//
// Everything an operator needs is an environment variable: the same image runs
// on a public deployment and inside a company, pointed at github.com, a GitHub
// Enterprise host or a self-managed GitLab. No configuration file is required
// and no credential is ever read from the analyzed repositories.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Analysis modes. Lint and read-only review never execute repository code;
// auto selects between them using deployment provider settings. Review runs the
// configured checks in Docker and therefore needs a Docker socket, which the
// operator must mount deliberately.
const (
	ModeAuto     = "auto"
	ModeLint     = "lint"
	ModeReadOnly = "review-read-only"
	ModeReview   = "review"
)

// Instance kinds. A public instance lets anybody sign in and subscribe a
// repository; lint and read-only AI review never execute what they analyze.
// Only a private instance, whose users the operator knows, may run review, and
// then only for the repositories whose policy the operator validated.
const (
	InstancePublic  = "public"
	InstancePrivate = "private"
)

// Forge identifiers.
const (
	GitHub = "github"
	GitLab = "gitlab"
)

// Provider settings belong to the deployment, exactly as for the CLI: the hub
// holds no API key of its own and only forwards these to the binary it runs.
const (
	EndpointEnvName = "SWIFTPROOF_REVIEWER_ENDPOINT"
	ModelEnvName    = "SWIFTPROOF_REVIEWER_MODEL"
)

// Forge holds the OAuth application and host of one code forge.
type Forge struct {
	Kind         string
	ClientID     string
	ClientSecret string
	// BaseURL serves the OAuth authorize/token endpoints and the web UI.
	BaseURL string
	// APIURL is the REST API root, which differs from BaseURL on github.com.
	APIURL string
	Scopes string
}

// Config is the resolved deployment configuration.
type Config struct {
	Addr    string
	BaseURL string
	DataDir string
	// SessionKey seals session cookies and forge tokens at rest.
	SessionKey []byte
	// PreviousSessionKeys still open values sealed before a key rotation; the
	// hub reseals them under SessionKey at start-up.
	PreviousSessionKeys [][]byte
	// Binary is the SwiftProof CLI the analysis runner executes.
	Binary string
	// Instance is public or private; it decides which modes are allowed.
	Instance string
	// Mode is the most the instance may run. Review applies only to the
	// repositories listed in ReviewPolicies; every other one is linted.
	Mode string
	// ReviewPolicies maps "<forge>:<owner/repo>" (lower case) onto the SHA-256
	// digests of the base-branch policies the operator validated for review.
	ReviewPolicies map[string]map[string]bool
	Workers        int
	QueueSize      int
	// UserQuota bounds the analyses one account may have queued or running.
	UserQuota int
	// HookRate bounds the webhook deliveries accepted per routing key and
	// minute, before any lookup or signature check.
	HookRate        int
	AnalysisTimeout time.Duration
	CloneDepth      int
	MaxRepos        int
	SessionTTL      time.Duration
	// CommitStatus publishes the outcome back onto the analyzed commit.
	CommitStatus bool
	// DefaultBranchOnly restricts push analysis to the default branch.
	DefaultBranchOnly bool
	// Forges is keyed by forge kind and holds only configured forges.
	Forges map[string]Forge
}

// Default values chosen so that a bare `docker run` with one OAuth app works.
const (
	defaultAddr      = ":8080"
	defaultDataDir   = "/var/lib/swiftproof-hub"
	defaultBinary    = "swiftproof"
	defaultWorkers   = 2
	defaultQueue     = 256
	defaultUserQuota = 8
	defaultHookRate  = 30
	defaultTimeout   = 10 * time.Minute
	defaultDepth     = 50
	defaultMaxRepos  = 500
	defaultSessionMs = 12 * time.Hour
)

// Load resolves the configuration from getenv, creating the data directory and
// a persistent session key when they do not exist yet.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:              env(getenv, "SWIFTPROOF_HUB_ADDR", defaultAddr),
		DataDir:           env(getenv, "SWIFTPROOF_HUB_DATA_DIR", defaultDataDir),
		Binary:            env(getenv, "SWIFTPROOF_HUB_BINARY", defaultBinary),
		Instance:          strings.ToLower(env(getenv, "SWIFTPROOF_HUB_INSTANCE", InstancePublic)),
		Mode:              strings.ToLower(env(getenv, "SWIFTPROOF_HUB_MODE", ModeAuto)),
		UserQuota:         envInt(getenv, "SWIFTPROOF_HUB_USER_QUOTA", defaultUserQuota),
		HookRate:          envInt(getenv, "SWIFTPROOF_HUB_HOOK_RATE", defaultHookRate),
		Workers:           envInt(getenv, "SWIFTPROOF_HUB_WORKERS", defaultWorkers),
		QueueSize:         envInt(getenv, "SWIFTPROOF_HUB_QUEUE_SIZE", defaultQueue),
		AnalysisTimeout:   envDuration(getenv, "SWIFTPROOF_HUB_ANALYSIS_TIMEOUT", defaultTimeout),
		CloneDepth:        envInt(getenv, "SWIFTPROOF_HUB_CLONE_DEPTH", defaultDepth),
		MaxRepos:          envInt(getenv, "SWIFTPROOF_HUB_MAX_REPOS", defaultMaxRepos),
		SessionTTL:        envDuration(getenv, "SWIFTPROOF_HUB_SESSION_TTL", defaultSessionMs),
		CommitStatus:      envBool(getenv, "SWIFTPROOF_HUB_COMMIT_STATUS", true),
		DefaultBranchOnly: envBool(getenv, "SWIFTPROOF_HUB_DEFAULT_BRANCH_ONLY", false),
		Forges:            map[string]Forge{},
	}
	base := strings.TrimRight(strings.TrimSpace(getenv("SWIFTPROOF_HUB_BASE_URL")), "/")
	if base == "" {
		return c, fmt.Errorf("SWIFTPROOF_HUB_BASE_URL is required: the forge needs a reachable callback and webhook URL")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return c, fmt.Errorf("SWIFTPROOF_HUB_BASE_URL must be an absolute http(s) URL, got %q", base)
	}
	c.BaseURL = base
	if c.Mode == ModeAuto {
		c.Mode = ModeLint
		if strings.TrimSpace(getenv(EndpointEnvName)) != "" || strings.TrimSpace(getenv(ModelEnvName)) != "" {
			c.Mode = ModeReadOnly
		}
	}
	if c.Mode != ModeLint && c.Mode != ModeReview && c.Mode != ModeReadOnly {
		return c, fmt.Errorf("SWIFTPROOF_HUB_MODE must be auto, lint, review-read-only or review, got %q", c.Mode)
	}
	if c.Mode == ModeReadOnly && (strings.TrimSpace(getenv(EndpointEnvName)) == "" || strings.TrimSpace(getenv(ModelEnvName)) == "") {
		return c, fmt.Errorf("read-only AI review requires deployment-configured %s and %s", EndpointEnvName, ModelEnvName)
	}
	if c.Instance != InstancePublic && c.Instance != InstancePrivate {
		return c, fmt.Errorf("SWIFTPROOF_HUB_INSTANCE must be %q or %q, got %q", InstancePublic, InstancePrivate, c.Instance)
	}
	c.ReviewPolicies, err = reviewPolicies(secret(getenv, "SWIFTPROOF_HUB_REVIEW_POLICIES"))
	if err != nil {
		return c, err
	}
	if c.Mode == ModeReview {
		// Review executes the checks of a policy that lives in the analyzed
		// repository. On a public instance anybody chooses that repository,
		// so the mode is refused outright rather than trusted to an env var.
		if c.Instance == InstancePublic {
			return c, fmt.Errorf("SWIFTPROOF_HUB_MODE=review is refused on a public instance: set SWIFTPROOF_HUB_INSTANCE=private for a deployment whose users you control")
		}
		if len(c.ReviewPolicies) == 0 {
			return c, fmt.Errorf("SWIFTPROOF_HUB_MODE=review needs SWIFTPROOF_HUB_REVIEW_POLICIES: list the repositories and policy digests you validated")
		}
	}
	if c.UserQuota < 1 || c.UserQuota > 10000 {
		return c, fmt.Errorf("SWIFTPROOF_HUB_USER_QUOTA must be between 1 and 10000")
	}
	if c.HookRate < 1 || c.HookRate > 100000 {
		return c, fmt.Errorf("SWIFTPROOF_HUB_HOOK_RATE must be between 1 and 100000")
	}
	if c.Workers < 1 || c.Workers > 64 {
		return c, fmt.Errorf("SWIFTPROOF_HUB_WORKERS must be between 1 and 64")
	}
	if c.QueueSize < 1 || c.QueueSize > 100000 {
		return c, fmt.Errorf("SWIFTPROOF_HUB_QUEUE_SIZE must be between 1 and 100000")
	}
	if c.CloneDepth < 1 || c.CloneDepth > 10000 {
		return c, fmt.Errorf("SWIFTPROOF_HUB_CLONE_DEPTH must be between 1 and 10000")
	}
	if c.MaxRepos < 1 || c.MaxRepos > 10000 {
		return c, fmt.Errorf("SWIFTPROOF_HUB_MAX_REPOS must be between 1 and 10000")
	}
	if c.AnalysisTimeout < time.Minute || c.AnalysisTimeout > 6*time.Hour {
		return c, fmt.Errorf("SWIFTPROOF_HUB_ANALYSIS_TIMEOUT must be between 1m and 6h")
	}
	if c.SessionTTL < time.Minute || c.SessionTTL > 30*24*time.Hour {
		return c, fmt.Errorf("SWIFTPROOF_HUB_SESSION_TTL must be between 1m and 720h")
	}

	if id := strings.TrimSpace(getenv("SWIFTPROOF_HUB_GITHUB_CLIENT_ID")); id != "" {
		f := Forge{
			Kind:         GitHub,
			ClientID:     id,
			ClientSecret: secret(getenv, "SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET"),
			BaseURL:      strings.TrimRight(env(getenv, "SWIFTPROOF_HUB_GITHUB_URL", "https://github.com"), "/"),
			// A GitHub Enterprise Server host serves its API under /api/v3.
			APIURL: strings.TrimRight(env(getenv, "SWIFTPROOF_HUB_GITHUB_API_URL", "https://api.github.com"), "/"),
			Scopes: env(getenv, "SWIFTPROOF_HUB_GITHUB_SCOPES", "repo,read:org"),
		}
		if f.ClientSecret == "" {
			return c, fmt.Errorf("SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET is required when the GitHub client id is set")
		}
		c.Forges[GitHub] = f
	}
	if id := strings.TrimSpace(getenv("SWIFTPROOF_HUB_GITLAB_CLIENT_ID")); id != "" {
		f := Forge{
			Kind:         GitLab,
			ClientID:     id,
			ClientSecret: secret(getenv, "SWIFTPROOF_HUB_GITLAB_CLIENT_SECRET"),
			BaseURL:      strings.TrimRight(env(getenv, "SWIFTPROOF_HUB_GITLAB_URL", "https://gitlab.com"), "/"),
			Scopes:       env(getenv, "SWIFTPROOF_HUB_GITLAB_SCOPES", "api"),
		}
		f.APIURL = f.BaseURL + "/api/v4"
		if f.ClientSecret == "" {
			return c, fmt.Errorf("SWIFTPROOF_HUB_GITLAB_CLIENT_SECRET is required when the GitLab client id is set")
		}
		c.Forges[GitLab] = f
	}
	// A deployment normally has to name a forge, so a typo in the OAuth
	// variables fails loudly instead of serving an application nobody can sign
	// in to. SWIFTPROOF_HUB_ALLOW_NO_FORGE is the deliberate exception: it lets
	// a public deployment answer on its domain before its OAuth application
	// exists, with a sign-in page that says no forge is configured.
	if len(c.Forges) == 0 && !envBool(getenv, "SWIFTPROOF_HUB_ALLOW_NO_FORGE", false) {
		return c, fmt.Errorf("configure at least one forge: set SWIFTPROOF_HUB_GITHUB_CLIENT_ID or SWIFTPROOF_HUB_GITLAB_CLIENT_ID, or SWIFTPROOF_HUB_ALLOW_NO_FORGE=true to start without sign-in")
	}

	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return c, fmt.Errorf("data directory: %w", err)
	}
	c.SessionKey, err = sessionKey(getenv, c.DataDir)
	if err != nil {
		return c, err
	}
	c.PreviousSessionKeys, err = previousKeys(secret(getenv, "SWIFTPROOF_HUB_SESSION_KEY_PREVIOUS"))
	if err != nil {
		return c, err
	}
	return c, nil
}

// ReviewAllowed reports whether a repository may run in review mode with the
// base-branch policy of the given SHA-256 digest. Any other repository, and
// any other version of the policy, is analyzed in lint mode.
func (c Config) ReviewAllowed(forge, fullName, policyDigest string) bool {
	if c.Mode != ModeReview || c.Instance != InstancePrivate {
		return false
	}
	return c.ReviewPolicies[strings.ToLower(forge+":"+fullName)][strings.ToLower(policyDigest)]
}

var reviewEntry = regexp.MustCompile(`^([a-z]+):([A-Za-z0-9._/-]+)@sha256:([0-9a-f]{64})$`)

// reviewPolicies parses the operator's allowlist: entries separated by commas
// or white space, each "<forge>:<owner/repo>@sha256:<digest of .swiftproof.json>".
func reviewPolicies(raw string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	}) {
		m := reviewEntry.FindStringSubmatch(strings.ToLower(entry))
		if m == nil || (m[1] != GitHub && m[1] != GitLab) {
			return nil, fmt.Errorf("SWIFTPROOF_HUB_REVIEW_POLICIES entry %q must read <github|gitlab>:<owner/repo>@sha256:<64 hex>", entry)
		}
		repo := m[1] + ":" + m[2]
		if out[repo] == nil {
			out[repo] = map[string]bool{}
		}
		out[repo][m[3]] = true
	}
	return out, nil
}

// previousKeys parses the keys retired by a rotation.
func previousKeys(raw string) ([][]byte, error) {
	var keys [][]byte
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		key, err := hex.DecodeString(item)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("SWIFTPROOF_HUB_SESSION_KEY_PREVIOUS must list 64-hex-character keys")
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// CallbackURL is the OAuth redirect registered in the forge application.
func (c Config) CallbackURL(kind string) string {
	return c.BaseURL + "/auth/" + kind + "/callback"
}

// WebhookURL is the push endpoint a repository hook posts to. The routing key
// is random per repository; the installation token authorizes the delivery
// and is only ever known to the forge the hook is registered on.
func (c Config) WebhookURL(hookKey, token string) string {
	return c.BaseURL + "/hooks/" + hookKey + "?token=" + url.QueryEscape(token)
}

// sessionKey prefers an operator-provided key so that several replicas share
// sessions; otherwise it persists a generated one next to the data.
func sessionKey(getenv func(string) string, dir string) ([]byte, error) {
	if raw := secret(getenv, "SWIFTPROOF_HUB_SESSION_KEY"); raw != "" {
		key, err := hex.DecodeString(raw)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("SWIFTPROOF_HUB_SESSION_KEY must be 64 hex characters (32 bytes)")
		}
		return key, nil
	}
	path := filepath.Join(dir, "session.key")
	if data, err := os.ReadFile(path); err == nil {
		key, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr == nil && len(key) == 32 {
			return key, nil
		}
		return nil, fmt.Errorf("%s is corrupt: remove it to rotate the key", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("session key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	return key, nil
}

// secret reads a value from the file named by <NAME>_FILE (the Docker secret
// convention the CLI already uses), then from /run/secrets/<NAME>, and only
// then from the variable itself, which a container exposes to anything that
// can read its environment.
func secret(getenv func(string) string, name string) string {
	if path := strings.TrimSpace(getenv(name + "_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	if data, err := os.ReadFile(filepath.Join("/run/secrets", name)); err == nil {
		return strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(getenv(name))
}

func env(getenv func(string) string, name, fallback string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envInt(getenv func(string) string, name string, fallback int) int {
	v, err := strconv.Atoi(env(getenv, name, ""))
	if err != nil {
		return fallback
	}
	return v
}

func envBool(getenv func(string) string, name string, fallback bool) bool {
	v, err := strconv.ParseBool(env(getenv, name, ""))
	if err != nil {
		return fallback
	}
	return v
}

func envDuration(getenv func(string) string, name string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(env(getenv, name, ""))
	if err != nil {
		return fallback
	}
	return v
}
