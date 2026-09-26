package model

import (
	"sort"
	"strings"
	"testing"
)

// The F4 additions to the mutation section validate: a mutant's column, end
// line and enclosing function, and the coverage_skipped counter. At most five
// failing test names are recorded, and the additions keep their bounds.
func TestSchemaMutationAdditions(t *testing.T) {
	v := newValidator(loadSchema(t))
	valid := [][]edit{
		{{"mutation/mutants/0/column", 11}, {"mutation/mutants/0/end_line", 4}, {"mutation/mutants/0/symbol", "Discount"}, {"mutation/coverage_skipped", 3}},
		{{"mutation/mutants/1/failed_tests", []string{"TestA", "TestB", "TestC", "TestD", "TestE"}}},
	}
	for i, edits := range valid {
		if errs := v.validateDocument(applyEdits(t, edits)); len(errs) > 0 {
			sort.Strings(errs)
			t.Errorf("valid case %d rejected:\n  %s", i, strings.Join(errs, "\n  "))
		}
	}
	invalid := map[string][]edit{
		"column 0":                  {{"mutation/mutants/0/column", 0}},
		"negative coverage_skipped": {{"mutation/coverage_skipped", -1}},
		"six failed tests":          {{"mutation/mutants/1/failed_tests", []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF"}}},
		"symbol not a string":       {{"mutation/mutants/0/symbol", 3}},
	}
	for name, edits := range invalid {
		if errs := v.validateDocument(applyEdits(t, edits)); len(errs) == 0 {
			t.Errorf("%s: the schema accepts it", name)
		}
	}
}
