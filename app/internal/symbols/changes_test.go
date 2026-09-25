package symbols

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func classify(t *testing.T, pairs ...sourcePair) ([]string, int) {
	t.Helper()
	changed, touched, failed := compareFunctions(pairs)
	var out []string
	for _, c := range changed {
		out = append(out, fmt.Sprintf("%s:%s:%d-%d:%s", c.path, c.name, c.line, c.end, c.change))
	}
	for _, c := range changed {
		if !touched[dirOf(c.path)+"\x00"+c.pkgName+"\x00"+c.name] {
			t.Errorf("changed %s is not in the touched set", c.name)
		}
	}
	return out, failed
}

func modified(p, before, after string) sourcePair {
	return sourcePair{headPath: p, head: []byte(after), basePath: p, base: []byte(before)}
}

func TestChangedFunctionClassification(t *testing.T) {
	const base = "package p\n\n// F doc.\nfunc F(a int) int {\n\treturn a + 1\n}\n\nfunc (s *S[K]) M() int { return 1 }\n\nfunc init() { setup() }\n\nfunc _() {}\n"
	for _, tc := range []struct {
		name  string
		pairs []sourcePair
		want  string
	}{
		{"comments and spacing only", []sourcePair{modified("p/p.go", base, "package p\n\n// F has new documentation.\nfunc F(a  int)  int {\n\treturn a+1 // same\n}\n\nfunc (s *S[K]) M() int { return 1 /* c */ }\n\nfunc init() { setup() }\n\nfunc _() {}\n")}, ""},
		// Joining or splitting lines changes where Go inserts semicolons, which
		// linter.TokenDigest keeps: such a change is reported, the conservative
		// direction.
		{"joined lines are reported", []sourcePair{modified("p/p.go", base, strings.Replace(base, "{\n\treturn a + 1\n}", "{ return a + 1 }", 1))}, "p/p.go:F:4-4:body_changed"},
		{"body", []sourcePair{modified("p/p.go", base, strings.Replace(base, "a + 1", "a + 2", 1))}, "p/p.go:F:4-6:body_changed"},
		{"signature", []sourcePair{modified("p/p.go", base, strings.Replace(base, "F(a int) int", "F(a int, b int) int", 1))}, "p/p.go:F:4-6:signature_changed"},
		{"generic method body", []sourcePair{modified("p/p.go", base, strings.Replace(base, "return 1 }", "return 2 }", 1))}, "p/p.go:S.M:8-8:body_changed"},
		{"init and blank functions are not listed", []sourcePair{modified("p/p.go", base, strings.Replace(strings.Replace(base, "setup()", "other()", 1), "func _() {}", "func _() { x() }", 1))}, ""},
		{"added and removed functions are not changed functions", []sourcePair{modified("p/p.go", base, strings.Replace(base, "func F(a int) int", "func G(a int) int", 1))}, ""},
		{"moved between files of a package", []sourcePair{
			modified("p/p.go", base, "package p\n\nfunc (s *S[K]) M() int { return 1 }\n"),
			{headPath: "p/q.go", head: []byte("package p\n\n// F doc.\nfunc F(a int) int {\n\treturn a + 1\n}\n")},
		}, ""},
		{"renamed file", []sourcePair{{headPath: "p/new.go", head: []byte(strings.Replace(base, "a + 1", "a - 1", 1)), basePath: "p/p.go", base: []byte(base)}}, "p/new.go:F:4-6:body_changed"},
		{"main in package main", []sourcePair{modified("cmd/main.go", "package main\n\nfunc main() { a() }\n", "package main\n\nfunc main() { b() }\n")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, failed := classify(t, tc.pairs...)
			if strings.Join(got, " ") != tc.want || failed != 0 {
				t.Fatalf("got %q (failed %d), want %q", got, failed, tc.want)
			}
		})
	}
}

// Per-OS variants of one function are compared as multisets.
func TestChangedVariants(t *testing.T) {
	linux := "package p\n\nfunc OS() string { return \"linux\" }\n"
	windows := "package p\n\nfunc OS() string { return \"windows\" }\n"
	got, _ := classify(t, modified("p/os_linux.go", linux, linux), modified("p/os_windows.go", windows, strings.Replace(windows, "windows", "win", 1)))
	if strings.Join(got, " ") != "p/os_windows.go:OS:3-3:body_changed" {
		t.Fatalf("%q", got)
	}
}

// A file that does not parse on either side is left out on both sides, so its
// functions are neither changed nor added.
func TestUnparseableFilesAreLeftOut(t *testing.T) {
	good := "package p\n\nfunc F() int { return 1 }\n"
	changed, touched, failed := compareFunctions([]sourcePair{
		modified("p/a.go", "package p\n\nfunc (\n", good),
		modified("p/b.go", good, "package p\nfunc {"),
	})
	if len(changed) != 0 || len(touched) != 0 || failed != 2 {
		t.Fatalf("%+v %+v %d", changed, touched, failed)
	}
}

func TestChangedGoFiles(t *testing.T) {
	change := model.Change{Files: []model.ChangedFile{
		{Path: "a.go", Status: "M"},
		{Path: "a_test.go", Status: "M"},
		{Path: "gone.go", Status: "D"},
		{Path: "vendor/x/x.go", Status: "M"},
		{Path: "secrets/k.go", Status: "M"},
		{Path: "new.go", OldPath: "old.go", Status: "R"},
		{Path: "README.md", Status: "M"},
	}}
	head, any := changedGoFiles(change, func(p string) bool { return strings.HasPrefix(p, "secrets/") })
	sort.Strings(head)
	if !any || strings.Join(head, " ") != "a.go new.go" {
		t.Fatalf("%v %q", any, head)
	}
	_, any = changedGoFiles(model.Change{Files: []model.ChangedFile{{Path: "README.md", Status: "M"}, {Path: "testdata/x.go", Status: "A"}}}, nil)
	if any {
		t.Fatal("no indexable Go file changed")
	}
	_, any = changedGoFiles(model.Change{Files: []model.ChangedFile{{Path: "x_test.go", Status: "M"}}}, nil)
	if !any {
		t.Fatal("a changed test file still builds the index for the tools")
	}
}

func TestIndexableAndModules(t *testing.T) {
	for p, want := range map[string]bool{
		"a/b.go": true, "go.mod": true, "a/go.mod": true, "a/b.txt": false,
		"a/testdata/b.go": false, "vendor/a/b.go": false, "a/_gen/b.go": false, "a/.hidden/b.go": false,
		"a/_b.go": false, "a/.b.go": false, "../a.go": false, "a\\b.go": false,
	} {
		if got := indexable(p, nil); got != want {
			t.Errorf("indexable(%q) = %v", p, got)
		}
	}
	mods := discoverModules(map[string][]byte{
		"go.mod":         []byte("module example.test/root // comment\n"),
		"app/go.mod":     []byte("module \"example.test/app\"\n"),
		"app/sub/go.mod": []byte("go 1.23\n"),
		"dup/go.mod":     []byte("module example.test/app\n"),
	})
	for dir, want := range map[string]string{
		"": "example.test/root", "x/y": "example.test/root/x/y", "app": "example.test/app",
		"app/internal/p": "example.test/app/internal/p", "app/sub/p": "", "dup": "",
	} {
		got, ok := mods.importPath(dir)
		if got != want || ok != (want != "") {
			t.Errorf("importPath(%q) = %q, %v; want %q", dir, got, ok, want)
		}
	}
	if mods.unreadable != 1 || len(mods.duplicates) != 1 || mods.duplicates[0] != "app" && mods.duplicates[0] != "dup" {
		t.Fatalf("%+v", mods)
	}
	for p, want := range map[string]string{"github.com/x/go-yaml": "yaml", "gopkg.in/yaml.v3": "yaml", "example.test/m/v2": "m", "example.test/a-b": "a_b"} {
		if got := guessName(p); got != want {
			t.Errorf("guessName(%q) = %q, want %q", p, got, want)
		}
	}
}
