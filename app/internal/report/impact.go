package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// This file belongs to F6a (symbol index / impact analysis), except
// verifyImpactedTests, which belongs to F6b. The static section is an
// observation: it never supports a hypothesis status, never sets an exit code,
// and requests human review only through impacted tests (F6b), whose statuses
// finalizeImpact derives from verified evidence alone.

// verifyImpactedTests re-derives the status of every
// impacted_test_differential evidence record from its recorded checks (F6b).
// It returns FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE for a record whose
// checks support exactly that stored status, and nothing for any other record
// (see impactedEvidenceStatus in impacted.go).
func verifyImpactedTests(r *model.Report, l *ledger) map[string]string {
	out := map[string]string{}
	for _, recorded := range r.Evidence {
		if recorded.Kind != model.EvidenceImpactedTestDifferential {
			continue
		}
		e, ok := l.item(recorded.ID)
		if !ok || e.Kind != model.EvidenceImpactedTestDifferential {
			continue
		}
		if status := impactedEvidenceStatus(e, l); status != "" && status == e.Status {
			out[e.ID] = status
		}
	}
	return out
}

// Markdown bounds of the Impact Analysis section; the JSON keeps every listed
// entry.
const (
	maxImpactFunctionsShown = 20
	maxImpactEntriesShown   = 5
	// impactTestUnsupportedText replaces the reason of an impacted test whose
	// stored FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE is not supported by
	// verified evidence (the same text as for changed baseline tests).
	impactTestUnsupportedText = "the recorded evidence does not support this result"
)

// finalizeImpact sets each impacted test's status from the verified ledger and
// returns true when the section needs a human: an impacted test that fails on
// candidate code or is unverified, or impacted tests that did not run. It
// mutates only r.Impact and is idempotent.
//
// A test's status is FAILS_ON_CANDIDATE or PASSES_ON_CANDIDATE only when its
// evidence_id resolves to exactly one impacted_test_differential record for
// that test (same name, same path) whose stored status a verifier re-derived;
// a cited record that does not qualify gives UNVERIFIED, and a test without
// evidence has no status. A status stored in a saved report is never trusted.
func finalizeImpact(r *model.Report, l *ledger) bool {
	im := r.Impact
	if im == nil {
		return false
	}
	// The note is fixed text: a stored note, edited or not, is never rendered.
	im.Note = model.ImpactNoteFor(im.Languages)
	needsHuman := im.TestsStatus == model.ImpactTestsNotRun
	for i := range im.ChangedFunctions {
		f := &im.ChangedFunctions[i]
		for j := range f.Tests {
			t := &f.Tests[j]
			stored := t.Status
			t.Status = impactTestStatus(*t, l)
			if t.Status == model.StatusUnverified && (stored == model.StatusFailsOnCandidate || stored == model.StatusPassesOnCandidate) {
				// A stored result the evidence does not support keeps no
				// reason of its own (the result it explained is gone).
				t.Reason = impactTestUnsupportedText
			}
			if t.Status == model.StatusFailsOnCandidate || t.Status == model.StatusUnverified {
				needsHuman = true
			}
		}
	}
	return needsHuman
}

// impactTestStatus is the status of one impacted test from its cited evidence.
func impactTestStatus(t model.ImpactTest, l *ledger) string {
	if t.EvidenceID == "" {
		return ""
	}
	e, ok := l.item(t.EvidenceID)
	if !ok || e.Kind != model.EvidenceImpactedTestDifferential || len(e.TestNames) != 1 || e.TestNames[0] != t.Name || e.Path != t.Path {
		return model.StatusUnverified
	}
	switch status := l.verified[t.EvidenceID]; status {
	case model.StatusFailsOnCandidate, model.StatusPassesOnCandidate:
		if status == e.Status {
			return status
		}
	}
	return model.StatusUnverified
}

// impactTargets returns a high review target at the declaration of each
// impacted test that fails on candidate code. The static link between the test
// and the changed function is approximate, and the failure may come from any
// part of the change or from flakiness.
func impactTargets(r *model.Report) []extraTarget {
	if r.Impact == nil {
		return nil
	}
	var out []extraTarget
	seen := map[string]bool{}
	for _, f := range r.Impact.ChangedFunctions {
		for _, t := range f.Tests {
			key := fmt.Sprintf("%s\x00%d", t.Path, t.Line)
			if t.Status != model.StatusFailsOnCandidate || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, extraTarget{path: t.Path, side: "new", start: t.Line, end: t.Line, severity: "high",
				reason: "Existing test fails on candidate code (FAILS_ON_CANDIDATE); its static link to a changed function is approximate"})
		}
	}
	return out
}

// writeImpact renders "## Impact Analysis" when r.Impact is present (F6a):
// the index status, each changed function with its callers and reaching
// tests (a few per function; the JSON lists up to 10 callers and 20 tests),
// the impacted-test status and the fixed note. Every string goes through
// inline().
func writeImpact(b *bytes.Buffer, r *model.Report) {
	im := r.Impact
	if im == nil {
		return
	}
	line(b, "\n## Impact Analysis\n")
	searched := false
	lexical := len(im.Languages) > 0
	switch im.Status {
	case model.ImpactIndexed:
		searched = true
		if lexical {
			fmt.Fprintf(b, "Static index of %d %s files (approximate; %s). Callers are reference sites in unchanged, non-test code; tests are existing tests that reach a changed function within 3 references.\n\n", im.IndexedFiles, impactLanguages(im.Languages), impactMethods(im.Languages))
		} else {
			fmt.Fprintf(b, "Static Go index of %d files (approximate). Callers are reference sites in unchanged, non-test code; tests are existing Go tests that reach a changed function within 3 references.\n\n", im.IndexedFiles)
		}
	case model.ImpactLimited:
		searched = true
		if lexical {
			fmt.Fprintf(b, "Static index of %d %s files (approximate), limited: %s. Callers and tests of changed functions may be missing.\n\n", im.IndexedFiles, impactLanguages(im.Languages), inline(noFinalPeriod(im.Reason)))
		} else {
			fmt.Fprintf(b, "Static Go index of %d files (approximate), limited: %s. Callers and tests of changed functions may be missing.\n\n", im.IndexedFiles, inline(noFinalPeriod(im.Reason)))
		}
	case model.ImpactUnavailable:
		if lexical {
			fmt.Fprintf(b, "The static index is unavailable: %s. Callers and tests of changed functions were not searched.\n\n", inline(orNone(im.Reason)))
		} else {
			fmt.Fprintf(b, "The static Go index is unavailable: %s. Callers and tests of changed functions were not searched.\n\n", inline(orNone(im.Reason)))
		}
	case model.ImpactNotApplicable:
		line(b, "No indexable Go, TypeScript/JavaScript, Python or Rust file changed (Go files under testdata or vendor or in directories whose name starts with _, dependency and build directories such as node_modules, target or venv, directories whose name starts with ., and sensitive paths are not indexed), so no static index was built.\n")
	default:
		fmt.Fprintf(b, "Impact analysis status: %s.\n\n", inline(im.Status))
	}
	if searched && len(im.ChangedFunctions) == 0 {
		if lexical {
			line(b, "The changed non-test source files contain no changed function or method.\n")
		} else {
			line(b, "The changed non-test Go files contain no changed function or method.\n")
		}
	}
	for i, f := range im.ChangedFunctions {
		if i == maxImpactFunctionsShown {
			fmt.Fprintf(b, "- … %d more changed functions in confidence-report.json\n", len(im.ChangedFunctions)-maxImpactFunctionsShown)
			break
		}
		change := "body changed"
		if f.Change == model.ChangeSignatureChanged {
			change = "signature changed"
		}
		fmt.Fprintf(b, "- **%s** (%s) %s:%d–%d\n", inline(f.Symbol), change, inline(f.Path), f.Line, f.EndLine)
		if !f.Indexed {
			fmt.Fprintf(b, "  Not indexed, so its callers and tests were not searched: %s.\n", inline(orNone(f.Reason)))
			continue
		}
		if f.Reason != "" {
			fmt.Fprintf(b, "  Search bound reached: %s.\n", inline(noFinalPeriod(f.Reason)))
		}
		if f.CallersTotal == 0 {
			line(b, "  Callers in unchanged code found by the index: 0 (not proof that none exist).")
		} else {
			fmt.Fprintf(b, "  Callers in unchanged code found by the index: %d.\n", f.CallersTotal)
		}
		for j, c := range f.Callers {
			if j == maxImpactEntriesShown {
				fmt.Fprintf(b, "  - … %d more in confidence-report.json\n", len(f.Callers)-maxImpactEntriesShown)
				break
			}
			fmt.Fprintf(b, "  - caller %s at %s:%d (%s)\n", inline(c.Symbol), inline(c.Path), c.Line, inline(c.Resolution))
		}
		fmt.Fprintf(b, "  Existing tests reaching it within 3 references: %d.\n", f.TestsTotal)
		for j, t := range f.Tests {
			if j == maxImpactEntriesShown {
				fmt.Fprintf(b, "  - … %d more in confidence-report.json\n", len(f.Tests)-maxImpactEntriesShown)
				break
			}
			extra := ""
			if t.FileChanged {
				extra += "; test file changed"
			}
			if t.Status != "" {
				extra += "; " + inline(t.Status)
				if t.EvidenceID != "" {
					extra += " (" + inline(t.EvidenceID) + ")"
				}
			}
			if t.Reason != "" && t.Status != model.StatusFailsOnCandidate && t.Status != model.StatusPassesOnCandidate {
				// Why a selected test got no result (only --impacted-tests
				// records reasons).
				extra += "; " + inline(t.Reason)
			}
			fmt.Fprintf(b, "  - test %s at %s:%d (depth %d, %s%s)\n", inline(t.Name), inline(t.Path), t.Line, t.Depth, inline(t.Resolution), extra)
		}
	}
	if im.TestsStatus != "" {
		reason := ""
		if r := noFinalPeriod(im.TestsReason); r != "" {
			reason = ": " + inline(r)
		}
		fmt.Fprintf(b, "\nImpacted tests: %s%s.\n", inline(im.TestsStatus), reason)
	}
	fmt.Fprintf(b, "\n%s\n", inline(im.Note))
}

// impactLanguages names the languages of a lexical section, e.g. "Go and
// Python" or "TypeScript/JavaScript, Python and Rust".
func impactLanguages(languages []string) string {
	var names []string
	for _, l := range languages {
		switch l {
		case "go":
			names = append(names, "Go")
		case "typescript":
			names = append(names, "TypeScript/JavaScript")
		case "python":
			names = append(names, "Python")
		case "rust":
			names = append(names, "Rust")
		default:
			names = append(names, inline(l))
		}
	}
	switch len(names) {
	case 0:
		return "source"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// impactMethods says how the languages of a lexical section are indexed.
func impactMethods(languages []string) string {
	for _, l := range languages {
		if l == "go" {
			return "Go is type-checked, the other languages are scanned lexically and linked by name"
		}
	}
	return "scanned lexically and linked by name"
}

// ImpactLanguages names the languages of an impact section for display.
func ImpactLanguages(languages []string) string { return impactLanguages(languages) }

// orNone replaces an empty reason with a fixed text, and drops a final
// period: every caller ends the sentence itself.
func orNone(s string) string {
	if s = noFinalPeriod(s); s == "" {
		return "no reason was recorded"
	}
	return s
}

// noFinalPeriod trims s and drops one final period, for a reason that is
// printed inside a sentence the writer ends itself. Reasons are recorded
// verbatim from the harness, whose fixed texts end with a period.
func noFinalPeriod(s string) string {
	return strings.TrimSuffix(strings.TrimSpace(s), ".")
}
