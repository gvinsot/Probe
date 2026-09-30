package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

func summarizedReport() *model.Report {
	r := &model.Report{Version: 1, ExitCode: 2, Change: model.Change{Files: []model.ChangedFile{{Path: "auth.go", Status: "M"}}}}
	r.PRSummary = &model.PRSummary{
		Title:           "Allow every user [click](https://evil.example) <script>",
		Overview:        "The admin check now returns true.\nIt removes the role test. Token ghp_abcdefghijklmnopqrstuvwxyz0123456789 leaked.",
		Changes:         []model.PRSummaryChange{{Area: "Authorization", Summary: "Allowed no longer compares the user.", Files: []string{"auth.go"}}},
		BehaviorChanges: []string{"Non-admin users are allowed"},
		Risks:           []string{"Unverified: every user becomes admin"},
		ReviewFocus:     []string{"auth.go line 3"},
		Testing:         "No test was executed.",
		Model:           "test-model",
	}
	return r
}

func TestPRSummaryIsWrittenWithMarkdown(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, summarizedReport(), []string{FormatMarkdown, FormatJSON}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "PR_SUMMARY.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# Allow every user", "## Changes\n\n- **Authorization**: Allowed no longer compares the user. (auth.go)", "## Risks\n\n- Unverified: every user becomes admin", "## Where to look first", "## Testing\n\nNo test was executed.", "_Written by test-model from the Probe review of 1 changed files (human review required).", PRSummaryMarker} {
		if !strings.Contains(text, want) {
			t.Fatalf("PR_SUMMARY.md lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<script>") || strings.Contains(text, "](https://evil.example)") || strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("untrusted text not escaped or redacted:\n%s", text)
	}
	if strings.Count(text, "Allow every user") != 1 {
		t.Fatalf("title repeated:\n%s", text)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "CONFIDENCE_REPORT.md"))
	if !strings.Contains(string(md), "## Pull Request Summary\n\nModel output, not evidence") || strings.Index(string(md), "## Pull Request Summary") > strings.Index(string(md), "## Change Summary") {
		t.Fatalf("markdown section:\n%s", md)
	}
}

func TestPRSummaryAbsentOrExplicit(t *testing.T) {
	dir := t.TempDir()
	r := summarizedReport()
	r.PRSummary = nil
	if err := Write(dir, r, []string{FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PR_SUMMARY.md")); !os.IsNotExist(err) {
		t.Fatal("PR_SUMMARY.md written without a summary")
	}
	md, _ := os.ReadFile(filepath.Join(dir, "CONFIDENCE_REPORT.md"))
	if strings.Contains(string(md), "Pull Request Summary") {
		t.Fatal("summary section without a summary")
	}
	if err := Write(dir, r, []string{FormatPRSummary}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "PR_SUMMARY.md"))
	if !strings.Contains(string(data), "No AI summary was generated") {
		t.Fatalf("explicit format:\n%s", data)
	}
	if !ValidFormat(FormatPRSummary) {
		t.Fatal("pr-summary is not a valid format")
	}
}
