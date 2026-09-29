package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/observe"
	"github.com/gvinsot/Probe/app/internal/redact"
)

const (
	obsTest = "TestProbeObserve"
	obsPath = "pkg/obs_test.go"
)

var obsCommand = []string{"go", "test", "./pkg", "-json", "-count=1", "-run", "^(" + obsTest + ")$"}

// obsLog renders a redacted go test -json log of the observation test, as the
// harness records it: run, one attr event per key/value pair, then action.
func obsLog(action string, pairs ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"Action":"run","Package":"example.com/m/pkg","Test":%q}`+"\n", obsTest)
	for i := 0; i+1 < len(pairs); i += 2 {
		line, _ := json.Marshal(map[string]string{"Action": "attr", "Package": "example.com/m/pkg", "Test": obsTest, "Key": "probe." + pairs[i], "Value": pairs[i+1]})
		b.Write(line)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, `{"Action":%q,"Package":"example.com/m/pkg","Test":%q}`+"\n", action, obsTest)
	return redact.Redact(b.String())
}

func obsCheck(id, kind string, pairs ...string) model.Check {
	return model.Check{ID: id, Kind: kind, Status: "PASS", Command: append([]string(nil), obsCommand...), Output: obsLog("pass", pairs...)}
}

// observationReport is a divergence experiment as the harness records it: the
// baseline and its live repeat recorded Discount(5,33)=4, the candidate 3, and
// Discount(100,10) is equal everywhere. evidence-1 is the both-pass
// differential_test of the same runs, evidence-2 the observation record, and
// h1 a DIVERGED claim anchored at a changed line.
func observationReport() *model.Report {
	return &model.Report{
		Change: model.Change{BaseRef: "main", Files: []model.ChangedFile{{Path: "price.go", Status: "M", Additions: 1, Deletions: 1, Hunks: []model.Hunk{{NewStart: 3, NewLines: 1, Lines: []model.DiffLine{{Kind: "add", NewLine: 3, Content: "return total * (100 - percent) / 100"}}}}}}},
		Checks: []model.Check{
			obsCheck("check-1", model.CheckGeneratedBase, "Discount(100,10)", "90", "Discount(5,33)", "4"),
			obsCheck("check-2", model.CheckGeneratedCandidate, "Discount(100,10)", "90", "Discount(5,33)", "3"),
			obsCheck("check-3", model.CheckGeneratedBaseRepeat, "Discount(100,10)", "90", "Discount(5,33)", "4"),
		},
		Evidence: []model.Evidence{
			{ID: "evidence-1", Kind: model.EvidenceDifferentialTest, Status: model.StatusNotReproduced, Path: obsPath, CheckID: "check-2", BaseCheckID: "check-1", Runner: "go_test_json", TestNames: []string{obsTest}},
			{ID: "evidence-2", Kind: model.EvidenceDifferentialObservation, Status: model.StatusDiverged, Path: obsPath, CheckID: "check-2", BaseCheckID: "check-1", RepeatCheckID: "check-3", Runner: "go_test_json", TestNames: []string{obsTest}},
		},
		Hypotheses: []model.Hypothesis{{ID: "h1", Title: "Discount rounding changed", Severity: "critical", Status: "DIVERGED", Rationale: "Recorded values differ", EvidenceIDs: []string{"evidence-2"}, Path: "price.go", Line: 3}},
	}
}

func TestObservationDivergenceIsVerifiedAndListed(t *testing.T) {
	for _, ci := range []bool{false, true} {
		r := observationReport()
		Finalize(r, ci)
		want := 0
		if ci {
			want = 2
		}
		if r.ExitCode != want || r.Hypotheses[0].Status != model.StatusDiverged || len(r.ReproducedIssues) != 0 {
			t.Fatalf("ci=%v: exit %d, status %s, reproduced %d", ci, r.ExitCode, r.Hypotheses[0].Status, len(r.ReproducedIssues))
		}
		if len(r.Divergences) != 1 {
			t.Fatalf("divergences %+v", r.Divergences)
		}
		d := r.Divergences[0]
		if d.EvidenceID != "evidence-2" || d.Kind != model.EvidenceDifferentialObservation || d.TestPath != obsPath || strings.Join(d.TestNames, ",") != obsTest ||
			strings.Join(d.CheckIDs, ",") != "check-1,check-2,check-3" || strings.Join(d.HypothesisIDs, ",") != "h1" ||
			d.Path != "price.go" || d.Line != 3 || d.AnchorSource != anchorHypothesis || d.Note != model.DivergenceNote {
			t.Fatalf("divergence %+v", d)
		}
		if len(d.Observations) != 1 {
			t.Fatalf("rows %+v", d.Observations)
		}
		if row := d.Observations[0]; row.Key != "Discount(5,33)" || row.Base != "4" || row.Candidate != "3" || row.Test != obsTest || row.Status != model.ObservationDiverged || !row.BaseRecorded || !row.CandidateRecorded || row.Truncated {
			t.Fatalf("row %+v", row)
		}
	}
	r := observationReport()
	Finalize(r, true)
	md := string(Markdown(r))
	body := section(t, md, "## Behavior Divergences")
	for _, want := range []string{
		"- **evidence-2** differential\\_observation — price.go:3 (model-chosen location); test pkg/obs\\_test.go (TestProbeObserve); hypotheses: h1\n",
		"  - Discount\\(5,33\\): baseline 4; candidate 3\n",
		inline(model.DivergenceNote),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Discount\\(100,10\\)") {
		t.Error("an equal key was listed as a divergence")
	}
	evidence := section(t, md, "## Recorded Evidence")
	if !strings.Contains(evidence, "- evidence-2 — differential\\_observation (DIVERGED): ") || !strings.Contains(evidence, "baseline repeat: check-3") {
		t.Fatalf("recorded evidence:\n%s", evidence)
	}
	// The both-pass generated test of the same runs no longer supports
	// NOT_REPRODUCED: it is withdrawn, never listed as accepted.
	if !strings.Contains(evidence, "- evidence-1 — differential\\_test (NOT\\_REPRODUCED; "+notAcceptedText+")") {
		t.Fatalf("masked NOT_REPRODUCED rendered as accepted:\n%s", evidence)
	}
}

// Every way of removing, forging or weakening what a divergence rests on makes
// the DIVERGED claim UNVERIFIED and lists no divergence.
func TestObservationTamperingFallsBackToUnverified(t *testing.T) {
	replayed := &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("a", 64), LiveRuns: 5}
	for name, mutate := range map[string]func(*model.Report){
		"stored status differs":       func(r *model.Report) { r.Evidence[1].Status = model.StatusNotDiverged },
		"no repeat cited":             func(r *model.Report) { r.Evidence[1].RepeatCheckID = "" },
		"repeat missing":              func(r *model.Report) { r.Checks = r.Checks[:2] },
		"repeat with another command": func(r *model.Report) { r.Checks[2].Command = []string{"go", "test", "./other"} },
		"repeat of another kind":      func(r *model.Report) { r.Checks[2].Kind = model.CheckGeneratedBase },
		"repeat replayed":             func(r *model.Report) { r.Checks[2].Cache = replayed },
		"repeat disagrees": func(r *model.Report) {
			r.Checks[2] = obsCheck("check-3", model.CheckGeneratedBaseRepeat, "Discount(100,10)", "90", "Discount(5,33)", "5")
		},
		"repeat fails":       func(r *model.Report) { r.Checks[2].Status, r.Checks[2].ExitCode = "FAIL", 1 },
		"duplicate check":    func(r *model.Report) { r.Checks = append(r.Checks, r.Checks[2]) },
		"duplicate evidence": func(r *model.Report) { r.Evidence = append(r.Evidence, r.Evidence[1]) },
		"candidate fails": func(r *model.Report) {
			r.Checks[1].Status, r.Checks[1].ExitCode = "FAIL", 1
			r.Checks[1].Output = obsLog("fail", "Discount(100,10)", "90", "Discount(5,33)", "3")
		},
		"candidate replayed":        func(r *model.Report) { r.Checks[1].Cache = replayed },
		"candidate of another kind": func(r *model.Report) { r.Checks[1].Kind = model.CheckGeneratedBase },
		"baseline truncated":        func(r *model.Report) { r.Checks[0].Truncated = true },
		"commands differ":           func(r *model.Report) { r.Checks[1].Command = []string{"go", "test", "./other"} },
		"unknown runner":            func(r *model.Report) { r.Evidence[1].Runner = "pytest" },
		"another test name":         func(r *model.Report) { r.Evidence[1].TestNames = []string{"TestOther"} },
		"no path":                   func(r *model.Report) { r.Evidence[1].Path = "" },
		"evidence of another kind":  func(r *model.Report) { r.Evidence[1].Kind = model.EvidenceDifferentialFuzz },
		"value altered by redaction": func(r *model.Report) {
			for i, v := range []string{"password=a", "password=b", "password=a"} {
				r.Checks[i].Output = obsLog("pass", "Discount(5,33)", v)
			}
		},
		"forged duplicate on the candidate": func(r *model.Report) {
			r.Checks[1].Output = obsLog("pass", "Discount(100,10)", "90", "Discount(5,33)", "3", "Discount(5,33)", "3")
		},
		"secret-shaped test name": func(r *model.Report) {
			const name = "TestAKIA0123456789ABCDEF"
			r.Evidence[1].TestNames = []string{name}
			for i := range r.Checks {
				r.Checks[i].Output = strings.ReplaceAll(r.Checks[i].Output, obsTest, name)
			}
		},
		"attr line altered by redaction": func(r *model.Report) {
			r.Checks[1].Output += `{"Action":"attr","Test":"` + obsTest + `","Key":"probe.x","Value":"[REDACTED]` + "\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := observationReport()
			mutate(r)
			Finalize(r, true)
			if r.Hypotheses[0].Status != model.StatusUnverified || len(r.Divergences) != 0 || r.ExitCode != 2 {
				t.Fatalf("status %s, divergences %+v, exit %d", r.Hypotheses[0].Status, r.Divergences, r.ExitCode)
			}
			if !strings.Contains(section(t, string(Markdown(r)), "## Behavior Divergences"), "No validated divergence was recorded.") {
				t.Fatal("the section does not say that nothing was validated")
			}
		})
	}
}

// A DIVERGED claim must cite the listed observation record itself, and only
// valid citations: the divergence stays listed, uncited.
func TestDivergedClaimMustCiteTheObservationRecord(t *testing.T) {
	for name, ids := range map[string][]string{
		"the differential test":         {"evidence-1"},
		"an unknown record as well":     {"evidence-2", "evidence-9"},
		"nothing":                       nil,
		"the record twice and no other": {"evidence-2", "evidence-2"},
	} {
		r := observationReport()
		r.Hypotheses[0].EvidenceIDs = ids
		Finalize(r, true)
		want := model.StatusUnverified
		if name == "the record twice and no other" {
			want = model.StatusDiverged
		}
		if r.Hypotheses[0].Status != want || len(r.Divergences) != 1 || r.ExitCode != 2 {
			t.Fatalf("%s: status %s, divergences %d, exit %d", name, r.Hypotheses[0].Status, len(r.Divergences), r.ExitCode)
		}
		if cited := len(r.Divergences[0].HypothesisIDs) > 0; cited != (want == model.StatusDiverged) {
			t.Fatalf("%s: hypothesis IDs %v", name, r.Divergences[0].HypothesisIDs)
		}
	}
}

func TestUncitedDivergenceRequestsReview(t *testing.T) {
	for _, ci := range []bool{false, true} {
		r := observationReport()
		r.Hypotheses = nil
		Finalize(r, ci)
		want := 0
		if ci {
			want = 2
		}
		if r.ExitCode != want || len(r.Divergences) != 1 || len(r.Divergences[0].HypothesisIDs) != 0 || r.Divergences[0].Path != "" {
			t.Fatalf("ci=%v: exit %d, divergences %+v", ci, r.ExitCode, r.Divergences)
		}
	}
}

// A both-pass generated test never supports NOT_REPRODUCED when its runs
// recorded different values, whatever became of the observation record.
func TestObservedDifferenceWithdrawsNotReproduced(t *testing.T) {
	claim := model.Hypothesis{ID: "h2", Title: "Discount unchanged", Severity: "high", Status: "NOT_REPRODUCED", Rationale: "passes on both", EvidenceIDs: []string{"evidence-1"}, Path: "price.go", Line: 3}
	for _, tc := range []struct {
		name   string
		mutate func(*model.Report)
		want   string
	}{
		{"verified divergence", func(*model.Report) {}, model.StatusUnverified},
		{"unstable observation", func(r *model.Report) {
			r.Checks[2] = obsCheck("check-3", model.CheckGeneratedBaseRepeat, "Discount(100,10)", "90", "Discount(5,33)", "5")
			r.Evidence[1].Status = model.StatusUnverified
		}, model.StatusUnverified},
		{"observation record removed", func(r *model.Report) {
			r.Evidence = r.Evidence[:1]
			r.Checks = r.Checks[:2]
		}, model.StatusUnverified},
		{"observation record with forged status", func(r *model.Report) { r.Evidence[1].Status = model.StatusNotDiverged }, model.StatusUnverified},
		{"key recorded by the candidate only", func(r *model.Report) {
			r.Checks[0] = obsCheck("check-1", model.CheckGeneratedBase, "Discount(100,10)", "90")
			r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate, "Discount(100,10)", "90", "Discount(5,33)", "3")
			r.Evidence = r.Evidence[:1]
			r.Checks = r.Checks[:2]
		}, model.StatusUnverified},
		{"equal values", func(r *model.Report) {
			r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate, "Discount(100,10)", "90", "Discount(5,33)", "4")
			r.Checks = r.Checks[:2]
			r.Evidence[1].Status, r.Evidence[1].RepeatCheckID = model.StatusNotDiverged, ""
		}, model.StatusNotReproduced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := observationReport()
			r.Hypotheses = []model.Hypothesis{claim}
			tc.mutate(r)
			Finalize(r, true)
			if got := r.Hypotheses[0].Status; got != tc.want {
				t.Fatalf("NOT_REPRODUCED claim became %s, want %s", got, tc.want)
			}
		})
	}
	// observationMasks names the candidate check of every record whose values
	// differ, whatever the record's verified status.
	r := observationReport()
	r.Evidence[1].Status = model.StatusUnverified
	if got := observationMasks(r, newLedger(r)); !got["check-2"] || len(got) != 1 {
		t.Fatalf("masks %v", got)
	}
}

// NOT_DIVERGED is an observation about the recorded values only: it supports
// no hypothesis status, however it is claimed.
func TestNotDivergedSupportsNothing(t *testing.T) {
	r := observationReport()
	r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate, "Discount(100,10)", "90", "Discount(5,33)", "4")
	r.Checks = r.Checks[:2]
	r.Evidence[1].Status, r.Evidence[1].RepeatCheckID = model.StatusNotDiverged, ""
	r.Evidence = append(r.Evidence, model.Evidence{ID: "evidence-3", Kind: model.EvidenceSourceObservation, Status: model.StatusObserved, Path: "price.go", Output: "return total"})
	r.Hypotheses = nil
	for i, claim := range allClaims {
		r.Hypotheses = append(r.Hypotheses, model.Hypothesis{ID: fmt.Sprintf("h%d", i+1), Title: "claim", Severity: "low", Status: claim, Rationale: "the values were equal", EvidenceIDs: []string{"evidence-2"}, Path: "price.go", Line: 3})
	}
	Finalize(r, false)
	if l := verifyReport(r); l.verified["evidence-2"] != model.StatusNotDiverged {
		t.Fatalf("NOT_DIVERGED not re-derived: %v", l.verified)
	}
	for _, h := range r.Hypotheses {
		if h.Status != model.StatusUnverified {
			t.Errorf("claim %q citing NOT_DIVERGED became %s", h.Title, h.Status)
		}
	}
	if got := strings.TrimSpace(section(t, string(Markdown(r)), "## Behavior Divergences")); got != noDivergenceText {
		t.Fatalf("section %q", got)
	}
}

// A NOT_DIVERGED record may rest on a replayed baseline only when two agreeing
// live runs recorded it.
func TestNotDivergedOnReplayedBaseline(t *testing.T) {
	for _, tc := range []struct {
		liveRuns int
		verified bool
	}{{1, false}, {2, true}} {
		r := observationReport()
		r.Checks[0].Cache = &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("b", 64), LiveRuns: tc.liveRuns}
		r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate, "Discount(100,10)", "90", "Discount(5,33)", "4")
		r.Checks = r.Checks[:2]
		r.Evidence[1].Status, r.Evidence[1].RepeatCheckID = model.StatusNotDiverged, ""
		r.Hypotheses = nil
		if got := verifyObservations(r, newLedger(r))["evidence-2"] == model.StatusNotDiverged; got != tc.verified {
			t.Fatalf("live_runs %d: verified %v", tc.liveRuns, got)
		}
	}
	// A divergence rests on the live repeat, so a replayed first baseline does
	// not prevent it.
	r := observationReport()
	r.Checks[0].Cache = &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("b", 64), LiveRuns: 2}
	Finalize(r, true)
	if r.Hypotheses[0].Status != model.StatusDiverged || len(r.Divergences) != 1 {
		t.Fatalf("status %s, divergences %d", r.Hypotheses[0].Status, len(r.Divergences))
	}
}

// Divergence rows survive Write -> Unmarshal -> Finalize byte for byte, and
// secret-shaped values are never compared: they stay INCOMPARABLE and never
// reach the written report.
func TestObservationRoundTripWithSecretShapedValues(t *testing.T) {
	r := observationReport()
	secrets := [][2]string{{"password=hunter2", "password=hunter3"}, {"token=ghp_0123456789abcdefghij", "token=ghp_0123456789abcdefghik"}}
	// A JSON escape hides this value from log redaction; decoded, it is a value
	// redaction would alter ("sk-abcdefghijklmnop0" or "...1").
	escaped := func(suffix string) string {
		return `{"Action":"attr","Package":"example.com/m/pkg","Test":"` + obsTest + `","Key":"probe.Escaped()","Value":"sk-abcdefghijklmnop` + suffix + `"}` + "\n"
	}
	for i, value := range []string{"4", "3", "4"} {
		side := 0
		if i == 1 {
			side = 1
		}
		log := obsLog("pass", "Discount(100,10)", "90", "Discount(5,33)", value, "Secret()", secrets[0][side], "Token()", secrets[1][side])
		r.Checks[i].Output = strings.Replace(log, `{"Action":"pass"`, escaped(fmt.Sprint(side))+`{"Action":"pass"`, 1)
	}
	Finalize(r, true)
	if r.Hypotheses[0].Status != model.StatusDiverged || len(r.Divergences) != 1 || r.ExitCode != 2 {
		t.Fatalf("first finalize: %s, %d divergences, exit %d", r.Hypotheses[0].Status, len(r.Divergences), r.ExitCode)
	}
	rows := r.Divergences[0].Observations
	if len(rows) != 1 || rows[0].Key != "Discount(5,33)" {
		t.Fatalf("secret-shaped values diverged: %+v", rows)
	}
	firstJSON, firstMD := writeAndRead(t, t.TempDir(), r)
	for _, secret := range []string{"hunter2", "hunter3", "ghp_0123", "sk-abcdefghijklmnop"} {
		if strings.Contains(string(firstJSON), secret) || strings.Contains(string(firstMD), secret) {
			t.Fatalf("secret %q reached the written report", secret)
		}
	}
	var loaded model.Report
	if err := json.Unmarshal(firstJSON, &loaded); err != nil {
		t.Fatal(err)
	}
	Finalize(&loaded, true)
	secondJSON, secondMD := writeAndRead(t, t.TempDir(), &loaded)
	if string(firstJSON) != string(secondJSON) || string(firstMD) != string(secondMD) {
		t.Fatalf("the re-render differs:\n%s\n---\n%s", firstJSON, secondJSON)
	}
	if loaded.Hypotheses[0].Status != model.StatusDiverged || len(loaded.Divergences) != 1 || loaded.Divergences[0].Observations[0].Key != "Discount(5,33)" {
		t.Fatalf("re-finalized divergences %+v", loaded.Divergences)
	}
	// The secret-shaped keys are recorded, compared and found incomparable.
	o, ok := observationOutcome(loaded.Evidence[1], newLedger(&loaded))
	if !ok {
		t.Fatal("the outcome is not re-derivable")
	}
	incomparable := 0
	for _, row := range o.Observations {
		if row.Key == "Secret()" || row.Key == "Token()" || row.Key == "Escaped()" {
			if row.Status != model.ObservationIncomparable {
				t.Errorf("row %+v", row)
			}
			if !redact.IsFixedPoint(row.Base) || !redact.IsFixedPoint(row.Candidate) {
				t.Errorf("row %s displays a value redaction would alter", row.Key)
			}
			incomparable++
		}
	}
	if incomparable != 3 {
		t.Fatalf("%d secret-shaped rows, want 3: %+v", incomparable, o.Observations)
	}
}

// Values are compared in full; the rows keep UTF-8-safe display cuts.
func TestObservationLongValuesCompareInFull(t *testing.T) {
	r := observationReport()
	prefix := strings.Repeat("ü", 200) // 400 bytes
	for i, v := range []string{prefix + "A", prefix + "B", prefix + "A"} {
		r.Checks[i].Output = obsLog("pass", "Long()", v)
	}
	Finalize(r, true)
	if len(r.Divergences) != 1 {
		t.Fatalf("divergences %+v", r.Divergences)
	}
	row := r.Divergences[0].Observations[0]
	if !row.Truncated || len(row.Base) > 256 || row.Base != row.Candidate || !strings.HasPrefix(prefix, row.Base) {
		t.Fatalf("display cut %+v", row)
	}
}

func TestVitestObservationVerification(t *testing.T) {
	const title, path = "observe total", "src/obs.test.ts"
	command := []string{"npx", "--no", "vitest", "run", path, "--reporter=json", "--outputFile=/tmp/probe-test-results.json"}
	results := func(meta string) string {
		return `{"testResults":[{"name":"/workspace/` + path + `","status":"passed","assertionResults":[{"ancestorTitles":[],"title":"` + title + `","status":"passed","meta":{"probe":` + meta + `}}]}]}`
	}
	check := func(id, kind, meta string) model.Check {
		return model.Check{ID: id, Kind: kind, Status: "PASS", Command: command, Results: results(meta)}
	}
	r := observationReport()
	r.Checks = []model.Check{
		check("check-1", model.CheckGeneratedBase, `{"total([10],0.5)":"5"}`),
		check("check-2", model.CheckGeneratedCandidate, `{"total([10],0.5)":"4"}`),
		check("check-3", model.CheckGeneratedBaseRepeat, `{"total([10],0.5)":"5"}`),
	}
	for i := range r.Evidence {
		r.Evidence[i].Runner, r.Evidence[i].Path, r.Evidence[i].TestNames = "jest_json", path, []string{title}
	}
	Finalize(r, true)
	if r.Hypotheses[0].Status != model.StatusDiverged || len(r.Divergences) != 1 || r.Divergences[0].Observations[0].Key != "total([10],0.5)" || r.Divergences[0].Observations[0].Test != title {
		t.Fatalf("status %s, divergences %+v", r.Hypotheses[0].Status, r.Divergences)
	}
	// A meta that is not an object of strings is a channel error.
	r.Checks[1].Results = results(`"probe: observations were not a key/value object"`)
	Finalize(r, true)
	if r.Hypotheses[0].Status != model.StatusUnverified || len(r.Divergences) != 0 {
		t.Fatalf("a marker meta diverged: %s", r.Hypotheses[0].Status)
	}
}

func TestObservationFinalizeIsIdempotent(t *testing.T) {
	r := observationReport()
	r.Hypotheses = append(r.Hypotheses, model.Hypothesis{ID: "h2", Title: "unchanged", Severity: "low", Status: "NOT_REPRODUCED", Rationale: "both pass", EvidenceIDs: []string{"evidence-1"}})
	Finalize(r, true)
	first, _ := json.Marshal(r)
	Finalize(r, true)
	second, _ := json.Marshal(r)
	if string(first) != string(second) {
		t.Fatalf("Finalize is not idempotent:\n%s\n%s", first, second)
	}
	if r.Hypotheses[0].Status != model.StatusDiverged || r.Hypotheses[1].Status != model.StatusUnverified {
		t.Fatalf("statuses %s %s", r.Hypotheses[0].Status, r.Hypotheses[1].Status)
	}
}

// lineLimitLog renders a passing run of the observation test whose value for
// key was too long for test2json: the framed line stays an output event.
func lineLimitLog(key, value string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"Action":"run","Package":"example.com/m/pkg","Test":%q}`+"\n", obsTest)
	for _, chunk := range []string{"\x16=== ATTR  " + obsTest + " probe." + key + " " + value[:len(value)/2], value[len(value)/2:] + "\n"} {
		line, _ := json.Marshal(map[string]string{"Action": "output", "Package": "example.com/m/pkg", "Test": obsTest, "Output": chunk})
		b.Write(line)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, `{"Action":"pass","Package":"example.com/m/pkg","Test":%q}`+"\n", obsTest)
	return b.String()
}

// Whatever does not establish equal full values withdraws a both-pass
// NOT_REPRODUCED: a value the runner could not convert, values that redaction
// made equal, and observations that could not be read at all. Equal values
// that were compared in full keep it.
func TestUnknownEqualityWithdrawsNotReproduced(t *testing.T) {
	claim := model.Hypothesis{ID: "h2", Title: "Label unchanged", Severity: "high", Status: "NOT_REPRODUCED", Rationale: "passes on both", EvidenceIDs: []string{"evidence-1"}, Path: "price.go", Line: 3}
	const title, path = "observe total", "src/obs.test.ts"
	vitest := func(r *model.Report, base, candidate string) {
		command := []string{"npx", "--no", "vitest", "run", path, "--reporter=json", "--outputFile=/tmp/probe-test-results.json"}
		results := func(meta string) string {
			return `{"testResults":[{"name":"/workspace/` + path + `","status":"passed","assertionResults":[{"ancestorTitles":[],"title":"` + title + `","status":"passed","meta":{"probe":` + meta + `}}]}]}`
		}
		r.Checks = []model.Check{
			{ID: "check-1", Kind: model.CheckGeneratedBase, Status: "PASS", Command: command, Results: results(base)},
			{ID: "check-2", Kind: model.CheckGeneratedCandidate, Status: "PASS", Command: command, Results: results(candidate)},
		}
		r.Evidence = r.Evidence[:1]
		r.Evidence[0].Runner, r.Evidence[0].Path, r.Evidence[0].TestNames = "jest_json", path, []string{title}
	}
	standIn := func(text string) string { b, _ := json.Marshal(observe.Oversized(text)); return string(b) }
	notObject := `"probe: observations were not a key/value object"`
	for _, tc := range []struct {
		name   string
		mutate func(*model.Report)
		want   string
	}{
		{"candidate value too long for the runner", func(r *model.Report) {
			r.Checks[0] = obsCheck("check-1", model.CheckGeneratedBase, "Label(1)", `"ok"`)
			r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate)
			r.Checks[1].Output = lineLimitLog("Label(1)", strings.Repeat("x", 5000))
			r.Checks = r.Checks[:2]
			r.Evidence[1].Status, r.Evidence[1].RepeatCheckID = model.StatusUnverified, ""
		}, model.StatusUnverified},
		{"both values too long for the runner", func(r *model.Report) {
			r.Checks[0] = obsCheck("check-1", model.CheckGeneratedBase)
			r.Checks[0].Output = lineLimitLog("Label(1)", strings.Repeat("x", 5000))
			r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate)
			r.Checks[1].Output = lineLimitLog("Label(1)", strings.Repeat("x", 5000))
			r.Checks = r.Checks[:2]
			r.Evidence = r.Evidence[:1]
		}, model.StatusUnverified},
		{"values equal only after redaction", func(r *model.Report) {
			r.Checks[0] = obsCheck("check-1", model.CheckGeneratedBase, "Secret()", "password=a1")
			r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate, "Secret()", "password=b2")
			r.Checks = r.Checks[:2]
			r.Evidence[1].Status, r.Evidence[1].RepeatCheckID = model.StatusUnverified, ""
		}, model.StatusUnverified},
		{"attr line that no longer parses", func(r *model.Report) {
			r.Checks[1] = obsCheck("check-2", model.CheckGeneratedCandidate, "Discount(100,10)", "90", "Discount(5,33)", "4")
			r.Checks[1].Output += `{"Action":"attr","Test":"` + obsTest + `","Key":"probe.x","Value":"[REDACTED]` + "\n"
			r.Checks = r.Checks[:2]
			r.Evidence = r.Evidence[:1]
		}, model.StatusUnverified},
		{"vitest markers on both sides", func(r *model.Report) { vitest(r, notObject, notObject) }, model.StatusUnverified},
		{"vitest stand-ins of different values", func(r *model.Report) {
			vitest(r, `{"k":`+standIn(strings.Repeat("a", 2000))+`}`, `{"k":`+standIn(strings.Repeat("b", 2000))+`}`)
		}, model.StatusUnverified},
		{"vitest stand-ins of equal values", func(r *model.Report) {
			vitest(r, `{"k":`+standIn(strings.Repeat("a", 2000))+`}`, `{"k":`+standIn(strings.Repeat("a", 2000))+`}`)
		}, model.StatusNotReproduced},
		{"vitest number against string", func(r *model.Report) { vitest(r, `{"k":4}`, `{"k":"4"}`) }, model.StatusUnverified},
		{"vitest equal values", func(r *model.Report) { vitest(r, `{"k":4,"s":"x"}`, `{"s":"x","k":4}`) }, model.StatusNotReproduced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := observationReport()
			r.Hypotheses = []model.Hypothesis{claim}
			tc.mutate(r)
			Finalize(r, true)
			if got := r.Hypotheses[0].Status; got != tc.want {
				t.Fatalf("NOT_REPRODUCED claim became %s, want %s", got, tc.want)
			}
			if tc.want == model.StatusUnverified && r.ExitCode != 2 {
				t.Fatalf("exit %d", r.ExitCode)
			}
		})
	}
}
