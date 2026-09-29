package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectLanguages(t *testing.T) {
	for files, want := range map[string]string{
		"go.mod": "go", "Cargo.toml": "rust", "tsconfig.json": "typescript", "package.json": "javascript",
		"pyproject.toml": "python", "requirements.txt": "python", "README.md": "unknown",
	} {
		got := detect(func(name string) bool { return name == files })
		if got != want {
			t.Errorf("detect(%s) = %s, want %s", files, got, want)
		}
	}
}

func TestInitRust(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := initialize([]string{"--repo", dir}, &out, &errOut); code != 0 {
		t.Fatalf("init exited %d: %s", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, ".probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Language string              `json:"language"`
		Commands map[string][]string `json:"commands"`
	}
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Language != "rust" || len(policy.Commands["test"]) == 0 || policy.Commands["test"][0] != "cargo" {
		t.Errorf("policy = %s", data)
	}
}
