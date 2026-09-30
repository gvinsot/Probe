package office

import (
	"fmt"
	"strings"
)

// Slides keep a stable id across edits, so a slide is compared with itself
// even when the deck is reordered.

type slide struct {
	id         string
	number     int
	hidden     bool
	paragraphs []string
	notes      []string
}

func readPowerPoint(p *pkg) ([]slide, error) {
	data, ok := p.read("ppt/presentation.xml")
	if !ok {
		return nil, ErrUnreadable
	}
	rels := relationships(p, "ppt/_rels/presentation.xml.rels", "ppt")
	var slides []slide
	for i, s := range parseTree(data).all("sldId") {
		part := rels[s.nsAttr("id")]
		sl := slide{id: s.plainAttr("id"), number: i + 1}
		if body, ok := p.read(part); ok {
			root := parseTree(body)
			if sld := root.first("sld"); sld != nil && sld.attr("show") == "0" {
				sl.hidden = true
			}
			sl.paragraphs = drawingParagraphs(root)
			sl.notes = slideNotes(p, part)
		}
		slides = append(slides, sl)
	}
	return slides, nil
}

func drawingParagraphs(root *node) []string {
	var out []string
	for _, para := range root.all("p") {
		var b strings.Builder
		for _, t := range para.all("t") {
			b.WriteString(t.chars.String())
		}
		if text := normalize(b.String()); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func slideNotes(p *pkg, slidePart string) []string {
	dir := slidePart[:strings.LastIndex(slidePart, "/")]
	base := slidePart[strings.LastIndex(slidePart, "/")+1:]
	rels := relationships(p, dir+"/_rels/"+base+".rels", dir)
	for _, target := range rels {
		if strings.Contains(target, "notesSlides/") {
			if data, ok := p.read(target); ok {
				return drawingParagraphs(parseTree(data))
			}
		}
	}
	return nil
}

func comparePowerPoint(r *reportBuilder, before, after *pkg) error {
	oldSlides, err := readPowerPoint(before)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	newSlides, err := readPowerPoint(after)
	if err != nil {
		return fmt.Errorf("current version: %w", err)
	}
	oldByID := map[string]slide{}
	for _, s := range oldSlides {
		oldByID[s.id] = s
	}
	newIDs := map[string]bool{}
	var commonOld, commonNew []string

	for _, s := range newSlides {
		newIDs[s.id] = true
		loc := fmt.Sprintf("Slide %d", s.number)
		r.current(loc, s.paragraphs...)
		r.current(loc+" (notes)", s.notes...)
		old, ok := oldByID[s.id]
		if !ok {
			r.change(Change{Kind: "added", Location: loc, After: strings.Join(s.paragraphs, " / ")})
			for _, p := range s.paragraphs {
				textRules(r, loc, "", p)
			}
			continue
		}
		commonNew = append(commonNew, s.id)
		if !old.hidden && s.hidden {
			r.flag(Finding{Severity: Medium, Rule: "powerpoint.slide-hidden", Title: "A slide was hidden: it will be skipped during the presentation", Location: loc})
			r.change(Change{Kind: "modified", Location: loc, Before: "visible", After: "hidden"})
		}
		compareParagraphs(r, loc, old.paragraphs, s.paragraphs)
		compareParagraphs(r, loc+" (notes)", old.notes, s.notes)
	}
	for _, s := range oldSlides {
		if !newIDs[s.id] {
			loc := fmt.Sprintf("Slide %d (baseline)", s.number)
			r.flag(Finding{Severity: Medium, Rule: "powerpoint.slide-removed", Title: "A slide was deleted", Location: loc, Before: strings.Join(s.paragraphs, " / ")})
			r.change(Change{Kind: "removed", Location: loc, Before: strings.Join(s.paragraphs, " / ")})
			continue
		}
		commonOld = append(commonOld, s.id)
	}
	if strings.Join(commonOld, ",") != strings.Join(commonNew, ",") {
		r.change(Change{Kind: "modified", Location: "Slide order", Before: "original order", After: "slides reordered"})
	}
	return nil
}

func compareParagraphs(r *reportBuilder, loc string, a, b []string) {
	for _, d := range diffSeq(a, b) {
		switch {
		case d.j < 0:
			r.change(Change{Kind: "removed", Location: loc, Before: a[d.i]})
			textRules(r, loc, a[d.i], "")
		case d.i < 0:
			r.change(Change{Kind: "added", Location: loc, After: b[d.j]})
			textRules(r, loc, "", b[d.j])
		default:
			r.change(Change{Kind: "modified", Location: loc, Before: a[d.i], After: b[d.j]})
			r.edited(loc, a[d.i], b[d.j])
			textRules(r, loc, a[d.i], b[d.j])
		}
	}
}
