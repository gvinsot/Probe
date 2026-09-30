package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyFoldersBecomeSources(t *testing.T) {
	dir := t.TempDir()
	a, b := t.TempDir(), t.TempDir()
	legacy := `{"folders":[` + quote(a) + `,` + quote(b) + `],"provider":"","language":"fr","scan_seconds":30,"max_file_mb":50}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Get()
	if len(s.Sources) != 2 || s.Sources[0].Type != SourceFolder || s.Sources[0].Path != a || s.Sources[0].ID != FolderSourceID(a) {
		t.Fatalf("migrated sources: %+v", s.Sources)
	}
	if s.Language != "fr" || s.ScanSeconds != 30 {
		t.Fatalf("other settings lost: %+v", s)
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if strings.Contains(string(data), `"folders"`) {
		t.Fatalf("legacy key written again: %s", data)
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func folders(paths ...string) Settings {
	s := Defaults()
	for _, p := range paths {
		s.Sources = append(s.Sources, Source{Type: SourceFolder, Path: p})
	}
	s.Normalize()
	return s
}

func TestNestedFoldersAreRefused(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "Contracts")
	sibling := root + "-other"
	for _, p := range []string{child, sibling} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })
	if err := folders(root, child).Validate(); err == nil {
		t.Fatal("a folder inside another one was accepted")
	}
	if err := folders(child, root).Validate(); err == nil {
		t.Fatal("a folder containing another one was accepted")
	}
	// A common prefix is not nesting.
	if err := folders(root, sibling).Validate(); err != nil {
		t.Fatalf("sibling folders refused: %v", err)
	}
	if s := folders(root, root+string(filepath.Separator)); len(s.Sources) != 1 {
		t.Fatalf("the same folder twice: %+v", s.Sources)
	}
}

// A watched folder that is unavailable (unplugged drive, network share)
// must not block saving the other settings; a new one must exist.
func TestUnavailableFolderAlreadyWatched(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	gone := filepath.Join(t.TempDir(), "share")
	os.MkdirAll(gone, 0o700)
	if err := st.Save(folders(gone)); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(gone)
	s := st.Get()
	s.Language = "fr"
	if err := st.Save(s); err != nil {
		t.Fatalf("saving with an unavailable watched folder: %v", err)
	}
	s.Sources = append(s.Sources, Source{Type: SourceFolder, Path: filepath.Join(gone, "..", "missing")})
	if err := st.Save(s); err == nil {
		t.Fatal("a missing new folder was accepted")
	}
}

func TestGoogleDriveSources(t *testing.T) {
	s := Defaults()
	s.Sources = []Source{{Type: SourceGoogleDrive, Account: " Me@Example.com ", FolderID: "abc_1-2", FolderName: "Legal", ScanSeconds: 3}}
	s.Normalize()
	src := s.Sources[0]
	if !sourceID.MatchString(src.ID) || src.Account != "me@example.com" || src.ScanSeconds != 10 {
		t.Fatalf("normalized source: %+v", src)
	}
	if err := s.Validate(); err == nil {
		t.Fatal("a source of an account that is not connected was accepted")
	}
	s.GoogleAccounts = []string{"me@example.com"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	id := src.ID
	s.Normalize()
	if s.Sources[0].ID != id {
		t.Fatal("the id of a source changed")
	}
	if got := s.Sources[0].Label(); got != "Google Drive · me@example.com · My Drive › Legal" {
		t.Fatalf("label %q", got)
	}

	// The id names a cache file: nothing else than a generated id is kept.
	s.Sources[0].ID = "../../settings"
	s.Normalize()
	if !sourceID.MatchString(s.Sources[0].ID) {
		t.Fatalf("unsafe id kept: %q", s.Sources[0].ID)
	}
	s.Sources[0].FolderID = "a/b"
	if err := s.Validate(); err == nil {
		t.Fatal("an invalid folder id was accepted")
	}
}

func TestUpdateSkipsFolderChecks(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	gone := filepath.Join(t.TempDir(), "gone")
	err := st.Update(func(s *Settings) {
		s.Sources = append(s.Sources, Source{Type: SourceFolder, Path: gone})
		s.GoogleAccounts = append(s.GoogleAccounts, "Me@Example.com", "me@example.com")
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := st.Get(); len(s.GoogleAccounts) != 1 || s.GoogleAccounts[0] != "me@example.com" || len(s.Sources) != 1 {
		t.Fatalf("updated settings: %+v", s)
	}
}

func TestDropboxFolders(t *testing.T) {
	info := filepath.Join(t.TempDir(), "info.json")
	os.WriteFile(info, []byte(`{"personal":{"path":"/home/me/Dropbox"},"business":{"path":"/home/me/Dropbox (Acme)"}}`), 0o600)
	got := dropboxFolders(info, filepath.Join(t.TempDir(), "missing.json"))
	if len(got) != 2 || got[0].Path != "/home/me/Dropbox" || got[1].Label != "Dropbox (business)" {
		t.Fatalf("dropbox folders: %+v", got)
	}
}

func TestLanguage(t *testing.T) {
	s := Defaults()
	if s.Language != SystemLanguage() {
		t.Fatalf("fresh installation language = %q, want the system's %q", s.Language, SystemLanguage())
	}
	for _, lang := range []string{"en", "fr", "es", "de", "pt", "it"} {
		s.Language = lang
		s.Normalize()
		if s.Language != lang {
			t.Errorf("supported language %q changed to %q", lang, s.Language)
		}
	}
	s.Language = "ja"
	s.Normalize()
	if s.Language != SystemLanguage() {
		t.Errorf("unsupported language kept or not reset: %q", s.Language)
	}
}
