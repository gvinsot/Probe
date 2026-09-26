package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func baseEnv(dir string) map[string]string {
	return map[string]string{
		"SWIFTPROOF_HUB_BASE_URL":             "https://swiftproof.example.com",
		"SWIFTPROOF_HUB_DATA_DIR":             dir,
		"SWIFTPROOF_HUB_GITHUB_CLIENT_ID":     "id",
		"SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET": "secret",
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(envOf(baseEnv(dir)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != ":8080" || c.Mode != ModeLint || c.Workers != 2 {
		t.Errorf("defaults = %+v", c)
	}
	if !c.CommitStatus || c.DefaultBranchOnly {
		t.Errorf("commit status defaults on, default-branch-only defaults off; got %v, %v", c.CommitStatus, c.DefaultBranchOnly)
	}
	if len(c.SessionKey) != 32 {
		t.Fatalf("session key length = %d, want 32", len(c.SessionKey))
	}
	if c.CallbackURL(GitHub) != "https://swiftproof.example.com/auth/github/callback" {
		t.Errorf("CallbackURL = %q", c.CallbackURL(GitHub))
	}
	if c.WebhookURL("abc", "t0k") != "https://swiftproof.example.com/hooks/abc?token=t0k" {
		t.Errorf("WebhookURL = %q", c.WebhookURL("abc", "t0k"))
	}
	gh := c.Forges[GitHub]
	if gh.APIURL != "https://api.github.com" || gh.BaseURL != "https://github.com" {
		t.Errorf("github hosts = %+v", gh)
	}
}

func TestSessionKeyIsPersistedAndReused(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(envOf(baseEnv(dir)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "session.key"))
	if err != nil {
		t.Fatalf("the generated key must be persisted: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("session key mode = %o, want 600", mode)
	}
	second, err := Load(envOf(baseEnv(dir)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(first.SessionKey) != string(second.SessionKey) {
		t.Fatal("restarting must not invalidate the sessions of the users")
	}
}

func TestSessionKeyFromTheEnvironmentIsValidated(t *testing.T) {
	dir := t.TempDir()
	values := baseEnv(dir)
	values["SWIFTPROOF_HUB_SESSION_KEY"] = strings.Repeat("ab", 32)
	c, err := Load(envOf(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.SessionKey) != 32 || c.SessionKey[0] != 0xab {
		t.Errorf("session key = %x", c.SessionKey)
	}
	if _, err := os.Stat(filepath.Join(dir, "session.key")); !os.IsNotExist(err) {
		t.Error("an operator-provided key must not be written to disk")
	}
	values["SWIFTPROOF_HUB_SESSION_KEY"] = "too-short"
	if _, err := Load(envOf(values)); err == nil {
		t.Fatal("a malformed key must be refused")
	}
}

func TestLoadRefusesAnIncompleteDeployment(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]map[string]string{
		"no base url": {"SWIFTPROOF_HUB_GITHUB_CLIENT_ID": "id", "SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET": "s"},
		"relative base url": {
			"SWIFTPROOF_HUB_BASE_URL": "swiftproof.example.com", "SWIFTPROOF_HUB_DATA_DIR": dir,
			"SWIFTPROOF_HUB_GITHUB_CLIENT_ID": "id", "SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET": "s",
		},
		"no forge": {"SWIFTPROOF_HUB_BASE_URL": "https://x.example", "SWIFTPROOF_HUB_DATA_DIR": dir},
		"github without secret": {
			"SWIFTPROOF_HUB_BASE_URL": "https://x.example", "SWIFTPROOF_HUB_DATA_DIR": dir,
			"SWIFTPROOF_HUB_GITHUB_CLIENT_ID": "id",
		},
	}
	for name, values := range cases {
		if _, err := Load(envOf(values)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A public deployment is allowed to answer on its domain before its OAuth
// application exists; the sign-in page then states that no forge is available.
func TestLoadAllowsNoForgeWhenTheDeploymentOptsIn(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(envOf(map[string]string{
		"SWIFTPROOF_HUB_BASE_URL":       "https://app.example",
		"SWIFTPROOF_HUB_DATA_DIR":       dir,
		"SWIFTPROOF_HUB_ALLOW_NO_FORGE": "true",
	}))
	if err != nil {
		t.Fatalf("an opted-in deployment without a forge must load: %v", err)
	}
	if len(c.Forges) != 0 {
		t.Errorf("no forge must be configured, got %d", len(c.Forges))
	}
	// The opt-in only covers a missing forge: a half-configured one still fails.
	if _, err := Load(envOf(map[string]string{
		"SWIFTPROOF_HUB_BASE_URL":             "https://app.example",
		"SWIFTPROOF_HUB_DATA_DIR":             dir,
		"SWIFTPROOF_HUB_ALLOW_NO_FORGE":       "true",
		"SWIFTPROOF_HUB_GITHUB_CLIENT_ID":     "id",
		"SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET": "",
	})); err == nil {
		t.Error("a client id without its secret must still be refused")
	}
}

func TestLoadValidatesBounds(t *testing.T) {
	dir := t.TempDir()
	for name, override := range map[string]map[string]string{
		"mode":     {"SWIFTPROOF_HUB_MODE": "audit"},
		"workers":  {"SWIFTPROOF_HUB_WORKERS": "0"},
		"depth":    {"SWIFTPROOF_HUB_CLONE_DEPTH": "-1"},
		"timeout":  {"SWIFTPROOF_HUB_ANALYSIS_TIMEOUT": "1s"},
		"sessions": {"SWIFTPROOF_HUB_SESSION_TTL": "10000h"},
	} {
		values := baseEnv(dir)
		for k, v := range override {
			values[k] = v
		}
		if _, err := Load(envOf(values)); err == nil {
			t.Errorf("%s: an out-of-range value must be refused", name)
		}
	}
}

func TestSelfManagedHosts(t *testing.T) {
	dir := t.TempDir()
	values := map[string]string{
		"SWIFTPROOF_HUB_BASE_URL":             "http://hub.internal:8080",
		"SWIFTPROOF_HUB_DATA_DIR":             dir,
		"SWIFTPROOF_HUB_GITHUB_CLIENT_ID":     "id",
		"SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET": "s",
		"SWIFTPROOF_HUB_GITHUB_URL":           "https://ghe.internal/",
		"SWIFTPROOF_HUB_GITHUB_API_URL":       "https://ghe.internal/api/v3/",
		"SWIFTPROOF_HUB_GITLAB_CLIENT_ID":     "gid",
		"SWIFTPROOF_HUB_GITLAB_CLIENT_SECRET": "gs",
		"SWIFTPROOF_HUB_GITLAB_URL":           "https://gitlab.internal",
		"SWIFTPROOF_HUB_MODE":                 "review",
		"SWIFTPROOF_HUB_INSTANCE":             "private",
		"SWIFTPROOF_HUB_REVIEW_POLICIES":      "github:acme/shop@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"SWIFTPROOF_HUB_DEFAULT_BRANCH_ONLY":  "true",
		"SWIFTPROOF_HUB_ANALYSIS_TIMEOUT":     "30m",
	}
	c, err := Load(envOf(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Forges) != 2 {
		t.Fatalf("both forges must be configured, got %d", len(c.Forges))
	}
	if c.Forges[GitHub].APIURL != "https://ghe.internal/api/v3" {
		t.Errorf("trailing slashes must be trimmed, got %q", c.Forges[GitHub].APIURL)
	}
	if c.Forges[GitLab].APIURL != "https://gitlab.internal/api/v4" {
		t.Errorf("gitlab api = %q", c.Forges[GitLab].APIURL)
	}
	if c.Mode != ModeReview || !c.DefaultBranchOnly || c.AnalysisTimeout != 30*time.Minute {
		t.Errorf("overrides = %+v", c)
	}
}

func TestSecretCanComeFromAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_secret")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	values := baseEnv(dir)
	delete(values, "SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET")
	values["SWIFTPROOF_HUB_GITHUB_CLIENT_SECRET_FILE"] = path
	c, err := Load(envOf(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Forges[GitHub].ClientSecret != "file-secret" {
		t.Errorf("secret = %q, want the trimmed file content", c.Forges[GitHub].ClientSecret)
	}
}

func TestReviewModeIsRefusedOnAPublicInstance(t *testing.T) {
	values := baseEnv(t.TempDir())
	values["SWIFTPROOF_HUB_MODE"] = "review"
	values["SWIFTPROOF_HUB_REVIEW_POLICIES"] = "github:acme/shop@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// The instance defaults to public: forgetting the variable must not open
	// review to whoever signs in.
	if _, err := Load(envOf(values)); err == nil || !strings.Contains(err.Error(), "public instance") {
		t.Fatalf("review on a default (public) instance must be refused, got %v", err)
	}
	values["SWIFTPROOF_HUB_INSTANCE"] = "public"
	if _, err := Load(envOf(values)); err == nil {
		t.Fatal("review on an explicitly public instance must be refused")
	}
	values["SWIFTPROOF_HUB_INSTANCE"] = "shared"
	if _, err := Load(envOf(values)); err == nil {
		t.Fatal("an unknown instance kind must be refused")
	}
}

func TestReviewModeNeedsValidatedPolicies(t *testing.T) {
	values := baseEnv(t.TempDir())
	values["SWIFTPROOF_HUB_MODE"] = "review"
	values["SWIFTPROOF_HUB_INSTANCE"] = "private"
	if _, err := Load(envOf(values)); err == nil {
		t.Fatal("review without any validated policy must be refused")
	}
	values["SWIFTPROOF_HUB_REVIEW_POLICIES"] = "acme/shop"
	if _, err := Load(envOf(values)); err == nil {
		t.Fatal("a malformed allowlist entry must be refused")
	}
	values["SWIFTPROOF_HUB_REVIEW_POLICIES"] = "github:Acme/Shop@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef, gitlab:team/api@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	c, err := Load(envOf(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ReviewAllowed("github", "acme/shop", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Error("a validated repository and policy must run in review mode")
	}
	if c.ReviewAllowed("github", "acme/shop", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee") {
		t.Error("another version of the policy must fall back to lint")
	}
	if c.ReviewAllowed("github", "acme/other", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Error("an unlisted repository must fall back to lint")
	}
	c.Mode = ModeLint
	if c.ReviewAllowed("github", "acme/shop", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Error("a lint instance never reviews")
	}
}

func TestLintIsAllowedOnAPublicInstanceWithQuotas(t *testing.T) {
	c, err := Load(envOf(baseEnv(t.TempDir())))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Instance != InstancePublic || c.Mode != ModeLint {
		t.Errorf("defaults = %s/%s, want public/lint", c.Instance, c.Mode)
	}
	if c.UserQuota < 1 || c.HookRate < 1 {
		t.Errorf("quotas must be on by default, got %d/%d", c.UserQuota, c.HookRate)
	}
}

func TestSessionKeyAndPreviousKeysCanComeFromFiles(t *testing.T) {
	dir := t.TempDir()
	current, previous := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	keyPath, prevPath := filepath.Join(dir, "key"), filepath.Join(dir, "prev")
	if err := os.WriteFile(keyPath, []byte(current+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prevPath, []byte(previous+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := baseEnv(dir)
	values["SWIFTPROOF_HUB_SESSION_KEY_FILE"] = keyPath
	values["SWIFTPROOF_HUB_SESSION_KEY_PREVIOUS_FILE"] = prevPath
	c, err := Load(envOf(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionKey[0] != 0xab || len(c.PreviousSessionKeys) != 1 || c.PreviousSessionKeys[0][0] != 0xcd {
		t.Errorf("keys = %x / %x", c.SessionKey, c.PreviousSessionKeys)
	}
	values["SWIFTPROOF_HUB_SESSION_KEY_PREVIOUS_FILE"] = ""
	values["SWIFTPROOF_HUB_SESSION_KEY_PREVIOUS"] = "short"
	if _, err := Load(envOf(values)); err == nil {
		t.Error("a malformed previous key must be refused")
	}
}
