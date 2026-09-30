// Package config holds the settings of Probe Desktop and the location of its
// data. API keys are not stored here but in the operating system keychain
// (see package secret).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Settings is the user configuration, saved as settings.json.
type Settings struct {
	// Folders are the watched roots, typically OneDrive or Google Drive folders.
	Folders []string `json:"folders"`
	// Provider selects the AI reviewer; empty keeps everything local.
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// BaseURL overrides the OpenAI endpoint, for a compatible server run
	// on premises (vLLM, Ollama…).
	BaseURL string `json:"base_url,omitempty"`
	// Language of the AI explanations: "en" or "fr".
	Language string `json:"language"`
	// ScanSeconds is the delay between two scans of the watched folders.
	ScanSeconds int `json:"scan_seconds"`
	// MaxFileMB skips larger documents.
	MaxFileMB int `json:"max_file_mb"`
	// DownloadCloudFiles lets the first scan read files that are only in the
	// cloud, which downloads them. Off by default: a large OneDrive would be
	// downloaded entirely just to capture baselines.
	DownloadCloudFiles bool `json:"download_cloud_files"`
}

// Defaults returns the settings of a fresh installation.
func Defaults() Settings {
	return Settings{Folders: []string{}, Language: "en", ScanSeconds: 60, MaxFileMB: 50}
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
	if s.Language != "fr" {
		s.Language = "en"
	}
	switch s.Provider {
	case ProviderAnthropic, ProviderOpenAI:
	default:
		s.Provider = ProviderNone
	}
	s.Model = strings.TrimSpace(s.Model)
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	seen := map[string]bool{}
	folders := []string{}
	for _, f := range s.Folders {
		f = filepath.Clean(strings.TrimSpace(f))
		if f == "." || seen[strings.ToLower(f)] {
			continue
		}
		seen[strings.ToLower(f)] = true
		folders = append(folders, f)
	}
	s.Folders = folders
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

// Validate checks settings coming from the interface.
func (s Settings) Validate() error {
	for _, f := range s.Folders {
		if !filepath.IsAbs(f) {
			return fmt.Errorf("folder %q must be an absolute path", f)
		}
		info, err := os.Stat(f)
		if err != nil {
			return fmt.Errorf("folder %q: %w", f, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%q is not a folder", f)
		}
	}
	if s.BaseURL != "" && !strings.HasPrefix(s.BaseURL, "https://") && !strings.HasPrefix(s.BaseURL, "http://") {
		return errors.New("the endpoint URL must start with https:// or http://")
	}
	return nil
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

// Open reads the settings of a data directory, or starts from the defaults.
func Open(dir string) (*Store, error) {
	st := &Store{path: filepath.Join(dir, "settings.json"), cur: Defaults()}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &st.cur); err != nil {
			return nil, fmt.Errorf("settings.json: %w", err)
		}
	}
	st.cur.Normalize()
	return st, nil
}

// Get returns a copy of the current settings.
func (st *Store) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.cur
	// Never nil: the interface reads folders as a JSON array, not null.
	s.Folders = append([]string{}, st.cur.Folders...)
	return s
}

// Save validates, persists and applies new settings.
func (st *Store) Save(s Settings) error {
	s.Normalize()
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
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
