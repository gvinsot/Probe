package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const fuzzCalcBase = `package calc

// Cents is an amount of money.
type Cents int64

// Discount takes ten percent off from 1000 cents.
func Discount(c Cents) Cents {
	if c >= 1000 {
		return c * 9 / 10
	}
	return c
}

// Twice doubles n.
func Twice(n int) int {
	return n * 2
}
`

const fuzzCalcCandidate = `package calc

// Cents is an amount of money.
type Cents int64

// Discount takes ten percent off above 1000 cents.
func Discount(c Cents) Cents {
	if c > 1000 {
		return c * 9 / 10
	}
	return c
}

// Twice doubles n.
func Twice(n int) int {
	return n + n
}
`

// fuzzValue says what one revision records for input i of a test.
type fuzzValue func(side string, confirm bool, test fuzz.HarnessTest, i int) string

// divergeAt records the call itself on both revisions, except the candidate
// value of the given call.
func divergeAt(call, candidate string) fuzzValue {
	return func(side string, _ bool, test fuzz.HarnessTest, i int) string {
		if side == "candidate" && test.Inputs[i].Call == call {
			return candidate
		}
		return "v:" + test.Inputs[i].Call
	}
}

// fakeFuzzRunner writes the raw stream a harness would write for each
// revision and records checks and evidence like the harness does.
type fakeFuzzRunner struct {
	t        testing.TB
	value    fuzzValue
	checks   []model.Check
	evidence []model.Evidence
}

func (f *fakeFuzzRunner) Observe(_ context.Context, req fuzz.Request) (fuzz.Side, fuzz.Side, error) {
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	return f.side("base", baseKind, req), f.side("candidate", candidateKind, req), nil
}

func (f *fakeFuzzRunner) side(name, kind string, req fuzz.Request) fuzz.Side {
	h := req.Harness
	var stream, log strings.Builder
	for n, test := range h.Tests {
		fmt.Fprintf(&stream, `{"f":%d,"k":"begin","n":%d}`+"\n", n+1, len(test.Inputs))
		for i := range test.Inputs {
			enc := f.value(name, req.Confirm, test, i)
			sum := sha256.Sum256([]byte(enc))
			fmt.Fprintf(&stream, `{"f":%d,"k":"obs","i":%d,"h":"%s","l":%d,"o":%s,"p":false,"d":false,"t":false}`+"\n", n+1, i, hex.EncodeToString(sum[:]), len(enc), fuzzQuote(enc))
		}
		fmt.Fprintf(&stream, `{"f":%d,"k":"end","n":%d}`+"\n", n+1, len(test.Inputs))
		fmt.Fprintf(&log, `{"Action":"run","Package":"example.test/fuzzdemo/calc","Test":%q}`+"\n", test.Name)
		fmt.Fprintf(&log, `{"Action":"pass","Package":"example.test/fuzzdemo/calc","Test":%q}`+"\n", test.Name)
	}
	results, err := h.Normalize([]byte(stream.String()))
	if err != nil {
		f.t.Fatalf("fake stream rejected: %v", err)
	}
	c := model.Check{
		ID: fmt.Sprintf("check-%d", len(f.checks)+1), Kind: kind, Status: "PASS",
		Command: []string{"go", "test", "./" + h.Tests[0].Target.Dir, "-json", "-count=1", "-run", "^(" + strings.Join(h.TestNames(), "|") + ")$"},
		Output:  log.String(), Results: results,
	}
	f.checks = append(f.checks, c)
	return fuzz.Side{Check: c}
}

func (f *fakeFuzzRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	e.ID = fmt.Sprintf("evidence-%d", len(f.evidence)+1)
	f.evidence = append(f.evidence, e)
	return e, nil
}

// fuzzQuote is the harness's JSON string encoder.
func fuzzQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// fuzzReport runs the calc fixture (Discount and Twice) through the real
// selection, rendering, normalization and comparison with a fake sandbox.
func fuzzReport(t *testing.T, value fuzzValue) *model.Report {
	t.Helper()
	limits := fuzz.Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeout: time.Second, MaxRuntime: time.Hour}
	return fuzzReportOf(t, fuzzCalcBase, fuzzCalcCandidate, limits, 2, value)
}

// fuzzReportOf runs one changed calc/calc.go through the real selection,
// rendering, normalization and comparison with a fake sandbox.
func fuzzReportOf(t testing.TB, baseSrc, candidateSrc string, limits fuzz.Limits, targets int, value fuzzValue) *model.Report {
	t.Helper()
	base, candidate := t.TempDir(), t.TempDir()
	for dir, src := range map[string]string{base: baseSrc, candidate: candidateSrc} {
		if err := os.MkdirAll(filepath.Join(dir, "calc"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{"go.mod": "module example.test/fuzzdemo\n\ngo 1.21\n", "calc/calc.go": src} {
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	change := model.Change{BaseRef: "main", BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40), Files: []model.ChangedFile{{Path: "calc/calc.go", Status: "M"}}}
	plan, err := fuzz.Select(base, candidate, change, nil, limits)
	if err != nil || plan.Targets() != targets {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	runner := &fakeFuzzRunner{t: t, value: value}
	rep := fuzz.Run(context.Background(), runner, plan, fuzz.Options{
		Limits: limits, ObservationsPath: "/tmp/swiftproof-observations.jsonl", PayloadLimit: harness.PayloadLimit(65536),
		NewSuffix: func() (string, error) { return "abcdef1234567890", nil },
	})
	return &model.Report{
		Version: 1, ToolVersion: "fuzz-test", GeneratedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Change: change, Checks: runner.checks, Evidence: runner.evidence, Fuzz: &rep,
	}
}

func fuzzFunction(t *testing.T, r *model.Report, symbol string) *model.FuzzFunction {
	t.Helper()
	for i := range r.Fuzz.Functions {
		if r.Fuzz.Functions[i].Symbol == symbol {
			return &r.Fuzz.Functions[i]
		}
	}
	t.Fatalf("no fuzz function %s in %+v", symbol, r.Fuzz.Functions)
	return nil
}

func reportJSON(t *testing.T, r *model.Report) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const discountCall = "Discount(Cents(1000))"

// A validated divergence is listed with its changed-function anchor and the
// four checks, requests review (exit 2 with --ci, 0 without), and never gives
// exit 1 or a reproduced issue, whatever a hypothesis claims.
func TestFuzzDivergenceIsListedAndNeverReproduced(t *testing.T) {
	r := fuzzReport(t, divergeAt(discountCall, "w:"+discountCall))
	discount, twice := fuzzFunction(t, r, "calc.Discount"), fuzzFunction(t, r, "calc.Twice")
	if discount.Outcome != model.FuzzDiverged || twice.Outcome != model.FuzzNotDiverged {
		t.Fatalf("outcomes %s %s", discount.Outcome, twice.Outcome)
	}
	r.Hypotheses = []model.Hypothesis{
		{ID: "h-reproduced", Title: "claimed defect", Severity: "critical", Status: model.StatusReproduced, EvidenceIDs: []string{discount.EvidenceID}, Path: "calc/calc.go", Line: 7},
		{ID: "h-diverged", Title: "boundary moved", Severity: "high", Status: model.StatusDiverged, EvidenceIDs: []string{discount.EvidenceID}, Path: "calc/calc.go", Line: 7},
		{ID: "h-not-reproduced", Title: "twice unchanged", Severity: "low", Status: model.StatusNotReproduced, EvidenceIDs: []string{twice.EvidenceID}},
		{ID: "h-dismissed", Title: "twice", Severity: "low", Status: model.StatusDismissed, Rationale: "no divergence", EvidenceIDs: []string{twice.EvidenceID}},
	}
	Finalize(r, false)
	if r.ExitCode != 0 {
		t.Fatalf("exit %d without --ci", r.ExitCode)
	}
	Finalize(r, true)
	if r.ExitCode != 2 || len(r.ReproducedIssues) != 0 {
		t.Fatalf("exit %d, reproduced %+v", r.ExitCode, r.ReproducedIssues)
	}
	for id, want := range map[string]string{"h-reproduced": model.StatusUnverified, "h-diverged": model.StatusDiverged, "h-not-reproduced": model.StatusUnverified, "h-dismissed": model.StatusUnverified} {
		for _, h := range r.Hypotheses {
			if h.ID == id && h.Status != want {
				t.Errorf("%s: %s, want %s", id, h.Status, want)
			}
		}
	}
	if len(r.Divergences) != 1 {
		t.Fatalf("divergences %+v", r.Divergences)
	}
	d := r.Divergences[0]
	discount = fuzzFunction(t, r, "calc.Discount")
	want := []string{discount.Checks.Base, discount.Checks.Candidate, discount.Checks.BaseConfirm, discount.Checks.CandidateConfirm}
	if d.EvidenceID != discount.EvidenceID || d.Kind != model.EvidenceDifferentialFuzz || d.Path != "calc/calc.go" || d.Line != discount.Line || d.Symbol != "calc.Discount" ||
		d.AnchorSource != anchorChangedFunction || strings.Join(d.CheckIDs, ",") != strings.Join(want, ",") || strings.Join(d.HypothesisIDs, ",") != "h-diverged" ||
		d.TestPath != "calc/swiftproof_fuzz_abcdef1234567890_test.go" || len(d.TestNames) != 1 || d.TestNames[0] != discount.TestName {
		t.Fatalf("divergence %+v", d)
	}
	if len(d.Observations) != 1 || d.Observations[0].Key != discountCall || d.Observations[0].Base != "v:"+discountCall || d.Observations[0].Candidate != "w:"+discountCall || d.Observations[0].Test != discount.TestName {
		t.Fatalf("rows %+v", d.Observations)
	}
	if c := discount.Counterexample; c == nil || c.Input != discountCall || c.Base != "v:"+discountCall || c.Candidate != "w:"+discountCall {
		t.Fatalf("counterexample %+v", c)
	}
	// Review targets: the diverged function, at least high (the critical
	// UNVERIFIED hypothesis on the same line merges into it).
	found := false
	for _, rt := range r.ReviewTargets {
		if rt.Path == "calc/calc.go" && rt.StartLine == discount.Line && rank(rt.Severity) >= rank("high") && strings.Contains(strings.Join(rt.Reasons, " "), discountCall) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no review target for the diverged function: %+v", r.ReviewTargets)
	}
}

// Equal recordings: every function is not_diverged, nothing requests review,
// and the Behavior Divergences section says only what was compared.
func TestFuzzNotDivergedRequestsNothing(t *testing.T) {
	r := fuzzReport(t, divergeAt("", ""))
	Finalize(r, true)
	if r.ExitCode != 0 || len(r.Divergences) != 0 || len(r.ReviewTargets) != 0 {
		t.Fatalf("exit %d, divergences %+v, targets %+v", r.ExitCode, r.Divergences, r.ReviewTargets)
	}
	for _, f := range r.Fuzz.Functions {
		if f.Outcome != model.FuzzNotDiverged || f.Compared != f.Inputs || f.Counterexample != nil {
			t.Fatalf("function %+v", f)
		}
	}
	md := string(Markdown(r))
	if !strings.Contains(section(t, md, "## Behavior Divergences"), noDivergenceText) {
		t.Fatalf("behavior divergences:\n%s", section(t, md, "## Behavior Divergences"))
	}
}

// Finalize is idempotent, also after Write -> decode, and the Markdown of a
// re-render is identical.
func TestFuzzFinalizeIsIdempotentAcrossRoundTrip(t *testing.T) {
	r := fuzzReport(t, divergeAt(discountCall, "w:"+discountCall))
	Finalize(r, true)
	if len(r.ReviewTargets) != 1 || r.ReviewTargets[0].Severity != "high" || r.ReviewTargets[0].StartLine != 7 || r.ReviewTargets[0].EndLine != 12 {
		t.Fatalf("targets %+v", r.ReviewTargets)
	}
	first := reportJSON(t, r)
	Finalize(r, true)
	if second := reportJSON(t, r); second != first {
		t.Fatal("Finalize is not idempotent")
	}
	dir := t.TempDir()
	data, md := writeAndRead(t, dir, r)
	var decoded model.Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	Finalize(&decoded, true)
	data2, md2 := writeAndRead(t, t.TempDir(), &decoded)
	if !bytes.Equal(data, data2) || !bytes.Equal(md, md2) {
		t.Fatal("a re-render changed the report")
	}
	if decoded.ExitCode != 2 || len(decoded.Divergences) != 1 || fuzzFunction(t, &decoded, "calc.Discount").Outcome != model.FuzzDiverged {
		t.Fatalf("re-render: exit %d, divergences %d", decoded.ExitCode, len(decoded.Divergences))
	}
}

// Any edit that the recorded checks do not support makes the function
// inconclusive, withdraws the divergence, and stays so on a second Finalize.
func TestFuzzTamperingDowngrades(t *testing.T) {
	checkOfKind := func(r *model.Report, kind string) *model.Check {
		for i := range r.Checks {
			if r.Checks[i].Kind == kind {
				return &r.Checks[i]
			}
		}
		t.Fatalf("no %s check", kind)
		return nil
	}
	evidenceOf := func(r *model.Report, id string) *model.Evidence {
		for i := range r.Evidence {
			if r.Evidence[i].ID == id {
				return &r.Evidence[i]
			}
		}
		t.Fatalf("no evidence %s", id)
		return nil
	}
	editStream := func(t *testing.T, c *model.Check, edit func(*fuzz.Stream)) {
		s, err := fuzz.ParseResults(c.Results)
		if err != nil {
			t.Fatal(err)
		}
		edit(&s)
		b, err := json.MarshalIndent(s, "", "")
		if err != nil {
			t.Fatal(err)
		}
		c.Results = string(b)
		if _, err := fuzz.ParseResults(c.Results); err != nil {
			t.Fatalf("the edited stream no longer parses: %v", err)
		}
	}
	hit := func(liveRuns int) *model.CheckCache {
		return &model.CheckCache{Status: model.CacheHit, Key: strings.Repeat("c", 64), RecordedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), RecordedRun: "run", RecordedCheck: "check-1", LiveRuns: liveRuns}
	}
	type want struct{ discount, twice string }
	for name, tc := range map[string]struct {
		edit func(t *testing.T, r *model.Report)
		want want
	}{
		"untouched": {func(*testing.T, *model.Report) {}, want{model.FuzzDiverged, model.FuzzNotDiverged}},
		"one hash of the counterexample": {func(t *testing.T, r *model.Report) {
			discount := fuzzFunction(t, r, "calc.Discount")
			editStream(t, checkOfKind(r, model.CheckFuzzCandidate), func(s *fuzz.Stream) {
				for i := range s.Functions {
					if s.Functions[i].Test == discount.TestName {
						s.Functions[i].Records[discount.Counterexample.Index].SHA256 = strings.Repeat("0", 64)
					}
				}
			})
		}, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"one hash of a compared input": {func(t *testing.T, r *model.Report) {
			twice := fuzzFunction(t, r, "calc.Twice")
			editStream(t, checkOfKind(r, model.CheckFuzzBase), func(s *fuzz.Stream) {
				for i := range s.Functions {
					if s.Functions[i].Test == twice.TestName {
						s.Functions[i].Records[3].SHA256 = strings.Repeat("0", 64)
					}
				}
			})
		}, want{model.FuzzDiverged, model.FuzzInconclusive}},
		"diverged count": {func(t *testing.T, r *model.Report) { fuzzFunction(t, r, "calc.Discount").Diverged = 2 }, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"counterexample display": {func(t *testing.T, r *model.Report) {
			fuzzFunction(t, r, "calc.Discount").Counterexample.Candidate = "forged"
		}, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"confirmation checks dropped": {func(t *testing.T, r *model.Report) {
			f := fuzzFunction(t, r, "calc.Discount")
			f.Checks.BaseConfirm, f.Checks.CandidateConfirm = "", ""
		}, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"half a confirmation pair": {func(t *testing.T, r *model.Report) { fuzzFunction(t, r, "calc.Discount").Checks.CandidateConfirm = "" }, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"baseline confirmation replayed": {func(t *testing.T, r *model.Report) {
			checkOfKind(r, model.CheckFuzzBaseConfirm).Cache = hit(5)
		}, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"candidate replayed": {func(t *testing.T, r *model.Report) {
			checkOfKind(r, model.CheckFuzzCandidate).Cache = hit(5)
		}, want{model.FuzzInconclusive, model.FuzzInconclusive}},
		"first baseline replayed from two live runs": {func(t *testing.T, r *model.Report) {
			checkOfKind(r, model.CheckFuzzBase).Cache = hit(2)
		}, want{model.FuzzDiverged, model.FuzzNotDiverged}},
		"first baseline replayed from one live run": {func(t *testing.T, r *model.Report) {
			checkOfKind(r, model.CheckFuzzBase).Cache = hit(1)
		}, want{model.FuzzInconclusive, model.FuzzInconclusive}},
		"evidence runner": {func(t *testing.T, r *model.Report) {
			evidenceOf(r, fuzzFunction(t, r, "calc.Discount").EvidenceID).Runner = harness.RunnerJest
		}, want{model.FuzzInconclusive, model.FuzzNotDiverged}},
		"evidence removed": {func(t *testing.T, r *model.Report) {
			id := fuzzFunction(t, r, "calc.Twice").EvidenceID
			kept := r.Evidence[:0]
			for _, e := range r.Evidence {
				if e.ID != id {
					kept = append(kept, e)
				}
			}
			r.Evidence = kept
		}, want{model.FuzzDiverged, model.FuzzInconclusive}},
		"two functions cite one record": {func(t *testing.T, r *model.Report) {
			fuzzFunction(t, r, "calc.Twice").EvidenceID = fuzzFunction(t, r, "calc.Discount").EvidenceID
		}, want{model.FuzzInconclusive, model.FuzzInconclusive}},
		"outcome forged": {func(t *testing.T, r *model.Report) {
			twice := fuzzFunction(t, r, "calc.Twice")
			twice.Outcome = model.FuzzDiverged
			twice.Counterexample = &model.FuzzCounterexample{Input: "Twice(1)", Base: "a", Candidate: "b"}
		}, want{model.FuzzDiverged, model.FuzzInconclusive}},
		"evidence status forged": {func(t *testing.T, r *model.Report) {
			evidenceOf(r, fuzzFunction(t, r, "calc.Twice").EvidenceID).Status = model.StatusDiverged
		}, want{model.FuzzDiverged, model.FuzzInconclusive}},
		// Every function of the package cites the confirmation pair, so the
		// four commands must agree for both.
		"command changed": {func(t *testing.T, r *model.Report) {
			c := checkOfKind(r, model.CheckFuzzCandidateConfirm)
			c.Command = append(append([]string{}, c.Command...), "-v")
		}, want{model.FuzzInconclusive, model.FuzzInconclusive}},
		"stream broken": {func(t *testing.T, r *model.Report) {
			c := checkOfKind(r, model.CheckFuzzCandidate)
			c.Results = strings.Replace(c.Results, `"v":`, `"v": `, 1)
		}, want{model.FuzzInconclusive, model.FuzzInconclusive}},
		"planned inputs changed": {func(t *testing.T, r *model.Report) { fuzzFunction(t, r, "calc.Twice").Inputs++ }, want{model.FuzzDiverged, model.FuzzInconclusive}},
	} {
		t.Run(name, func(t *testing.T) {
			r := fuzzReport(t, divergeAt(discountCall, "w:"+discountCall))
			tc.edit(t, r)
			Finalize(r, true)
			first := reportJSON(t, r)
			discount, twice := fuzzFunction(t, r, "calc.Discount"), fuzzFunction(t, r, "calc.Twice")
			if discount.Outcome != tc.want.discount || twice.Outcome != tc.want.twice {
				t.Fatalf("outcomes %s (%s) / %s (%s), want %s / %s", discount.Outcome, discount.Reason, twice.Outcome, twice.Reason, tc.want.discount, tc.want.twice)
			}
			if (len(r.Divergences) == 1) != (tc.want.discount == model.FuzzDiverged) {
				t.Fatalf("divergences %+v", r.Divergences)
			}
			for _, f := range []*model.FuzzFunction{discount, twice} {
				if f.Outcome == model.FuzzInconclusive && (f.Counterexample != nil || f.Reason == "") {
					t.Fatalf("inconclusive function %+v", f)
				}
			}
			if r.ExitCode != 2 {
				t.Fatalf("exit %d", r.ExitCode)
			}
			Finalize(r, true)
			if reportJSON(t, r) != first {
				t.Fatal("Finalize is not idempotent after a downgrade")
			}
		})
	}
}

// The Markdown section summarizes outcomes, shows one value pair per diverged
// function, escapes recorded values and never shows a percentage.
func TestFuzzMarkdownSection(t *testing.T) {
	hostile := "**bold** [link](https://example.invalid) <script>alert(1)</script> `tick` | #"
	r := fuzzReport(t, divergeAt(discountCall, hostile))
	r.Fuzz.Skipped = append(r.Fuzz.Skipped,
		model.FuzzSkip{Path: "calc/more.go", Line: 3, Symbol: "calc.(*T).M", Reason: "methods are not fuzzed in this version"},
		model.FuzzSkip{Path: "web/price.ts", Line: 1, Symbol: "price", Reason: fuzz.ReasonScriptNotImplemented})
	r.Fuzz.SkippedTotal = 2
	Finalize(r, true)
	md := string(Markdown(r))
	body := section(t, md, "## Differential Fuzzing")
	for _, want := range []string{
		"Seeded inputs (swiftproof-fuzz/v1) ran through 2 changed Go functions on the baseline and the candidate: 1 diverged, 1 not diverged, 0 inconclusive.",
		"- **diverged** calc.Discount (calc/calc.go:7): 1 of 64 compared inputs recorded different values",
		"Smallest divergent input tried: Discount\\(Cents\\(1000\\)\\); baseline v:Discount\\(Cents\\(1000\\)\\); candidate \\*\\*bold\\*\\* \\[link\\]\\(https://example.invalid\\) &lt;script&gt;",
		"- **not diverged** calc.Twice (calc/calc.go:15): 64 of 64 inputs compared",
		"Not fuzzed (2):",
		"- web/price.ts:1 price: TS/JS differential fuzzing is not implemented in this build",
		inline(model.FuzzNote),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("section lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "%") || strings.Contains(body, "<script>") || strings.Count(body, "; baseline ") != 1 {
		t.Fatalf("section shows a percentage, raw HTML, or more than one value pair:\n%s", body)
	}
	for _, word := range []string{"tested", "verified", "safe", "regression", "bug"} {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(body, inline(model.FuzzNote), "")), word) {
			t.Errorf("section uses %q:\n%s", word, body)
		}
	}
	// The section comes after Changed-line Execution and before Recorded Evidence.
	if i, j, k := strings.Index(md, "## Changed-line Execution"), strings.Index(md, "## Differential Fuzzing"), strings.Index(md, "## Recorded Evidence"); !(i < j && j < k) {
		t.Fatal("the section is out of order")
	}
	if len(strayHeadings(md)) != 0 {
		t.Fatalf("stray headings %v", strayHeadings(md))
	}
}

// Section statuses: not_run requests review; disabled and no_candidates do
// not; without a section nothing is rendered.
func TestFuzzSectionStatuses(t *testing.T) {
	base := func(f *model.FuzzReport) *model.Report {
		return &model.Report{Change: model.Change{Files: []model.ChangedFile{{Path: "a.go", Status: "M"}}}, Checks: []model.Check{{ID: "check-1", Kind: "test", Status: "PASS", Command: []string{"go", "test", "./..."}}}, Fuzz: f}
	}
	for status, wantExit := range map[string]int{model.FuzzNotRun: 2, model.FuzzDisabled: 0, model.FuzzNoCandidates: 0, model.FuzzRan: 0} {
		r := base(&model.FuzzReport{Status: status, Reason: "because"})
		Finalize(r, true)
		if r.ExitCode != wantExit {
			t.Errorf("%s: exit %d, want %d", status, r.ExitCode, wantExit)
		}
		if r.Fuzz.Functions == nil || r.Fuzz.Skipped == nil || r.Fuzz.Note != model.FuzzNote || r.Fuzz.SeedScheme != model.FuzzSeedScheme {
			t.Errorf("%s: section not normalized %+v", status, r.Fuzz)
		}
		b, _ := json.Marshal(r.Fuzz)
		if !strings.Contains(string(b), `"functions":[]`) || !strings.Contains(string(b), `"skipped":[]`) {
			t.Errorf("%s: %s", status, b)
		}
	}
	r := base(nil)
	Finalize(r, true)
	if r.ExitCode != 0 || strings.Contains(string(Markdown(r)), "## Differential Fuzzing") {
		t.Fatal("a report without a fuzz section rendered one or requested review")
	}
	// An inconclusive function without evidence (budget) requests review and
	// becomes a medium review target at its lines.
	r = base(&model.FuzzReport{Status: model.FuzzRan, Functions: []model.FuzzFunction{{Path: "a.go", Line: 3, EndLine: 5, Symbol: "a.F", Outcome: model.FuzzInconclusive, Reason: fuzz.ReasonRuntimeBudget, Inputs: 8, NotRecorded: 8}}})
	Finalize(r, true)
	if r.ExitCode != 2 || len(r.ReviewTargets) != 1 || r.ReviewTargets[0].Severity != "medium" || r.ReviewTargets[0].StartLine != 3 || r.ReviewTargets[0].EndLine != 5 {
		t.Fatalf("exit %d, targets %+v", r.ExitCode, r.ReviewTargets)
	}
	// A diverged or not_diverged function without evidence is downgraded.
	r = base(&model.FuzzReport{Status: model.FuzzRan, Functions: []model.FuzzFunction{{Path: "a.go", Line: 3, EndLine: 5, Symbol: "a.F", Outcome: model.FuzzNotDiverged, Inputs: 8, Compared: 8}}})
	Finalize(r, true)
	if f := r.Fuzz.Functions[0]; f.Outcome != model.FuzzInconclusive || f.Reason != fuzzRevalidationText || r.ExitCode != 2 {
		t.Fatalf("function %+v, exit %d", f, r.ExitCode)
	}
}

// BenchmarkFinalizeFuzzLargestPackage measures Finalize on the largest package
// run a policy allows: 16 functions with 64 inputs each (1,024 records per
// stream), each diverging on one input, so the four streams of the package
// are parsed and every function is derived again.
func BenchmarkFinalizeFuzzLargestPackage(b *testing.B) {
	var base, candidate strings.Builder
	base.WriteString("package calc\n")
	candidate.WriteString("package calc\n")
	for i := 0; i < 16; i++ {
		fmt.Fprintf(&base, "\nfunc F%d(n int) int {\n\treturn n + %d\n}\n", i, i)
		fmt.Fprintf(&candidate, "\nfunc F%d(n int) int {\n\treturn %d + n\n}\n", i, i)
	}
	limits := fuzz.Limits{MaxFunctions: 16, MaxPackages: 1, MaxInputs: 64, CallTimeout: time.Second, MaxRuntime: time.Hour}
	value := func(side string, _ bool, test fuzz.HarnessTest, i int) string {
		if side == "candidate" && i == 3 {
			return "w:" + test.Inputs[i].Call
		}
		return "v:" + test.Inputs[i].Call
	}
	r := fuzzReportOf(b, base.String(), candidate.String(), limits, 16, value)
	Finalize(r, true)
	if len(r.Divergences) != 16 {
		b.Fatalf("%d divergences", len(r.Divergences))
	}
	saved, err := json.Marshal(r)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var decoded model.Report
		if err := json.Unmarshal(saved, &decoded); err != nil {
			b.Fatal(err)
		}
		Finalize(&decoded, true)
	}
	b.ReportMetric(float64(len(saved)), "report-bytes")
}
