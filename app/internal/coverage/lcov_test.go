package coverage

import (
	"errors"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// istanbulLCOV is the shape Vitest and Jest write with the lcovonly reporter:
// paths relative to the project root, function and branch records around the
// line entries.
const istanbulLCOV = `TN:
SF:src/price.ts
FN:1,price
FNF:1
FNH:1
FNDA:3,price
DA:1,3
DA:2,3
DA:3,0
DA:5,3
LF:4
LH:3
BRDA:2,0,0,0
BRDA:2,0,1,3
BRF:2
BRH:1
end_of_record
TN:
SF:/workspace/src/util.js
DA:1,1
DA:2,0,abcd
end_of_record
`

func mustLCOV(t *testing.T, body string) *Profile {
	t.Helper()
	p, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("fixture LCOV: %v", err)
	}
	if p.Format != FormatLCOV {
		t.Fatalf("format %q, want lcov", p.Format)
	}
	return p
}

func lcovRun(status string) Run {
	return Run{CheckID: "check-4", Status: status, Command: []string{"npx", "--no", "vitest", "run", "--coverage.reportsDirectory=/tmp/probe-coverage"}, SHA256: strings.Repeat("b", 64)}
}

func TestParseRecognizesFormats(t *testing.T) {
	if p, err := Parse([]byte("\nmode: set\nexample.com/m/a.go:1.1,2.2 1 0\n")); err != nil || p.Format != FormatGo {
		t.Fatalf("Go profile: %+v, %v", p, err)
	}
	p := mustLCOV(t, istanbulLCOV)
	want := []Block{
		{File: "/workspace/src/util.js", StartLine: 1, EndLine: 1, NumStmts: 1, Count: 1},
		{File: "/workspace/src/util.js", StartLine: 2, EndLine: 2, NumStmts: 1, Count: 0},
		{File: "src/price.ts", StartLine: 1, EndLine: 1, NumStmts: 1, Count: 3},
		{File: "src/price.ts", StartLine: 2, EndLine: 2, NumStmts: 1, Count: 3},
		{File: "src/price.ts", StartLine: 3, EndLine: 3, NumStmts: 1, Count: 0},
		{File: "src/price.ts", StartLine: 5, EndLine: 5, NumStmts: 1, Count: 3},
	}
	if len(p.Blocks) != len(want) {
		t.Fatalf("blocks %+v", p.Blocks)
	}
	for i := range want {
		if p.Blocks[i] != want[i] {
			t.Fatalf("block %d = %+v, want %+v", i, p.Blocks[i], want[i])
		}
	}
	for _, body := range []string{"", "PASS\n", "hello: world\n", "{\"total\":{}}\n"} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrFormat) {
			t.Errorf("Parse(%q) error %v, want ErrFormat", body, err)
		}
	}
}

// A report that is not the one the tool wrote, or was cut short, fails as a
// whole: a partial report would read as "these lines never ran".
func TestParseLCOVRejectsMalformedReports(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":      "SF:a.ts\nXX:1\nend_of_record\n",
		"DA outside record":  "TN:\nDA:1,1\n",
		"nested record":      "SF:a.ts\nSF:b.ts\nend_of_record\n",
		"empty path":         "SF:\nend_of_record\n",
		"stray end":          "TN:\nend_of_record\n",
		"line zero":          "SF:a.ts\nDA:0,1\nend_of_record\n",
		"negative count":     "SF:a.ts\nDA:1,-1\nend_of_record\n",
		"missing count":      "SF:a.ts\nDA:1\nend_of_record\n",
		"extra fields":       "SF:a.ts\nDA:1,1,x,y\nend_of_record\n",
		"FN outside record":  "TN:\nFN:1,f\n",
		"polluted by output": "SF:a.ts\nDA:1,1\nend_of_record\nPASS src/a.test.ts\n",
	} {
		if _, err := ParseLCOV([]byte(body)); err == nil {
			t.Errorf("%s: accepted %q", name, body)
		}
	}
	if _, err := ParseLCOV([]byte("SF:a.ts\nDA:1,1\n")); !errors.Is(err, ErrTruncated) {
		t.Fatalf("unterminated record: %v, want ErrTruncated", err)
	}
	if _, err := ParseLCOV([]byte("TN:\n")); !errors.Is(err, ErrFormat) {
		t.Fatalf("report without a record: %v, want ErrFormat", err)
	}
}

// Several records for one file (one per worker) sum, which can only turn a
// not-executed line into an executed one; huge counts saturate.
func TestParseLCOVMergesAndSaturates(t *testing.T) {
	p := mustLCOV(t, "SF:a.ts\nDA:1,0\nDA:2,0\nend_of_record\nSF:a.ts\nDA:1,2\nDA:2,99999999999999999999999\nend_of_record\n")
	if len(p.Blocks) != 2 || p.Blocks[0].Count != 2 || p.Blocks[1].Count != 1<<30 {
		t.Fatalf("blocks %+v", p.Blocks)
	}
}

func TestAnalyzeLCOVClassifiesScriptSources(t *testing.T) {
	change := model.Change{Files: []model.ChangedFile{
		file("src/price.ts", []int{2, 3, 4}, []int{7}),
		file("src/util.js", []int{2}, nil),
		file("src/missing.tsx", []int{1, 2}, nil),
		file("src/price.test.ts", []int{1}, nil),
		file("src/types.d.ts", []int{1}, nil),
		file("main.go", []int{1}, nil),
	}}
	result := Analyze(mustLCOV(t, istanbulLCOV), lcovRun("PASS"), change)
	c := result.Report()
	if c.Status != StatusMeasured || c.Format != FormatLCOV || c.Note != NoteLCOV {
		t.Fatalf("report header %+v", c)
	}
	// price.ts: 2 executed, 3 not executed, 4 no entry; util.js through the
	// absolute /workspace path: 2 not executed; missing.tsx: not measured.
	if c.AddedLines != 6 || c.ExecutedLines != 1 || c.NotExecutedLines != 2 || c.NoBlockLines != 1 || c.NotMeasuredLines != 2 || c.RemovedLines != 1 {
		t.Fatalf("counters %+v", c)
	}
	if len(c.Files) != 3 || c.Files[0].Path != "src/missing.tsx" || c.Files[0].Status != StatusNotMeasured {
		t.Fatalf("files %+v", c.Files)
	}
	signals := result.Signals()
	if len(signals) != 2 || signals[0].Path != "src/price.ts" || signals[0].Line != 3 || signals[1].Path != "src/util.js" {
		t.Fatalf("signals %+v", signals)
	}
	s := signals[0]
	if s.Kind != Kind || s.Severity != "medium" || s.Summary != summaryLCOVPassed || !strings.HasPrefix(s.Evidence, "LCOV report recorded in check-4") {
		t.Fatalf("signal %+v", s)
	}
	if got := result.NotExecuted("src/price.ts"); len(got) != 1 || got[0] != 3 {
		t.Fatalf("NotExecuted = %v", got)
	}
}

// A Go profile never speaks about script sources and an LCOV report never
// about Go files: neither becomes "not measured" lines of the other.
func TestAnalyzeScopesByFormat(t *testing.T) {
	change := model.Change{Files: []model.ChangedFile{file("a/a.go", []int{1}, nil), file("src/a.ts", []int{1}, nil)}}
	goResult := Analyze(mustParse(t, module+"/a/a.go:1.1,1.9 1 1"), pass(module), change).Report()
	if goResult.Format != FormatGo || goResult.AddedLines != 1 || goResult.ExecutedLines != 1 || len(goResult.Files) != 1 {
		t.Fatalf("Go measurement %+v", goResult)
	}
	lcov := Analyze(mustLCOV(t, "SF:src/a.ts\nDA:1,0\nend_of_record\n"), lcovRun("FAIL"), change)
	if r := lcov.Report(); r.AddedLines != 1 || r.NotExecutedLines != 1 || len(r.Files) != 1 || r.Files[0].Path != "src/a.ts" {
		t.Fatalf("LCOV measurement %+v", r)
	}
	if s := lcov.Signals(); len(s) != 1 || s[0].Severity != "low" || s[0].Summary != summaryStopped || !strings.HasSuffix(s[0].Evidence, evidenceStopped) {
		t.Fatalf("stopped-run signal %+v", s)
	}
}

// Paths are asked for under the repository root only: a report relative to
// a subdirectory, or naming a path outside /workspace, never matches.
func TestAnalyzeLCOVNeverResolvesOtherRoots(t *testing.T) {
	change := model.Change{Files: []model.ChangedFile{file("web/src/a.ts", []int{1}, nil)}}
	for _, sf := range []string{"src/a.ts", "/src/web/src/a.ts", "./web/src/a.ts", "/workspace/./web/src/a.ts"} {
		r := Analyze(mustLCOV(t, "SF:"+sf+"\nDA:1,0\nend_of_record\n"), lcovRun("PASS"), change).Report()
		if r.NotMeasuredLines != 1 || r.NotExecutedLines != 0 {
			t.Errorf("SF:%s resolved: %+v", sf, r)
		}
	}
}

func TestScriptSource(t *testing.T) {
	for p, want := range map[string]bool{
		"src/a.ts": true, "src/a.tsx": true, "lib/a.mjs": true, "lib/a.cjs": true, "a.jsx": true, "a.mts": true,
		"src/a.d.ts": false, "src/a.test.ts": false, "src/a.spec.js": false, "src/__tests__/a.ts": false,
		"dist/app.min.js": false, "node_modules/x/index.js": false, "a.go": false, "a.py": false,
	} {
		if ScriptSource(p) != want {
			t.Errorf("ScriptSource(%q) = %v, want %v", p, !want, want)
		}
	}
}

func TestExpandAndExpectedFormat(t *testing.T) {
	argv, capture := Expand([]string{"go", "test", "-coverprofile=" + Placeholder, "./..."})
	if capture != ProfilePath || argv[2] != "-coverprofile="+ProfilePath {
		t.Fatalf("Go expansion %q, %s", argv, capture)
	}
	source := []string{"npx", "--no", "jest", "--coverage", "--coverageReporters=lcovonly", "--coverageDirectory=" + DirPlaceholder}
	argv, capture = Expand(source)
	if capture != LCOVPath || argv[5] != "--coverageDirectory="+ReportDir || source[5] != "--coverageDirectory="+DirPlaceholder {
		t.Fatalf("LCOV expansion %q, %s (source %q)", argv, capture, source)
	}
	if ExpectedFormat(source, "go") != FormatLCOV || ExpectedFormat([]string{"x", Placeholder}, "typescript") != FormatLCOV || ExpectedFormat([]string{"x", Placeholder}, "go") != FormatGo {
		t.Fatal("ExpectedFormat")
	}
	if NotMeasuredAs(FormatLCOV, "r").Report().Note != NoteLCOV || NotMeasured("r").Report().Note != Note {
		t.Fatal("not-measured note")
	}
}
