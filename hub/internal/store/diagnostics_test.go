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
	rec := &Record{UserKey: "user", RepoKey: "repo", Run: Run{Commit: "abc", QueuedAt: time.Now(), Error: "git fetch: secret-token"}}
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
