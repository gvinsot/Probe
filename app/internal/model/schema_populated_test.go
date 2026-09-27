package model

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Cross-feature schema test (integration agent I): a report in which every
// field reachable from Report holds a non-zero value somewhere must validate
// against the published schema. TestSchemaMirrorsModel compares the schema
// with the Go types; this test feeds the validator a document that exercises
// every property at least once, optional ones included.

// modelFieldPaths lists every JSON field path reachable from t, such as
// "report.fuzz.functions[].checks.base_confirm".
func modelFieldPaths(t reflect.Type, path string, out map[string]bool, depth int) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		if t.Kind() != reflect.Pointer {
			path += "[]"
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == timeType || depth > 12 {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, ok := jsonName(f)
		if !ok {
			continue
		}
		out[path+"."+name] = true
		modelFieldPaths(f.Type, path+"."+name, out, depth+1)
	}
}

// populatedFieldPaths records the paths of v whose value is non-zero in at
// least one instance.
func populatedFieldPaths(v reflect.Value, path string, out map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			populatedFieldPaths(v.Elem(), path, out)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			populatedFieldPaths(v.Index(i), path+"[]", out)
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			name, ok := jsonName(f)
			if !ok {
				continue
			}
			if !v.Field(i).IsZero() {
				out[path+"."+name] = true
			}
			populatedFieldPaths(v.Field(i), path+"."+name, out)
		}
	}
}

func jsonName(f reflect.StructField) (string, bool) {
	if !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, true
}

// neverRecorded lists the field paths no run can fill, with the rule that
// keeps them empty; the schema refuses most of them.
var neverRecorded = map[string]string{
	// intent_judgment is kept only on DIVERGED hypotheses, and neither list
	// holds one.
	"report.reproduced_issues[].intent_judgment":    "judgment only on DIVERGED",
	"report.intent_test_failures[].intent_judgment": "judgment only on DIVERGED",
	// The mutation ledger is never cached (no kind ends in _base) and never
	// captures a payload.
	"report.mutation.checks[].cache":                      "mutation checks are never cached",
	"report.mutation.checks[].cache.status":               "mutation checks are never cached",
	"report.mutation.checks[].cache.key":                  "mutation checks are never cached",
	"report.mutation.checks[].cache.recorded_at":          "mutation checks are never cached",
	"report.mutation.checks[].cache.recorded_run":         "mutation checks are never cached",
	"report.mutation.checks[].cache.recorded_check":       "mutation checks are never cached",
	"report.mutation.checks[].cache.recorded_duration_ms": "mutation checks are never cached",
	"report.mutation.checks[].cache.live_runs":            "mutation checks are never cached",
	"report.mutation.checks[].results":                    "mutation checks capture no payload",
}

// fullyPopulatedReport is populatedReport with every field that fixture
// leaves zero, apart from neverRecorded, set to a value a run could record.
func fullyPopulatedReport() Report {
	r := populatedReport()
	r.Change.Files = append(r.Change.Files, ChangedFile{Path: "assets/logo.png", Status: "M", Binary: true, Hunks: []Hunk{}})
	r.Checks = append(r.Checks, Check{ID: "check-15", Kind: CheckExistingTest, Status: "FAIL", Command: []string{"go", "test", "-json", "./calc"}, ExitCode: 1, DurationMS: 40, Output: "{\"Action\":\"output\"}", Truncated: true})
	r.Coverage.NotExecutedLines, r.Coverage.NoBlockLines, r.Coverage.NotMeasuredLines = 1, 1, 1
	r.Coverage.Files = append(r.Coverage.Files, CoverageFile{Path: "calc/other.go", Status: "measured", AddedLines: 3, NotExecutedLines: 1, NoBlockLines: 1, NotMeasuredLines: 1})
	r.Execution.Budget.DeadlineReached = true
	c := &r.Execution.Cache
	c.Runtime, c.Rejected, c.WriteFailures, c.Evicted, c.Contradicted = "28.4.0 linux/amd64", 1, 1, 1, 1
	f := &r.Fuzz.Functions[0]
	f.Compared, f.Unstable, f.Unconfirmed, f.NotRecorded = 61, 1, 1, 1
	r.Fuzz.Skipped = append(r.Fuzz.Skipped, FuzzSkip{Path: "calc/calc.go", Line: 9, Symbol: "Scale", Reason: "signature changed"})
	r.Fuzz.SkippedTotal = 2
	r.Impact.Languages = []string{"go", "python"}
	fn := &r.Impact.ChangedFunctions[0]
	fn.Indexed, fn.Reason, fn.TestsTotal = true, "r", 2
	fn.Tests[0].Via = []string{"shop.TestTotal", "shop.Checkout", "calc.Discount"}
	fn.Tests = append(fn.Tests, ImpactTest{Name: "TestTotalZero", Path: "shop/zero_test.go", Line: 3, Package: "example.com/shop", Depth: 1, Resolution: ResolutionStatic, FileChanged: true, Reason: "the change modified this test file"})
	r.Mutation.CoverageSkipped, r.Mutation.Invalid, r.Mutation.TimedOut, r.Mutation.Inconclusive = 1, 1, 1, 1
	r.Mutation.Mutants = append(r.Mutation.Mutants,
		Mutant{ID: "mutant-4", Path: "calc/calc.go", Line: 3, Column: 9, EndLine: 4, Symbol: "Discount", Package: "example.com/calc", Operator: "boundary", Original: "<", Mutated: "<=", Status: MutantInvalid, Reason: "r"},
		Mutant{ID: "mutant-5", Path: "calc/calc.go", Line: 3, Package: "example.com/calc", Operator: "boundary", Original: ">", Mutated: ">=", Status: MutantTimeout, Reason: "r"},
		Mutant{ID: "mutant-6", Path: "calc/calc.go", Line: 3, Package: "example.com/calc", Operator: "flip_boolean", Original: "true", Mutated: "false", Status: MutantInconclusive, Reason: "r"})
	r.Mutation.Checks = append(r.Mutation.Checks, Check{ID: "mutation-check-4", Kind: CheckMutant, Status: "FAIL", Command: []string{"go", "test", "-json", "./calc"}, ExitCode: 1, DurationMS: 25, Output: "{\"Action\":\"output\"}", Truncated: true})
	r.Prepare.Network = true
	r.ReproducedIssues[0].CriterionID = "AC-1"
	r.Hypotheses[0].CriterionID = "AC-1"
	r.PlanDrift = &PlanDrift{
		Status: PlanDriftDrifted, PlanSHA256: strings.Repeat("a", 64), IntentSHA256: strings.Repeat("b", 64), BaseMatches: true,
		CriticalGlobs: []string{"**/auth/**"},
		Contract: PlanContract{
			BaseCommit: strings.Repeat("c", 40), Files: []string{"calc/calc.go", "go.mod"},
			Symbols:       []PlanContractSymbol{{Path: "calc/calc.go", Name: "Discount", Change: PlanSymbolSignature}},
			CriticalFiles: []string{"calc/auth/token.go"}, Manifests: []string{"go.mod"}, Dependencies: true, NewPackages: []string{"calc/rules"},
		},
		Items: []PlanDriftItem{{Kind: DriftUnannouncedExported, Severity: "high", Path: "calc/calc.go", Symbol: "Scale", Summary: "Exported declaration changed although the plan did not announce it"}},
		Note:  PlanDriftNote,
	}
	return r
}

func TestSchemaAcceptsAFullyPopulatedReport(t *testing.T) {
	r := fullyPopulatedReport()
	all, populated := map[string]bool{}, map[string]bool{}
	modelFieldPaths(reflect.TypeOf(r), "report", all, 0)
	populatedFieldPaths(reflect.ValueOf(r), "report", populated)
	var missing []string
	for path := range all {
		if !populated[path] && neverRecorded[path] == "" {
			missing = append(missing, path)
		}
	}
	for path := range neverRecorded {
		if !all[path] {
			t.Errorf("neverRecorded names %s, which is not a model field", path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("fields the fixture leaves zero everywhere (%d):\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	v := newValidator(loadSchema(t))
	if errs := v.validateDocument(decodeJSON(t, data)); len(errs) > 0 {
		sort.Strings(errs)
		t.Errorf("the schema rejects a fully populated report:\n  %s", strings.Join(errs, "\n  "))
	}
}

// A report whose baseline run was replayed from the execution cache records
// the replay's audit event with status HIT (harness run.go, contract §1.11),
// and must validate like any other report.
func TestSchemaAcceptsAReplayedCheckAndItsHitAuditEvent(t *testing.T) {
	r := fullyPopulatedReport()
	if !r.Checks[4].Replayed() {
		t.Fatal("the fixture's fuzz_base check is expected to be a replay")
	}
	r.Audit = append(r.Audit, AuditEvent{Time: r.GeneratedAt, Tool: AuditStagePrefix + "execution_cache", Arguments: "{\"check\":\"check-5\"}", Status: "HIT"})
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	v := newValidator(loadSchema(t))
	if errs := v.validateDocument(decodeJSON(t, data)); len(errs) > 0 {
		sort.Strings(errs)
		t.Errorf("the schema rejects a report with a cache replay:\n  %s", strings.Join(errs, "\n  "))
	}
	r.Audit[len(r.Audit)-1].Status = "MISS"
	data, _ = json.Marshal(r)
	if len(v.validateDocument(decodeJSON(t, data))) == 0 {
		t.Error("the schema accepts an unknown audit status")
	}
}
