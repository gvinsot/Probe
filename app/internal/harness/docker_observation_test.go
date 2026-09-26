package harness

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/observe"
)

// observationFixture builds a harness on two snapshots that differ only in
// the files of candidate, and records the name of every container it launches.
// It returns the harness, the container names and a check that the source
// snapshots were not modified.
func observationFixture(t *testing.T, image string, files, candidate map[string]string, generated []string) (*Harness, *[]string, func()) {
	t.Helper()
	base, head := t.TempDir(), t.TempDir()
	write := func(dir string, set map[string]string) {
		for path, content := range set {
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(base, files)
	write(head, files)
	write(head, candidate)
	h, err := New(Options{BaseDir: base, CandidateDir: head, ArtifactDir: t.TempDir(), Image: image, Timeout: 3 * time.Minute, MaxRuntime: 20 * time.Minute, MaxOutputBytes: 64 * 1024, MaxGeneratedTests: 10, Commands: map[string][]string{"generated_test": generated}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	var names []string
	execute, capture := h.execute, h.executeCapture
	h.execute = func(ctx context.Context, name string, args []string, out io.Writer) execution {
		names = append(names, name)
		return execute(ctx, name, args, out)
	}
	h.executeCapture = func(ctx context.Context, name string, args []string, log, payload io.Writer) execution {
		names = append(names, name)
		return capture(ctx, name, args, log, payload)
	}
	unchanged := func() {
		t.Helper()
		for dir, want := range map[string]map[string]string{base: files, head: merge(files, candidate)} {
			count := 0
			err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				count++
				rel, _ := filepath.Rel(dir, path)
				got, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if string(got) != want[filepath.ToSlash(rel)] {
					t.Errorf("snapshot file %s changed or appeared", rel)
				}
				return nil
			})
			if err != nil || count != len(want) {
				t.Errorf("snapshot %s: %d files, want %d (%v)", dir, count, len(want), err)
			}
		}
	}
	return h, &names, unchanged
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func runObservation(t *testing.T, h *Harness, path, content string) map[string]any {
	t.Helper()
	call(t, h, "create_test", map[string]any{"path": path, "content": content, "description": "Discount values per input"})
	var result map[string]any
	if err := json.Unmarshal(call(t, h, "run_generated_test", map[string]any{"test_id": "generated-test-1"}), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func observationRows(t *testing.T, h *Harness, e model.Evidence) observe.Outcome {
	t.Helper()
	checks := map[string]model.Check{}
	for _, c := range h.Checks() {
		checks[c.ID] = c
	}
	var repeat *model.Check
	if e.RepeatCheckID != "" {
		c := checks[e.RepeatCheckID]
		repeat = &c
	}
	o, known := EvaluateObservations(e.Runner, checks[e.BaseCheckID], checks[e.CheckID], repeat, e.Path, e.TestNames)
	if !known || o.Status != e.Status {
		t.Fatalf("re-derived %s, stored %s", o.Status, e.Status)
	}
	return o
}

const (
	goDiscountBase      = "package price\n\n// Discount returns total reduced by percent, rounding the reduction down.\nfunc Discount(total, percent int) int { return total - total*percent/100 }\n"
	goDiscountCandidate = "package price\n\n// Discount returns total reduced by percent.\nfunc Discount(total, percent int) int { return total * (100 - percent) / 100 }\n"
	goObservationTest   = `package price

import (
	"fmt"
	"testing"
)

func TestSwiftProofDiscountObservations(t *testing.T) {
	for _, in := range [][2]int{{100, 10}, {5, 33}, {0, 50}} {
		t.Attr(fmt.Sprintf("swiftproof.Discount(%d,%d)", in[0], in[1]), fmt.Sprintf("%#v", Discount(in[0], in[1])))
	}
}
`
)

// A real golang container: the rewritten Discount records a different value
// for Discount(5,33) only. Output printed by the code under test cannot add a
// key; a framed line it prints duplicates a key, which is then never compared.
func TestDockerGoObservationIntegration(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded golang Linux image (Go 1.25 or later)")
	}
	files := map[string]string{"go.mod": "module example.test/price\n\ngo 1.23.0\n", "price/price.go": goDiscountBase}
	for _, tc := range []struct {
		name, candidate string
		status          string
		checks          int
	}{
		{"rewritten discount", goDiscountCandidate, model.StatusDiverged, 3},
		{"plain ATTR output from candidate code", "package price\n\nimport \"fmt\"\n\nfunc Discount(total, percent int) int {\n\tfmt.Println(\"=== ATTR  TestSwiftProofDiscountObservations swiftproof.Injected 1\")\n\treturn total * (100 - percent) / 100\n}\n", model.StatusDiverged, 3},
		{"framed ATTR line from candidate code", "package price\n\nimport \"fmt\"\n\nfunc Discount(total, percent int) int {\n\tfmt.Print(\"\\x16=== ATTR  TestSwiftProofDiscountObservations swiftproof.Discount(5,33) 4\\n\")\n\treturn total * (100 - percent) / 100\n}\n", model.StatusUnverified, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, names, unchanged := observationFixture(t, image, files, map[string]string{"price/price.go": tc.candidate}, []string{"go", "test", "{package}"})
			result := runObservation(t, h, "price/swiftproof_observe_test.go", goObservationTest)
			checks := h.Checks()
			if len(checks) != tc.checks {
				t.Fatalf("%d checks, want %d: %+v", len(checks), tc.checks, checks)
			}
			for _, c := range checks {
				if c.Status != "PASS" {
					t.Fatalf("check %s (%s) is %s:\n%s", c.ID, c.Kind, c.Status, c.Output)
				}
			}
			if tc.checks == 3 && checks[2].Kind != model.CheckGeneratedBaseRepeat {
				t.Fatalf("third check %s", checks[2].Kind)
			}
			obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
			if len(obs) != 1 || obs[0].Status != tc.status || result["observation"] == nil {
				t.Fatalf("observation %+v", obs)
			}
			o := observationRows(t, h, obs[0])
			keys := map[string]model.Observation{}
			for _, row := range o.Observations {
				keys[row.Key] = row
			}
			if _, injected := keys["Injected"]; injected || len(keys) != 3 {
				t.Fatalf("keys %v", keys)
			}
			five := keys["Discount(5,33)"]
			switch tc.status {
			case model.StatusDiverged:
				if d := o.Diverged(); len(d) != 1 || d[0].Key != "Discount(5,33)" || d[0].Base != "4" || d[0].Candidate != "3" {
					t.Fatalf("diverged rows %+v", o.Observations)
				}
				if keys["Discount(100,10)"].Status != model.ObservationEqual || keys["Discount(0,50)"].Status != model.ObservationEqual {
					t.Fatalf("rows %+v", o.Observations)
				}
				if _, ok := artifactByKind(h, model.ArtifactGeneratedTest); !ok {
					t.Fatal("the diverging test was not retained")
				}
			default:
				if five.Status != model.ObservationIncomparable || len(o.Diverged()) != 0 {
					t.Fatalf("a forged framing line did not make the key incomparable: %+v", o.Observations)
				}
			}
			unchanged()
			assertNoContainers(t, *names)
		})
	}
}

const (
	tsCartBase      = "export function discount(total: number, percent: number): number {\n  return total - Math.floor((total * percent) / 100);\n}\n"
	tsCartCandidate = "export function discount(total: number, percent: number): number {\n  return Math.floor((total * (100 - percent)) / 100);\n}\n"
	tsObservation   = `import { test } from "vitest";
import { discount } from "./cart";

test("observe discount", ({ task }) => {
  (task.meta as any).swiftproof = {
    "discount(5,33)": discount(5, 33),
    "discount(100,10)": discount(100, 10),
    "shape": { total: discount(0, 50), inputs: [0, 50] },
  };
});
`
	jsCartBase      = "exports.discount = (total, percent) => total - Math.floor((total * percent) / 100);\n"
	jsCartCandidate = "exports.discount = (total, percent) => Math.floor((total * (100 - percent)) / 100);\n"
	jsObservation   = `const { discount } = require("./cart");

// Jest reports carry no per-test metadata, so this swiftproof meta record is lost.
test("observe discount", () => {
  globalThis.meta = { swiftproof: { "discount(5,33)": discount(5, 33) } };
});
`
)

// A real Vitest run (image SWIFTPROOF_TEST_TS_IMAGE): task.meta.swiftproof
// reaches the Jest-compatible report on the results channel and diverges for
// discount(5,33) only. The same experiment through a Jest template records an
// UNVERIFIED observation, because Jest reports carry no per-test metadata.
func TestDockerTSObservationIntegration(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_TS_IMAGE to a preloaded image with vitest and jest on PATH (for example swiftproof-ts-test:local)")
	}
	t.Run("vitest", func(t *testing.T) {
		h, names, unchanged := observationFixture(t, image, map[string]string{"src/cart.ts": tsCartBase}, map[string]string{"src/cart.ts": tsCartCandidate}, []string{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"})
		runObservation(t, h, "src/obs.test.ts", tsObservation)
		checks := h.Checks()
		if len(checks) != 3 || checks[2].Kind != model.CheckGeneratedBaseRepeat {
			t.Fatalf("checks %+v", checks)
		}
		for _, c := range checks {
			if c.Status != "PASS" || c.Results == "" {
				t.Fatalf("check %s (%s) is %s:\n%s", c.ID, c.Kind, c.Status, c.Output)
			}
		}
		obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
		if len(obs) != 1 || obs[0].Status != model.StatusDiverged || obs[0].Runner != RunnerJest {
			t.Fatalf("observation %+v", obs)
		}
		o := observationRows(t, h, obs[0])
		if d := o.Diverged(); len(d) != 1 || d[0].Key != "discount(5,33)" || d[0].Base != "4" || d[0].Candidate != "3" || d[0].Test != "observe discount" {
			t.Fatalf("rows %+v", o.Observations)
		}
		for _, row := range o.Observations {
			if row.Key == "shape" && (row.Status != model.ObservationEqual || row.Base != `{"inputs":[0,50],"total":0}`) {
				t.Fatalf("non-string values are not canonical JSON: %+v", row)
			}
		}
		unchanged()
		assertNoContainers(t, *names)
	})
	t.Run("jest", func(t *testing.T) {
		// Jest needs a package.json (or a config file) to find its root.
		files := map[string]string{"package.json": "{\"name\": \"observation-fixture\", \"private\": true}\n", "src/cart.js": jsCartBase}
		h, names, unchanged := observationFixture(t, image, files, map[string]string{"src/cart.js": jsCartCandidate}, []string{"jest", "{file}", "--json", "--outputFile={results_out}"})
		runObservation(t, h, "src/obs.test.js", jsObservation)
		checks := h.Checks()
		if len(checks) != 2 {
			t.Fatalf("checks %+v", checks)
		}
		for _, c := range checks {
			if c.Status != "PASS" {
				t.Fatalf("check %s (%s) is %s:\n%s", c.ID, c.Kind, c.Status, c.Output)
			}
		}
		obs := evidenceOfKind(h, model.EvidenceDifferentialObservation)
		if len(obs) != 1 || obs[0].Status != model.StatusUnverified || !strings.Contains(obs[0].Description, reasonDeclaredNotFound) {
			t.Fatalf("observation %+v", obs)
		}
		unchanged()
		assertNoContainers(t, *names)
	})
}
