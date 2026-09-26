package report

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func commentOf(t *testing.T, r *model.Report, url string) string {
	t.Helper()
	_, data, err := renderFormat(FormatPRComment, Sanitize(r), writeOptions{reportURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func commentWith(r *model.Report, v exportVerification) string {
	return string(renderPRComment(r, collectFindings(r, v), ""))
}

// The escaper required by the contract: mentions, autolinks, shortcodes and
// issue references are broken, HTML is escaped and Markdown punctuation is
// backslash-escaped as inline() does. Quotes stay quotes: inline()'s numeric
// entities would show as "&#39;" and put a "#" before a digit back.
func TestCommentTextEscapes(t *testing.T) {
	cases := map[string]string{
		"@user":              "@" + zeroWidthSpace + "user",
		"https://evil":       "https:" + zeroWidthSpace + "//evil",
		"www.x":              "www" + zeroWidthSpace + ".x",
		"WWW.x":              "WWW" + zeroWidthSpace + ".x",
		":smile:":            ":" + zeroWidthSpace + "smile:",
		"#12":                `\#` + zeroWidthSpace + "12",
		"fixes GH-12":        "fixes GH-" + zeroWidthSpace + "12",
		"gh-7":               "gh-" + zeroWidthSpace + "7",
		"GH-x":               "GH-x",
		"a_b*c":              `a\_b\*c`,
		"<img src=x>":        "&lt;img src=x&gt;",
		"a & b":              "a &amp; b",
		"it's":               "it's",
		`if s == "admin"`:    `if s == "admin"`,
		"&#39; and &#x27;":   `&amp;\#` + zeroWidthSpace + `39; and &amp;\#x27;`,
		"[x](y)":             `\[x\]\(y\)`,
		"owner/repo#3 GH-99": `owner/repo\#` + zeroWidthSpace + "3 GH-" + zeroWidthSpace + "99",
	}
	for in, want := range cases {
		if got := commentText(in, 256); got != want {
			t.Errorf("commentText(%q) = %q, want %q", in, got, want)
		}
	}
	hostile := "@octocat see https://evil.test and www.evil.test :tada: fixes #1 and GH-12 \n## Forged \xe2\x80\xae <b>x</b> `code` [link](https://x) Guest's \"admin\" role &#34;"
	got := commentText(hostile, 1024)
	for _, pattern := range []string{`@[^\x{200b}]`, `://`, `(?i)www\.`, `:[A-Za-z0-9_+-]`, `#[0-9]`, `(?i)GH-[0-9]`, `&#`, `<`, "\n", "\xe2\x80\xae", "(^|[^\\\\])`", `[^\\]\[`} {
		if regexp.MustCompile(pattern).MatchString(got) {
			t.Errorf("pattern %q matches %q", pattern, got)
		}
	}
	if !strings.Contains(got, `Guest's "admin" role`) {
		t.Errorf("quotes altered: %q", got)
	}
	// Caps: the value is cut before escaping, with an ellipsis.
	if got := commentText(strings.Repeat("a", 300), maxTitleBytes); got != strings.Repeat("a", maxTitleBytes)+ellipsis {
		t.Fatalf("cut %q", got)
	}
}

func TestPRCommentStatusBlock(t *testing.T) {
	// Exit 2 with zero findings: an unverified area, a stage that did not run.
	r := &model.Report{Version: 1, ToolVersion: "v1.2.3", Change: model.Change{Files: []model.ChangedFile{changedFile("a.go", 2)}},
		Checks:     []model.Check{{ID: "check-1", Kind: "test", Status: "PASS"}},
		Unverified: []string{"Reviewer incomplete: provider timeout"},
		Fuzz:       &model.FuzzReport{Status: model.FuzzNotRun, Reason: "differential fuzzing did not run"},
	}
	r = finalized(r, true)
	if r.ExitCode != 2 {
		t.Fatalf("fixture exit %d", r.ExitCode)
	}
	got := commentOf(t, r, "")
	for _, want := range []string{
		"## SwiftProof: no evidence-backed finding recorded",
		"**Status (not a finding)**",
		"- Exit code 2: human review requested.",
		"- Unverified areas: 1 (1 recorded note and 0 hypotheses that stayed UNVERIFIED).",
		"- Checks that did not pass: 0 (the mutation ledger is not counted).",
		`- Stages that did not run: 1: differential fuzzing (not\_run: differential fuzzing did not run).`,
		"- No finding is not approval.",
		evidenceOnlyText,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, noExecutionText) {
		t.Error("no-execution sentence with a recorded check")
	}
	// Exit 4, set after Finalize as the cli does.
	r = finalized(exportFixture(), true)
	r.ExitCode = 4
	r.Checks = append(r.Checks, model.Check{ID: "check-3", Kind: "build", Status: "ERROR", ExitCode: 125})
	got = commentOf(t, r, "")
	for _, want := range []string{
		"## SwiftProof: 1 evidence-backed finding",
		"- Exit code 4: operational failure; the run did not finish as configured, so findings may be missing.",
		`- Checks that did not pass: 2 (the mutation ledger is not counted): candidate generated\_test\_candidate FAIL; check-3 build ERROR.`,
		"- No finding is not approval.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The status block precedes every finding.
	if strings.Index(got, "- No finding is not approval.") > strings.Index(got, "### Reproduced hypotheses") {
		t.Fatal("finding before the status block")
	}
}

var (
	forbiddenWords = regexp.MustCompile(`(?i)\b(verified|tested|approved?|lgtm|looks good|safe to merge|safe|no issues|all clear|regressions?|bugs?|masked|contradicts?|contradiction|complete|score|confidence|precision|correct|correctness)\b`)
	allowedPhrases = []string{"which revision is correct", "that the change is correct", "%SRCROOT%", "confidence-report.json", "confidence-report.sarif", `CONFIDENCE\_REPORT.md`, "CONFIDENCE_REPORT.md"}
)

func assertNoApprovalWording(t *testing.T, name, text string) {
	t.Helper()
	cleaned := text
	for _, phrase := range allowedPhrases {
		cleaned = strings.ReplaceAll(cleaned, phrase, "")
	}
	if m := forbiddenWords.FindString(cleaned); m != "" {
		t.Errorf("%s: forbidden wording %q in:\n%s", name, m, text)
	}
	if strings.Contains(cleaned, "%") {
		t.Errorf("%s: percent sign in:\n%s", name, text)
	}
}

func TestPRCommentNeverReadsAsApproval(t *testing.T) {
	variants := map[string]func() *model.Report{
		"no checks": func() *model.Report {
			return finalized(&model.Report{Version: 1, ToolVersion: "v1.2.3", Change: model.Change{Files: []model.ChangedFile{changedFile("a.go", 2)}}}, false)
		},
		"one finding": func() *model.Report {
			r := exportFixture()
			r.Hypotheses[0].Title = "Guest is allowed through authorization"
			return finalized(r, true)
		},
		"sixty findings": func() *model.Report { return manyMutants(60) },
	}
	for name, build := range variants {
		for _, exit := range []int{0, 1, 2, 4} {
			r := build()
			r.ExitCode = exit
			var got string
			if name == "sixty findings" {
				got = commentWith(r, allSurvived(r))
			} else {
				got = commentOf(t, r, "")
			}
			label := fmt.Sprintf("%s/exit %d", name, exit)
			if !strings.Contains(got, "No finding is not approval.") || !strings.Contains(got, exitSentence(exit)) {
				t.Errorf("%s: status block incomplete:\n%s", label, got)
			}
			if (name == "no checks") != strings.Contains(got, noExecutionText) {
				t.Errorf("%s: no-execution sentence mismatch", label)
			}
			assertNoApprovalWording(t, label, got)
		}
	}
	for class, fixture := range classFixtures {
		r, v := fixture()
		if class == ClassReproduced {
			r.Hypotheses[0].Title, r.ReproducedIssues[0].Title = "Guest is allowed through authorization", "Guest is allowed through authorization"
		}
		got := commentWith(r, v)
		cls, _ := classByName(class)
		if !strings.Contains(got, "### "+cls.Heading+" (1)") || !strings.Contains(got, cls.Caveat) {
			t.Errorf("%s: group missing:\n%s", class, got)
		}
		assertNoApprovalWording(t, class, got)
		data, err := renderSARIF(r, collectFindings(r, v))
		if err != nil {
			t.Fatal(err)
		}
		assertNoApprovalWording(t, class+" sarif", strings.ReplaceAll(string(data), "no mutation score", ""))
	}
}

func TestPRCommentEscapesUntrustedText(t *testing.T) {
	r := exportFixture()
	hostile := "@octocat #1 GH-12 https://evil.test <img src=x onerror=alert(1)> ``` \n## Forged :smile: www.evil.test [x](y) \xe2\x80\xae Guest's \"admin\" role"
	r.Hypotheses[0].Title = hostile
	r.Hypotheses[0].ID = "h1 @admin #2"
	r.Hypotheses[0].Path = "dir/@team #3.go"
	r.Change.Files[0].Path = "dir/@team #3.go"
	r.Evidence[0].ID = "ev @admin"
	r.Hypotheses[0].EvidenceIDs = []string{"ev @admin"}
	r.Checks[1].Kind = "generated_test_candidate"
	r.Unverified = []string{hostile}
	r.Fuzz = &model.FuzzReport{Status: model.FuzzNotRun, Reason: hostile}
	r.ToolVersion = "@release https://evil.test"
	r = finalized(r, true)
	got := commentOf(t, r, "")
	if !strings.Contains(got, "## SwiftProof: 1 evidence-backed finding") {
		t.Fatalf("hostile fixture lost its finding:\n%s", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if lines[0] != model.PRCommentBegin || lines[len(lines)-1] != model.PRCommentEnd {
		t.Fatalf("markers: %q ... %q", lines[0], lines[len(lines)-1])
	}
	headings := 0
	for i, l := range lines {
		marker := i == 0 || i == len(lines)-1
		if strings.HasPrefix(l, "#") {
			headings++
			if !strings.HasPrefix(l, "## SwiftProof: ") && !strings.HasPrefix(l, "### ") {
				t.Errorf("unexpected heading %q", l)
			}
		}
		if strings.HasPrefix(l, "## Forged") || strings.Contains(l, "\xe2\x80\xae") {
			t.Errorf("forged structure %q", l)
		}
		if !marker && strings.Contains(l, "<") {
			t.Errorf("raw HTML outside the markers: %q", l)
		}
		if marker || strings.HasPrefix(l, "Generated by SwiftProof ") {
			continue
		}
		// The line number of an anchor is rendered by SwiftProof, not taken
		// from untrusted text.
		l = anchorLine.ReplaceAllString(l, " (")
		for _, pattern := range []string{`@[^\x{200b}]`, `://`, `(?i)www\.`, `:[A-Za-z0-9_+-]`, `#[0-9]`, `(?i)GH-[0-9]`, `&#`, "(^|[^\\\\])`", `\]\(`} {
			if regexp.MustCompile(pattern).MatchString(l) {
				t.Errorf("pattern %q in %q", pattern, l)
			}
		}
	}
	if headings != 2 {
		t.Fatalf("%d heading lines", headings)
	}
	// The attribution keeps its text; the version cannot mention anyone.
	if !strings.Contains(got, "Generated by SwiftProof @"+zeroWidthSpace+"release https:"+zeroWidthSpace+"//evil.test (https://github.com/gvinsot/SwiftProof).") {
		t.Fatalf("attribution:\n%s", got)
	}
}

func TestPRCommentBudget(t *testing.T) {
	r := manyMutants(500)
	for i := range r.Mutation.Mutants {
		r.Mutation.Mutants[i].Original = fmt.Sprintf("x%d := ", i) + strings.Repeat("a", 240)
		r.Mutation.Mutants[i].Mutated = fmt.Sprintf("x%d := ", i) + strings.Repeat("b", 240)
		r.Mutation.Mutants[i].Operator = strings.Repeat("o", 250)
		r.Mutation.Mutants[i].Package = strings.Repeat("p", 250)
	}
	got := commentWith(r, allSurvived(r))
	if len(got) > maxCommentBytes {
		t.Fatalf("comment of %d bytes", len(got))
	}
	shown := strings.Count(got, "\n- Location: ")
	if shown == 0 || shown > maxCommentFindings {
		t.Fatalf("%d findings shown", shown)
	}
	want := fmt.Sprintf("%d further evidence-backed findings are not shown in this comment, which lists at most 50 findings in at most 60000 bytes; the underlying records are in confidence-report.json.", 500-shown)
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q", want)
	}
	if !strings.Contains(got, "## SwiftProof: 500 evidence-backed findings") || !strings.HasSuffix(got, Attribution("")+"\n"+model.PRCommentEnd+"\n") {
		t.Fatalf("head or tail missing:\n%s", got[len(got)-500:])
	}
	// Short findings stop at the count limit.
	small := manyMutants(80)
	got = commentWith(small, allSurvived(small))
	if n := strings.Count(got, "\n- Location: "); n != maxCommentFindings || !strings.Contains(got, "30 further evidence-backed findings are not shown") {
		t.Fatalf("%d shown:\n%s", n, got)
	}
	// Findings the finding cap omitted are counted too.
	big := manyMutants(maxFindings + 5)
	got = commentWith(big, allSurvived(big))
	if !strings.Contains(got, fmt.Sprintf("## SwiftProof: %d evidence-backed findings", maxFindings+5)) || !strings.Contains(got, fmt.Sprintf("%d further evidence-backed findings", maxFindings+5-maxCommentFindings)) {
		t.Fatal("omitted findings not counted")
	}
}

func TestPRCommentPointerAndAttribution(t *testing.T) {
	r := finalized(exportFixture(), true)
	got := commentOf(t, r, "https://example.invalid/runs/1?x=1#y")
	if !strings.Contains(got, "\nFull report: [CONFIDENCE\\_REPORT.md and confidence-report.json](https://example.invalid/runs/1?x=1#y)\n") {
		t.Fatalf("pointer with URL:\n%s", got)
	}
	if strings.Count(got, "](") != 1 {
		t.Fatalf("links besides the report URL:\n%s", got)
	}
	got = commentOf(t, r, "")
	if !strings.Contains(got, "\nFull report: CONFIDENCE\\_REPORT.md and confidence-report.json in the SwiftProof report directory.\n") || strings.Contains(got, "](") {
		t.Fatalf("pointer without URL:\n%s", got)
	}
	if !strings.HasPrefix(got, model.PRCommentBegin+"\n") || !strings.HasSuffix(got, "\n---\n\n"+Attribution("v1.2.3")+"\n"+model.PRCommentEnd+"\n") {
		t.Fatalf("markers or attribution:\n%s", got)
	}
	// An invalid URL never becomes a link, even when the option bypasses the cli.
	if got := string(renderPRComment(r, collectFindings(r, verifyExports(r)), "javascript:alert(1)")); strings.Contains(got, "](") {
		t.Fatal("invalid URL linked")
	}
	if _, _, err := renderFormat(FormatPRComment, r, writeOptions{reportURL: "http://example.invalid"}); err == nil {
		t.Fatal("invalid URL rendered")
	}
}

func TestPRCommentGolden(t *testing.T) {
	r := finalized(exportFixture(), true)
	first := commentOf(t, r, "https://example.invalid/runs/1")
	if again := commentOf(t, r, "https://example.invalid/runs/1"); again != first {
		t.Fatal("two renders differ")
	}
	golden(t, "reproduced-pr-comment.md", []byte(first))
}

func TestValidateReportURL(t *testing.T) {
	for _, ok := range []string{"https://github.com/o/r/actions/runs/1", "https://example.invalid", "https://example.invalid:8443/a/b?c=d&e=f#g", "https://h/%20"} {
		if err := ValidateReportURL(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://example.invalid", "javascript:alert(1)", "https://", "https:///x", "https://user:pw@example.invalid", "https://user@example.invalid",
		"https://example.invalid/a b", "https://example.invalid/(x)", "https://example.invalid/[x]", "https://example.invalid/'x'", "https://example.invalid/\"x\"",
		"https://example.invalid/`x`", "https://example.invalid/<x>", "https://example.invalid/a\\b", "https://example.invalid/\x07", "HTTPS://example.invalid",
		"https://example.invalid/" + strings.Repeat("a", 500), "ftp://example.invalid"} {
		if err := ValidateReportURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The URL bypasses the report's sanitizing, so a URL that the credential
	// redaction would change is refused instead of being written into the
	// comment.
	for _, secret := range []string{
		"https://example.invalid/run?token=ghp_0123456789abcdefghijABCDEFGHIJ",
		"https://example.invalid/run?access_token=abc123",
		"https://example.invalid/run?x=1&client_secret=abc",
		"https://example.invalid/a/sk-0123456789abcdef",
		"https://example.invalid/dl?sig=eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4",
	} {
		err := ValidateReportURL(secret)
		if err == nil || !strings.Contains(err.Error(), "credential") {
			t.Errorf("%q: %v", secret, err)
		}
		if _, _, err := renderFormat(FormatPRComment, Sanitize(finalized(exportFixture(), true)), writeOptions{reportURL: secret}); err == nil {
			t.Errorf("%q rendered", secret)
		}
	}
}

var anchorLine = regexp.MustCompile(`:[0-9]+(–[0-9]+)? \(`)
