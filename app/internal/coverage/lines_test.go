package coverage

import (
	"reflect"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// NotExecuted returns exactly the added lines a measured run reported with
// count 0, and nothing for a file or a result that was not measured.
func TestNotExecutedLines(t *testing.T) {
	p := mustParse(t,
		module+"/internal/a/a.go:10.20,14.3 2 7", // executed
		module+"/internal/a/a.go:20.20,24.3 2 0", // not executed
	)
	change := model.Change{Files: []model.ChangedFile{
		file("internal/a/a.go", []int{1, 11, 21, 22, 40}, nil),
		file("internal/b/b.go", []int{3}, nil), // no block: not measured
	}}
	result := Analyze(p, pass(module), change)
	if got := result.NotExecuted("internal/a/a.go"); !reflect.DeepEqual(got, []int{21, 22}) {
		t.Fatalf("not executed %v, want [21 22]", got)
	}
	got := result.NotExecuted("internal/a/a.go")
	got[0] = 99
	if result.NotExecuted("internal/a/a.go")[0] != 21 {
		t.Fatal("NotExecuted exposes internal state")
	}
	for _, path := range []string{"internal/b/b.go", "missing.go"} {
		if got := result.NotExecuted(path); got != nil {
			t.Fatalf("%s: %v, want nil", path, got)
		}
	}
	if got := NotMeasured("no profile").NotExecuted("internal/a/a.go"); got != nil {
		t.Fatalf("not measured result returned %v", got)
	}
	if got := (Result{}).NotExecuted("internal/a/a.go"); got != nil {
		t.Fatalf("zero result returned %v", got)
	}
}
