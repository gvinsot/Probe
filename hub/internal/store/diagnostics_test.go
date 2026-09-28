package store

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSafeErrorRedactsBeforeTruncating(t *testing.T) {
	for _, message := range []string{
		"git fetch: exit status 128: GIT_CONFIG_VALUE_0=Authorization: Basic secret-token https://user:password@host/repo?token=secret-token",
		"Authorization: Bearer secret-token",
		"fetch https://user:secret-token@host/repo?access_token=secret-token#secret-token failed",
		"access_token=" + strings.Repeat("secret-token", 1000),
	} {
		got := SafeError(message)
		if strings.Contains(got, "secret-token") || len(got) > MaxErrorBytes {
			t.Fatalf("unsafe error: %q", got)
		}
	}
	if got := SafeError(strings.Repeat("é", MaxErrorBytes)); len(got) > MaxErrorBytes || !utf8.ValidString(got) {
		t.Fatal("invalid truncation")
	}
}
func TestStoredAndLegacyRunErrorsAreSafe(t *testing.T) {
	s := open(t)
	rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "abc", QueuedAt: time.Now(), Error: "git fetch: token=secret-token"}}
	if err := s.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Record("user", "repo", "abc")
	if err != nil || strings.Contains(got.Error, "secret-token") {
		t.Fatalf("stored error: %+v %v", got, err)
	}
	path, err := s.path("reports", "user", "repo", "abc.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, rec); err != nil {
		t.Fatal(err)
	} // pre-upgrade artifact
	got, err = s.Record("user", "repo", "abc")
	if err != nil || strings.Contains(got.Error, "secret-token") {
		t.Fatalf("legacy error: %+v %v", got, err)
	}
}

func TestSafeErrorBareCredentialsAndUsefulDiagnostics(t *testing.T) {
	for _, secret := range []string{
		"ghs_SECRETVALUE",
		"ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"github_pat_0123456789_abcdefghijklmnopqrstuvwxyz",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.dGVzdHNpZ25hdHVyZQ",
		"X7u9Qp2kR6v3Nj8tS1w4Za5cB0dEfGhI",
	} {
		got := SafeError("fetch '" + secret + "' failed")
		if strings.Contains(got, secret) || !strings.Contains(got, "failed") {
			t.Fatalf("unsafe diagnostic: %s", got)
		}
	}
	message := "git fetch: exit status 128: repository unavailable"
	if got := SafeError(message); got != message {
		t.Fatalf("useful diagnostic removed: %s", got)
	}
	if got := SafeError("fetch short-opaque failed", "short-opaque"); strings.Contains(got, "short-opaque") {
		t.Fatalf("known token leaked: %s", got)
	}
}
