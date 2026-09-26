package mutation

import (
	"errors"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// fakeSource is an in-memory candidate snapshot.
type fakeSource struct{ files map[string]string }

func (f fakeSource) ReadSource(rel string) ([]byte, error) {
	content, ok := f.files[rel]
	if !ok {
		return nil, errors.New("C:\\host\\temp\\snapshot\\" + rel + ": not found")
	}
	return []byte(content), nil
}

func (f fakeSource) HasTestFile(dir string) (bool, error) {
	for name := range f.files {
		if path.Dir(name) == dir && strings.HasSuffix(name, "_test.go") {
			return true, nil
		}
	}
	return false, nil
}

// addedFile describes a changed file whose every line is added.
func addedFile(p, content string) model.ChangedFile {
	f := model.ChangedFile{Path: p, Status: "A"}
	h := model.Hunk{NewStart: 1}
	for i := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		h.Lines = append(h.Lines, model.DiffLine{Kind: "add", NewLine: i + 1})
	}
	f.Hunks = []model.Hunk{h}
	return f
}

// threeLineFile has one comparison per line on lines 4, 5 and 6, each with a
// boundary and an increment_constant site.
func threeLineFile(pkg string) string {
	return "package " + pkg + "\n\nfunc F(a int) (bool, bool, bool) {\n\tx := a < 1\n\ty := a > 2\n\tz := a <= 3\n\treturn x, y, z\n}\n"
}

func TestBreadthFirstSelection(t *testing.T) {
	src := fakeSource{files: map[string]string{
		"a/a.go": threeLineFile("a"), "a/a_test.go": "package a\n",
		"b/b.go": threeLineFile("b"), "b/b_test.go": "package b\n",
		"c/c.go": threeLineFile("c"), "c/c_test.go": "package c\n",
	}}
	change := model.Change{Files: []model.ChangedFile{addedFile("c/c.go", threeLineFile("c")), addedFile("a/a.go", threeLineFile("a")), addedFile("b/b.go", threeLineFile("b"))}}
	p := NewPlan(src, change, 4, nil)
	if p.Generated != 18 || len(p.Selected) != 4 {
		t.Fatalf("generated %d selected %d", p.Generated, len(p.Selected))
	}
	// Selection: first site of line 1 of a, b, c, then first site of line 2 of a.
	// Execution order groups by package: a (lines 4, 5), b (4), c (4).
	var got []string
	for _, s := range p.Selected {
		got = append(got, s.Path+":"+strconv.Itoa(s.Line)+":"+s.Operator)
	}
	want := []string{"a/a.go:4:boundary", "a/a.go:5:boundary", "b/b.go:4:boundary", "c/c.go:4:boundary"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected %q, want %q", got, want)
	}
	for _, f := range p.Files {
		wantLines := map[string]int{"a/a.go": 2, "b/b.go": 1, "c/c.go": 1}[f.Path]
		if f.Status != model.MutationFileEligible || f.MutatedLines != wantLines || f.AddedLines != 8 {
			t.Fatalf("file %+v", f)
		}
	}
	// max_mutants beyond the candidates selects them all, second operators last.
	all := NewPlan(src, change, 200, nil)
	if len(all.Selected) != 18 {
		t.Fatalf("selected %d of 18", len(all.Selected))
	}
}

func TestMaxMutantsCapAndDeterminism(t *testing.T) {
	src := fakeSource{files: map[string]string{"price/price.go": discountSource, "price/price_test.go": "package price\n"}}
	change := model.Change{Files: []model.ChangedFile{addedFile("price/price.go", discountSource)}}
	p := NewPlan(src, change, 3, nil)
	if p.Generated != 8 || len(p.Selected) != 3 {
		t.Fatalf("generated %d selected %d", p.Generated, len(p.Selected))
	}
	var ops []string
	for _, s := range p.Selected {
		ops = append(ops, strconv.Itoa(s.Line)+":"+s.Operator)
	}
	if want := []string{"6:negate_condition", "7:drop_error", "9:negate_condition"}; !reflect.DeepEqual(ops, want) {
		t.Fatalf("selected %q, want %q", ops, want)
	}
	for i := 0; i < 10; i++ {
		if again := NewPlan(src, change, 3, nil); !reflect.DeepEqual(again.Selected, p.Selected) {
			t.Fatal("selection is not deterministic")
		}
	}
}

func TestNotExecutedLinesSkipped(t *testing.T) {
	src := fakeSource{files: map[string]string{"price/price.go": discountSource, "price/price_test.go": "package price\n"}}
	change := model.Change{Files: []model.ChangedFile{addedFile("price/price.go", discountSource)}}
	p := NewPlan(src, change, 10, func(path string) []int {
		if path == "price/price.go" {
			return []int{6, 7}
		}
		return nil
	})
	if p.Generated != 4 || p.CoverageSkipped != 4 || len(p.Selected) != 4 {
		t.Fatalf("generated %d coverage-skipped %d selected %d", p.Generated, p.CoverageSkipped, len(p.Selected))
	}
	for _, s := range p.Selected {
		if s.Line == 6 || s.Line == 7 {
			t.Fatalf("a site on a not-executed line was selected: %+v", s)
		}
	}
	if !strings.Contains(p.Files[0].Reason, "4 candidate mutants on lines the coverage run did not execute") {
		t.Fatalf("file reason %q", p.Files[0].Reason)
	}
}

// Files that cannot be mutated are listed with a fixed reason; OS error text
// (host paths) never reaches the report.
func TestFileSkipReasons(t *testing.T) {
	body := "package p\n\nfunc F(a int) bool { return a > 1 }\n"
	src := fakeSource{files: map[string]string{
		"notests/n.go": body,
		"tests/t.go":   body, "tests/t_test.go": "package p\n",
		"x_windows.go": body, "x_linux_arm64.go": body, "x_test_helper.go": body,
		"_hidden/h.go": body, ".dot/d.go": body, "testdata/t.go": body, "vendor/v.go": body,
		"x_test.go": "package p\n",
	}}
	files := []model.ChangedFile{
		addedFile("notests/n.go", body), addedFile("tests/t.go", body), addedFile("x_windows.go", body),
		addedFile("x_linux_arm64.go", body), addedFile("_hidden/h.go", body), addedFile(".dot/d.go", body),
		addedFile("testdata/t.go", body), addedFile("vendor/v.go", body), addedFile("missing/m.go", body),
		addedFile("x_test_helper.go", body),
		{Path: "deleted.go", Status: "D"}, {Path: "image.go", Status: "M", Binary: true},
		addedFile("readme.md", "text\n"), addedFile("tests/t_test.go", "package p\n"),
		{Path: "tests/nothing_added.go", Status: "M"},
	}
	p := NewPlan(src, model.Change{Files: files}, 10, nil)
	got := map[string]string{}
	for _, f := range p.Files {
		got[f.Path] = f.Status + ": " + f.Reason
	}
	want := map[string]string{
		"notests/n.go":           "skipped: " + skipNoTestFile,
		"tests/t.go":             "eligible: ",
		"x_windows.go":           "skipped: " + skipOSArchName,
		"x_linux_arm64.go":       "skipped: " + skipOSArchName,
		"_hidden/h.go":           "skipped: " + skipIgnoredPath,
		".dot/d.go":              "skipped: " + skipIgnoredPath,
		"testdata/t.go":          "skipped: " + skipIgnoredPath,
		"vendor/v.go":            "skipped: " + skipIgnoredPath,
		"missing/m.go":           "skipped: " + skipUnreadable,
		"x_test_helper.go":       "eligible: ",
		"tests/nothing_added.go": "skipped: " + skipNoAddedLines,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files:\n got %q\nwant %q", got, want)
	}
	for _, f := range p.Files {
		if strings.Contains(f.Reason, "host") || strings.Contains(f.Reason, `\`) {
			t.Fatalf("an OS error reached the report: %q", f.Reason)
		}
	}
	if p.Files[0].Path > p.Files[len(p.Files)-1].Path {
		t.Fatal("files are not sorted by path")
	}
}

func TestPackageArgAndExpandCommand(t *testing.T) {
	for in, want := range map[string]string{"main.go": ".", "a/b/c.go": "./a/b", "x/y.go": "./x"} {
		if got := PackageArg(in); got != want {
			t.Errorf("PackageArg(%q) = %q, want %q", in, got, want)
		}
	}
	command := []string{"go", "test", "-json", "-count=1", "{package}"}
	got := ExpandCommand(command, "./price")
	if strings.Join(got, " ") != "go test -json -count=1 ./price" || command[4] != "{package}" {
		t.Fatalf("expanded %q (template %q)", got, command)
	}
}

func TestOSArchConstrained(t *testing.T) {
	for name, want := range map[string]bool{
		"x_windows.go": true, "x_linux_arm64.go": true, "x_amd64.go": true, "dir/y_darwin_test.go": true,
		"linux.go": false, "x.go": false, "x_helper.go": false, "x_linux_helper.go": false, "windows_x.go": false,
	} {
		if got := osArchConstrained(name); got != want {
			t.Errorf("osArchConstrained(%q) = %v, want %v", name, got, want)
		}
	}
}
