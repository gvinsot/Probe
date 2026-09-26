package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// The design's Discount scenario through the whole CLI with real Docker: the
// tests assert only Discount(200) == 190 and an error for Discount(-5), so the
// four mutants that change a boundary survive and the four others are killed.
// Survivors become medium signals and review targets and never change the
// exit code; killed mutants are only counted; every patch is retained with its
// hash; the mutation ledger stays apart from the checks; the report re-renders
// byte-identically; the checkout stays clean and no container survives.
func TestDockerMutationEndToEnd(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	dir := discountFixture(t)
	policy := mutationPolicy(t, image, nil)
	out := filepath.Join(t.TempDir(), "report")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"review", "--repo", dir, "--config", policy, "--reviewer=false", "--ci", "--out", out}, &stdout, &stderr, "mutation-e2e")
	r := readReport(t, filepath.Join(out, "confidence-report.json"))
	m := r.Mutation
	if code != 0 {
		t.Fatalf("exit %d, want 0: checks %v mutation %+v unverified %q\n%s", code, checkKinds(r), m, r.Unverified, stderr.String())
	}
	if m == nil || m.Status != model.MutationRan || m.Generated != 8 || len(m.Mutants) != 8 || m.Dropped != 0 {
		t.Fatalf("mutation %+v", m)
	}
	if m.Killed != 4 || m.Survived != 4 || m.Invalid+m.TimedOut+m.Inconclusive+m.NotRun != 0 {
		for _, mu := range m.Mutants {
			t.Logf("%s %s %q -> %q: %s %s", mu.ID, mu.Operator, mu.Original, mu.Mutated, mu.Status, mu.Reason)
		}
		t.Fatalf("killed %d survived %d", m.Killed, m.Survived)
	}
	var survivors, killed []string
	for _, mu := range m.Mutants {
		switch mu.Status {
		case model.MutantSurvived:
			survivors = append(survivors, mu.Operator+" "+mu.Original+" -> "+mu.Mutated)
		case model.MutantKilled:
			killed = append(killed, mu.Operator)
		}
	}
	sort.Strings(survivors)
	sort.Strings(killed)
	if want := "boundary < -> <=,boundary >= -> >,increment_constant 0 -> (0+1),increment_constant 100 -> (100+1)"; strings.Join(survivors, ",") != want {
		t.Fatalf("survivors %q, want %q", survivors, want)
	}
	if want := "drop_error,negate_condition,negate_condition,swap_arithmetic"; strings.Join(killed, ",") != want {
		t.Fatalf("killed %q, want %q", killed, want)
	}
	// The mutation ledger: one control for ./price, then the eight mutants.
	if len(m.Checks) != 9 {
		t.Fatalf("%d mutation checks, want 9", len(m.Checks))
	}
	for i, c := range m.Checks {
		if c.ID != fmt.Sprintf("mutation-check-%d", i+1) || strings.Join(c.Command, " ") != "go test -json -count=1 -failfast ./price" {
			t.Fatalf("mutation check %d: %s %q", i, c.ID, c.Command)
		}
	}
	for _, c := range r.Checks {
		if c.Status != "PASS" || strings.HasPrefix(c.ID, "mutation-check-") {
			t.Fatalf("main ledger check %s %s %s", c.ID, c.Kind, c.Status)
		}
	}
	// Each survivor's patch is retained, and its recorded hash is the file's.
	artifacts := map[string]string{}
	for _, a := range r.Artifacts {
		if a.Kind == model.ArtifactMutantPatch {
			b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(a.Path)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != a.SHA256 {
				t.Fatalf("patch %s does not match its hash", a.Path)
			}
			artifacts[a.SHA256] = string(b)
		}
	}
	for _, mu := range m.Mutants {
		if mu.Status == model.MutantSurvived {
			patch, ok := artifacts[mu.PatchSHA256]
			if !ok || !strings.Contains(patch, "+++ b/price/price.go") || mu.TestsRun != 2 {
				t.Fatalf("survivor %+v has no matching patch", mu)
			}
		}
	}
	if len(artifacts) != 4 {
		t.Fatalf("%d patch artifacts, want 4", len(artifacts))
	}
	// Survivors are medium signals on the two if lines and reach the review
	// targets; mutation adds no evidence and no hypothesis.
	signals := signalsOfKind(r, model.SignalSurvivingMutant)
	if len(signals) != 4 {
		t.Fatalf("%d surviving_mutant signals, want 4", len(signals))
	}
	for _, s := range signals {
		if s.Severity != "medium" || s.Path != "price/price.go" || (s.Line != 15 && s.Line != 18) || s.Symbol != "Discount" {
			t.Fatalf("signal %+v", s)
		}
		target := false
		for _, rt := range r.ReviewTargets {
			target = target || rt.Path == s.Path && rt.StartLine <= s.Line && s.Line <= rt.EndLine
		}
		if !target {
			t.Fatalf("signal on line %d reached no review target", s.Line)
		}
	}
	if len(r.Evidence) != 0 || len(r.Hypotheses) != 0 || len(r.Unverified) != 0 {
		t.Fatalf("evidence %d hypotheses %d unverified %q", len(r.Evidence), len(r.Hypotheses), r.Unverified)
	}
	skipped := false
	for _, f := range m.Files {
		skipped = skipped || f.Path == "price/fast_windows.go" && f.Status == model.MutationFileSkipped
	}
	if !skipped {
		t.Fatalf("the Windows-only file was not skipped: %+v", m.Files)
	}
	md, err := os.ReadFile(filepath.Join(out, "CONFIDENCE_REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(md)
	if !strings.Contains(body, "## Mutation of Added Lines") || strings.Contains(body, "%") {
		t.Fatal("the Markdown lacks the mutation section or contains a percentage")
	}
	if !strings.Contains(stdout.String(), "Mutation of added lines (ran): 8 mutants selected of 8 generated; 4 killed, 4 survived") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	// Re-rendering the saved report is byte-identical.
	rendered := filepath.Join(t.TempDir(), "rendered")
	var renderOut, renderErr bytes.Buffer
	if code := Run(context.Background(), []string{"report", "--input", filepath.Join(out, "confidence-report.json"), "--out", rendered}, &renderOut, &renderErr, "mutation-e2e"); code != 0 {
		t.Fatalf("render exit %d: %s", code, renderErr.String())
	}
	for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
		a, _ := os.ReadFile(filepath.Join(out, name))
		b, _ := os.ReadFile(filepath.Join(rendered, name))
		if len(a) == 0 || string(a) != string(b) {
			t.Fatalf("%s changed on re-render", name)
		}
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("the checkout changed:\n%s", status)
	}
}
