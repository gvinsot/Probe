package harness

// Changed-line execution of TypeScript sources with real Docker: a Vitest and
// a Jest coverage command write lcov.info into {coverage_dir}, the wrapper
// returns it on the payload channel, and coverage.Analyze resolves the diff
// against it. Gated on PROBE_TEST_TS_IMAGE (a preloaded image with node,
// vitest, @vitest/coverage-v8, jest and ts-jest, for example
// probe-ts-test:local).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/coverage"
	"github.com/gvinsot/Probe/app/internal/model"
)

const tsCoveragePrice = `export function price(total: number, percent: number): number {
  if (percent > 100) {
    throw new Error("too much");
  }
  return total - Math.floor((total * percent) / 100);
}

export function unused(): number {
  return 1;
}
`

func TestDockerTSCoverageLCOV(t *testing.T) {
	image := os.Getenv("PROBE_TEST_TS_IMAGE")
	if image == "" {
		t.Skip("set PROBE_TEST_TS_IMAGE to a preloaded image with node, vitest, @vitest/coverage-v8, jest and ts-jest (for example probe-ts-test:local)")
	}
	variants := map[string]struct {
		command []string
		project map[string]string
	}{
		"vitest": {
			[]string{"vitest", "run", "--coverage.enabled", "--coverage.reporter=lcovonly", "--coverage.reportsDirectory=" + coverage.DirPlaceholder},
			map[string]string{"src/price.test.ts": "import { test, expect } from \"vitest\";\nimport { price } from \"./price\";\ntest(\"discounts\", () => { expect(price(100, 10)).toBe(90); });\n"},
		},
		"jest": {
			[]string{"npx", "--no", "--", "jest", "--coverage", "--coverageReporters=lcovonly", "--coverageDirectory=" + coverage.DirPlaceholder},
			map[string]string{
				"src/price.test.ts": "import { price } from \"./price\";\ntest(\"discounts\", () => { expect(price(100, 10)).toBe(90); });\n",
				"package.json":      "{\"name\": \"price\", \"private\": true}\n",
				"jest.config.js":    "module.exports = { preset: \"ts-jest\", testEnvironment: \"node\" };\n",
				"tsconfig.json":     "{\"compilerOptions\": {\"target\": \"ES2020\", \"module\": \"commonjs\", \"strict\": true, \"esModuleInterop\": true, \"types\": [\"jest\"]}}\n",
			},
		},
	}
	for name, v := range variants {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			v.project["src/price.ts"] = tsCoveragePrice
			for path, content := range v.project {
				full := filepath.Join(source, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			h, err := New(Options{CandidateDir: source, ArtifactDir: t.TempDir(), Image: image, Timeout: 2 * time.Minute, MaxRuntime: 5 * time.Minute, MaxOutputBytes: 1 << 20, Commands: map[string][]string{coverage.CommandKey: v.command}})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			c, profile, sha, reason := h.RunCoverage(context.Background())
			if reason != "" || c.Status != "PASS" {
				t.Fatalf("coverage not measured: %q, check %s\n%s", reason, c.Status, c.Output)
			}
			if !strings.Contains(strings.Join(c.Command, " "), coverage.ReportDir) || strings.Contains(strings.Join(c.Command, " "), coverage.DirPlaceholder) {
				t.Fatalf("recorded command was not expanded: %q", c.Command)
			}
			parsed, err := coverage.Parse(profile)
			if err != nil || parsed.Format != coverage.FormatLCOV {
				t.Fatalf("payload is not an LCOV report (%v):\n%s", err, profile)
			}
			artifacts := 0
			for _, a := range h.Artifacts() {
				if a.Kind == "coverage_profile" {
					artifacts++
					if !strings.HasSuffix(a.Path, c.ID+"-coverage.lcov") || a.SHA256 != sha {
						t.Fatalf("coverage artifact %+v, want %s-coverage.lcov with digest %s", a, c.ID, sha)
					}
				}
			}
			if artifacts != 1 {
				t.Fatalf("%d coverage artifacts, want 1", artifacts)
			}
			// Line 3 (the throw) and line 9 (unused's body) never ran; line 5
			// ran for the one test.
			change := model.Change{Files: []model.ChangedFile{{Path: "src/price.ts", Status: "M", Hunks: []model.Hunk{{Lines: []model.DiffLine{
				{Kind: "add", NewLine: 3, Content: "x"}, {Kind: "add", NewLine: 5, Content: "x"}, {Kind: "add", NewLine: 9, Content: "x"},
			}}}}}}
			result := coverage.Analyze(parsed, coverage.Run{CheckID: c.ID, Status: c.Status, Command: c.Command, SHA256: sha}, change)
			got := result.Report()
			if got.Format != coverage.FormatLCOV || got.ExecutedLines != 1 || got.NotExecutedLines != 2 || got.NotMeasuredLines != 0 {
				t.Fatalf("measurement %+v\nLCOV:\n%s", got, profile)
			}
			if lines := result.NotExecuted("src/price.ts"); len(lines) != 2 || lines[0] != 3 || lines[1] != 9 {
				t.Fatalf("not executed lines %v", lines)
			}
		})
	}
}
