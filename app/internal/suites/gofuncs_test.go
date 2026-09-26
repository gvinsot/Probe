package suites

import (
	"strings"
	"testing"
)

const header = "package clamp\n\nimport (\n\t\"testing\"\n)\n\n"

func mustParse(t *testing.T, src string) testFile {
	t.Helper()
	f, err := parseTestFile("clamp_test.go", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	return f
}

func testNames(f testFile) []string {
	var names []string
	for _, t := range f.tests {
		names = append(names, t.name)
	}
	return names
}

func TestParseTestFileFindsRunnableTests(t *testing.T) {
	src := `package clamp

import (
	tt "testing"
)

func TestA(t *tt.T) {}
func Test(t *tt.T) {}
func TestÜmlaut(t *tt.T) {}
func Test_underscore(t *tt.T) {}
func TestMain(m *tt.M) {}
func Testlower(t *tt.T) {}
func BenchmarkA(b *tt.B) {}
func FuzzA(f *tt.F) {}
func ExampleA() {}
func (s suite) TestMethod(t *tt.T) {}
func TestTwoParams(t *tt.T, x int) {}
func TestResult(t *tt.T) error { return nil }
func TestValue(t tt.T) {}
func TestGeneric[P any](t *tt.T) {}
func TestA(t *tt.T) { t.Log("duplicate") }
type suite struct{}
`
	f := mustParse(t, src)
	if got := strings.Join(testNames(f), ","); got != "TestA,Test,TestÜmlaut,Test_underscore" {
		t.Fatalf("tests %s", got)
	}
	a, ok := f.test("TestA")
	if !ok || a.line != 7 || a.endLine != 7 {
		t.Fatalf("TestA %+v (the first declaration is kept)", a)
	}
	for _, key := range []string{"func TestMain", "func Testlower", "func BenchmarkA", "func FuzzA", "func ExampleA", "func (suite).TestMethod", "func TestTwoParams", "func TestResult", "func TestValue", "func TestGeneric", "type suite"} {
		if _, ok := f.shared[key]; !ok {
			t.Errorf("shared declaration %q missing from %v", key, f.shared)
		}
	}
	if _, ok := f.shared["func TestA"]; ok {
		t.Error("a runnable test is also a shared declaration")
	}
}

func TestParseTestFileDotAndUnaliasedImports(t *testing.T) {
	f := mustParse(t, "package clamp\n\nimport . \"testing\"\n\nfunc TestDot(t *T) {}\n")
	if got := strings.Join(testNames(f), ","); got != "TestDot" {
		t.Fatalf("dot import: %s", got)
	}
}

func TestParseTestFileRejectsInvalidSource(t *testing.T) {
	for name, src := range map[string]string{
		"syntax":   header + "func TestA(t *testing.T) {\n",
		"not_utf8": header + "func TestA(t *testing.T) { _ = \"\xff\" }\n",
	} {
		if _, err := parseTestFile("x_test.go", []byte(src)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestDigestIgnoresCommentsAndLayoutOnly(t *testing.T) {
	base := mustParse(t, header+"func TestA(t *testing.T) {\n\tif Clamp(3) != 3 {\n\t\tt.Fatal(\"inside\")\n\t}\n}\n")
	digestOf := func(src string) string {
		t.Helper()
		f := mustParse(t, header+src)
		a, ok := f.test("TestA")
		if !ok {
			t.Fatalf("TestA missing in %s", src)
		}
		return a.digest
	}
	a, _ := base.test("TestA")
	same := []string{
		"// TestA documents the inside case.\nfunc TestA(t *testing.T) {\n\t// a comment\n\tif Clamp(3) != 3 {\n\t\tt.Fatal(\"inside\") /* trailing */\n\t}\n}\n",
		"func TestA( t *testing.T ) {\n    if Clamp(3)!=3 {\n  t.Fatal( \"inside\" )\n\t\t}\n}\n",
	}
	for _, src := range same {
		if d := digestOf(src); d != a.digest {
			t.Errorf("layout or comment edit changed the digest:\n%s", src)
		}
	}
	different := []string{
		"func TestA(t *testing.T) {\n\tif Clamp(3) != 4 {\n\t\tt.Fatal(\"inside\")\n\t}\n}\n",
		"func TestA(t *testing.T) {\n\tif Clamp(3) < 3 {\n\t\tt.Fatal(\"inside\")\n\t}\n}\n",
		"func TestA(t *testing.T) {\n\tif Clamp(3) != 3 {\n\t\tt.Fatal(\"outside\")\n\t}\n}\n",
		"func TestA(t *testing.T) {\n\tif Clamp(3) != 3 {\n\t\tt.Log(\"inside\")\n\t}\n}\n",
	}
	for _, src := range different {
		if d := digestOf(src); d == a.digest {
			t.Errorf("a literal or operator edit kept the digest:\n%s", src)
		}
	}
}

func TestSharedDeclarations(t *testing.T) {
	base := mustParse(t, header+`const (
	low = iota
	high
)

var fixtures = []int{1, 2}

type table struct{ in, want int }

func helper(t *testing.T, got, want int) {
	if got != want {
		t.Fatalf("got %d", got)
	}
}

func (tb table) check() bool { return tb.in == tb.want }

func init() {}
func init() {}

func TestMain(m *testing.M) { m.Run() }

func TestA(t *testing.T) { helper(t, 1, 1) }
`)
	for _, key := range []string{"const low", "const high", "var fixtures", "type table", "func helper", "func (table).check", "func init", "func init#2", "func TestMain"} {
		if base.shared[key] == "" {
			t.Errorf("missing shared key %q in %v", key, base.shared)
		}
	}
	if base.shared["const low"] != base.shared["const high"] {
		t.Error("an iota group must share one digest")
	}
	if sharedChanged(base, base) {
		t.Fatal("identical files reported a shared change")
	}
	withHelper := func(body string) testFile {
		return mustParse(t, header+"const (\n\tlow = iota\n\thigh\n)\n\nvar fixtures = []int{1, 2}\n\ntype table struct{ in, want int }\n\n"+body+"\n\nfunc (tb table) check() bool { return tb.in == tb.want }\n\nfunc init() {}\nfunc init() {}\n\nfunc TestMain(m *testing.M) { m.Run() }\n\nfunc TestA(t *testing.T) { helper(t, 1, 1) }\n")
	}
	unchanged := withHelper("func helper(t *testing.T, got, want int) {\n\tif got != want {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}")
	if sharedChanged(base, unchanged) {
		t.Fatal("reformatted identical helper reported as changed")
	}
	edited := withHelper("func helper(t *testing.T, got, want int) {\n\tif got < want {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}")
	if !sharedChanged(base, edited) {
		t.Fatal("an edited helper was not reported")
	}
	removed := withHelper("")
	if !sharedChanged(base, removed) {
		t.Fatal("a removed helper was not reported")
	}
	added := withHelper("func helper(t *testing.T, got, want int) {\n\tif got != want {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}\n\nfunc extra() {}")
	if sharedChanged(base, added) {
		t.Fatal("a declaration only the candidate added counts as a shared change")
	}
	iota := mustParse(t, strings.Replace(header+"const (\n\tlow = iota\n\thigh\n)\n", "low = iota", "low = iota + 1", 1))
	for _, key := range []string{"const low", "const high"} {
		if iota.shared[key] == base.shared[key] {
			t.Errorf("editing the iota group kept the digest of %s", key)
		}
	}
}

func TestFileHeaderSeesWhatCommentsHide(t *testing.T) {
	base := mustParse(t, "// Package clamp tests.\npackage clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n")
	same := []string{
		// Import order, a non-directive comment and layout do not count.
		"// Package clamp tests the bounds.\npackage clamp\n\nimport (\n\t\"testing\"\n\t\"strings\"\n)\n\n// TestA is unchanged.\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"package clamp\n\nimport \"strings\"\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\t_ = strings.ToLower(\"A\")\n}\n",
	}
	for _, src := range same {
		if cand := mustParse(t, src); fileChanged(base, cand) {
			t.Errorf("reported a file change for:\n%s\nheaders %q / %q", src, base.header, cand.header)
		}
	}
	different := map[string]string{
		"go_build":      "//go:build linux\n\npackage clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"plus_build":    "// +build ignore\n\npackage clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"package":       "package clamp_test\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"import_path":   "package clamp\n\nimport (\n\tstrings \"example.test/strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"blank_import":  "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n\n\t_ \"time/tzdata\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"dot_import":    "package clamp\n\nimport (\n\t. \"strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = ToLower(\"A\") }\n",
		"embed":         "package clamp\n\nimport (\n\t_ \"embed\"\n\t\"strings\"\n\t\"testing\"\n)\n\n//go:embed testdata/golden.txt\nvar golden string\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"line":          "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\n//line other.go:10\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"test_main":     "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {}\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"init":          "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc init() {}\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"var_with_init": "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nvar configured = configure()\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
		"method":        "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc (c *Config[T]) String() string { return \"\" }\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n",
	}
	for name, src := range different {
		if cand := mustParse(t, src); !fileChanged(base, cand) {
			t.Errorf("%s: no file change reported\nheaders %q / %q", name, base.header, cand.header)
		}
	}
	// Adding only a test, a function, a type with its methods, a constant or
	// a variable without initializer changes nothing for the existing tests.
	added := "package clamp\n\nimport (\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { _ = strings.ToLower(\"A\") }\n\nfunc TestB(t *testing.T) {}\n\nfunc helper() {}\n\ntype fake[T any] struct{}\n\nfunc (f *fake[T]) String() string { return \"\" }\n\nconst answer = 42\n\nvar scratch []int\n"
	if cand := mustParse(t, added); fileChanged(base, cand) {
		t.Errorf("added declarations without effect reported a file change: %v", cand.effects)
	}
	// A method added to a type the baseline file declared counts.
	withType := mustParse(t, "package clamp\n\nimport \"testing\"\n\ntype table struct{}\n\nfunc TestA(t *testing.T) { _ = table{} }\n")
	withMethod := mustParse(t, "package clamp\n\nimport \"testing\"\n\ntype table struct{}\n\nfunc (table) String() string { return \"t\" }\n\nfunc TestA(t *testing.T) { _ = table{} }\n")
	if !fileChanged(withType, withMethod) {
		t.Error("a method added to a type of the baseline file was not reported")
	}
}

func TestFileNameTags(t *testing.T) {
	for p, want := range map[string]string{
		"clamp_test.go":                 "",
		"pkg/clamp_test.go":             "",
		"windows_test.go":               "",
		"clamp_windows_test.go":         "windows",
		"pkg/clamp_arm64_test.go":       "arm64",
		"clamp_linux_amd64_test.go":     "linux_amd64",
		"clamp_linux_test_test.go":      "",
		"clamp_notanos_test.go":         "",
		"clamp_wasip1_wasm_test.go":     "wasip1_wasm",
		"clamp_amd64_linux_test.go":     "linux",
		"clamp.windows_test.go":         "",
		"a_b_c_freebsd_riscv64_test.go": "freebsd_riscv64",
	} {
		if got := fileNameTags(p); got != want {
			t.Errorf("fileNameTags(%q) = %q, want %q", p, got, want)
		}
	}
}
