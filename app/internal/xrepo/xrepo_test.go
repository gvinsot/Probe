package xrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/model"
)

func gitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "main")
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
}

func TestOpenListReadSearch(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, filepath.Join(root, "company", "shared-types"), map[string]string{
		"api/refund.ts":  "export interface Refund {\n  amountCents: number;\n}\n",
		"api/order.ts":   "export interface Order { id: string }\n",
		"docs/README.md": "Refund types live in api/.\n",
		".env":           "SECRET=1\n",
		"bin/blob.dat":   "a\x00b",
	})
	gitRepo(t, filepath.Join(root, "sdk"), map[string]string{"client.go": "func Refund(amountCents int) {}\n"})
	// Uncommitted working-tree edits are never read.
	os.WriteFile(filepath.Join(root, "sdk", "client.go"), []byte("func Refund(amount float64) {}\n"), 0644)

	set, records := Open(context.Background(), []config.ResolvedRepo{
		{ContextRepo: config.ContextRepo{Name: "company/shared-types", Paths: []string{"api/**", "bin/**", ".env"}}, Clusters: []string{"payments"}},
		{ContextRepo: config.ContextRepo{Name: "company/sdk", Role: "payment SDK"}},
		{ContextRepo: config.ContextRepo{Name: "company/missing"}},
		{ContextRepo: config.ContextRepo{Name: "company/remote", URL: "https://example.invalid/remote.git"}},
	}, Options{Dir: root})
	if len(records) != 4 || records[0].Status != model.ContextAvailable || records[0].Files != 3 || records[0].Source != model.ContextLocal || len(records[0].Commit) != 40 {
		t.Fatalf("records %+v", records)
	}
	if records[1].Status != model.ContextAvailable || records[1].Role != "payment SDK" {
		t.Fatalf("DIR/<repo> checkout %+v", records[1])
	}
	if records[2].Status != model.ContextUnavailable || !strings.Contains(records[2].Reason, "--context-dir") {
		t.Fatalf("missing %+v", records[2])
	}
	if records[3].Status != model.ContextUnavailable || !strings.Contains(records[3].Reason, "--fetch-context") {
		t.Fatalf("unfetched %+v", records[3])
	}
	if len(set.Repos()) != 2 {
		t.Fatalf("repos %+v", set.Repos())
	}
	files, total, err := set.List("COMPANY/shared-types", "api/", 400)
	if err != nil || total != 2 || strings.Join(files, ",") != "api/order.ts,api/refund.ts" {
		t.Fatalf("list %v %d %v", files, total, err)
	}
	content, lines, truncated, err := set.Read(context.Background(), "company/shared-types", "api/refund.ts", 2, 2)
	if err != nil || content != "2:   amountCents: number;\n" || lines != 4 || truncated {
		t.Fatalf("read %q %d %v %v", content, lines, truncated, err)
	}
	for _, path := range []string{".env", "docs/README.md", "../x", "bin/blob.dat"} {
		if _, _, _, err := set.Read(context.Background(), "company/shared-types", path, 0, 0); err == nil {
			t.Errorf("read %s: accepted", path)
		}
	}
	matches, truncated, err := set.Search(context.Background(), "", "amountCents")
	if err != nil || truncated || len(matches) != 2 {
		t.Fatalf("search %+v %v %v", matches, truncated, err)
	}
	for _, m := range matches {
		if m.Repo == "company/sdk" && !strings.Contains(m.Text, "amountCents int") {
			t.Fatalf("the committed file is read, not the working tree: %+v", m)
		}
	}
	if _, _, err := set.Search(context.Background(), "company/unknown", "x"); err == nil {
		t.Fatal("unknown repository accepted")
	}
	if _, _, err := set.Search(context.Background(), "", " "); err == nil {
		t.Fatal("empty query accepted")
	}
}

// A directory inside another repository is not a checkout of its own, and an
// explicit path must be a repository root.
func TestLocalCheckoutMustBeARoot(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root, map[string]string{"company/shared-types/README.md": "x\n"})
	_, records := Open(context.Background(), []config.ResolvedRepo{{ContextRepo: config.ContextRepo{Name: "company/shared-types"}}}, Options{Dir: root})
	if records[0].Status != model.ContextUnavailable {
		t.Fatalf("a subdirectory of another repository was used: %+v", records[0])
	}
	_, records = Open(context.Background(), []config.ResolvedRepo{{ContextRepo: config.ContextRepo{Name: "company/shared-types"}}}, Options{Paths: map[string]string{"company/shared-types": filepath.Join(root, "company")}})
	if records[0].Status != model.ContextUnavailable || !strings.Contains(records[0].Reason, "not the root") {
		t.Fatalf("explicit path %+v", records[0])
	}
}
