package knowledge

import (
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const sample = `# Team notes

Hand-written preamble.

## Refunds are validated by the gateway
- kind: architecture
- paths: pay/**, gateway/refund.go
- updated: 2026-09-01

The refund handler trusts the amount.

` + "```" + `
## not a heading inside a fence
` + "```" + `

## Errors are wrapped
- kind: convention

Wrap every returned error with context.
`

func TestParseAndRender(t *testing.T) {
	b, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if b.Preamble != "# Team notes\n\nHand-written preamble." || len(b.Entries) != 2 {
		t.Fatalf("parsed %+v", b)
	}
	first := b.Entries[0]
	if first.Kind != model.KnowledgeArchitecture || strings.Join(first.Paths, "|") != "pay/**|gateway/refund.go" || first.Updated != "2026-09-01" || !strings.Contains(first.Text, "## not a heading inside a fence") {
		t.Fatalf("first entry %+v", first)
	}
	again, err := Parse(b.Render())
	if err != nil || len(again.Entries) != 2 || again.Entries[0].Text != first.Text || again.Preamble != b.Preamble {
		t.Fatalf("render does not round-trip: %v\n%s", err, b.Render())
	}
	if empty := (&Base{}).Render(); !strings.HasPrefix(string(empty), "# Probe knowledge base") {
		t.Fatalf("an empty base renders its header: %s", empty)
	}
	if _, err := Parse((&Base{}).Render()); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejectsInvalidEdits(t *testing.T) {
	for name, text := range map[string]string{
		"duplicate title": "## A\n- kind: note\n\nx\n\n## a\n- kind: note\n\ny\n",
		"unknown kind":    "## A\n- kind: gossip\n\nx\n",
		"open fence":      "## A\n- kind: note\n\n```\nx\n",
		"not utf-8":       "## A\n\xff\n",
		"long title":      "## " + strings.Repeat("t", MaxTitle+1) + "\n",
	} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMergeAddsReplacesAndRemoves(t *testing.T) {
	b, _ := Parse([]byte(sample))
	added, replaced, removed := b.Merge([]model.KnowledgeUpdate{
		{Title: "errors are WRAPPED", Kind: model.KnowledgeConvention, Text: "Wrap with fmt.Errorf and %w."},
		{Title: "Refunds are validated by the gateway", Kind: model.KnowledgeArchitecture, Obsolete: true},
		{Title: "Cart totals", Kind: model.KnowledgeComponent, Paths: []string{"cart/**"}, Text: "Totals are computed in cents."},
		{Title: "Never existed", Kind: model.KnowledgeNote, Obsolete: true},
	}, "2026-09-30 (review of abc)")
	if added != 1 || replaced != 1 || removed != 1 || len(b.Entries) != 2 {
		t.Fatalf("added %d replaced %d removed %d: %+v", added, replaced, removed, b.Entries)
	}
	if b.Entries[0].Title != "errors are WRAPPED" || b.Entries[0].Updated != "2026-09-30 (review of abc)" || b.Entries[1].Title != "Cart totals" {
		t.Fatalf("entries %+v", b.Entries)
	}
}

func TestRelevantPrefersMatchesWithinBudget(t *testing.T) {
	b := &Base{Entries: []model.KnowledgeEntry{
		{Title: "General", Kind: model.KnowledgeArchitecture, Paths: []string{}, Text: "g"},
		{Title: "Pay", Kind: model.KnowledgeRisk, Paths: []string{"pay/**"}, Text: "p"},
		{Title: "Cart", Kind: model.KnowledgeComponent, Paths: []string{"cart/**"}, Text: "c"},
	}}
	got := b.Relevant([]string{"pay/refund.go"}, 1000)
	if len(got) != 2 || got[0].Title != "Pay" || got[1].Title != "General" {
		t.Fatalf("relevant = %+v", got)
	}
	if got := b.Relevant([]string{"pay/refund.go"}, 70); len(got) != 1 || got[0].Title != "Pay" {
		t.Fatalf("budget = %+v", got)
	}
}

func TestValidUpdate(t *testing.T) {
	ok := model.KnowledgeUpdate{Title: "T", Kind: model.KnowledgeRisk, Paths: []string{"a/**"}, Text: "x"}
	if err := ValidUpdate(ok); err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]model.KnowledgeUpdate{
		"no text":     {Title: "T", Kind: model.KnowledgeRisk},
		"heading":     {Title: "T", Kind: model.KnowledgeRisk, Text: "a\n## b"},
		"comma path":  {Title: "T", Kind: model.KnowledgeRisk, Text: "x", Paths: []string{"a,b"}},
		"bad kind":    {Title: "T", Kind: "x", Text: "x"},
		"multi-line":  {Title: "T\nU", Kind: model.KnowledgeRisk, Text: "x"},
		"long reason": {Title: "T", Kind: model.KnowledgeRisk, Text: "x", Reason: strings.Repeat("r", MaxReason+1)},
	} {
		if err := ValidUpdate(u); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidUpdate(model.KnowledgeUpdate{Title: "T", Kind: model.KnowledgeRisk, Obsolete: true}); err != nil {
		t.Fatalf("an obsolete update needs no text: %v", err)
	}
}
