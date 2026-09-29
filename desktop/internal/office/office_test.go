package office

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// build writes a minimal OOXML package from part names and contents.
func build(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, ok := parts["[Content_Types].xml"]; !ok {
		parts["[Content_Types].xml"] = `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`
	}
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func docx(t *testing.T, paragraphs ...string) []byte {
	var body strings.Builder
	for _, p := range paragraphs {
		fmt.Fprintf(&body, `<w:p><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>`, p)
	}
	return build(t, map[string]string{
		"word/document.xml": `<?xml version="1.0"?><w:document ` + wNS + `><w:body>` + body.String() + `</w:body></w:document>`,
	})
}

func rules(r *Report) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range r.Findings {
		out[f.Rule] = f
	}
	return out
}

func mustCompare(t *testing.T, kind Kind, before, after []byte) *Report {
	t.Helper()
	r, err := Compare(kind, before, after)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return r
}

func TestWordFlagsAmountAndObligation(t *testing.T) {
	before := docx(t,
		"Article 1 - Objet du contrat.",
		"Le prestataire doit livrer avant le 15/03/2026.",
		"Le prix est fixé à 12 000 € HT.",
		"Clause de style inchangée.",
	)
	after := docx(t,
		"Article 1 - Objet du contrat.",
		"Le prestataire peut livrer avant le 15/03/2026.",
		"Le prix est fixé à 15 000 € HT.",
		"Clause de style inchangée.",
	)
	r := mustCompare(t, Word, before, after)
	got := rules(r)
	if _, ok := got["text.obligation-softened"]; !ok {
		t.Errorf("obligation softening not flagged: %+v", r.Findings)
	}
	if f, ok := got["text.amount-changed"]; !ok || f.Severity != High {
		t.Errorf("amount change not flagged high: %+v", r.Findings)
	}
	if _, ok := got["text.date-changed"]; ok {
		t.Errorf("unchanged date flagged: %+v", r.Findings)
	}
	if r.Severity != High || r.ChangeCount != 2 {
		t.Errorf("severity %s, %d changes; want high, 2", r.Severity, r.ChangeCount)
	}
}

func TestWordNegationAndRemovedClause(t *testing.T) {
	before := docx(t, "The supplier shall be liable for delays.", "Payment is due within 30 days of invoice.")
	after := docx(t, "The supplier shall not be liable for delays.")
	got := rules(mustCompare(t, Word, before, after))
	if _, ok := got["text.negation-changed"]; !ok {
		t.Errorf("negation not flagged: %v", got)
	}
	if f, ok := got["text.sensitive-clause-removed"]; !ok || f.Severity != High {
		t.Errorf("removed payment clause not flagged: %v", got)
	}
}

func TestWordIdenticalHasNoChange(t *testing.T) {
	d := docx(t, "Same text.", "Nothing moves.")
	r := mustCompare(t, Word, d, docx(t, "Same  text.", "Nothing moves."))
	if r.ChangeCount != 0 || r.Severity != None {
		t.Fatalf("whitespace-only edit reported: %+v", r)
	}
}

func TestWordTrackingDisabled(t *testing.T) {
	settings := func(on bool) string {
		inner := ""
		if on {
			inner = "<w:trackRevisions/>"
		}
		return `<w:settings ` + wNS + `>` + inner + `</w:settings>`
	}
	doc := `<w:document ` + wNS + `><w:body><w:p><w:r><w:t>Hello</w:t></w:r></w:p></w:body></w:document>`
	before := build(t, map[string]string{"word/document.xml": doc, "word/settings.xml": settings(true)})
	after := build(t, map[string]string{"word/document.xml": doc, "word/settings.xml": settings(false)})
	if _, ok := rules(mustCompare(t, Word, before, after))["word.tracking-disabled"]; !ok {
		t.Fatal("disabling track changes not flagged")
	}
}

type xcell struct{ ref, formula, value, typ, shared string }

func xlsx(t *testing.T, sheets map[string][]xcell, extra map[string]string) []byte {
	parts := map[string]string{}
	var list, rels strings.Builder
	i := 0
	names := make([]string, 0, len(sheets))
	for name := range sheets {
		names = append(names, name)
	}
	for _, name := range sortedKeys(sheets) {
		i++
		fmt.Fprintf(&list, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, name, i, i)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
		var data strings.Builder
		for _, c := range sheets[name] {
			attrs := fmt.Sprintf(`r="%s"`, c.ref)
			if c.typ != "" {
				attrs += fmt.Sprintf(` t="%s"`, c.typ)
			}
			inner := ""
			switch {
			case c.shared != "" && c.formula != "":
				inner = fmt.Sprintf(`<f t="shared" ref="%s" si="0">%s</f>`, c.shared, c.formula)
			case c.shared != "":
				inner = `<f t="shared" si="0"/>`
			case c.formula != "":
				inner = "<f>" + c.formula + "</f>"
			}
			if c.value != "" {
				inner += "<v>" + c.value + "</v>"
			}
			fmt.Fprintf(&data, `<c %s>%s</c>`, attrs, inner)
		}
		parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i)] = `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1">` + data.String() + `</row></sheetData></worksheet>`
	}
	_ = names
	parts["xl/workbook.xml"] = `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + list.String() + `</sheets></workbook>`
	parts["xl/_rels/workbook.xml.rels"] = `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() + `</Relationships>`
	for k, v := range extra {
		parts[k] = v
	}
	return build(t, parts)
}

func sortedKeys(m map[string][]xcell) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func TestExcelFormulaRules(t *testing.T) {
	before := xlsx(t, map[string][]xcell{
		"Budget": {
			{ref: "A1", value: "100"},
			{ref: "A2", value: "200"},
			{ref: "A3", value: "300"},
			{ref: "B1", formula: "SUM(A1:A3)", value: "600"},
			{ref: "C1", formula: "A1*2", value: "200"},
			{ref: "D1", formula: "A2*1.2", value: "240"},
			{ref: "E1", formula: "B1/12", value: "50"},
		},
		"Archive": {{ref: "A1", value: "1"}},
	}, nil)
	after := xlsx(t, map[string][]xcell{
		"Budget": {
			{ref: "A1", value: "100"},
			{ref: "A2", value: "200"},
			{ref: "A3", value: "300"},
			{ref: "B1", formula: "SUM(A1:A2)", value: "300"},
			{ref: "C1", value: "200"},
			{ref: "D1", formula: "A2*1.5", value: "300"},
			{ref: "E1", formula: "B1/12", value: "25"},
		},
	}, nil)
	got := rules(mustCompare(t, Excel, before, after))
	for _, rule := range []string{"excel.range-reduced", "excel.formula-hardcoded", "excel.formula-constant-changed", "excel.result-moved", "excel.sheet-removed"} {
		if _, ok := got[rule]; !ok {
			t.Errorf("rule %s not fired; findings: %v", rule, got)
		}
	}
	if got["excel.formula-hardcoded"].Location != "Budget!C1" {
		t.Errorf("hardcoded location = %q", got["excel.formula-hardcoded"].Location)
	}
}

func TestExcelSharedFormulaIsShifted(t *testing.T) {
	before := xlsx(t, map[string][]xcell{"S": {
		{ref: "B2", formula: "A2*$C$1", value: "2", shared: "B2:B3"},
		{ref: "B3", value: "4", shared: "yes"},
	}}, nil)
	after := xlsx(t, map[string][]xcell{"S": {
		{ref: "B2", formula: "A2*$C$1", value: "2", shared: "B2:B3"},
		{ref: "B3", value: "4"},
	}}, nil)
	r := mustCompare(t, Excel, before, after)
	f, ok := rules(r)["excel.formula-hardcoded"]
	if !ok || f.Before != "=A3*$C$1" {
		t.Fatalf("shared formula not rebuilt: %+v", r.Findings)
	}
}

func TestShiftFormula(t *testing.T) {
	cases := []struct{ f, from, to, want string }{
		{"A1+B$2+$C3+$D$4", "E5", "F7", "B3+C$2+$C5+$D$4"},
		{"SUM(A1:A10)", "B1", "C1", "SUM(B1:B10)"},
		{`LOG10(A1)&"B2"`, "A2", "A3", `LOG10(A2)&"B2"`},
		{"Sheet2!A1", "A1", "A2", "Sheet2!A2"},
		{"A1", "B2", "B1", "#REF!"},
	}
	for _, c := range cases {
		if got := shiftFormula(c.f, c.from, c.to); got != c.want {
			t.Errorf("shiftFormula(%q, %s→%s) = %q, want %q", c.f, c.from, c.to, got, c.want)
		}
	}
}

func TestExcelHiddenSheetAndMacro(t *testing.T) {
	before := xlsx(t, map[string][]xcell{"A": {{ref: "A1", value: "1"}}}, nil)
	after := xlsx(t, map[string][]xcell{"A": {{ref: "A1", value: "1"}}}, map[string]string{
		"xl/vbaProject.bin": "binary",
		"xl/workbook.xml":   `<workbook xmlns:r="r"><sheets><sheet name="A" sheetId="1" state="veryHidden" r:id="rId1"/></sheets></workbook>`,
	})
	got := rules(mustCompare(t, Excel, before, after))
	if _, ok := got["package.macro-added"]; !ok {
		t.Errorf("macro not flagged: %v", got)
	}
	if _, ok := got["excel.sheet-very-hidden"]; !ok {
		t.Errorf("very hidden sheet not flagged: %v", got)
	}
}

func pptx(t *testing.T, slides ...[]string) []byte {
	parts := map[string]string{}
	var ids, rels strings.Builder
	for i, texts := range slides {
		n := i + 1
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="rId%d"/>`, 255+n, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="slide" Target="slides/slide%d.xml"/>`, n, n)
		var body strings.Builder
		for _, text := range texts {
			fmt.Fprintf(&body, `<a:p><a:r><a:t>%s</a:t></a:r></a:p>`, text)
		}
		parts[fmt.Sprintf("ppt/slides/slide%d.xml", n)] = `<p:sld xmlns:p="p" xmlns:a="a"><p:cSld><p:spTree><p:sp><p:txBody>` + body.String() + `</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
	}
	parts["ppt/presentation.xml"] = `<p:presentation xmlns:p="p" xmlns:r="r"><p:sldIdLst>` + ids.String() + `</p:sldIdLst></p:presentation>`
	parts["ppt/_rels/presentation.xml.rels"] = `<Relationships>` + rels.String() + `</Relationships>`
	return build(t, parts)
}

func TestPowerPointFigureAndRemovedSlide(t *testing.T) {
	before := pptx(t, []string{"Résultats 2025", "Croissance de 12 %"}, []string{"Annexe"})
	after := pptx(t, []string{"Résultats 2025", "Croissance de 21 %"})
	r := mustCompare(t, PowerPoint, before, after)
	got := rules(r)
	if f, ok := got["text.amount-changed"]; !ok || f.Location != "Slide 1" {
		t.Errorf("percentage change not flagged on slide 1: %+v", r.Findings)
	}
	if _, ok := got["powerpoint.slide-removed"]; !ok {
		t.Errorf("slide removal not flagged: %+v", r.Findings)
	}
}

func TestUnreadablePackage(t *testing.T) {
	if _, err := Compare(Word, []byte("not a zip"), docx(t, "x")); err == nil {
		t.Fatal("expected an error for a truncated file")
	}
}

func TestDiffSeq(t *testing.T) {
	a := []string{"a", "b", "c", "d", "e"}
	b := []string{"a", "x", "c", "e", "f"}
	got := diffSeq(a, b)
	want := []pair{{1, 1}, {3, -1}, {-1, 4}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("diffSeq = %v, want %v", got, want)
	}
}

func TestCanonicalNumber(t *testing.T) {
	for in, want := range map[string]string{"1 200,50": "1200.50", "1,200": "1200", "12": "12", "3.5": "3.5", "0,125": "0.125"} {
		if got := canonicalNumber(in); got != want {
			t.Errorf("canonicalNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
