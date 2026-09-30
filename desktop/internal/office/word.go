package office

import (
	"fmt"
	"strings"
)

// wordDoc is the comparable content of a Word document.
type wordDoc struct {
	sections       []wordSection
	trackedChanges int
	trackRevisions bool
	protected      bool
	comments       int
}

type wordSection struct {
	label      string // "Paragraph", "Header", "Footer", "Footnote"
	paragraphs []string
}

func readWord(p *pkg) (*wordDoc, error) {
	body, ok := p.read("word/document.xml")
	if !ok {
		return nil, ErrUnreadable
	}
	root := parseTree(body)
	doc := &wordDoc{
		sections:       []wordSection{{label: "Paragraph", paragraphs: paragraphs(root)}},
		trackedChanges: len(root.all("ins")) + len(root.all("del")),
	}
	for _, part := range []struct{ prefix, label string }{
		{"word/header", "Header"},
		{"word/footer", "Footer"},
		{"word/footnotes", "Footnote"},
	} {
		var paras []string
		for _, name := range p.list(part.prefix) {
			if !strings.HasSuffix(name, ".xml") {
				continue
			}
			if data, ok := p.read(name); ok {
				paras = append(paras, paragraphs(parseTree(data))...)
			}
		}
		doc.sections = append(doc.sections, wordSection{label: part.label, paragraphs: paras})
	}
	if data, ok := p.read("word/settings.xml"); ok {
		settings := parseTree(data)
		doc.trackRevisions = settings.first("trackRevisions") != nil && settings.first("trackRevisions").attr("val") != "false" && settings.first("trackRevisions").attr("val") != "0"
		if prot := settings.first("documentProtection"); prot != nil {
			enforced := prot.attr("enforcement")
			doc.protected = enforced == "1" || enforced == "true" || enforced == "on"
		}
	}
	if data, ok := p.read("word/comments.xml"); ok {
		doc.comments = len(parseTree(data).all("comment"))
	}
	return doc, nil
}

// paragraphs returns the non-empty paragraphs of a part, text boxes included.
func paragraphs(root *node) []string {
	var out []string
	var rec func(n *node)
	rec = func(n *node) {
		for _, c := range n.children {
			if c.name != "p" {
				rec(c)
				continue
			}
			if text := normalize(paragraphText(c)); text != "" {
				out = append(out, text)
			}
			for _, box := range c.all("txbxContent") {
				rec(box)
			}
		}
	}
	rec(root)
	return out
}

// paragraphText extracts the visible text of a paragraph. Deleted runs of
// pending tracked changes and field codes are not visible, so they are left out.
func paragraphText(p *node) string {
	var b strings.Builder
	var rec func(n *node)
	rec = func(n *node) {
		for _, c := range n.children {
			switch c.name {
			case "txbxContent", "del", "delText", "instrText", "p":
				continue
			case "t":
				b.WriteString(c.text())
			case "tab":
				b.WriteString(" ")
			case "br", "cr":
				b.WriteString(" ")
			case "noBreakHyphen":
				b.WriteString("-")
			default:
				rec(c)
			}
		}
	}
	rec(p)
	return b.String()
}

func compareWord(r *reportBuilder, before, after *pkg) error {
	oldDoc, err := readWord(before)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	newDoc, err := readWord(after)
	if err != nil {
		return fmt.Errorf("current version: %w", err)
	}

	textChanged := false
	for s := range newDoc.sections {
		label := newDoc.sections[s].label
		a, b := oldDoc.sections[s].paragraphs, newDoc.sections[s].paragraphs
		for j, p := range b {
			r.current(fmt.Sprintf("%s %d", label, j+1), p)
		}
		for _, d := range diffSeq(a, b) {
			textChanged = true
			var old, cur, loc string
			switch {
			case d.j < 0:
				old, loc = a[d.i], fmt.Sprintf("%s %d (baseline)", label, d.i+1)
				r.change(Change{Kind: "removed", Location: loc, Before: old})
			case d.i < 0:
				cur, loc = b[d.j], fmt.Sprintf("%s %d", label, d.j+1)
				r.change(Change{Kind: "added", Location: loc, After: cur})
			default:
				old, cur, loc = a[d.i], b[d.j], fmt.Sprintf("%s %d", label, d.j+1)
				r.change(Change{Kind: "modified", Location: loc, Before: old, After: cur})
				r.edited(loc, old, cur)
			}
			textRules(r, loc, old, cur)
		}
	}

	if oldDoc.trackedChanges > 0 && newDoc.trackedChanges == 0 && textChanged {
		r.flag(Finding{Severity: Medium, Rule: "word.tracked-changes-resolved", Title: fmt.Sprintf("%d pending tracked changes were accepted or rejected without trace", oldDoc.trackedChanges)})
	}
	if oldDoc.trackRevisions && !newDoc.trackRevisions {
		r.flag(Finding{Severity: Medium, Rule: "word.tracking-disabled", Title: "Track changes was turned off"})
		r.change(Change{Kind: "modified", Location: "Settings", Before: "Track changes on", After: "Track changes off"})
	}
	if oldDoc.protected && !newDoc.protected {
		r.flag(Finding{Severity: Medium, Rule: "word.protection-removed", Title: "Document protection was removed"})
		r.change(Change{Kind: "modified", Location: "Settings", Before: "Protected", After: "Not protected"})
	}
	if newDoc.comments < oldDoc.comments {
		n := oldDoc.comments - newDoc.comments
		r.flag(Finding{Severity: Low, Rule: "word.comments-removed", Title: fmt.Sprintf("%d review comment(s) deleted", n)})
		r.change(Change{Kind: "removed", Location: "Comments", Before: fmt.Sprintf("%d comment(s)", n)})
	}
	return nil
}
