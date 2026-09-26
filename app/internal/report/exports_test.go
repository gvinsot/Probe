package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// Write renders every format from the same finalized report and changes
// neither the report nor its exit code.
func TestWriteAllFormats(t *testing.T) {
	r := exportFixture()
	Finalize(r, true)
	before, _ := json.Marshal(r)
	dir := t.TempDir()
	if err := Write(dir, r, []string{FormatMarkdown, FormatJSON, FormatSARIF, FormatPRComment}, WithReportURL("https://example.invalid/runs/1")); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) || r.ExitCode != 1 {
		t.Fatal("Write changed the report")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "CONFIDENCE_REPORT.md,PR_COMMENT.md,confidence-report.json,confidence-report.sarif" {
		t.Fatalf("files %v", names)
	}
	// The canonical reports do not depend on the export formats.
	plain := t.TempDir()
	if err := Write(plain, r, []string{FormatMarkdown, FormatJSON}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CONFIDENCE_REPORT.md", "confidence-report.json"} {
		a, _ := os.ReadFile(filepath.Join(dir, name))
		b, _ := os.ReadFile(filepath.Join(plain, name))
		if string(a) != string(b) {
			t.Fatalf("%s depends on the export formats", name)
		}
	}
}

// A report URL that fails validation writes nothing, even when a caller skips
// the cli's own validation.
func TestWriteRejectsInvalidReportURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	r := exportFixture()
	Finalize(r, true)
	if err := Write(dir, r, []string{FormatMarkdown, FormatPRComment}, WithReportURL("http://example.invalid")); err == nil {
		t.Fatal("invalid URL accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a rejected write created the directory: %v", err)
	}
	// The URL is ignored without the pr-comment format.
	if err := Write(dir, r, []string{FormatSARIF}, WithReportURL("http://example.invalid")); err != nil {
		t.Fatal(err)
	}
}

// Write -> decode -> Finalize -> Write renders byte-identical exports.
func TestExportsSurviveRoundTrip(t *testing.T) {
	for name, build := range map[string]func() *model.Report{
		"reproduced": exportFixture,
		"hostile": func() *model.Report {
			r := exportFixture()
			r.Hypotheses[0].Title = "@octocat [x](https://evil.test) token=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
			r.Intent = "- Guests are rejected.\n" + model.PRCommentBegin + "\nold comment\n" + model.PRCommentEnd
			return r
		},
		"unverified": func() *model.Report {
			r := exportFixture()
			r.Checks[1].Truncated = true
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := build()
			Finalize(r, true)
			first := t.TempDir()
			formats := []string{FormatJSON, FormatSARIF, FormatPRComment}
			if err := Write(first, r, formats, WithReportURL("https://example.invalid/runs/1")); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(first, "confidence-report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved model.Report
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			Finalize(&saved, saved.ExitCode == 2)
			second := t.TempDir()
			if err := Write(second, &saved, formats, WithReportURL("https://example.invalid/runs/1")); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"confidence-report.json", "confidence-report.sarif", "PR_COMMENT.md"} {
				a, _ := os.ReadFile(filepath.Join(first, file))
				b, _ := os.ReadFile(filepath.Join(second, file))
				if string(a) != string(b) {
					t.Fatalf("%s differs after a round trip:\n%s\n---\n%s", file, a, b)
				}
			}
			comment, _ := os.ReadFile(filepath.Join(first, "PR_COMMENT.md"))
			if strings.Contains(string(comment), "ghp_0123456789") {
				t.Fatal("secret-shaped text reached the PR comment")
			}
		})
	}
}

// The Markdown Reproduced Issues section, the SARIF results with the
// unanchored-finding notifications, and the PR comment list the same
// evidence.
func TestFormatsAgreeOnReproducedIssues(t *testing.T) {
	r := exportFixture()
	second := r.Evidence[0]
	second.ID, second.BaseCheckID, second.CheckID, second.Path = "experiment-2", "base-2", "candidate-2", "second_test.go"
	baseCheck, candidateCheck := r.Checks[0], r.Checks[1]
	baseCheck.ID, candidateCheck.ID = "base-2", "candidate-2"
	r.Checks = append(r.Checks, baseCheck, candidateCheck)
	r.Evidence = append(r.Evidence, second)
	r.Hypotheses = append(r.Hypotheses, model.Hypothesis{ID: "h2", Title: "Second", Severity: "medium", Status: "REPRODUCED", EvidenceIDs: []string{"experiment-2"}, Path: "elsewhere.go", Line: 9})
	Finalize(r, true)
	if len(r.ReproducedIssues) != 2 {
		t.Fatalf("fixture: %d reproduced", len(r.ReproducedIssues))
	}
	dir := t.TempDir()
	if err := Write(dir, r, []string{FormatMarkdown, FormatSARIF, FormatPRComment}); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	md := read("CONFIDENCE_REPORT.md")
	section := md[strings.Index(md, "## Reproduced Issues"):strings.Index(md, "## Behavior Divergences")]
	markdownIDs := regexp.MustCompile(`Evidence: ([^\n]+)`).FindAllStringSubmatch(section, -1)
	var fromMarkdown []string
	for _, m := range markdownIDs {
		fromMarkdown = append(fromMarkdown, strings.Split(m[1], ", ")...)
	}
	var log map[string]any
	if err := json.Unmarshal([]byte(read("confidence-report.sarif")), &log); err != nil {
		t.Fatal(err)
	}
	var fromSARIF []string
	for _, raw := range results(log) {
		for _, id := range raw.(map[string]any)["properties"].(map[string]any)["swiftproof"].(map[string]any)["evidence_ids"].([]any) {
			fromSARIF = append(fromSARIF, id.(string))
		}
	}
	for _, raw := range invocation(log)["toolExecutionNotifications"].([]any) {
		n := raw.(map[string]any)
		if n["properties"].(map[string]any)["swiftproof_kind"] == "unanchored_finding" {
			m := regexp.MustCompile(`Evidence: "([^"]+)"`).FindStringSubmatch(n["message"].(map[string]any)["text"].(string))
			fromSARIF = append(fromSARIF, strings.Split(m[1], ", ")...)
		}
	}
	var fromComment []string
	for _, m := range regexp.MustCompile(`- Evidence: ([^.]+)\.`).FindAllStringSubmatch(read("PR_COMMENT.md"), -1) {
		fromComment = append(fromComment, strings.Split(strings.ReplaceAll(m[1], `\-`, "-"), ", ")...)
	}
	sort.Strings(fromMarkdown)
	sort.Strings(fromSARIF)
	sort.Strings(fromComment)
	want := "experiment,experiment-2"
	if strings.Join(fromMarkdown, ",") != want || strings.Join(fromSARIF, ",") != want || strings.Join(fromComment, ",") != want {
		t.Fatalf("markdown %v, sarif %v, comment %v", fromMarkdown, fromSARIF, fromComment)
	}
	if len(results(log)) != 1 {
		t.Fatalf("the unanchored finding became a result: %d", len(results(log)))
	}
}
