package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// discountSource is the candidate price.go of the mutation fixture: Clamp is
// baseline code, Discount is added one statement per line. ErrNegative keeps
// the errors import used outside Discount, so dropping the error compiles.
const discountSource = `package price

import "errors"

// Clamp returns n, or 0 when n is negative.
func Clamp(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Discount takes 10 off totals of at least 100 and rejects negative totals.
func Discount(total int) (int, error) {
	if total < 0 {
		return 0, ErrNegative
	}
	if total >= 100 {
		return total - 10, nil
	}
	return total, nil
}

// ErrNegative is returned for a negative total.
var ErrNegative = errors.New("negative total")
`

// discountFixture is a Go repository whose candidate adds Discount to an
// existing package, a test that asserts only Discount(200) == 190 and an error
// for Discount(-5), and a file restricted to Windows by its name.
func discountFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "go.mod", "module example.test/shop\n\ngo 1.23.0\n")
	write(t, dir, "price/price.go", "package price\n\n// Clamp returns n, or 0 when n is negative.\nfunc Clamp(n int) int {\n\tif n < 0 {\n\t\treturn 0\n\t}\n\treturn n\n}\n")
	write(t, dir, "price/price_test.go", "package price\n\nimport \"testing\"\n\nfunc TestClamp(t *testing.T) {\n\tif Clamp(-1) != 0 || Clamp(5) != 5 {\n\t\tt.Fatal(\"clamp\")\n\t}\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "baseline")
	git(t, dir, "checkout", "-b", "feature")
	write(t, dir, "price/price.go", discountSource)
	write(t, dir, "price/discount_test.go", "package price\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tif got, err := Discount(200); err != nil || got != 190 {\n\t\tt.Fatalf(\"Discount(200) = %d, %v\", got, err)\n\t}\n\tif _, err := Discount(-5); err == nil {\n\t\tt.Fatal(\"Discount(-5) returned no error\")\n\t}\n}\n")
	write(t, dir, "price/fast_windows.go", "package price\n\nfunc fast(n int) bool { return n > 1 }\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "discount")
	return dir
}

// mutationPolicy writes the default Go policy with image and the documented
// mutation object, applies edit, and returns the path of the file, which lies
// outside the fixture repository.
func mutationPolicy(t *testing.T, image string, edit func(*config.Config)) string {
	t.Helper()
	cfg := config.Default("go")
	cfg.Sandbox.Image = image
	cfg.Sandbox.MaxRuntimeSeconds = 900
	cfg.Mutation = &config.Mutation{Command: []string{"go", "test", "-json", "-count=1", "-failfast", "{package}"}, MaxMutants: 10, TimeoutSeconds: 60, MaxRuntimeSeconds: 400}
	if edit != nil {
		edit(&cfg)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signalsOfKind returns the signals of one kind.
func signalsOfKind(r model.Report, kind string) []model.Signal {
	var out []model.Signal
	for _, s := range r.Signals {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// Without a usable sandbox nothing is concluded: the control run records an
// infrastructure ERROR in the mutation ledger, the section is not_run with one
// Unverified sentence, no mutant is KILLED or SURVIVED, no surviving_mutant
// signal is added, and the run is an operational failure.
func TestMutationWithoutSandboxClaimsNothing(t *testing.T) {
	dir := discountFixture(t)
	calls := countExecution(t)
	policy := mutationPolicy(t, absentImage, nil)
	code, r, _, output := runReport(t, context.Background(), dir, "review", "--config", policy, "--reviewer=false", "--ci")
	if code != 4 {
		t.Fatalf("exit %d, want 4 (the sandbox image does not exist)", code)
	}
	if *calls != 1 {
		t.Fatalf("execution hook called %d times, want once (the harness)", *calls)
	}
	m := r.Mutation
	if m == nil || m.Status != model.MutationNotRun || !strings.Contains(m.Reason, "the unmutated control run mutation-check-1 did not pass (status ERROR") {
		t.Fatalf("mutation %+v, want not_run after the control's infrastructure error", m)
	}
	if m.Generated != 8 || len(m.Mutants) != 8 || m.NotRun != 8 || m.Killed+m.Survived != 0 {
		t.Fatalf("mutants %+v", m)
	}
	if len(m.Checks) != 1 || m.Checks[0].ID != "mutation-check-1" || m.Checks[0].Kind != model.CheckMutationControl || m.Checks[0].Status != "ERROR" {
		t.Fatalf("mutation ledger %+v", m.Checks)
	}
	if got := strings.Join(m.Checks[0].Command, " "); got != "go test -json -count=1 -failfast ./price" {
		t.Fatalf("control argv %q", got)
	}
	for _, c := range r.Checks {
		if c.Kind == model.CheckMutationControl || c.Kind == model.CheckMutant || strings.HasPrefix(c.ID, "mutation-check-") {
			t.Fatalf("a mutation run reached the main ledger: %+v", c)
		}
	}
	if n := len(signalsOfKind(r, model.SignalSurvivingMutant)); n != 0 {
		t.Fatalf("%d surviving_mutant signals without a run", n)
	}
	sentences := 0
	for _, u := range r.Unverified {
		if strings.HasPrefix(u, "Mutation analysis") {
			sentences++
			if !strings.HasPrefix(u, "Mutation analysis did not run: ") {
				t.Fatalf("unverified %q", u)
			}
		}
	}
	if sentences != 1 {
		t.Fatalf("%d mutation sentences in %q, want 1", sentences, r.Unverified)
	}
	if !strings.Contains(output, "Mutation of added lines did not run: the unmutated control run mutation-check-1 did not pass") {
		t.Fatalf("stdout lacks the mutation line:\n%s", output)
	}
	files := map[string]string{}
	for _, f := range m.Files {
		files[f.Path] = f.Status
	}
	if files["price/price.go"] != model.MutationFileEligible || files["price/fast_windows.go"] != model.MutationFileSkipped {
		t.Fatalf("files %+v", m.Files)
	}
}

// Only the trusted policy can enable mutation: a mutation object the candidate
// adds to .swiftproof.json is ignored.
func TestMutationPolicyOnCandidateIsIgnored(t *testing.T) {
	dir := discountFixture(t)
	base := config.Default("go")
	base.Sandbox.Image = absentImage
	b, _ := json.Marshal(base)
	git(t, dir, "checkout", "main")
	write(t, dir, config.Filename, string(b))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "policy")
	git(t, dir, "checkout", "feature")
	git(t, dir, "merge", "--no-edit", "main")
	candidate := base
	candidate.Mutation = &config.Mutation{Command: []string{"go", "test", "-json", "{package}"}, MaxMutants: 5, TimeoutSeconds: 30, MaxRuntimeSeconds: 60}
	b, _ = json.Marshal(candidate)
	write(t, dir, config.Filename, string(b))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "enable mutation on the candidate")
	forbidExecution(t)
	_, r, members, _ := runReport(t, context.Background(), dir, "review", "--checks=false", "--reviewer=false")
	if _, ok := members["mutation"]; ok || r.Mutation != nil {
		t.Fatalf("a candidate-side mutation policy was applied: %+v", r.Mutation)
	}
}

// The stdout line reports counts only: killed mutants are counted, never
// listed, and no percentage or score appears.
func TestMutationLine(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(tested|verified|covered|safe|correct|score|complete)\b|%`)
	for _, tc := range []struct {
		m    *model.Mutation
		want string
	}{
		{nil, ""},
		{&model.Mutation{Status: model.MutationNotRun, Reason: "initial checks disabled (--checks=false)"}, "Mutation of added lines did not run: initial checks disabled (--checks=false)."},
		{&model.Mutation{Status: model.MutationNoCandidates, Reason: "no changed files"}, "Mutation of added lines: no mutant was run (no changed files)."},
		{&model.Mutation{Status: model.MutationRan, Generated: 8, Mutants: make([]model.Mutant, 8), Killed: 4, Survived: 4}, "Mutation of added lines (ran): 8 mutants selected of 8 generated; 4 killed, 4 survived, 0 did not build or pass go vet, 0 timed out, 0 inconclusive, 0 not run."},
		{&model.Mutation{Status: model.MutationIncomplete, Generated: 8, Mutants: make([]model.Mutant, 3), Killed: 1, Survived: 1, NotRun: 1}, "Mutation of added lines (incomplete): 3 mutants selected of 8 generated; 1 killed, 1 survived, 0 did not build or pass go vet, 0 timed out, 0 inconclusive, 1 not run."},
		{&model.Mutation{Status: model.MutationNotRun}, "Mutation of added lines did not run: no reason was recorded."},
	} {
		got := mutationLine(tc.m)
		if got != tc.want {
			t.Errorf("mutationLine = %q, want %q", got, tc.want)
		}
		if forbidden.MatchString(got) {
			t.Errorf("forbidden wording in %q", got)
		}
	}
}
