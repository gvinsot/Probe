package fuzz

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

func scalar(basic string) Param       { return Param{Kind: ParamScalar, Basic: basic} }
func named(n, basic string) Param     { return Param{Kind: ParamScalar, Basic: basic, Named: n} }
func slice(basic string) Param        { return Param{Kind: ParamSlice, Basic: basic} }
func variadic(basic string) Param     { return Param{Kind: ParamVariadic, Basic: basic} }
func array(basic string, n int) Param { return Param{Kind: ParamArray, Basic: basic, Len: n} }

// target hand-builds a planned target of package dir "obs".
func target(name string, results, inputs int, params ...Param) Target {
	types := make([]string, len(params))
	for i, p := range params {
		types[i] = p.Type()
		if p.Kind == ParamVariadic {
			types[i] = "..." + p.Elem()
		}
	}
	return Target{Dir: "obs", Path: "obs/obs.go", Line: 1, EndLine: 2, Name: name, Symbol: "obs." + name,
		Signature: "func(" + strings.Join(types, ", ") + ")", Params: params, Results: results, Inputs: inputs, Exported: true}
}

func obsPlan(targets ...Target) PackagePlan {
	idents := map[string]bool{"Cents": true, "Ratio": true}
	for _, t := range targets {
		idents[t.Name] = true
	}
	return PackagePlan{Dir: "obs", Name: "obs", Targets: targets, Idents: idents}
}

func renderOptions(suffix string) RenderOptions {
	return RenderOptions{Suffix: suffix, ObservationsPath: "/tmp/probe-observations.jsonl", PayloadLimit: harness.PayloadLimit(32 * 1024), CallTimeout: time.Second}
}

func TestRenderPrefixesEveryIdentifierAndParses(t *testing.T) {
	plan := obsPlan(
		target("Percent", 1, 16, scalar("int"), scalar("int")),
		target("Join", 1, 16, slice("string")),
		target("Discount", 1, 16, named("Cents", "int64")),
		target("Nothing", 0, 1),
		target("Sum", 1, 16, scalar("rune"), variadic("float32")),
		target("Grid", 2, 16, array("uint8", 3), scalar("bool")),
	)
	h, err := Render(plan, renderOptions("0123abcd"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Path != "obs/probe_fuzz_0123abcd_test.go" {
		t.Fatalf("path = %s", h.Path)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, h.Path, h.Content, 0)
	if err != nil {
		t.Fatalf("rendered harness does not parse: %v\n%s", err, h.Content)
	}
	if file.Name.Name != "obs" {
		t.Fatalf("package = %s", file.Name.Name)
	}
	prefix := "probeFuzz0123abcd"
	declared := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Name == nil || !strings.HasPrefix(imp.Name.Name, prefix+"_") {
			t.Fatalf("import %s is not aliased with the prefix", imp.Path.Value)
		}
		declared[imp.Name.Name] = true
	}
	var tests []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				continue
			}
			if strings.HasPrefix(d.Name.Name, "TestProbeFuzz_0123abcd_") {
				tests = append(tests, d.Name.Name)
				continue
			}
			declared[d.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					declared[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name != "_" {
							declared[n.Name] = true
						}
					}
				}
			}
		}
	}
	want := map[string]bool{}
	for _, n := range harnessNames {
		want[prefix+n] = true
	}
	for name := range declared {
		if !want[name] {
			t.Errorf("declared identifier %s is not in harnessNames", name)
		}
	}
	for name := range want {
		if !declared[name] {
			t.Errorf("harnessNames lists %s, which the file does not declare", name)
		}
	}
	if strings.Join(tests, ",") != strings.Join(h.TestNames(), ",") || len(tests) != 6 {
		t.Fatalf("tests %v, harness %v", tests, h.TestNames())
	}
	// Every local variable of a test function carries the prefix, so no local
	// can shadow the target or a named type.
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fd.Name.Name, "TestProbeFuzz_") {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if x.Tok == token.DEFINE {
					for _, lhs := range x.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && !strings.HasPrefix(id.Name, prefix+"_") {
							t.Errorf("local %s in %s lacks the prefix", id.Name, fd.Name.Name)
						}
					}
				}
			case *ast.Field:
				for _, id := range x.Names {
					if !strings.HasPrefix(id.Name, prefix+"_") {
						t.Errorf("parameter %s in %s lacks the prefix", id.Name, fd.Name.Name)
					}
				}
			}
			return true
		})
	}
	// The harness never asserts: the only Fatal calls are the file errors.
	if strings.Count(h.Content, ".Fatal(") != 2 || strings.Contains(h.Content, "Errorf") || strings.Contains(h.Content, "\"fmt\"") {
		t.Fatalf("harness asserts or uses fmt:\n%s", h.Content)
	}
	names, err := parseTestNames(h)
	if err != nil || strings.Join(names, ",") != strings.Join(h.TestNames(), ",") {
		t.Fatalf("test names %v (%v)", names, err)
	}
}

// parseTestNames lists the Test functions the way the harness package does:
// the generated file must declare exactly the planned names.
func parseTestNames(h Harness) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), h.Path, h.Content, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Test") {
			names = append(names, fd.Name.Name)
		}
	}
	return names, nil
}

func TestRenderIsDeterministicPerSuffix(t *testing.T) {
	plan := obsPlan(target("Percent", 1, 64, scalar("int"), scalar("int")), target("Join", 1, 64, slice("string")))
	a, err := Render(plan, renderOptions("aaaabbbb"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(plan, renderOptions("aaaabbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Content != b.Content || a.Path != b.Path {
		t.Fatal("same plan and suffix rendered different bytes")
	}
	c, err := Render(plan, renderOptions("ccccdddd"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Content == a.Content || strings.Contains(c.Content, "aaaabbbb") {
		t.Fatal("a different suffix must rename every harness identifier")
	}
	// The inputs themselves depend only on the function identity.
	for i := range a.Tests {
		if len(a.Tests[i].Inputs) != len(c.Tests[i].Inputs) {
			t.Fatal("inputs differ between suffixes")
		}
		for j := range a.Tests[i].Inputs {
			if a.Tests[i].Inputs[j].Call != c.Tests[i].Inputs[j].Call {
				t.Fatal("inputs differ between suffixes")
			}
		}
	}
	suffix, err := NewSuffix()
	if err != nil || !suffixPattern.MatchString(suffix) {
		t.Fatalf("NewSuffix = %q, %v", suffix, err)
	}
	other, _ := NewSuffix()
	if other == suffix {
		t.Fatal("two suffixes were equal")
	}
}

func TestRenderRefusesUnsafeInputs(t *testing.T) {
	good := obsPlan(target("Percent", 1, 8, scalar("int")))
	cases := map[string]func() (PackagePlan, RenderOptions){
		"short suffix": func() (PackagePlan, RenderOptions) { return good, renderOptions("abc") },
		"upper suffix": func() (PackagePlan, RenderOptions) { return good, renderOptions("ABCDEF12") },
		"relative observations path": func() (PackagePlan, RenderOptions) {
			o := renderOptions("abcdef12")
			o.ObservationsPath = "tmp/x"
			return good, o
		},
		"quote in observations path": func() (PackagePlan, RenderOptions) {
			o := renderOptions("abcdef12")
			o.ObservationsPath = "/tmp/\"x"
			return good, o
		},
		"no timeout": func() (PackagePlan, RenderOptions) {
			o := renderOptions("abcdef12")
			o.CallTimeout = 0
			return good, o
		},
		"collision": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Percent", 1, 8, scalar("int")))
			p.Idents["probeFuzzabcdef12_run"] = true
			return p, renderOptions("abcdef12")
		},
		"test name collision": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Percent", 1, 8, scalar("int")))
			p.Idents["TestProbeFuzz_abcdef12_1"] = true
			return p, renderOptions("abcdef12")
		},
		"function name not an identifier": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Percent", 1, 8, scalar("int")))
			p.Targets[0].Name = "Percent(0); os.Exit"
			return p, renderOptions("abcdef12")
		},
		"unknown function": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Percent", 1, 8, scalar("int")))
			delete(p.Idents, "Percent")
			return p, renderOptions("abcdef12")
		},
		"named type not an identifier": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Discount", 1, 8, named("Cents(1)", "int64")))
			return p, renderOptions("abcdef12")
		},
		"basic type not generated": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Discount", 1, 8, scalar("uintptr")))
			return p, renderOptions("abcdef12")
		},
		"package name": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Percent", 1, 8, scalar("int")))
			p.Name = "obs; import \"os\""
			return p, renderOptions("abcdef12")
		},
		"escaping directory": func() (PackagePlan, RenderOptions) {
			p := obsPlan(target("Percent", 1, 8, scalar("int")))
			p.Dir = "../obs"
			return p, renderOptions("abcdef12")
		},
		"no targets": func() (PackagePlan, RenderOptions) {
			return PackagePlan{Dir: "obs", Name: "obs"}, renderOptions("abcdef12")
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			p, o := build()
			if h, err := Render(p, o); err == nil {
				t.Fatalf("rendered:\n%s", h.Content)
			}
		})
	}
}

func TestRenderHalvesInputsToFitThePayload(t *testing.T) {
	plan := obsPlan(
		target("A", 1, 256, slice("string"), slice("string")),
		target("B", 1, 256, scalar("int")),
		target("C", 1, 256, scalar("int")),
		target("D", 1, 256, scalar("int")),
	)
	o := renderOptions("abcdef12")
	o.PayloadLimit = 128 * 1024
	h, err := Render(plan, o)
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for _, test := range h.Tests {
		records += len(test.Inputs)
		if test.Target.Inputs != len(test.Inputs) {
			t.Fatalf("%s: Target.Inputs %d, inputs %d", test.Name, test.Target.Inputs, len(test.Inputs))
		}
	}
	if records >= 1024 || h.Display < minDisplayBytes || h.Display > MaxDisplayBytes {
		t.Fatalf("records %d, display %d", records, h.Display)
	}
	if records*(recordOverhead+2*h.Display)+functionOverhead*len(h.Tests)+frameReserve > o.PayloadLimit {
		t.Fatal("the worst-case stream does not fit the payload limit")
	}
	if len(h.Content) > MaxHarnessBytes {
		t.Fatal("harness too large")
	}
	o.PayloadLimit = 4096
	if _, err := Render(plan, o); err == nil {
		t.Fatal("a payload limit that cannot hold the stream must fail after halving")
	}
}

// The fixture of the compile-and-observe test: every behavior the encoder
// and the stream must capture.
const obsSource = `package obs

import (
	"errors"
	"os"
	"sort"
	"time"
)

type Cents int64
type Ratio float64

func Percent(part, total int) int { return part * 100 / total }

func Sorter(xs []int) int { sort.Ints(xs); return len(xs) }

func Stamp(label string) string { return label + time.Now().Format(time.RFC3339Nano) }

func Scale(r Ratio) Ratio { return r * 2 }

func Pick(xs []byte) []byte { return xs }

func Fail(n int8) (int8, error) {
	if n < 0 {
		return 0, errors.New("negative")
	}
	return n, nil
}

type pair struct {
	name string
	next *pair
	tags map[string]int
}

func Link(s string) *pair {
	p := &pair{name: s, tags: map[string]int{"b": 2, "a": 1}}
	p.next = p
	return p
}

func Nothing() {}

func Loop(n uint8) uint8 {
	if n == 7 {
		select {}
	}
	return n
}

func After(s string) string { return s }

func Exit(n int) int {
	if n == 7 {
		os.Exit(3)
	}
	return n
}

func Join(xs ...string) int { return len(xs) }

func Endpoint(host string) string { return "tcp://" + host }

func Labels(n int) []string { return []string{"Username:", "Password:"} }
`

// runHarness writes the obs module with the harness and runs its tests with
// the host go tool, returning the go test log and the raw stream.
func runHarness(t *testing.T, h Harness, observations string) (string, []byte, int) {
	t.Helper()
	dir := t.TempDir()
	// go 1.13: the harness must compile with the language features of an old
	// module (no generics, no any, no min or max).
	writeTree(t, dir, map[string]string{"go.mod": "module example.test/obs\n\ngo 1.13\n", "obs/obs.go": obsSource, h.Path: h.Content})
	quoted := make([]string, len(h.Tests))
	for i, test := range h.Tests {
		quoted[i] = test.Name
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./obs", "-json", "-count=1", "-run", "^("+strings.Join(quoted, "|")+")$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("go test: %v\n%s", err, log.String())
		}
		code = ee.ExitCode()
	}
	stream, readErr := os.ReadFile(observations)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return log.String(), stream, code
}

func recordFor(t *testing.T, f FunctionStream, call string) Record {
	t.Helper()
	for _, r := range f.Records {
		if r.Call == call {
			return r
		}
	}
	calls := []string{}
	for _, r := range f.Records {
		calls = append(calls, r.Call)
	}
	t.Fatalf("%s: no record for %s in %v", f.Test, call, calls)
	return Record{}
}

// TestGeneratedHarnessCompilesAndObserves runs rendered harnesses with the go
// tool (it is skipped where none is installed, such as the Windows host; CI
// and the golang container run it) and checks what the stream records.
func TestGeneratedHarnessCompilesAndObserves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness writes to an absolute in-container path")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool")
	}
	observations := filepath.Join(t.TempDir(), "observations.jsonl")
	o := renderOptions("feedc0de")
	o.ObservationsPath = observations
	o.CallTimeout = 300 * time.Millisecond
	plan := obsPlan(
		target("Percent", 1, 24, scalar("int"), scalar("int")),
		target("Sorter", 1, 8, slice("int")),
		target("Stamp", 1, 16, scalar("string")),
		target("Scale", 1, 24, named("Ratio", "float64")),
		target("Pick", 1, 8, slice("byte")),
		target("Fail", 2, 8, scalar("int8")),
		target("Link", 1, 2, scalar("string")),
		target("Nothing", 0, 1),
		target("Join", 1, 8, variadic("string")),
		target("Endpoint", 1, 64, scalar("string")),
		target("Labels", 1, 2, scalar("int")),
		target("Loop", 1, 8, scalar("uint8")),
		target("After", 1, 4, scalar("string")),
	)
	plan.Idents["pair"] = true
	h, err := Render(plan, o)
	if err != nil {
		t.Fatal(err)
	}
	log, raw, code := runHarness(t, h, observations)
	if code != 0 {
		t.Fatalf("go test exit %d\n%s\n%s", code, log, h.Content)
	}
	for _, name := range h.TestNames() {
		if action, pkg := harness.GoTestOutcome(log, name); action != "pass" || pkg != "example.test/obs/obs" {
			t.Fatalf("%s: outcome %q in %q\n%s", name, action, pkg, log)
		}
	}
	results, err := h.Normalize(raw)
	if err != nil {
		t.Fatalf("normalize: %v\n%s", err, raw)
	}
	stream, err := ParseResults(results)
	if err != nil {
		t.Fatal(err)
	}
	fn := func(i int) FunctionStream {
		f, _ := stream.Function(h.Tests[i].Name)
		return f
	}
	percent := recordFor(t, fn(0), "Percent(0, 0)")
	if percent.Display != `panic(error("runtime error: integer divide by zero"))` || !percent.Panicked {
		t.Fatalf("Percent(0, 0) = %+v", percent)
	}
	if r := recordFor(t, fn(0), "Percent(0, 1)"); r.Display != "int(0)" || r.Panicked {
		t.Fatalf("Percent(0, 1) = %+v", r)
	}
	if r := recordFor(t, fn(1), "Sorter([]int{1, 0})"); r.Display != "int(2); arg 1 after call: []int{0, 1}" {
		t.Fatalf("Sorter mutation not encoded: %+v", r)
	}
	unstable := 0
	for _, r := range fn(2).Records {
		if r.Unstable {
			unstable++
		}
	}
	if fn(2).State != StateComplete || unstable*10 < len(fn(2).Records)*9 {
		t.Fatalf("Stamp: %d of %d records unstable", unstable, len(fn(2).Records))
	}
	nan := recordFor(t, fn(3), "Scale(Ratio(math.NaN()))")
	negZero := recordFor(t, fn(3), "Scale(Ratio(math.Copysign(0, -1)))")
	zero := recordFor(t, fn(3), "Scale(Ratio(0))")
	if nan.Display != "obs.Ratio(NaN)" || negZero.Display != "obs.Ratio(-0)" || zero.Display != "obs.Ratio(0)" {
		t.Fatalf("float encodings: %q %q %q", nan.Display, negZero.Display, zero.Display)
	}
	if nan.SHA256 == zero.SHA256 || negZero.SHA256 == zero.SHA256 {
		t.Fatal("NaN, -0 and 0 must hash differently")
	}
	nilSlice := recordFor(t, fn(4), "Pick([]byte(nil))")
	empty := recordFor(t, fn(4), "Pick([]byte{})")
	if nilSlice.Display != "[]uint8(nil); arg 1 after call: []uint8(nil)" || empty.Display != "[]uint8{}; arg 1 after call: []uint8{}" {
		t.Fatalf("nil and empty: %q %q", nilSlice.Display, empty.Display)
	}
	if r := recordFor(t, fn(5), "Fail(-1)"); r.Display != `int8(0), error("negative")` {
		t.Fatalf("Fail(-1) = %q", r.Display)
	}
	if r := recordFor(t, fn(5), "Fail(0)"); r.Display != "int8(0), error(nil)" {
		t.Fatalf("Fail(0) = %q", r.Display)
	}
	if r := recordFor(t, fn(6), `Link("")`); r.Display != `&obs.pair{name: "", next: <cycle>, tags: {"a": 1, "b": 2}}` {
		t.Fatalf("Link = %q", r.Display)
	}
	if r := recordFor(t, fn(7), "Nothing()"); r.Display != "()" {
		t.Fatalf("Nothing() = %q", r.Display)
	}
	if r := recordFor(t, fn(8), "Join()"); r.Display != "int(0); arg 1 after call: []string(nil)" {
		t.Fatalf("Join() = %q", r.Display)
	}
	if r := recordFor(t, fn(8), `Join([]string{}...)`); r.Display != "int(0); arg 1 after call: []string{}" {
		t.Fatalf("Join([]string{}...) = %q", r.Display)
	}
	// Endpoint's displays are harmless URLs next to inputs holding "@": the
	// stream is accepted (it used to be rejected as a whole), and a display is
	// either whole or redacted in place.
	whole := 0
	for _, r := range fn(9).Records {
		if r.Whole() {
			whole++
		} else if !strings.Contains(r.Display, "[REDACTED]") {
			t.Fatalf("Endpoint record neither whole nor redacted: %+v", r)
		}
	}
	if whole < len(fn(9).Records)/2 {
		t.Fatalf("Endpoint: only %d of %d displays whole", whole, len(fn(9).Records))
	}
	if r := recordFor(t, fn(10), "Labels(0)"); r.Display != "[REDACTED]" || r.Length != len(`[]string{"Username:", "Password:"}`) {
		t.Fatalf("Labels(0) = %+v", r)
	}
	loop := fn(11)
	if loop.State != StateStopped || loop.Stop != StopTimeout || loop.AtCall != "Loop(7)" || loop.At != len(loop.Records) {
		t.Fatalf("Loop = %+v", loop)
	}
	after := fn(12)
	if after.State != StateStopped || after.Stop != StopPoisoned || after.At != 0 || len(after.Records) != 0 {
		t.Fatalf("After = %+v", after)
	}
	for i := 0; i < 11; i++ {
		if fn(i).State != StateComplete || len(fn(i).Records) != len(h.Tests[i].Inputs) {
			t.Fatalf("%s: %+v", h.Tests[i].Target.Name, fn(i))
		}
	}

	// A process that ends mid-function leaves an interrupted function and
	// functions that never started; the rest of the stream stays valid.
	if err := os.Remove(observations); err != nil {
		t.Fatal(err)
	}
	exitPlan := obsPlan(target("Exit", 1, 16, scalar("int")), target("After", 1, 4, scalar("string")))
	o.Suffix = "0badc0de"
	he, err := Render(exitPlan, o)
	if err != nil {
		t.Fatal(err)
	}
	log, raw, code = runHarness(t, he, observations)
	if code == 0 {
		t.Fatalf("a process exit must fail go test\n%s", log)
	}
	if action, _ := harness.GoTestOutcome(log, he.Tests[0].Name); action == "pass" {
		t.Fatalf("the interrupted test must not pass\n%s", log)
	}
	results, err = he.Normalize(raw)
	if err != nil {
		t.Fatalf("normalize: %v\n%s", err, raw)
	}
	stream, err = ParseResults(results)
	if err != nil {
		t.Fatal(err)
	}
	exit, _ := stream.Function(he.Tests[0].Name)
	if exit.State != StateInterrupted || exit.AtCall != "Exit(7)" || exit.At != 5 {
		t.Fatalf("Exit = %+v", exit)
	}
	if notStarted, _ := stream.Function(he.Tests[1].Name); notStarted.State != StateNotStarted {
		t.Fatalf("After = %+v", notStarted)
	}
}

func TestHarnessIdentifiersMatchTheirList(t *testing.T) {
	names := harnessIdentifiers("probeFuzzabcdef12", "abcdef12", 2)
	sort.Strings(names)
	if len(names) != len(harnessNames)+2 || names[0] != "TestProbeFuzz_abcdef12_1" {
		t.Fatalf("%v", names)
	}
	_ = model.FuzzSeedScheme
}
