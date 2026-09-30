package model

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const planSchemaPath = "../../schema/plan.schema.json"

func loadPlanSchema(t *testing.T) *schemaDoc {
	t.Helper()
	data, err := os.ReadFile(planSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		t.Fatalf("plan schema: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		t.Fatal(err)
	}
	defs, _ := root["$defs"].(map[string]any)
	return &schemaDoc{root: root, defs: defs}
}

func populatedPlan() Plan {
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	return Plan{
		Format: PlanFormat, Version: PlanVersion, ToolVersion: "v0.5.0", GeneratedAt: at,
		Intent: "Add a discount rule", IntentSHA256: strings.Repeat("a", 64), BaseRef: "main", BaseCommit: strings.Repeat("b", 40),
		Policy: Policy{Source: PolicyBaseRef, Commit: strings.Repeat("b", 40), Path: ".probe.json"}, Model: "planner-model",
		Proposal: PlanProposal{
			Summary: "Add a rule", Steps: []string{"edit calc"},
			Files: []PlannedFile{
				{Path: "calc/calc.go", Change: PlanFileModify, Reason: "rule"},
				{Path: "calc/rules/rules.go", OldPath: "calc/rules.go", Change: PlanFileRename, Reason: "move"},
			},
			Symbols:      []PlannedSymbol{{Path: "calc/calc.go", Name: "Discount", Change: PlanSymbolSignature, Reason: "new parameter"}},
			Dependencies: []PlannedDependency{{Manifest: "go.mod", Name: "example.com/x", Change: PlanDependencyAdd, Version: "v1.2.3"}},
			Assumptions:  []string{"callers can pass a default"},
		},
		Assessment: PlanAssessment{
			Major:         true,
			Categories:    []PlanCategory{{Name: PlanCategoryArchitecture, Flagged: true, SignalIDs: []string{"sig-0123456789abcdef"}}},
			Signals:       []Signal{{ID: "sig-0123456789abcdef", Kind: PlanSignalExportedSignature, Path: "calc/calc.go", Line: 3, EndLine: 4, Side: "old", Scope: SignalScopeFile, Symbol: "Discount", Severity: "high", Summary: "The plan changes an exported API", Evidence: "exported"}},
			CriticalPaths: []PlanCriticalPath{{Path: "calc/auth/token.go", Pattern: "**/auth/**"}},
			Symbols: []PlanSymbolImpact{{
				Path: "calc/calc.go", Name: "Discount", Change: PlanSymbolSignature, Found: true, Symbol: "example.com/calc.Discount", Line: 3, Exported: true, Signature: "func Discount(int) int",
				CallersTotal: 1, Callers: []ImpactCaller{{Path: "shop/shop.go", Line: 9, Symbol: "example.com/shop.Total", Depth: 1, Resolution: ResolutionStatic}},
				TestsTotal: 1, Tests: []ImpactTest{{Name: "TestTotal", Path: "shop/shop_test.go", Line: 5, Package: "example.com/shop", Depth: 2, Resolution: ResolutionStatic, Via: []string{"shop.TestTotal", "calc.Discount"}, FileChanged: true, EvidenceID: "evidence-1", Status: StatusUnverified, Reason: "r"}},
				Complete: true, Reason: "r",
			}},
			Manifests: []string{"go.mod"}, NewPackages: []string{"calc/rules"},
			Index:           PlanIndex{Status: ImpactIndexed, Reason: "r"},
			Thresholds:      PlanAssessmentBoundary{WideImpactCallers: 10, LargeScopeFiles: 20},
			Inconsistencies: []string{"x"},
		},
		Contract:   PlanContract{BaseCommit: strings.Repeat("b", 40), Files: []string{"calc/calc.go"}, Symbols: []PlanContractSymbol{{Path: "calc/calc.go", Name: "Discount", Change: PlanSymbolSignature}}, CriticalFiles: []string{"calc/auth/token.go"}, Manifests: []string{"go.mod"}, Dependencies: true, NewPackages: []string{"calc/rules"}},
		Unverified: []string{"u"},
		Audit:      []AuditEvent{{Time: at, Tool: "planner_completion", Arguments: "iteration=1", Status: "OK", DurationMS: 1, Agent: "planner"}},
		Note:       PlanNote, ExitCode: 2,
	}
}

func TestPlanSchemaMirrorsModel(t *testing.T) {
	s := loadPlanSchema(t)
	s.checkMirror(t, "plan", reflect.TypeOf(Plan{}), s.root, true, map[string]bool{})
}

func TestPlanSchemaAcceptsAFullyPopulatedPlan(t *testing.T) {
	p := populatedPlan()
	all, populated := map[string]bool{}, map[string]bool{}
	modelFieldPaths(reflect.TypeOf(p), "plan", all, 0)
	populatedFieldPaths(reflect.ValueOf(p), "plan", populated)
	var missing []string
	for path := range all {
		if !populated[path] {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("fields the fixture leaves zero: %s", strings.Join(missing, ", "))
	}
	data, _ := json.Marshal(p)
	v := newValidator(loadPlanSchema(t))
	if errs := v.validateDocument(decodeJSON(t, data)); len(errs) > 0 {
		t.Errorf("the plan schema rejects a populated plan:\n  %s", strings.Join(errs, "\n  "))
	}
	p.Proposal.Files[0].Change = "rewrite"
	data, _ = json.Marshal(p)
	if len(v.validateDocument(decodeJSON(t, data))) == 0 {
		t.Error("the plan schema accepts an unknown file change")
	}
}
