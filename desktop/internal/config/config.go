// Package config holds the settings of Probe Desktop and the location of its
// data. API keys and Google tokens are not stored here but in the operating
// system keychain (see package secret).
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/gvinsot/Probe/desktop/internal/i18n"
	"github.com/gvinsot/Probe/desktop/internal/msg"
	"github.com/gvinsot/Probe/desktop/internal/platform"
)

// Provider identifiers for the optional AI reviewer.
const (
	ProviderNone      = ""
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// Default models, overridable in the settings.
const (
	DefaultAnthropicModel = "claude-opus-5-5"
	DefaultOpenAIModel    = "gpt-5"
)

// Source types.
const (
	// SourceFolder is a folder of this computer: a OneDrive, Google Drive,
	// Dropbox or iCloud folder synchronized by its client, a network share
	// or any local folder.
	SourceFolder = "folder"
	// SourceGoogleDrive reads a Google Drive through its API, without a
	// synchronization client, Google Docs, Sheets and Slides included.
	SourceGoogleDrive = "gdrive"
)

// Source is one watched location.
type Source struct {
	// ID identifies the source in the state of the documents. A folder's is
	// derived from its path; a Google Drive source gets a random one.
	ID   string `json:"id"`
	Type string `json:"type"`
	// Path is the root of a folder source.
	Path string `json:"path,omitempty"`
	// Account is the Google account of a Google Drive source, DriveID the
	// shared drive ("" for My Drive) and FolderID the watched folder ("" for
	// the whole drive). The names are only shown.
	Account    string `json:"account,omitempty"`
	DriveID    string `json:"drive_id,omitempty"`
	DriveName  string `json:"drive_name,omitempty"`
	FolderID   string `json:"folder_id,omitempty"`
	FolderName string `json:"folder_name,omitempty"`
	// ScanSeconds overrides the scan interval for this source; 0 keeps the
	// global one. A network share or a large drive deserves a slower pace.
	ScanSeconds int `json:"scan_seconds,omitempty"`
}

// Label names the source in the interface and the logs.
func (s Source) Label() string {
	if s.Type != SourceGoogleDrive {
		return s.Path
	}
	name := s.DriveName
	if name == "" {
		name = "My Drive"
	}
	if s.FolderID != "" {
		folder := s.FolderName
		if folder == "" {
			folder = s.FolderID
		}
		name += " › " + folder
	}
	return "Google Drive · " + s.Account + " · " + name
}

// Settings is the user configuration, saved as settings.json.
type Settings struct {
	// Sources are the watched locations.
	Sources []Source `json:"sources"`
	// Provider selects the AI reviewer; empty keeps everything local.
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// BaseURL overrides the OpenAI endpoint, for a compatible server run
	// on premises (vLLM, Ollama…).
	BaseURL string `json:"base_url,omitempty"`
	// Language of the interface, the reports, the tray menu and the AI
	// explanations: one of i18n.Languages. A fresh installation takes the
	// language of the system.
	Language string `json:"language"`
	// ScanSeconds is the delay between two scans of a source.
	ScanSeconds int `json:"scan_seconds"`
	// MaxFileMB skips larger documents.
	MaxFileMB int `json:"max_file_mb"`
	// DownloadCloudFiles lets the first scan read files that are only in the
	// cloud, which downloads them. Off by default: a large OneDrive would be
	// downloaded entirely just to capture baselines.
	DownloadCloudFiles bool `json:"download_cloud_files"`
	// GoogleClientID is the OAuth client used to connect Google accounts,
	// typically created by the company in its own Google Cloud project; its
	// secret is in the keychain. Empty uses the client built into the
	// application, if any.
	GoogleClientID string `json:"google_client_id,omitempty"`
	// GoogleAccounts are the connected Google accounts. Only the engine
	// changes this list, when an account is connected or disconnected.
	GoogleAccounts []string `json:"google_accounts"`
}

// Defaults returns the settings of a fresh installation.
func Defaults() Settings {
	return Settings{Sources: []Source{}, GoogleAccounts: []string{}, Language: SystemLanguage(), ScanSeconds: 60, MaxFileMB: 50}
}

var systemLanguage = sync.OnceValue(func() string {
	if l := i18n.Match(platform.Locale()); l != "" {
		return l
	}
	return "en"
})

// SystemLanguage is the supported language of the system locale, English
// when the system uses another language.
func SystemLanguage() string { return systemLanguage() }

// FolderSourceID derives the id of a folder source from its path, so the
// documents found before sources existed keep their state.
func FolderSourceID(path string) string {
	sum := sha256.Sum256([]byte(FoldPath(path)))
	return "folder-" + hex.EncodeToString(sum[:8])
}

// FoldPath cleans a path and folds its case where the file system ignores
// it (Windows and macOS), to compare paths.
func FoldPath(path string) string {
	p := filepath.Clean(path)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

// sourceID is the shape of a generated id; it names a file of the data
// directory, so nothing else is accepted.
var sourceID = regexp.MustCompile(`^gdrive-[0-9a-f]{16}$`)

// driveID is the shape of the Google Drive ids of drives and folders.
var driveID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

func newSourceID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// Normalize fills missing values and cleans user input.
func (s *Settings) Normalize() {
	d := Defaults()
	if s.ScanSeconds < 10 {
		s.ScanSeconds = d.ScanSeconds
	}
	if s.MaxFileMB <= 0 {
		s.MaxFileMB = d.MaxFileMB
	}
	if !i18n.Supported(s.Language) {
		s.Language = d.Language
	}
	switch s.Provider {
	case ProviderAnthropic, ProviderOpenAI:
	default:
		s.Provider = ProviderNone
	}
	s.Model = strings.TrimSpace(s.Model)
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	s.GoogleClientID = strings.TrimSpace(s.GoogleClientID)

	accounts := []string{}
	seenAccount := map[string]bool{}
	for _, a := range s.GoogleAccounts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seenAccount[a] {
			continue
		}
		seenAccount[a] = true
		accounts = append(accounts, a)
	}
	s.GoogleAccounts = accounts

	seen := map[string]bool{}
	sources := []Source{}
	for _, src := range s.Sources {
		if src.ScanSeconds < 0 {
			src.ScanSeconds = 0
		} else if src.ScanSeconds > 0 && src.ScanSeconds < 10 {
			src.ScanSeconds = 10
		}
		var key string
		switch src.Type {
		case SourceFolder, "":
			path := filepath.Clean(strings.TrimSpace(src.Path))
			if strings.TrimSpace(src.Path) == "" || path == "." {
				continue
			}
			src = Source{ID: FolderSourceID(path), Type: SourceFolder, Path: path, ScanSeconds: src.ScanSeconds}
			key = "folder\x00" + FoldPath(path)
		case SourceGoogleDrive:
			src.Path = ""
			src.Account = strings.ToLower(strings.TrimSpace(src.Account))
			src.DriveID = strings.TrimSpace(src.DriveID)
			src.FolderID = strings.TrimSpace(src.FolderID)
			src.DriveName = strings.TrimSpace(src.DriveName)
			src.FolderName = strings.TrimSpace(src.FolderName)
			if !sourceID.MatchString(src.ID) {
				src.ID = newSourceID("gdrive")
			}
			key = "gdrive\x00" + src.Account + "\x00" + src.DriveID + "\x00" + src.FolderID
		default:
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, src)
	}
	s.Sources = sources
}

// Folders returns the paths of the folder sources.
func (s Settings) Folders() []string {
	var out []string
	for _, src := range s.Sources {
		if src.Type == SourceFolder {
			out = append(out, src.Path)
		}
	}
	return out
}

// EffectiveModel returns the configured model or the provider default.
func (s Settings) EffectiveModel() string {
	if s.Model != "" {
		return s.Model
	}
	switch s.Provider {
	case ProviderAnthropic:
		return DefaultAnthropicModel
	case ProviderOpenAI:
		return DefaultOpenAIModel
	}
	return ""
}

// Interval returns the delay between two scans of a source.
func (s Settings) Interval(src Source) int {
	if src.ScanSeconds > 0 {
		return src.ScanSeconds
	}
	return s.ScanSeconds
}

// Validate checks settings coming from the interface. A folder must exist
// when it is added; one that was already watched is accepted while it is
// unavailable (a network share, an unplugged drive), so that the other
// settings can still be changed.
func (s Settings) Validate(previous ...Settings) error {
	known := map[string]bool{}
	for _, p := range previous {
		for _, src := range p.Sources {
			known[src.ID] = true
		}
	}
	accounts := map[string]bool{}
	for _, a := range s.GoogleAccounts {
		accounts[a] = true
	}
	var folders []string
	for _, src := range s.Sources {
		switch src.Type {
		case SourceFolder:
			if !filepath.IsAbs(src.Path) {
				return fmt.Errorf(msg.M("folder %q must be an absolute path"), src.Path)
			}
			if !known[src.ID] {
				info, err := os.Stat(src.Path)
				if err != nil {
					return fmt.Errorf("folder %q: %w", src.Path, err)
				}
				if !info.IsDir() {
					return fmt.Errorf(msg.M("%q is not a folder"), src.Path)
				}
			}
			for _, other := range folders {
				if a, b, nested := nestedFolders(other, src.Path); nested {
					return fmt.Errorf(msg.M("folder %q is inside %q: watch only one of them"), b, a)
				}
			}
			folders = append(folders, src.Path)
		case SourceGoogleDrive:
			if !accounts[src.Account] {
				return fmt.Errorf(msg.M("the Google account %q is not connected"), src.Account)
			}
			if src.DriveID != "" && !driveID.MatchString(src.DriveID) {
				return fmt.Errorf(msg.M("invalid shared drive id %q"), src.DriveID)
			}
			if src.FolderID != "" && !driveID.MatchString(src.FolderID) {
				return fmt.Errorf(msg.M("invalid Google Drive folder id %q"), src.FolderID)
			}
		}
	}
	if s.BaseURL != "" && !strings.HasPrefix(s.BaseURL, "https://") && !strings.HasPrefix(s.BaseURL, "http://") {
		return errors.New(msg.M("the endpoint URL must start with https:// or http://"))
	}
	return nil
}

// nestedFolders reports whether one folder contains the other, returning
// the outer one first. A document seen through both would belong to either
// depending on the scan order.
func nestedFolders(a, b string) (outer, inner string, nested bool) {
	fa, fb := FoldPath(a), FoldPath(b)
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	switch {
	case fa == fb:
		return a, b, true
	case inside(fa, fb):
		return a, b, true
	case inside(fb, fa):
		return b, a, true
	}
	return "", "", false
}

// Dir returns the data directory: %AppData%\Probe Desktop on Windows,
// ~/Library/Application Support/Probe Desktop on macOS. PROBE_DESKTOP_HOME
// overrides it, which tests and portable installs use.
func Dir() (string, error) {
	if dir := os.Getenv("PROBE_DESKTOP_HOME"); dir != "" {
		return dir, os.MkdirAll(dir, 0o700)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "Probe Desktop")
	return dir, os.MkdirAll(dir, 0o700)
}

// Store loads and saves the settings file.
type Store struct {
	path string
	mu   sync.Mutex
	cur  Settings
}

// legacySettings reads the settings of the versions that only watched
// folders, listed as plain paths.
type legacySettings struct {
	Settings
	Folders []string `json:"folders"`
}

// Open reads the settings of a data directory, or starts from the defaults.
func Open(dir string) (*Store, error) {
	st := &Store{path: filepath.Join(dir, "settings.json"), cur: Defaults()}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		legacy := legacySettings{Settings: Defaults()}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("settings.json: %w", err)
		}
		st.cur = legacy.Settings
		if len(st.cur.Sources) == 0 {
			for _, f := range legacy.Folders {
				st.cur.Sources = append(st.cur.Sources, Source{Type: SourceFolder, Path: f})
			}
		}
	}
	st.cur.Normalize()
	return st, nil
}

// Get returns a copy of the current settings.
func (st *Store) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cur.clone()
}

// clone copies the lists, which the interface reads as JSON arrays, never
// null.
func (s Settings) clone() Settings {
	s.Sources = append([]Source{}, s.Sources...)
	s.GoogleAccounts = append([]string{}, s.GoogleAccounts...)
	return s
}

// Save validates, persists and applies new settings.
func (st *Store) Save(s Settings) error {
	s.Normalize()
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := s.Validate(st.cur); err != nil {
		return err
	}
	return st.write(s)
}

// Update changes the current settings in place. It is meant for the engine
// (a Google account connected), so it does not check that the watched
// folders are available.
func (st *Store) Update(fn func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.cur.clone()
	fn(&s)
	s.Normalize()
	return st.write(s)
}

func (st *Store) write(s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(st.path, data); err != nil {
		return err
	}
	st.cur = s
	return nil
}

// WriteFileAtomic replaces a file without leaving it half written if the
// application stops in the middle.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
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
