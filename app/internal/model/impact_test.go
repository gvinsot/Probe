package model

import (
	"sort"
	"strings"
	"testing"
)

// The F6a fields of the impact section (indexed, reason, tests_total, via,
// file_changed) validate, and their bounds are enforced.
func TestSchemaImpactAdditions(t *testing.T) {
	v := newValidator(loadSchema(t))
	fn := ImpactFunction{
		Path: "price/price.go", Line: 4, EndLine: 10, Symbol: "example.test/shop/price.Total", Change: ChangeBodyChanged,
		Callers:      []ImpactCaller{{Path: "api/handler.go", Line: 7, Symbol: "example.test/shop/api.Checkout", Depth: 1, Resolution: ResolutionStatic}},
		CallersTotal: 1, Indexed: true, TestsTotal: 1,
		Tests: []ImpactTest{{Name: "TestCheckout", Path: "api/handler_test.go", Line: 5, Package: "example.test/shop/api", Depth: 2, Resolution: ResolutionStatic,
			Via: []string{"example.test/shop/api.TestCheckout", "example.test/shop/api.Checkout", "example.test/shop/price.Total"}, FileChanged: true}},
	}
	unindexed := ImpactFunction{Path: "osx/a_windows.go", Line: 3, EndLine: 3, Symbol: "example.test/shop/osx.OS", Change: ChangeSignatureChanged,
		Callers: []ImpactCaller{}, Tests: []ImpactTest{}, Reason: "the file is excluded by the linux/amd64 build constraints"}
	valid := map[string]*Impact{
		"indexed":        {Status: ImpactIndexed, IndexedFiles: 6, ChangedFunctions: []ImpactFunction{fn, unindexed}, Note: ImpactNote},
		"limited":        {Status: ImpactLimited, Reason: "1 Go files could not be parsed and were not indexed", IndexedFiles: 5, ChangedFunctions: []ImpactFunction{fn}, Note: ImpactNote},
		"not applicable": {Status: ImpactNotApplicable, Reason: "no indexable Go file changed (files under testdata or vendor, in directories whose name starts with _ or ., and sensitive paths are not indexed)", ChangedFunctions: []ImpactFunction{}, Note: ImpactNote},
	}
	for name, impact := range valid {
		r := populatedReport()
		r.Impact = impact
		if errs := v.validateDocument(toJSONValue(t, r)); len(errs) > 0 {
			sort.Strings(errs)
			t.Errorf("%s: %s", name, strings.Join(errs, "; "))
		}
	}
	tooManyTests := fn
	for len(tooManyTests.Tests) <= 20 {
		tooManyTests.Tests = append(tooManyTests.Tests, fn.Tests[0])
	}
	longVia := fn
	longVia.Tests = []ImpactTest{fn.Tests[0]}
	longVia.Tests[0].Via = []string{"a", "b", "c", "d", "e"}
	shortVia := fn
	shortVia.Tests = []ImpactTest{fn.Tests[0]}
	shortVia.Tests[0].Via = []string{"a"}
	for name, f := range map[string]ImpactFunction{"21 tests": tooManyTests, "via of 5": longVia, "via of 1": shortVia} {
		r := populatedReport()
		r.Impact = &Impact{Status: ImpactIndexed, IndexedFiles: 1, ChangedFunctions: []ImpactFunction{f}, Note: ImpactNote}
		if errs := v.validateDocument(toJSONValue(t, r)); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	// indexed and tests_total are required.
	r := populatedReport()
	r.Impact = &Impact{Status: ImpactIndexed, IndexedFiles: 1, ChangedFunctions: []ImpactFunction{fn}, Note: ImpactNote}
	for _, field := range []string{"indexed", "tests_total"} {
		doc := toJSONValue(t, r)
		delete(doc.(map[string]any)["impact"].(map[string]any)["changed_functions"].([]any)[0].(map[string]any), field)
		if errs := v.validateDocument(doc); len(errs) == 0 {
			t.Errorf("missing %s accepted", field)
		}
	}
}
