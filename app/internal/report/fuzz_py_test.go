package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/fuzz"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

const pyPriceBase = "def price(n: int) -> int:\n    return n\n\n\ndef tax(n: int) -> int:\n    return n * 2\n"
const pyPriceCandidate = "def price(n: int) -> int:\n    return n - 1 if n > 100 else n\n\n\ndef tax(n: int) -> int:\n    return n + n\n"

// fakePytestRunner writes the framed stream a Python harness writes and
// records pytest checks and evidence like the harness does.
type fakePytestRunner struct {
	fakeFuzzRunner
}

func (f *fakePytestRunner) Observe(_ context.Context, req fuzz.Request) (fuzz.Side, fuzz.Side, error) {
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	return f.pytestSide("base", baseKind, req), f.pytestSide("candidate", candidateKind, req), nil
}

func (f *fakePytestRunner) pytestSide(name, kind string, req fuzz.Request) fuzz.Side {
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
		Command: []string{"python", "-m", "pytest", "-p", "no:cacheprovider", h.Path, "--junitxml=" + harness.ResultsPath},
		Output:  "pytest log", Results: results,
	}
	f.checks = append(f.checks, c)
	return fuzz.Side{Check: c}
}

// pythonFuzzReport runs shop/price.py (price and tax) through the real
// Python selection, rendering, normalization and comparison with a fake
// sandbox.
func pythonFuzzReport(t *testing.T, value fuzzValue) *model.Report {
	t.Helper()
	base, candidate := t.TempDir(), t.TempDir()
	for dir, src := range map[string]string{base: pyPriceBase, candidate: pyPriceCandidate} {
		if err := os.MkdirAll(filepath.Join(dir, "shop"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "shop", "price.py"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	limits := fuzz.Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeout: time.Second, MaxRuntime: time.Hour}
	change := model.Change{BaseRef: "main", BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40), Files: []model.ChangedFile{{Path: "shop/price.py", Status: "M"}}}
	plan, err := fuzz.SelectAll(context.Background(), base, candidate, change, nil, limits, fuzz.FamilyPytest)
	if err != nil || plan.Targets() != 2 {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	runner := &fakePytestRunner{fakeFuzzRunner{t: t, value: value}}
	rep := fuzz.Run(context.Background(), runner, plan, fuzz.Options{
		Limits: limits, ObservationsPath: harness.FuzzObservationsPath, PayloadLimit: harness.PayloadLimit(65536),
		NewSuffix: func() (string, error) { return "abcdef1234567890", nil },
	})
	return &model.Report{
		Version: 1, ToolVersion: "fuzz-test", GeneratedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		Change: change, Checks: runner.checks, Evidence: runner.evidence, Fuzz: &rep,
	}
}

func TestPythonFuzzDivergenceIsVerified(t *testing.T) {
	r := pythonFuzzReport(t, divergeAt("price(1000)", "999"))
	Finalize(r, true)
	price := fuzzFunction(t, r, "price")
	if price.Outcome != model.FuzzDiverged || price.Counterexample.Input != "price(1000)" || fuzzFunction(t, r, "tax").Outcome != model.FuzzNotDiverged {
		t.Fatalf("functions %+v", r.Fuzz.Functions)
	}
	if r.ExitCode != 2 || len(r.Divergences) != 1 || r.Divergences[0].TestPath != "shop/test_probe_fuzz_abcdef1234567890.py" {
		t.Fatalf("exit %d, divergences %+v", r.ExitCode, r.Divergences)
	}
	for _, e := range r.Evidence {
		if e.Runner != harness.RunnerPytest {
			t.Fatalf("evidence %+v", e)
		}
	}
	md := string(Markdown(r))
	for _, want := range []string{"were planned for 2 changed Python functions", "- **diverged** price (shop/price.py:1)", inline(fuzzPythonLexicalNote)} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q", want)
		}
	}
	if strings.Contains(md, inline(fuzzLexicalNote)) {
		t.Error("a Python-only section carries the TS/JS note")
	}
}

func TestPythonFuzzTamperingDowngrades(t *testing.T) {
	edits := map[string]func(r *model.Report){
		"jest runner": func(r *model.Report) {
			for i := range r.Evidence {
				r.Evidence[i].Runner = harness.RunnerJest
			}
		},
		"command without the harness": func(r *model.Report) {
			r.Checks[0].Command = []string{"python", "-m", "pytest", "shop/test_other.py", "--junitxml=" + harness.ResultsPath}
		},
		"deselecting command": func(r *model.Report) {
			r.Checks[0].Command = append(r.Checks[0].Command, "-k", "nothing")
		},
		"jest stream": func(r *model.Report) {
			r.Checks[1].Results = strings.Replace(r.Checks[1].Results, `"pytest_junit"`, `"jest_json"`, 1)
		},
		"failing candidate": func(r *model.Report) {
			r.Checks[1].Status, r.Checks[1].ExitCode = "FAIL", 1
		},
	}
	for name, edit := range edits {
		r := pythonFuzzReport(t, divergeAt("price(1000)", "999"))
		edit(r)
		Finalize(r, true)
		price := fuzzFunction(t, r, "price")
		if price.Outcome != model.FuzzInconclusive || price.Reason != fuzzRevalidationText || len(r.Divergences) != 0 {
			t.Errorf("%s: price %+v, divergences %d", name, price, len(r.Divergences))
		}
	}
}
