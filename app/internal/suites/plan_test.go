package suites

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const (
	baseCommit = "1111111111111111111111111111111111111111"
	headCommit = "2222222222222222222222222222222222222222"
)

// fakeRepo serves blobs by commit and path.
type fakeRepo map[string]string

func (r fakeRepo) reader(errs map[string]error) Reader {
	return func(ctx context.Context, commit, path string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := commit + ":" + path
		if err, ok := errs[key]; ok {
			return nil, err
		}
		src, ok := r[key]
		if !ok {
			return nil, errors.New("not found")
		}
		return []byte(src), nil
	}
}

func base(path string) string { return baseCommit + ":" + path }
func head(path string) string { return headCommit + ":" + path }

const clampTests = `package clamp

import "testing"

func assertClamp(t *testing.T, in, want int) {
	if got := Clamp(in); got != want {
		t.Fatalf("Clamp(%d) = %d, want %d", in, got, want)
	}
}

func TestClampNegative(t *testing.T) {
	assertClamp(t, -5, 0)
}

func TestClampUpper(t *testing.T) {
	if got := Clamp(50); got != 10 {
		t.Fatalf("Clamp(50) = %d", got)
	}
}

func TestClampInside(t *testing.T) {
	if got := Clamp(3); got != 3 {
		t.Fatalf("Clamp(3) = %d", got)
	}
}
`

// allShared is every test of clampTests, selected as shared_code_changed.
const allShared = "TestClampNegative:shared_code_changed,TestClampUpper:shared_code_changed,TestClampInside:shared_code_changed"

func modified(path string) model.ChangedFile {
	return model.ChangedFile{Path: path, Status: "M"}
}

func plan(t *testing.T, repo fakeRepo, errs map[string]error, files ...model.ChangedFile) Selection {
	t.Helper()
	sel, err := Plan(context.Background(), repo.reader(errs), model.Change{BaseCommit: baseCommit, HeadCommit: headCommit, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func summary(sel Selection) string {
	var parts []string
	for _, bt := range sel.Tests {
		parts = append(parts, bt.Name+":"+bt.Change)
	}
	return strings.Join(parts, ",")
}

func TestPlanSelection(t *testing.T) {
	cases := []struct {
		name      string
		candidate string // "" when the file is deleted
		file      model.ChangedFile
		want      string
	}{
		{"comment_only", strings.Replace(clampTests, "func TestClampUpper", "// TestClampUpper checks the upper bound.\nfunc TestClampUpper", 1), modified("clamp_test.go"), ""},
		{"body_edit", strings.Replace(clampTests, "got != 10", "got < 10", 1), modified("clamp_test.go"), "TestClampUpper:modified"},
		{"deleted_test", strings.Replace(clampTests, "func TestClampInside(t *testing.T) {\n\tif got := Clamp(3); got != 3 {\n\t\tt.Fatalf(\"Clamp(3) = %d\", got)\n\t}\n}\n", "", 1), modified("clamp_test.go"), "TestClampInside:removed"},
		{"renamed_test", strings.Replace(clampTests, "TestClampInside", "TestClampMiddle", 1), modified("clamp_test.go"), "TestClampInside:removed"},
		{"no_longer_runnable", strings.Replace(clampTests, "func TestClampInside(t *testing.T)", "func TestClampInside(t *testing.T, extra int)", 1), modified("clamp_test.go"), "TestClampInside:removed"},
		{"helper_edited", strings.Replace(clampTests, "got != want", "got < want", 1), modified("clamp_test.go"), "TestClampNegative:shared_code_changed,TestClampUpper:shared_code_changed,TestClampInside:shared_code_changed"},
		{"helper_removed", strings.Replace(clampTests, "func assertClamp(t *testing.T, in, want int) {\n\tif got := Clamp(in); got != want {\n\t\tt.Fatalf(\"Clamp(%d) = %d, want %d\", in, got, want)\n\t}\n}\n", "", 1), modified("clamp_test.go"), "TestClampNegative:shared_code_changed,TestClampUpper:shared_code_changed,TestClampInside:shared_code_changed"},
		{"helper_added", clampTests + "\nfunc extraHelper() {}\n", modified("clamp_test.go"), ""},
		{"test_added", clampTests + "\nfunc TestLater(t *testing.T) { t.Skip(\"later\") }\n", modified("clamp_test.go"), ""},
		{"file_deleted", "", model.ChangedFile{Path: "clamp_test.go", Status: "D"}, "TestClampNegative:file_deleted,TestClampUpper:file_deleted,TestClampInside:file_deleted"},
		{"renamed_same", clampTests, model.ChangedFile{OldPath: "clamp_test.go", Path: "bounds_test.go", Status: "R"}, ""},
		{"renamed_edited", strings.Replace(clampTests, "got != 3", "got > 3", 1), model.ChangedFile{OldPath: "clamp_test.go", Path: "bounds_test.go", Status: "R"}, "TestClampInside:modified"},
		{"renamed_to_non_test", clampTests, model.ChangedFile{OldPath: "clamp_test.go", Path: "clamp_helpers.go", Status: "R"}, "TestClampNegative:file_deleted,TestClampUpper:file_deleted,TestClampInside:file_deleted"},
		{"candidate_unparsable", clampTests + "\nfunc broken( {\n", modified("clamp_test.go"), "TestClampNegative:modified,TestClampUpper:modified,TestClampInside:modified"},
		// File-level edits that change whether or how go test runs the
		// unchanged tests of the file select every test of the file.
		{"build_constraint_added", "//go:build never\n\n" + clampTests, modified("clamp_test.go"), allShared},
		{"legacy_build_constraint_added", "// +build never\n\n" + clampTests, modified("clamp_test.go"), allShared},
		{"goos_suffix_rename", clampTests, model.ChangedFile{OldPath: "clamp_test.go", Path: "clamp_windows_test.go", Status: "R"}, allShared},
		{"goarch_suffix_rename", clampTests, model.ChangedFile{OldPath: "clamp_test.go", Path: "clamp_s390x_test.go", Status: "R"}, allShared},
		{"goos_suffix_kept", clampTests, model.ChangedFile{OldPath: "clamp_windows_test.go", Path: "bounds_windows_test.go", Status: "R"}, ""},
		// Another directory is another package, even with the same clause.
		{"moved_to_another_directory", clampTests, model.ChangedFile{OldPath: "clamp_test.go", Path: "legacy/clamp_test.go", Status: "R"}, allShared},
		{"moved_between_directories", clampTests, model.ChangedFile{OldPath: "a/clamp_test.go", Path: "b/clamp_test.go", Status: "R"}, allShared},
		{"import_added", strings.Replace(clampTests, "import \"testing\"", "import (\n\t\"testing\"\n\n\t_ \"example.test/clamp/strict\"\n)", 1), modified("clamp_test.go"), allShared},
		{"package_clause_changed", strings.Replace(clampTests, "package clamp", "package clamp_test", 1), modified("clamp_test.go"), allShared},
		{"test_main_added", clampTests + "\nfunc TestMain(m *testing.M) {}\n", modified("clamp_test.go"), allShared},
		{"init_added", clampTests + "\nfunc init() { limit = 20 }\n", modified("clamp_test.go"), allShared},
		{"initialized_var_added", clampTests + "\nvar _ = setLimit(20)\n", modified("clamp_test.go"), allShared},
		{"method_of_package_type_added", clampTests + "\nfunc (b Bounds) String() string { return \"\" }\n", modified("clamp_test.go"), allShared},
		{"directive_added", strings.Replace(clampTests, "func assertClamp", "//go:noinline\nfunc assertClamp", 1), modified("clamp_test.go"), allShared},
		{"plain_var_added", clampTests + "\nvar unused int\n", modified("clamp_test.go"), ""},
		{"method_of_new_type_added", clampTests + "\ntype fake struct{}\n\nfunc (fake) String() string { return \"\" }\n", modified("clamp_test.go"), ""},
		{"license_comment_added", "// Copyright the authors.\n\n" + clampTests, modified("clamp_test.go"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			basePath := tc.file.Path
			if tc.file.OldPath != "" {
				basePath = tc.file.OldPath
			}
			repo := fakeRepo{base(basePath): clampTests}
			if tc.candidate != "" {
				repo[head(tc.file.Path)] = tc.candidate
			}
			sel := plan(t, repo, nil, tc.file)
			// Plan sorts by (path, line): the declaration order of the file.
			if got := summary(sel); got != tc.want {
				t.Fatalf("selected %q, want %q (notes %q)", got, tc.want, sel.Notes)
			}
			for _, bt := range sel.Tests {
				if bt.Status != model.StatusUnverified || bt.EvidenceID != "" || bt.Path != basePath || bt.Line == 0 || bt.EndLine < bt.Line {
					t.Fatalf("selected test %+v", bt)
				}
			}
			if len(sel.Notes) != 0 {
				t.Fatalf("notes %q", sel.Notes)
			}
		})
	}
}

func TestPlanRecordsCandidateLocation(t *testing.T) {
	candidate := strings.Replace(clampTests, "\tif got := Clamp(50); got != 10 {", "\t// looser\n\tif got := Clamp(50); got < 10 {", 1)
	sel := plan(t, fakeRepo{base("clamp_test.go"): clampTests, head("clamp_test.go"): candidate}, nil, modified("clamp_test.go"))
	if len(sel.Tests) != 1 {
		t.Fatalf("selected %+v", sel.Tests)
	}
	bt := sel.Tests[0]
	if bt.Name != "TestClampUpper" || bt.Line != 15 || bt.EndLine != 19 || bt.CandidatePath != "clamp_test.go" || bt.CandidateLine != 15 || bt.CandidateEndLine != 20 {
		t.Fatalf("locations %+v", bt)
	}
	sel = plan(t, fakeRepo{base("clamp_test.go"): clampTests}, nil, model.ChangedFile{Path: "clamp_test.go", Status: "D"})
	for _, bt := range sel.Tests {
		if bt.CandidatePath != "" || bt.CandidateLine != 0 {
			t.Fatalf("a deleted file has a candidate location: %+v", bt)
		}
	}
}

func TestPlanSkipsFilesGoDoesNotCompileAsTests(t *testing.T) {
	repo := fakeRepo{}
	var files []model.ChangedFile
	for _, p := range []string{"a_test.go", "testdata/x_test.go", "vendor/v/x_test.go", "_hidden/x_test.go", ".tools/x_test.go", "pkg/_x_test.go", "pkg/.x_test.go", "x_test.go.orig", "x.go"} {
		repo[base(p)] = clampTests
		repo[head(p)] = strings.Replace(clampTests, "got != 10", "got < 10", 1)
	}
	files = append(files,
		model.ChangedFile{Path: "a_test.go", Status: "A"},
		model.ChangedFile{Path: "a_test.go", OldPath: "b_test.go", Status: "C"},
		model.ChangedFile{Path: "a_test.go", Status: "T"},
		model.ChangedFile{Path: "a_test.go", Status: "M", Binary: true},
		modified("testdata/x_test.go"), modified("vendor/v/x_test.go"), modified("_hidden/x_test.go"), modified(".tools/x_test.go"),
		modified("pkg/_x_test.go"), modified("pkg/.x_test.go"), modified("x_test.go.orig"), modified("x.go"),
		model.ChangedFile{OldPath: "x.go", Path: "x_test.go", Status: "R"},
	)
	if sel := plan(t, repo, nil, files...); len(sel.Tests) != 0 || len(sel.Notes) != 0 {
		t.Fatalf("selected %q notes %q", summary(sel), sel.Notes)
	}
}

func TestPlanNotesWhatItCannotAnalyze(t *testing.T) {
	edited := strings.Replace(clampTests, "got != 10", "got < 10", 1)
	repo := fakeRepo{
		base("big_test.go"): clampTests, head("big_test.go"): edited,
		base("broken_test.go"): "package clamp\nfunc TestX(t *testing.T) {", head("broken_test.go"): edited,
		base("missing_test.go"): clampTests, head("missing_test.go"): edited,
		base("ok_test.go"): clampTests, head("ok_test.go"): edited,
	}
	errs := map[string]error{base("big_test.go"): fmt.Errorf("git show: %w", ErrTooLarge), base("missing_test.go"): errors.New("git failed")}
	sel := plan(t, repo, errs, modified("ok_test.go"), modified("big_test.go"), modified("broken_test.go"), modified("missing_test.go"))
	if got := summary(sel); got != "TestClampUpper:modified" {
		t.Fatalf("selected %q", got)
	}
	want := []string{
		"Baseline versions of changed tests: the baseline version of big_test.go exceeds the read limit, so its tests were not re-run.",
		"Baseline versions of changed tests: the baseline version of broken_test.go could not be parsed as Go, so its tests were not re-run.",
		"Baseline versions of changed tests: the baseline version of missing_test.go could not be read, so its tests were not re-run.",
	}
	if strings.Join(sel.Notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s", strings.Join(sel.Notes, "\n"))
	}
	// A candidate that cannot be read selects every test of the file.
	sel = plan(t, fakeRepo{base("ok_test.go"): clampTests}, map[string]error{head("ok_test.go"): fmt.Errorf("x: %w", ErrTooLarge)}, modified("ok_test.go"))
	if got := summary(sel); got != "TestClampNegative:modified,TestClampUpper:modified,TestClampInside:modified" {
		t.Fatalf("unreadable candidate selected %q", got)
	}
}

func TestPlanLimitsAndOrder(t *testing.T) {
	repo := fakeRepo{}
	var files []model.ChangedFile
	for i := MaxTestFiles + 3; i >= 1; i-- {
		p := fmt.Sprintf("pkg%02d/x_test.go", i)
		repo[base(p)] = clampTests
		files = append(files, model.ChangedFile{Path: p, Status: "D"})
	}
	sel := plan(t, repo, nil, files...)
	if len(sel.Tests) != MaxSelectedTests {
		t.Fatalf("selected %d tests, want the cap %d", len(sel.Tests), MaxSelectedTests)
	}
	want := []string{
		fmt.Sprintf("Baseline versions of changed tests: 3 changed Go test files beyond the limit of %d were not analyzed, so their tests were not re-run.", MaxTestFiles),
		fmt.Sprintf("Baseline versions of changed tests: %d selected tests beyond the limit of %d were not re-run.", 3*MaxTestFiles-MaxSelectedTests, MaxSelectedTests),
	}
	if strings.Join(sel.Notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s", strings.Join(sel.Notes, "\n"))
	}
	for i := 1; i < len(sel.Tests); i++ {
		a, b := sel.Tests[i-1], sel.Tests[i]
		if a.Path > b.Path || a.Path == b.Path && a.Line >= b.Line {
			t.Fatalf("unsorted at %d: %+v then %+v", i, a, b)
		}
	}
	if sel.Tests[0].Path != "pkg01/x_test.go" || sel.Tests[0].Name != "TestClampNegative" {
		t.Fatalf("first test %+v", sel.Tests[0])
	}
	again := plan(t, repo, nil, files...)
	if summary(again) != summary(sel) {
		t.Fatal("planning is not deterministic")
	}
}

func TestPlanStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := fakeRepo{base("x_test.go"): clampTests}
	if _, err := Plan(ctx, repo.reader(nil), model.Change{BaseCommit: baseCommit, HeadCommit: headCommit, Files: []model.ChangedFile{{Path: "x_test.go", Status: "D"}}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
}
