package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/fuzz"
	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const tsPriceBase = "export function price(n: number): number {\n  return n;\n}\n\nexport function tax(n: number): number {\n  return n * 2;\n}\n"
const tsPriceCandidate = "export function price(n: number): number {\n  return n > 100 ? n - 1 : n;\n}\n\nexport function tax(n: number): number {\n  return n + n;\n}\n"

// fakeScriptRunner writes the framed stream a TS/JS harness writes (head and
// done records around each test) and records Vitest checks and evidence like
// the harness does.
type fakeScriptRunner struct {
	fakeFuzzRunner
}

func (f *fakeScriptRunner) Observe(_ context.Context, req fuzz.Request) (fuzz.Side, fuzz.Side, error) {
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	return f.scriptSide("base", baseKind, req), f.scriptSide("candidate", candidateKind, req), nil
}

func (f *fakeScriptRunner) scriptSide(name, kind string, req fuzz.Request) fuzz.Side {
	h := req.Harness
	var stream strings.Builder
	for n, test := range h.Tests {
		fmt.Fprintf(&stream, `{"f":%d,"k":"head","s":%s,"w":%s}`+"\n", n+1, fuzzQuote(h.Suffix), fuzzQuote(test.Name))
		fmt.Fprintf(&stream, `{"f":%d,"k":"begin","n":%d}`+"\n", n+1, len(test.Inputs))
		for i := range test.Inputs {
			enc := f.value(name, req.Confirm, test, i)
			sum := sha256.Sum256([]byte(enc))
			fmt.Fprintf(&stream, `{"f":%d,"k":"obs","i":%d,"h":"%s","l":%d,"o":%s,"p":false,"d":false,"t":false}`+"\n", n+1, i, hex.EncodeToString(sum[:]), len(enc), fuzzQuote(enc))
		}
		fmt.Fprintf(&stream, `{"f":%d,"k":"end","n":%d}`+"\n", n+1, len(test.Inputs))
		fmt.Fprintf(&stream, `{"f":%d,"k":"done","w":%s,"n":%d}`+"\n", n+1, fuzzQuote(test.Name), len(test.Inputs))
	}
	results, err := h.Normalize([]byte(stream.String()))
	if err != nil {
		f.t.Fatalf("fake stream rejected: %v", err)
	}
	c := model.Check{
		ID: fmt.Sprintf("check-%d", len(f.checks)+1), Kind: kind, Status: "PASS",
		Command: []string{"vitest", "run", h.Path, "--reporter=json", "--outputFile=" + harness.ResultsPath},
		Output:  "vitest log", Results: results,
	}
	f.checks = append(f.checks, c)
	return fuzz.Side{Check: c}
}

// scriptFuzzReport runs web/price.ts (price and tax) through the real TS/JS
// selection, rendering, normalization and comparison with a fake sandbox.
func scriptFuzzReport(t *testing.T, value fuzzValue) *model.Report {
	t.Helper()
	base, candidate := t.TempDir(), t.TempDir()
	for dir, src := range map[string]string{base: tsPriceBase, candidate: tsPriceCandidate} {
		if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "web", "price.ts"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	limits := fuzz.Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeout: time.Second, MaxRuntime: time.Hour}
	change := model.Change{BaseRef: "main", BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40), Files: []model.ChangedFile{{Path: "web/price.ts", Status: "M"}}}
	plan, err := fuzz.SelectAll(context.Background(), base, candidate, change, nil, limits, fuzz.FamilyVitest)
	if err != nil || plan.Targets() != 2 {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	runner := &fakeScriptRunner{fakeFuzzRunner{t: t, value: value}}
	rep := fuzz.Run(context.Background(), runner, plan, fuzz.Options{
		Limits: limits, ObservationsPath: harness.FuzzObservationsPath, PayloadLimit: harness.PayloadLimit(65536), ScriptFamily: fuzz.FamilyVitest,
		NewSuffix: func() (string, error) { return "abcdef1234567890", nil },
	})
	return &model.Report{
		Version: 1, ToolVersion: "fuzz-test", GeneratedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Change: change, Checks: runner.checks, Evidence: runner.evidence, Fuzz: &rep,
	}
}

// A TS/JS divergence is verified from its jest_json record, listed with its
// changed-function anchor and the four checks, requests review (exit 2 with
// --ci), never exit 1, and survives a round trip unchanged.
func TestScriptFuzzDivergenceIsVerified(t *testing.T) {
	r := scriptFuzzReport(t, divergeAt("price(1000)", "999"))
	Finalize(r, true)
	price := fuzzFunction(t, r, "price")
	if price.Outcome != model.FuzzDiverged || price.Counterexample.Input != "price(1000)" || fuzzFunction(t, r, "tax").Outcome != model.FuzzNotDiverged {
		t.Fatalf("functions %+v", r.Fuzz.Functions)
	}
	if r.ExitCode != 2 || len(r.ReproducedIssues) != 0 || len(r.Divergences) != 1 {
		t.Fatalf("exit %d, divergences %+v", r.ExitCode, r.Divergences)
	}
	d := r.Divergences[0]
	if d.Kind != model.EvidenceDifferentialFuzz || d.Path != "web/price.ts" || d.Line != 1 || d.Symbol != "price" || d.AnchorSource != "changed_function" ||
		d.TestPath != "web/swiftproof-fuzz-abcdef1234567890.test.ts" || len(d.CheckIDs) != 4 {
		t.Fatalf("divergence %+v", d)
	}
	for _, e := range r.Evidence {
		if e.Runner != harness.RunnerJest {
			t.Fatalf("evidence %+v", e)
		}
	}
	md := string(Markdown(r))
	for _, want := range []string{
		"Seeded inputs (swiftproof-fuzz/v1) were planned for 2 changed TS/JS functions, 2 of them with recorded fuzz checks",
		"- **diverged** price (web/price.ts:1)",
		"- **not diverged** tax (web/price.ts:5)",
		inline(fuzzLexicalNote),
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	if m := regexp.MustCompile(`(?i)\b(tested|verified|safe|correct|approved|regression|bug|masked|contradicts|complete|score|equivalen\w*)\b|%`).FindString(fuzzLexicalNote); m != "" {
		t.Fatalf("the lexical note contains %q", m)
	}
	// A section without a TS/JS function or skip does not carry the note.
	goOnly := fuzzReport(t, divergeAt(discountCall, "w:"+discountCall))
	Finalize(goOnly, true)
	if strings.Contains(string(Markdown(goOnly)), inline(fuzzLexicalNote)) {
		t.Fatal("a Go-only section carries the TS/JS note")
	}
	first := reportJSON(t, r)
	var again model.Report
	if err := json.Unmarshal([]byte(first), &again); err != nil {
		t.Fatal(err)
	}
	Finalize(&again, true)
	if reportJSON(t, &again) != first {
		t.Fatal("Finalize is not idempotent across a round trip")
	}
}

// Each edit that breaks what establishes a TS/JS record makes the diverged
// function inconclusive on re-render: a record naming the Go runner, a
// command that does not run the harness file, a Go stream in place of the
// TS/JS one, a failing candidate run, and a stream without done records.
func TestScriptFuzzTamperingDowngrades(t *testing.T) {
	edits := map[string]func(r *model.Report){
		"go runner": func(r *model.Report) {
			for i := range r.Evidence {
				r.Evidence[i].Runner = harness.RunnerGo
			}
		},
		"command without the harness": func(r *model.Report) {
			r.Checks[0].Command = []string{"vitest", "run", "web/other.test.ts", "--reporter=json"}
		},
		"go test filter": func(r *model.Report) {
			r.Checks[0].Command = append(r.Checks[0].Command, "-run", "x")
		},
		"go stream": func(r *model.Report) {
			r.Checks[1].Results = strings.Replace(r.Checks[1].Results, "\"runner\": \"jest_json\",\n", "", 1)
		},
		"failing candidate": func(r *model.Report) {
			r.Checks[1].Status, r.Checks[1].ExitCode = "FAIL", 1
		},
		"no done record": func(r *model.Report) {
			s, err := fuzz.ParseResults(r.Checks[1].Results)
			if err != nil {
				t.Fatal(err)
			}
			for i := range s.Functions {
				s.Functions[i].State, s.Functions[i].At = fuzz.StateInterrupted, len(s.Functions[i].Records)
			}
			b, _ := json.MarshalIndent(s, "", "")
			r.Checks[1].Results = string(b)
		},
	}
	for name, edit := range edits {
		r := scriptFuzzReport(t, divergeAt("price(1000)", "999"))
		edit(r)
		Finalize(r, true)
		price := fuzzFunction(t, r, "price")
		if price.Outcome != model.FuzzInconclusive || price.Reason != fuzzRevalidationText || len(r.Divergences) != 0 {
			t.Errorf("%s: price %+v, divergences %d", name, price, len(r.Divergences))
		}
	}
}
