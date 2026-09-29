package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

func validProposal() model.PlanProposal {
	return model.PlanProposal{
		Summary: "Add a discount rule",
		Steps:   []string{" change Discount "},
		Files: []model.PlannedFile{
			{Path: "calc/calc.go", Change: model.PlanFileModify},
			{Path: "calc/calc_test.go", Change: model.PlanFileModify},
		},
		Symbols: []model.PlannedSymbol{{Path: "calc/calc.go", Name: "(*Cart).Total", Change: model.PlanSymbolBody}},
	}
}

func TestNormalizeAcceptsAndSorts(t *testing.T) {
	p := validProposal()
	p.Files[0], p.Files[1] = p.Files[1], p.Files[0]
	got, err := Normalize(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files[0].Path != "calc/calc.go" || got.Symbols[0].Name != "Cart.Total" || got.Steps[0] != "change Discount" {
		t.Fatalf("normalized = %+v", got)
	}
	if got.Dependencies == nil || got.Assumptions == nil {
		t.Error("lists must serialize as arrays")
	}
}

func TestNormalizeRejectsInvalidPlans(t *testing.T) {
	cases := map[string]func(*model.PlanProposal){
		"no summary":         func(p *model.PlanProposal) { p.Summary = " " },
		"no files":           func(p *model.PlanProposal) { p.Files = nil },
		"traversal":          func(p *model.PlanProposal) { p.Files[0].Path = "../etc/passwd" },
		"unclean":            func(p *model.PlanProposal) { p.Files[0].Path = "calc//calc.go" },
		"bad change":         func(p *model.PlanProposal) { p.Files[0].Change = "rewrite" },
		"old path on modify": func(p *model.PlanProposal) { p.Files[0].OldPath = "x.go" },
		"rename without old": func(p *model.PlanProposal) { p.Files[0].Change = model.PlanFileRename },
		"duplicate path":     func(p *model.PlanProposal) { p.Files[1].Path = p.Files[0].Path },
		"symbol off plan":    func(p *model.PlanProposal) { p.Symbols[0].Path = "other.go" },
		"bad symbol name":    func(p *model.PlanProposal) { p.Symbols[0].Name = "a b" },
		"body in added file": func(p *model.PlanProposal) { p.Files[0].Change = model.PlanFileAdd },
		"dependency off plan": func(p *model.PlanProposal) {
			p.Dependencies = []model.PlannedDependency{{Manifest: "go.mod", Name: "x", Change: "add"}}
		},
	}
	for name, mutate := range cases {
		p := validProposal()
		mutate(&p)
		if _, err := Normalize(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func evaluate(t *testing.T, p model.PlanProposal, tree Tree) (model.PlanAssessment, model.PlanContract) {
	t.Helper()
	p, err := Normalize(p)
	if err != nil {
		t.Fatal(err)
	}
	a, c, _, err := Evaluate(context.Background(), Input{
		BaseCommit: "base", Proposal: p, SensitiveGlobs: []string{"**/auth/**"}, Tree: tree,
		Index: func(context.Context) (*symbols.Index, *model.Impact, error) {
			return nil, &model.Impact{Status: model.ImpactUnavailable, Reason: "no go.mod"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, c
}

func kinds(a model.PlanAssessment) map[string]int {
	out := map[string]int{}
	for _, s := range a.Signals {
		out[s.Kind]++
	}
	return out
}

func flagged(a model.PlanAssessment) map[string]bool {
	out := map[string]bool{}
	for _, c := range a.Categories {
		out[c.Name] = c.Flagged
	}
	return out
}

func TestEvaluateAppliesTheFixedRules(t *testing.T) {
	tree := Tree{"calc/calc.go": true, "calc/calc_test.go": true, "internal/auth/token.go": true, "go.mod": true, "old/legacy.go": true}
	p := model.PlanProposal{
		Summary: "s",
		Files: []model.PlannedFile{
			{Path: "calc/calc.go", Change: model.PlanFileModify},
			{Path: "internal/auth/token.go", Change: model.PlanFileModify},
			{Path: "go.mod", Change: model.PlanFileModify},
			{Path: "rules/rules.go", Change: model.PlanFileAdd},
			{Path: "old/legacy.go", Change: model.PlanFileDelete},
			{Path: "calc/missing.go", Change: model.PlanFileModify},
		},
		Symbols: []model.PlannedSymbol{
			{Path: "calc/calc.go", Name: "Discount", Change: model.PlanSymbolSignature},
			{Path: "internal/auth/token.go", Name: "verifyToken", Change: model.PlanSymbolBody},
			{Path: "rules/rules.go", Name: "Rule", Change: model.PlanSymbolAdd},
		},
		Dependencies: []model.PlannedDependency{{Manifest: "go.mod", Name: "example.com/x", Change: model.PlanDependencyAdd}},
	}
	a, c := evaluate(t, p, tree)
	k := kinds(a)
	for kind, want := range map[string]int{
		model.PlanSignalCriticalPath: 1, model.PlanSignalDependency: 1, model.PlanSignalNewPackage: 1, model.PlanSignalDeletion: 1,
		model.PlanSignalExportedSignature: 1, model.PlanSignalSensitiveSymbol: 1, model.PlanSignalInconsistent: 1, model.PlanSignalUnmeasured: 2,
	} {
		if k[kind] != want {
			t.Errorf("%s signals = %d, want %d (all: %v)", kind, k[kind], want, k)
		}
	}
	f := flagged(a)
	if !f[model.PlanCategoryCriticalParts] || !f[model.PlanCategoryArchitecture] || !f[model.PlanCategoryOtherMajor] || f[model.PlanCategoryRegressionRisk] || !a.Major {
		t.Errorf("categories = %+v", a.Categories)
	}
	if len(c.CriticalFiles) != 1 || c.CriticalFiles[0] != "internal/auth/token.go" || !c.Dependencies || len(c.NewPackages) != 1 || c.NewPackages[0] != "rules" {
		t.Errorf("contract = %+v", c)
	}
	if a.Index.Status != model.ImpactUnavailable {
		t.Errorf("index = %+v", a.Index)
	}
	for _, s := range a.Signals {
		if !strings.HasPrefix(s.ID, "sig-") {
			t.Errorf("signal without an ID: %+v", s)
		}
	}
}

func TestEvaluateQuietPlanIsNotMajor(t *testing.T) {
	a, c := evaluate(t, model.PlanProposal{Summary: "s", Files: []model.PlannedFile{{Path: "README.md", Change: model.PlanFileModify}}}, Tree{"README.md": true})
	if a.Major || len(a.Signals) != 0 {
		t.Errorf("a documentation-only plan = major %v, signals %v", a.Major, a.Signals)
	}
	if len(c.Files) != 1 || c.Dependencies {
		t.Errorf("contract = %+v", c)
	}
	a, _ = evaluate(t, model.PlanProposal{Summary: "s", Files: manyFiles(LargeScopeFiles)}, Tree{})
	if kinds(a)[model.PlanSignalLargeScope] != 1 || !a.Major {
		t.Errorf("a large plan must be flagged: %v", kinds(a))
	}
}

func manyFiles(n int) []model.PlannedFile {
	var out []model.PlannedFile
	for i := 0; i < n; i++ {
		out = append(out, model.PlannedFile{Path: "docs/" + string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ".md", Change: model.PlanFileAdd})
	}
	return out
}

func TestDriftComparesTheDiffWithTheContract(t *testing.T) {
	contract := model.PlanContract{
		BaseCommit: "base", Files: []string{"calc/calc.go", "calc/untouched.go", "old/name.go", "new/name.go"},
		Symbols:       []model.PlanContractSymbol{{Path: "calc/calc.go", Name: "Discount", Change: model.PlanSymbolSignature}},
		CriticalFiles: []string{}, Manifests: []string{},
	}
	change := model.Change{BaseCommit: "base", Files: []model.ChangedFile{
		{Path: "calc/calc.go", Status: "M", Hunks: []model.Hunk{{Lines: []model.DiffLine{{Kind: "add", NewLine: 7, Content: "x"}}}}},
		{Path: "new/name.go", OldPath: "old/name.go", Status: "R"},
		{Path: "calc/extra.go", Status: "A"},
		{Path: "calc/extra_test.go", Status: "A"},
		{Path: "internal/auth/token.go", Status: "M"},
		{Path: "go.mod", Status: "M"},
	}}
	signals := []model.Signal{
		{Kind: "public_api_change", Path: "calc/calc.go", Symbol: "Discount", Summary: "Exported Go declaration changed"},
		{Kind: "public_api_change", Path: "calc/calc.go", Symbol: "Scale", Summary: "Exported Go declaration removed"},
		{Kind: "public_api_change", Path: "calc/extra.go", Symbol: "Extra", Summary: "Exported Go declaration added"},
	}
	items := Drift(contract, []string{"**/auth/**"}, change, signals)
	got := map[string][]string{}
	for _, it := range items {
		got[it.Kind] = append(got[it.Kind], it.Path+"#"+it.Symbol+"#"+it.Severity)
	}
	want := map[string][]string{
		model.DriftUnplannedFile:       {"calc/extra.go##medium", "go.mod##medium", "internal/auth/token.go##medium"},
		model.DriftUnplannedTestFile:   {"calc/extra_test.go##low"},
		model.DriftPlannedUntouched:    {"calc/untouched.go##low"},
		model.DriftUnannouncedExported: {"calc/calc.go#Scale#high"},
		model.DriftUnannouncedCritical: {"internal/auth/token.go##high"},
		model.DriftUnannouncedManifest: {"go.mod##medium"},
	}
	for kind, paths := range want {
		if strings.Join(got[kind], ",") != strings.Join(paths, ",") {
			t.Errorf("%s = %v, want %v", kind, got[kind], paths)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected kinds: %v", got)
	}
	if DriftStatus(items) != model.PlanDriftDrifted {
		t.Error("an unplanned file must drift")
	}
	sigs := DriftSignals(items, change)
	for _, s := range sigs {
		if s.Kind != model.SignalPlanDrift || s.Path == "calc/untouched.go" {
			t.Errorf("drift signal %+v", s)
		}
		if s.Path == "calc/calc.go" && s.Line != 7 {
			t.Errorf("a drift signal lands on the first changed line, got %d", s.Line)
		}
	}

	// Within the plan: only low items, conforming.
	contract.Files = append(contract.Files, "calc/extra.go", "calc/extra_test.go", "internal/auth/token.go", "go.mod")
	contract.CriticalFiles, contract.Manifests = []string{"internal/auth/token.go"}, []string{"go.mod"}
	contract.Symbols = append(contract.Symbols, model.PlanContractSymbol{Path: "calc/calc.go", Name: "Scale", Change: model.PlanSymbolRemove})
	items = Drift(contract, []string{"**/auth/**"}, change, signals)
	if DriftStatus(items) != model.PlanDriftConforming || len(items) != 1 || items[0].Kind != model.DriftPlannedUntouched {
		t.Errorf("a change within the plan = %v", items)
	}
}
