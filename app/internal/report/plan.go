package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/plan"
)

// Plan output file names, written next to the confidence report.
const (
	PlanJSONName     = "PLAN.json"
	PlanMarkdownName = "PLAN.md"
)

// FinalizePlan recomputes the categories, the major flag and the exit code of
// a plan from its signals, so that an edited document is corrected when it is
// rendered again: 0, or 2 with ci when a category is flagged or something was
// left unverified. An operational failure (4) is set by the caller afterwards.
func FinalizePlan(p *model.Plan, ci bool) {
	p.Format, p.Version, p.Note = model.PlanFormat, model.PlanVersion, model.PlanNote
	if p.Assessment.Signals == nil {
		p.Assessment.Signals = []model.Signal{}
	}
	p.Assessment.Categories, p.Assessment.Major = plan.Categorize(p.Assessment.Signals)
	p.Unverified = unique(p.Unverified)
	p.ExitCode = 0
	if ci && (p.Assessment.Major || len(p.Unverified) > 0) {
		p.ExitCode = 2
	}
}

// SanitizePlan returns a deep copy with every string redacted like a report.
func SanitizePlan(p *model.Plan) *model.Plan {
	if p == nil {
		p = &model.Plan{}
	}
	data, _ := json.Marshal(p)
	var safe model.Plan
	_ = json.Unmarshal(data, &safe)
	redactValue(reflect.ValueOf(&safe).Elem())
	return &safe
}

// WritePlan writes PLAN.json and PLAN.md atomically into dir. Callers run
// FinalizePlan first.
func WritePlan(dir string, p *model.Plan) error {
	safe := SanitizePlan(p)
	data, err := json.MarshalIndent(safe, "", "  ")
	if err != nil {
		return err
	}
	md := renderPlanMarkdown(safe)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, PlanJSONName), append(data, '\n')); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, PlanMarkdownName), md)
}

// PlanMarkdown renders a plan as Markdown.
func PlanMarkdown(p *model.Plan) []byte { return renderPlanMarkdown(SanitizePlan(p)) }

var categoryTitles = map[string]string{
	model.PlanCategoryCriticalParts:  "Critical parts",
	model.PlanCategoryArchitecture:   "Architecture",
	model.PlanCategoryRegressionRisk: "Regression risk",
	model.PlanCategoryOtherMajor:     "Other major change",
}

func renderPlanMarkdown(p *model.Plan) []byte {
	var b bytes.Buffer
	a := p.Assessment
	line(&b, "# Change Plan Assessment\n")
	fmt.Fprintf(&b, "Base: %s (%s)\n\n", inline(p.BaseCommit), inline(p.BaseRef))
	switch p.Policy.Source {
	case model.PolicyBaseRef:
		fmt.Fprintf(&b, "Policy: %s at %s (%s)\n\n", inline(p.Policy.Path), inline(p.BaseRef), inline(p.Policy.Commit))
	case model.PolicyExplicit:
		fmt.Fprintf(&b, "Policy: explicit local file %s\n\n", inline(p.Policy.Path))
	case model.PolicyDefault:
		fmt.Fprintf(&b, "Policy: built-in defaults; no policy file at %s\n\n", inline(p.BaseRef))
	}
	fmt.Fprintf(&b, "Planner model: %s · intent SHA-256 %s\n\n", inline(p.Model), inline(shortHash(p.IntentSHA256)))
	verdict := "No category is flagged. If the implementation conforms to this plan, its checks pass and nothing else in its report requests review, `review --plan` can lift the human review of the change (plan gate)."
	if a.Major {
		verdict = "**Major change**: at least one category is flagged. A human should validate this plan before the work starts, and `review --plan` will require human review of the change whatever it does."
	}
	fmt.Fprintf(&b, "Exit code: %d. %s The model did not judge risk; the categories come from fixed rules applied to the plan.\n\n", p.ExitCode, verdict)

	line(&b, "## Assessment\n")
	line(&b, "| Category | Flagged | Signals |")
	line(&b, "| --- | --- | --- |")
	for _, c := range a.Categories {
		flag := "no"
		if c.Flagged {
			flag = "**yes**"
		}
		fmt.Fprintf(&b, "| %s | %s | %d |\n", categoryTitles[c.Name], flag, len(c.SignalIDs))
	}
	line(&b, "")
	if len(a.Signals) == 0 {
		line(&b, "No rule raised a signal.\n")
	}
	for _, s := range a.Signals {
		where := s.Path
		if s.Symbol != "" {
			where += " (" + s.Symbol + ")"
		}
		fmt.Fprintf(&b, "- **%s** %s — %s: %s [%s]\n", inline(s.Severity), inline(s.Summary), inline(where), inline(s.Evidence), inline(s.Kind))
	}

	line(&b, "\n## Planned Files\n")
	critical := map[string]string{}
	for _, c := range a.CriticalPaths {
		critical[c.Path] = c.Pattern
	}
	for _, f := range p.Proposal.Files {
		name := f.Path
		if f.OldPath != "" {
			name = f.OldPath + " → " + f.Path
		}
		extra := ""
		if glob := critical[f.Path] + critical[f.OldPath]; glob != "" {
			extra = " · critical path (" + glob + ")"
		}
		fmt.Fprintf(&b, "- %s %s%s", inline(f.Change), inline(name), inline(extra))
		if f.Reason != "" {
			fmt.Fprintf(&b, ": %s", inline(f.Reason))
		}
		b.WriteByte('\n')
	}

	if len(a.Symbols) > 0 {
		line(&b, "\n## Planned Symbols\n")
		fmt.Fprintf(&b, "Measured on the static index of the base commit (%s", inline(a.Index.Status))
		if a.Index.Reason != "" {
			fmt.Fprintf(&b, ": %s", inline(a.Index.Reason))
		}
		line(&b, "). Callers are reference sites in non-test code; tests reach the symbol within 3 references. Both are approximate, and an empty list is not proof of absence.\n")
		line(&b, "| Symbol | Change | Exported | Callers | Reaching tests |")
		line(&b, "| --- | --- | --- | --- | --- |")
		for _, s := range a.Symbols {
			callers, tests := "—", "—"
			if s.Found {
				callers, tests = fmt.Sprint(s.CallersTotal), fmt.Sprint(s.TestsTotal)
				if !s.Complete {
					callers, tests = callers+"+", tests+"+"
				}
			}
			exported := "no"
			if s.Exported {
				exported = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", inline(s.Path+" "+s.Name), inline(s.Change), exported, callers, tests)
		}
		for _, s := range a.Symbols {
			if s.Reason != "" {
				fmt.Fprintf(&b, "\n- %s %s: %s", inline(s.Path), inline(s.Name), inline(s.Reason))
			}
		}
		line(&b, "")
	}

	if len(p.Proposal.Dependencies) > 0 || len(a.Manifests) > 0 || len(a.NewPackages) > 0 {
		line(&b, "\n## Dependencies and Packages\n")
		for _, d := range p.Proposal.Dependencies {
			version := ""
			if d.Version != "" {
				version = " " + d.Version
			}
			fmt.Fprintf(&b, "- %s %s%s in %s\n", inline(d.Change), inline(d.Name), inline(version), inline(d.Manifest))
		}
		if len(a.Manifests) > 0 {
			fmt.Fprintf(&b, "- Dependency manifests changed: %s\n", inline(strings.Join(a.Manifests, ", ")))
		}
		if len(a.NewPackages) > 0 {
			fmt.Fprintf(&b, "- New Go packages: %s\n", inline(strings.Join(a.NewPackages, ", ")))
		}
	}

	if len(a.Inconsistencies) > 0 {
		line(&b, "\n## Plan Inconsistencies\n")
		for _, text := range a.Inconsistencies {
			fmt.Fprintf(&b, "- %s\n", inline(text))
		}
	}

	line(&b, "\n## Proposal (model-written, not evidence)\n")
	fmt.Fprintf(&b, "%s\n\n", inline(p.Proposal.Summary))
	for i, step := range p.Proposal.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, inline(step))
	}
	if len(p.Proposal.Assumptions) > 0 {
		line(&b, "\nAssumptions:\n")
		for _, text := range p.Proposal.Assumptions {
			fmt.Fprintf(&b, "- %s\n", inline(text))
		}
	}

	line(&b, "\n## Unverified Areas\n")
	if len(p.Unverified) == 0 {
		line(&b, "No specific unresolved area was recorded. This does not establish that the plan is complete or correct.")
	}
	for _, u := range p.Unverified {
		fmt.Fprintf(&b, "- %s\n", inline(u))
	}
	line(&b, "\n"+inline(p.Note))
	line(&b, "\nCheck the implementation against this plan with `probe review --plan "+PlanJSONName+"`.")
	return b.Bytes()
}
