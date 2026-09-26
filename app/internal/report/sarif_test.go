package report

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

var updateExports = flag.Bool("update-exports", false, "rewrite the SARIF and PR-comment golden files")

func sarifOf(t *testing.T, r *model.Report) []byte {
	t.Helper()
	_, data, err := renderFormat(FormatSARIF, Sanitize(r), writeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sarifWith(t *testing.T, r *model.Report, v exportVerification) []byte {
	t.Helper()
	data, err := renderSARIF(r, collectFindings(r, v))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeSARIF(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var log map[string]any
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	return log
}

func run0(log map[string]any) map[string]any {
	return log["runs"].([]any)[0].(map[string]any)
}

func results(log map[string]any) []any {
	return run0(log)["results"].([]any)
}

func invocation(log map[string]any) map[string]any {
	return run0(log)["invocations"].([]any)[0].(map[string]any)
}

func notificationKinds(log map[string]any) map[string]int {
	out := map[string]int{}
	for _, n := range invocation(log)["toolExecutionNotifications"].([]any) {
		out[n.(map[string]any)["properties"].(map[string]any)["swiftproof_kind"].(string)]++
	}
	return out
}

func runSummary(log map[string]any) map[string]any {
	return run0(log)["properties"].(map[string]any)["swiftproof"].(map[string]any)
}

// walkKeys calls visit for every object key and every string value.
func walkKeys(v any, visit func(key string, value any)) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			visit(k, val)
			walkKeys(val, visit)
		}
	case []any:
		for _, val := range x {
			walkKeys(val, visit)
		}
	}
}

func TestSARIFMinimalShape(t *testing.T) {
	log := decodeSARIF(t, sarifOf(t, finalized(exportFixture(), true)))
	if log["$schema"] != sarifSchema || log["version"] != "2.1.0" || len(log["runs"].([]any)) != 1 {
		t.Fatalf("log header %v %v", log["$schema"], log["version"])
	}
	run := run0(log)
	driver := run["tool"].(map[string]any)["driver"].(map[string]any)
	if driver["name"] != "SwiftProof" || driver["informationUri"] != "https://github.com/gvinsot/SwiftProof" || driver["version"] != "v1.2.3" || driver["semanticVersion"] != "1.2.3" {
		t.Fatalf("driver %v", driver)
	}
	if run["columnKind"] != "utf16CodeUnits" {
		t.Fatalf("columnKind %v", run["columnKind"])
	}
	rules := driver["rules"].([]any)
	ruleIDs := map[string]int{}
	for i, raw := range rules {
		rule := raw.(map[string]any)
		ruleIDs[rule["id"].(string)] = i
		for _, key := range []string{"shortDescription", "fullDescription", "help"} {
			if rule[key].(map[string]any)["text"].(string) == "" {
				t.Fatalf("rule %v has no %s", rule["id"], key)
			}
		}
		if rule["name"] == "" || rule["defaultConfiguration"].(map[string]any)["level"] == "" {
			t.Fatalf("rule %v incomplete", rule)
		}
	}
	res := results(log)
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fingerprint := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, raw := range res {
		r := raw.(map[string]any)
		index, ok := ruleIDs[r["ruleId"].(string)]
		if !ok || int(r["ruleIndex"].(float64)) != index {
			t.Fatalf("rule reference %v %v", r["ruleId"], r["ruleIndex"])
		}
		if r["level"] != "error" || r["message"].(map[string]any)["text"] == "" {
			t.Fatalf("result %v", r)
		}
		if _, markdown := r["message"].(map[string]any)["markdown"]; markdown {
			t.Fatal("message.markdown emitted")
		}
		if kind, present := r["kind"]; present {
			t.Fatalf("kind %v emitted", kind)
		}
		locations := r["locations"].([]any)
		if len(locations) != 1 {
			t.Fatalf("%d locations", len(locations))
		}
		physical := locations[0].(map[string]any)["physicalLocation"].(map[string]any)
		artifact := physical["artifactLocation"].(map[string]any)
		region := physical["region"].(map[string]any)
		if artifact["uri"] != "auth.go" || artifact["uriBaseId"] != "%SRCROOT%" || region["startLine"].(float64) != 3 || region["endLine"].(float64) != 3 {
			t.Fatalf("location %v", physical)
		}
		if !fingerprint.MatchString(r["partialFingerprints"].(map[string]any)["swiftproof/v1"].(string)) {
			t.Fatalf("fingerprint %v", r["partialFingerprints"])
		}
		props := r["properties"].(map[string]any)["swiftproof"].(map[string]any)
		if props["class"] != "reproduced" || props["status"] != "REPRODUCED" || props["severity"] != "high" || props["severity_source"] != "reviewer model" ||
			props["title_source"] != "reviewer model" || props["location_source"] != "model-chosen location" || props["location_precision"] != "line" {
			t.Fatalf("properties %v", props)
		}
		if ids := props["evidence_ids"].([]any); len(ids) != 1 || ids[0] != "experiment" {
			t.Fatalf("evidence ids %v", ids)
		}
		related := r["relatedLocations"].([]any)
		relatedArtifact := related[0].(map[string]any)["physicalLocation"].(map[string]any)["artifactLocation"].(map[string]any)
		if len(related) != 1 || relatedArtifact["uri"] != "artifacts/0a1b2c3d-generated-test-1-guest_test.go" || relatedArtifact["uriBaseId"] != reportDirBase {
			t.Fatalf("related %v", related)
		}
		artifacts := props["artifacts"].([]any)
		if len(artifacts) != 1 || artifacts[0].(map[string]any)["sha256"] != strings.Repeat("a", 64) {
			t.Fatalf("artifact properties %v", artifacts)
		}
	}
	inv := invocation(log)
	if inv["executionSuccessful"] != true || inv["exitCode"].(float64) != 1 {
		t.Fatalf("invocation %v", inv)
	}
	if run["originalUriBaseIds"].(map[string]any)["%SRCROOT%"] == nil {
		t.Fatal("no source root base")
	}
	summary := runSummary(log)
	if summary["note"] != sarifRunNote || summary["findings"].(float64) != 1 || summary["exit_code"].(float64) != 1 || summary["generated_at"] != "2026-09-26T12:00:00Z" {
		t.Fatalf("run summary %v", summary)
	}
	assertNoForbiddenSARIFKeys(t, log)
}

func assertNoForbiddenSARIFKeys(t *testing.T, log map[string]any) {
	t.Helper()
	walkKeys(log, func(key string, value any) {
		switch strings.ToLower(key) {
		case "precision", "security-severity", "confidence", "rank", "baselinestate", "suppressions":
			t.Fatalf("forbidden key %q", key)
		case "kind":
			if value == "pass" || value == "informational" || value == "notApplicable" {
				t.Fatalf("result kind %v", value)
			}
		}
	})
}

func TestSARIFLevels(t *testing.T) {
	for severity, level := range map[string]string{"critical": "error", "high": "error", "medium": "warning", "low": "warning"} {
		r := exportFixture()
		r.Hypotheses[0].Severity = severity
		res := results(decodeSARIF(t, sarifOf(t, finalized(r, true))))
		if len(res) != 1 || res[0].(map[string]any)["level"] != level {
			t.Errorf("%s: %v", severity, res)
		}
	}
	for class, fixture := range classFixtures {
		if class == ClassReproduced {
			continue
		}
		r, v := fixture()
		log := decodeSARIF(t, sarifWith(t, r, v))
		cls, _ := classByName(class)
		res := results(log)
		if class == ClassImpactedTestFailsOnCandidate {
			// An impacted test is never in the changed files: a notification.
			if len(res) != 0 || notificationKinds(log)["unanchored_finding"] != 1 {
				t.Fatalf("impacted test: %v", res)
			}
			continue
		}
		if len(res) != 1 || res[0].(map[string]any)["level"] != cls.DefaultLevel || res[0].(map[string]any)["ruleId"] != cls.RuleID {
			t.Errorf("%s: %v", class, res)
		}
		assertNoForbiddenSARIFKeys(t, log)
	}
}

func TestSARIFExecutionSuccessful(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Report)
		want   bool
	}{
		{"reproduced run", func(*model.Report) {}, true},
		{"exit 4", func(r *model.Report) { r.ExitCode = 4 }, false},
		{"error check", func(r *model.Report) {
			r.Checks = append(r.Checks, model.Check{ID: "c", Kind: "build", Status: "ERROR", ExitCode: 125})
		}, false},
		{"skipped check", func(r *model.Report) {
			r.Checks = append(r.Checks, model.Check{ID: "c", Kind: "build", Status: "SKIPPED"})
		}, false},
		{"timeout check", func(r *model.Report) {
			r.Checks = append(r.Checks, model.Check{ID: "c", Kind: "build", Status: "TIMEOUT", ExitCode: -1})
		}, false},
		{"deadline reached", func(r *model.Report) {
			r.Execution = &model.Execution{Budget: model.ExecutionBudget{DeadlineReached: true}}
		}, false},
		{"failing test check", func(r *model.Report) {
			r.Checks = append(r.Checks, model.Check{ID: "c", Kind: "test", Status: "FAIL", ExitCode: 1})
		}, true},
		{"mutant ledger failures", func(r *model.Report) {
			r.Mutation = &model.Mutation{Status: model.MutationRan, Checks: []model.Check{{ID: "mutation-check-1", Kind: model.CheckMutant, Status: "TIMEOUT"}}}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := finalized(exportFixture(), true)
			tc.mutate(r)
			if got := invocation(decodeSARIF(t, sarifOf(t, r)))["executionSuccessful"]; got != tc.want {
				t.Fatalf("executionSuccessful %v, want %v", got, tc.want)
			}
		})
	}
}

// An empty result list is always accompanied by what it must not hide.
func TestSARIFEmptyIsNotApproval(t *testing.T) {
	lint := finalized(&model.Report{Version: 1, ToolVersion: "v1.2.3", Change: model.Change{Files: []model.ChangedFile{changedFile("a.go", 2)}}}, true)
	data := sarifOf(t, lint)
	if !strings.Contains(string(data), `"results": []`) {
		t.Fatalf("results must serialize as []:\n%s", data)
	}
	log := decodeSARIF(t, data)
	if kinds := notificationKinds(log); kinds["no_execution"] != 1 {
		t.Fatalf("notifications %v", kinds)
	}
	if runSummary(log)["note"] != sarifRunNote || len(run0(log)["tool"].(map[string]any)["driver"].(map[string]any)["rules"].([]any)) != 0 {
		t.Fatal("empty run without note or with rules")
	}

	r := finalized(exportFixture(), true)
	r.Hypotheses = append(r.Hypotheses, model.Hypothesis{ID: "h9", Title: "Unproven", Status: model.StatusUnverified, Severity: "medium"})
	r.Unverified = append(r.Unverified, "Reviewer incomplete: provider timeout")
	r.Checks = append(r.Checks, model.Check{ID: "check-9", Kind: "test", Status: "TIMEOUT", ExitCode: -1})
	r.Fuzz = &model.FuzzReport{Status: model.FuzzNotRun, Reason: "dependency preparation did not produce an image"}
	r.Mutation = &model.Mutation{Status: model.MutationNotRun, Reason: "initial checks disabled (--checks=false)"}
	r.Prepare = &model.Prepare{Status: model.PrepareFailed, Reason: "the command exited 1"}
	r.BaseTests = &model.BaseTests{Status: model.BaseTestsNotRun, Reason: "no verifiable template"}
	r.Impact = &model.Impact{Status: model.ImpactIndexed, TestsStatus: model.ImpactTestsNotRun, TestsReason: "budget"}
	r.ExitCode = 4
	log = decodeSARIF(t, sarifOf(t, r))
	kinds := notificationKinds(log)
	want := map[string]int{"operational_failure": 1, "check_not_passed": 2, "unverified_area": 1, "unverified_hypothesis": 1, "stage_not_run": 5}
	for kind, n := range want {
		if kinds[kind] != n {
			t.Errorf("%s: %d notifications, want %d (%v)", kind, kinds[kind], n, kinds)
		}
	}
	if kinds["no_execution"] != 0 {
		t.Error("no_execution with recorded checks")
	}
	for _, n := range invocation(log)["toolExecutionNotifications"].([]any) {
		if n.(map[string]any)["level"] != "warning" {
			t.Fatalf("notification level %v", n)
		}
	}
	summary := runSummary(log)
	if summary["unverified_areas"].(float64) != 2 || summary["stages_not_run"].(float64) != 5 || summary["checks_not_passed"].(float64) != 2 {
		t.Fatalf("summary %v", summary)
	}
}

func TestSARIFTextNeutralizesLinks(t *testing.T) {
	got := sarifText("see [click](https://evil.test) and WWW.evil.test \\[x\\] \"quoted\" \xe2\x80\xaeevil\xe2\x81\xa6 bell\a", 1024)
	for _, bad := range []string{"[click]", "://", "WWW.", "\"", "\xe2\x80\xae", "\xe2\x81\xa6", "\a", "\\[x"} {
		if strings.Contains(got, bad) && !(bad == "\\[x" && strings.Contains(got, "\\\\\\[x")) {
			t.Errorf("%q survives in %q", bad, got)
		}
	}
	for _, want := range []string{`\[click\]`, "https:\xe2\x80\x8b//evil.test", "WWW\xe2\x80\x8b.evil", "'quoted'", `\\\[x\\\]`} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	r := exportFixture()
	r.Hypotheses[0].Title = strings.Repeat("[x](https://evil.test) ", 200)
	res := results(decodeSARIF(t, sarifOf(t, finalized(r, true))))
	text := res[0].(map[string]any)["message"].(map[string]any)["text"].(string)
	if len(text) > maxSARIFMessage || strings.Contains(text, "](") && !strings.Contains(text, `\](`) || strings.Contains(text, "://") {
		t.Fatalf("message not bounded or neutralized (%d bytes): %s", len(text), text)
	}
}

func TestSARIFAnchoring(t *testing.T) {
	// A model-chosen line outside the recorded diff degrades to file level.
	r := exportFixture()
	r.Hypotheses[0].Line = 40
	res := results(decodeSARIF(t, sarifOf(t, finalized(r, true))))
	result := res[0].(map[string]any)
	region := result["locations"].([]any)[0].(map[string]any)["physicalLocation"].(map[string]any)["region"].(map[string]any)
	props := result["properties"].(map[string]any)["swiftproof"].(map[string]any)
	if region["startLine"].(float64) != 1 || props["location_precision"] != "file" || !strings.Contains(result["message"].(map[string]any)["text"].(string), fileLevelText) {
		t.Fatalf("file-level anchor %v %v", region, props)
	}
	// A path outside the change, a deleted file or no path: no result, one
	// notification, and the finding stays in the PR comment.
	for name, mutate := range map[string]func(*model.Report){
		"outside the change": func(r *model.Report) { r.Hypotheses[0].Path = "elsewhere.go" },
		"deleted file":       func(r *model.Report) { r.Change.Files[0].Status = "D" },
		"binary file":        func(r *model.Report) { r.Change.Files[0].Binary = true },
		"no path":            func(r *model.Report) { r.Hypotheses[0].Path = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := exportFixture()
			mutate(r)
			r = finalized(r, true)
			log := decodeSARIF(t, sarifOf(t, r))
			if len(results(log)) != 0 || notificationKinds(log)["unanchored_finding"] != 1 || runSummary(log)["unanchored_findings"].(float64) != 1 {
				t.Fatalf("unanchored finding: results %v, notifications %v", results(log), notificationKinds(log))
			}
			_, comment, err := renderFormat(FormatPRComment, Sanitize(r), writeOptions{})
			if err != nil || !strings.Contains(string(comment), "Location: none in the changed files.") || strings.Contains(string(comment), "elsewhere") {
				t.Fatalf("PR comment:\n%s", comment)
			}
		})
	}
}

func TestSARIFURIEncoding(t *testing.T) {
	for in, want := range map[string]string{
		"auth.go":                "auth.go",
		"dir with space/ä#?.go":  "dir%20with%20space/%C3%A4%23%3F.go",
		"c:drive.go":             "c%3Adrive.go",
		"a\\b.go":                "a%5Cb.go",
		"pkg/100%.go":            "pkg/100%25.go",
		"artifacts/x-results.js": "artifacts/x-results.js",
	} {
		got, ok := artifactURI(in)
		if !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "/abs/x.go", "../x.go", "a/../x.go", "a//b.go", "./x.go", "a/.", "x\x00.go"} {
		if got, ok := artifactURI(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// Rendering is deterministic, independent of record order, and matches the
// golden file (regenerate with -update-exports).
func TestSARIFGoldenAndDeterminism(t *testing.T) {
	r := finalized(exportFixture(), true)
	first := sarifOf(t, r)
	if second := sarifOf(t, r); string(first) != string(second) {
		t.Fatal("two renders differ")
	}
	permuted := finalized(exportFixture(), true)
	permuted.Checks = reverseChecks(permuted.Checks)
	permuted.Signals = []model.Signal{}
	if got := sarifOf(t, permuted); string(got) != string(first) {
		t.Fatalf("record order changes the SARIF:\n%s", got)
	}
	golden(t, "reproduced.sarif", first)
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateExports {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-exports)", err)
	}
	normalize := func(b []byte) string { return strings.ReplaceAll(string(b), "\r\n", "\n") }
	if normalize(got) != normalize(want) {
		t.Fatalf("%s differs from the golden file:\n%s", name, got)
	}
}

func TestSARIFBounds(t *testing.T) {
	r := manyMutants(maxFindings + 200)
	data := sarifWith(t, r, allSurvived(r))
	if len(data) > 8<<20 {
		t.Fatalf("SARIF of %d bytes", len(data))
	}
	log := decodeSARIF(t, data)
	if n := len(results(log)); n != maxSARIFResults {
		t.Fatalf("%d results", n)
	}
	if notificationKinds(log)["omitted_findings"] != 1 || runSummary(log)["omitted_findings"].(float64) != 200 {
		t.Fatalf("omitted findings not reported: %v", notificationKinds(log))
	}
	// Many notifications are capped with a final count.
	s := exportStatus{}
	for i := 0; i < 300; i++ {
		s.UnverifiedNotes = append(s.UnverifiedNotes, strings.Repeat("u", 2000))
	}
	notes := sarifNotifications(s, nil, 0)
	if len(notes) != maxNotifications || notes[len(notes)-1].Properties.Kind != "omitted_notifications" || !strings.HasPrefix(notes[len(notes)-1].Message.Text, "201 further notifications") {
		t.Fatalf("%d notifications, last %+v", len(notes), notes[len(notes)-1])
	}
	for _, n := range notes {
		if len(n.Message.Text) > maxNotificationText {
			t.Fatalf("notification of %d bytes", len(n.Message.Text))
		}
	}
}

func TestSARIFAttribution(t *testing.T) {
	for version, want := range map[string]string{
		"v1.2.3": "Generated by SwiftProof v1.2.3 (https://github.com/gvinsot/SwiftProof).",
		"e2e-f9": "Generated by SwiftProof e2e-f9 (https://github.com/gvinsot/SwiftProof).",
		"":       "Generated by SwiftProof (https://github.com/gvinsot/SwiftProof).",
	} {
		r := finalized(exportFixture(), true)
		r.ToolVersion = version
		log := decodeSARIF(t, sarifOf(t, r))
		if got := log["properties"].(map[string]any)["attribution"]; got != want {
			t.Errorf("%q: attribution %v", version, got)
		}
		driver := run0(log)["tool"].(map[string]any)["driver"].(map[string]any)
		if _, semantic := driver["semanticVersion"]; semantic != (version == "v1.2.3") {
			t.Errorf("%q: semanticVersion %v", version, driver["semanticVersion"])
		}
	}
}
