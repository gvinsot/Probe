package office

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gvinsot/Probe/desktop/internal/msg"
)

// A workbook is compared cell by cell from the XML of its worksheets. Excel
// stores the last computed value next to each formula, so a change of an
// input also shows where its effect lands: the formulas whose result moved.

type xlCell struct {
	formula string
	value   string
	isError bool
	isText  bool
}

type xlSheet struct {
	name        string
	state       string // "", "hidden", "veryHidden"
	cells       map[string]xlCell
	hiddenRows  map[int]bool
	hiddenCols  map[int]bool
	validations int
	protected   bool
}

type xlBook struct {
	sheets    []*xlSheet
	names     map[string]string
	protected bool
}

// maxCells bounds the cells read per workbook to keep memory predictable.
const maxCells = 3_000_000

func readExcel(p *pkg) (*xlBook, error) {
	wbData, ok := p.read("xl/workbook.xml")
	if !ok {
		return nil, ErrUnreadable
	}
	wb := parseTree(wbData)
	rels := relationships(p, "xl/_rels/workbook.xml.rels", "xl")
	shared := sharedStrings(p)

	book := &xlBook{names: map[string]string{}, protected: wb.first("workbookProtection") != nil}
	for _, dn := range wb.all("definedName") {
		name := dn.attr("name")
		if strings.HasPrefix(name, "_xlnm.") {
			continue // print areas and filters: layout, not content
		}
		book.names[name] = dn.text()
	}
	total := 0
	for _, s := range wb.all("sheet") {
		sh := &xlSheet{name: s.attr("name"), state: s.attr("state")}
		part := rels[s.nsAttr("id")]
		data, ok := p.read(part)
		if !ok {
			// Chart sheets and dialog sheets have no cells.
			sh.cells = map[string]xlCell{}
		} else if err := readSheet(sh, data, shared, &total); err != nil {
			return nil, err
		}
		book.sheets = append(book.sheets, sh)
	}
	return book, nil
}

func sharedStrings(p *pkg) []string {
	data, ok := p.read("xl/sharedStrings.xml")
	if !ok {
		return nil
	}
	var out []string
	for _, si := range parseTree(data).all("si") {
		var b strings.Builder
		for _, c := range si.children {
			switch c.name {
			case "t":
				b.WriteString(c.chars.String())
			case "r": // rich text run; phonetic runs (rPh) only repeat the reading
				for _, t := range c.all("t") {
					b.WriteString(t.chars.String())
				}
			}
		}
		out = append(out, b.String())
	}
	return out
}

// readSheet streams a worksheet: they can be large, so no tree is built.
func readSheet(sh *xlSheet, data []byte, shared []string, total *int) error {
	sh.cells = map[string]xlCell{}
	sh.hiddenRows = map[int]bool{}
	sh.hiddenCols = map[int]bool{}
	masters := map[string]sharedFormula{}
	var pendingShared []struct{ ref, si string }

	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var (
		ref, typ   string
		cur        xlCell
		inCell     bool
		field      string // "v", "f", "t"
		sharedSI   string
		isShared   bool
		fBuf, vBuf strings.Builder
		inInline   bool
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				if attrOf(t, "hidden") == "1" || attrOf(t, "hidden") == "true" {
					if n, err := strconv.Atoi(attrOf(t, "r")); err == nil {
						sh.hiddenRows[n] = true
					}
				}
			case "col":
				if attrOf(t, "hidden") == "1" || attrOf(t, "hidden") == "true" {
					lo, _ := strconv.Atoi(attrOf(t, "min"))
					hi, _ := strconv.Atoi(attrOf(t, "max"))
					if hi-lo < 16384 {
						for c := lo; c <= hi; c++ {
							sh.hiddenCols[c] = true
						}
					}
				}
			case "c":
				inCell, ref, typ = true, attrOf(t, "r"), attrOf(t, "t")
				cur = xlCell{}
				fBuf.Reset()
				vBuf.Reset()
				isShared, sharedSI = false, ""
			case "f":
				if inCell {
					field = "f"
					if attrOf(t, "t") == "shared" {
						isShared, sharedSI = true, attrOf(t, "si")
					}
				}
			case "v":
				if inCell {
					field = "v"
				}
			case "is":
				inInline = true
			case "t":
				if inCell && inInline {
					field = "t"
				}
			case "dataValidation":
				sh.validations++
			case "sheetProtection":
				sh.protected = attrOf(t, "sheet") != "0" && attrOf(t, "sheet") != "false"
			}
		case xml.CharData:
			switch field {
			case "f":
				fBuf.Write(t)
			case "v", "t":
				vBuf.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "f", "v", "t":
				field = ""
			case "is":
				inInline = false
			case "c":
				inCell = false
				cur.formula = strings.TrimSpace(fBuf.String())
				raw := vBuf.String()
				switch typ {
				case "s":
					if i, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && i >= 0 && i < len(shared) {
						cur.value, cur.isText = shared[i], true
					}
				case "str", "inlineStr":
					cur.value, cur.isText = raw, true
				case "e":
					cur.value, cur.isError = raw, true
				case "b":
					if raw == "1" {
						cur.value = "TRUE"
					} else {
						cur.value = "FALSE"
					}
				default:
					cur.value = raw
				}
				if isShared {
					if cur.formula != "" {
						masters[sharedSI] = sharedFormula{origin: ref, formula: cur.formula}
					} else {
						pendingShared = append(pendingShared, struct{ ref, si string }{ref, sharedSI})
					}
				}
				if cur.formula == "" && cur.value == "" && !isShared {
					continue // styled but empty cell
				}
				*total++
				if *total > maxCells {
					return fmt.Errorf("workbook has more than %d cells, too large to compare", maxCells)
				}
				sh.cells[ref] = cur
			}
		}
	}
	// Cells that share a formula store only a pointer to the first cell of
	// the group: rebuild their own formula by shifting the relative references.
	for _, ps := range pendingShared {
		m, ok := masters[ps.si]
		if !ok {
			continue
		}
		c := sh.cells[ps.ref]
		c.formula = shiftFormula(m.formula, m.origin, ps.ref)
		sh.cells[ps.ref] = c
	}
	return nil
}

type sharedFormula struct {
	origin  string
	formula string
}

func attrOf(t xml.StartElement, local string) string {
	for _, a := range t.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

var (
	cellRefPattern = regexp.MustCompile(`^(\$?)([A-Z]{1,3})(\$?)(\d+)$`)
	refToken       = regexp.MustCompile(`\$?[A-Z]{1,3}\$?\d+`)
	rangePattern   = regexp.MustCompile(`(?:'[^']+'|[A-Za-z0-9_.]+)?!?\$?([A-Z]{1,3})\$?(\d+):\$?([A-Z]{1,3})\$?(\d+)`)
	errorTokens    = []string{"#REF!", "#DIV/0!", "#N/A", "#VALUE!", "#NAME?", "#NUM!", "#NULL!", "#SPILL!", "#CALC!"}
)

// splitRef splits "B12" into column 2 and row 12.
func splitRef(ref string) (col, row int, ok bool) {
	m := cellRefPattern.FindStringSubmatch(strings.ToUpper(ref))
	if m == nil {
		return 0, 0, false
	}
	for _, ch := range m[2] {
		col = col*26 + int(ch-'A'+1)
	}
	row, _ = strconv.Atoi(m[4])
	return col, row, true
}

func colName(col int) string {
	var s []byte
	for col > 0 {
		col--
		s = append([]byte{byte('A' + col%26)}, s...)
		col /= 26
	}
	return string(s)
}

// shiftFormula moves the relative references of a shared formula from its
// origin cell to another cell of the group. String literals and function
// names are left alone.
func shiftFormula(formula, origin, target string) string {
	oc, or, ok1 := splitRef(origin)
	tc, tr, ok2 := splitRef(target)
	if !ok1 || !ok2 {
		return formula
	}
	dc, dr := tc-oc, tr-or
	var b strings.Builder
	inString := false
	i := 0
	for i < len(formula) {
		ch := formula[i]
		if ch == '"' {
			inString = !inString
			b.WriteByte(ch)
			i++
			continue
		}
		if inString {
			b.WriteByte(ch)
			i++
			continue
		}
		loc := refToken.FindStringIndex(formula[i:])
		if loc == nil || loc[0] != 0 || (i > 0 && isNameChar(formula[i-1])) {
			b.WriteByte(ch)
			i++
			continue
		}
		end := i + loc[1]
		if end < len(formula) && (isNameChar(formula[end]) || formula[end] == '(') {
			b.WriteByte(ch)
			i++
			continue
		}
		m := cellRefPattern.FindStringSubmatch(formula[i:end])
		col, row, _ := splitRef(strings.ReplaceAll(formula[i:end], "$", ""))
		if m[1] == "" {
			col += dc
		}
		if m[3] == "" {
			row += dr
		}
		if col < 1 || row < 1 {
			b.WriteString("#REF!")
		} else {
			b.WriteString(m[1] + colName(col) + m[3] + strconv.Itoa(row))
		}
		i = end
	}
	return b.String()
}

func isNameChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.'
}

func compareExcel(r *reportBuilder, before, after *pkg) error {
	oldBook, err := readExcel(before)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	newBook, err := readExcel(after)
	if err != nil {
		return fmt.Errorf("current version: %w", err)
	}

	oldSheets := map[string]*xlSheet{}
	for _, s := range oldBook.sheets {
		oldSheets[s.name] = s
	}
	newSheets := map[string]bool{}
	for _, s := range newBook.sheets {
		newSheets[s.name] = true
		old, ok := oldSheets[s.name]
		if !ok {
			r.flag(Finding{Severity: Low, Rule: "excel.sheet-added", Title: msg.M("A sheet was added"), Location: s.name})
			r.change(Change{Kind: "added", Location: fmt.Sprintf(msg.M("Sheet %s"), s.name), After: fmt.Sprintf(msg.M("%d cells"), len(s.cells))})
			continue
		}
		compareSheet(r, old, s)
	}
	for _, s := range oldBook.sheets {
		if !newSheets[s.name] {
			r.flag(Finding{Severity: High, Rule: "excel.sheet-removed", Title: msg.M("A sheet was deleted (or renamed)"), Location: s.name})
			r.change(Change{Kind: "removed", Location: fmt.Sprintf(msg.M("Sheet %s"), s.name), Before: fmt.Sprintf(msg.M("%d cells"), len(s.cells))})
		}
	}

	for name, def := range oldBook.names {
		cur, ok := newBook.names[name]
		switch {
		case !ok:
			r.flag(Finding{Severity: Medium, Rule: "excel.name-removed", Title: msg.M("A named range was deleted"), Location: name, Before: def})
			r.change(Change{Kind: "removed", Location: fmt.Sprintf(msg.M("Name %s"), name), Before: def})
		case cur != def:
			r.flag(Finding{Severity: Medium, Rule: "excel.name-changed", Title: msg.M("A named range now points elsewhere"), Location: name, Before: def, After: cur})
			r.change(Change{Kind: "modified", Location: fmt.Sprintf(msg.M("Name %s"), name), Before: def, After: cur})
		}
	}
	for name, def := range newBook.names {
		if _, ok := oldBook.names[name]; !ok {
			r.change(Change{Kind: "added", Location: fmt.Sprintf(msg.M("Name %s"), name), After: def})
		}
	}
	if oldBook.protected && !newBook.protected {
		r.flag(Finding{Severity: Medium, Rule: "excel.protection-removed", Title: msg.M("Workbook structure protection was removed")})
	}
	return nil
}

func compareSheet(r *reportBuilder, old, cur *xlSheet) {
	name := cur.name
	if old.state != cur.state {
		switch cur.state {
		case "veryHidden":
			r.flag(Finding{Severity: High, Rule: "excel.sheet-very-hidden", Title: msg.M("The sheet was made \"very hidden\" (invisible from the Excel interface)"), Location: name})
		case "hidden":
			r.flag(Finding{Severity: Medium, Rule: "excel.sheet-hidden", Title: msg.M("The sheet was hidden"), Location: name})
		}
		r.change(Change{Kind: "modified", Location: fmt.Sprintf(msg.M("Sheet %s"), name), Before: visibility(old.state), After: visibility(cur.state)})
	}
	if old.protected && !cur.protected {
		r.flag(Finding{Severity: Medium, Rule: "excel.sheet-protection-removed", Title: msg.M("Sheet protection was removed"), Location: name})
	}
	if cur.validations < old.validations {
		r.flag(Finding{Severity: Medium, Rule: "excel.validation-removed", Title: fmt.Sprintf(msg.M("%d data validation rule(s) removed: inputs are no longer checked"), old.validations-cur.validations), Location: name})
	}
	if rows := newlyHidden(old.hiddenRows, cur.hiddenRows); len(rows) > 0 {
		r.flag(Finding{Severity: Medium, Rule: "excel.rows-hidden", Title: fmt.Sprintf(msg.M("%d row(s) hidden"), len(rows)), Location: name + "!" + rowList(rows)})
	}
	if cols := newlyHidden(old.hiddenCols, cur.hiddenCols); len(cols) > 0 {
		var labels []string
		for _, c := range cols {
			labels = append(labels, colName(c))
		}
		r.flag(Finding{Severity: Medium, Rule: "excel.columns-hidden", Title: fmt.Sprintf(msg.M("%d column(s) hidden"), len(cols)), Location: name + "!" + strings.Join(limitList(labels), ",")})
	}

	refs := make([]string, 0, len(cur.cells)+len(old.cells))
	seen := map[string]bool{}
	for ref := range old.cells {
		refs, seen[ref] = append(refs, ref), true
	}
	for ref := range cur.cells {
		if !seen[ref] {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refLess(refs[i], refs[j]) })

	for _, ref := range refs {
		a, inOld := old.cells[ref]
		b, inNew := cur.cells[ref]
		loc := name + "!" + ref
		switch {
		case !inNew:
			r.change(Change{Kind: "removed", Location: loc, Before: cellText(a)})
			if a.formula != "" {
				r.flag(Finding{Severity: Medium, Rule: "excel.formula-removed", Title: msg.M("A formula was deleted"), Location: loc, Before: "=" + a.formula})
			}
		case !inOld:
			r.change(Change{Kind: "added", Location: loc, After: cellText(b)})
			if b.isText {
				textRules(r, loc, "", b.value)
			}
		default:
			compareCell(r, loc, a, b)
		}
	}
}

func compareCell(r *reportBuilder, loc string, a, b xlCell) {
	switch {
	case a.formula != "" && b.formula == "":
		if a.value == b.value && a.value != "" {
			r.change(Change{Kind: "modified", Location: loc, Before: "=" + a.formula, After: b.value})
			r.flag(Finding{Severity: High, Rule: "excel.formula-hardcoded", Title: msg.M("A formula was replaced by its value: the cell no longer updates"), Location: loc, Before: "=" + a.formula, After: b.value})
		} else {
			r.change(Change{Kind: "modified", Location: loc, Before: "=" + a.formula, After: cellText(b)})
			r.flag(Finding{Severity: High, Rule: "excel.formula-overwritten", Title: msg.M("A formula was overwritten by a typed value"), Location: loc, Before: "=" + a.formula + " → " + a.value, After: b.value})
		}
		return
	case a.formula == "" && b.formula != "":
		r.change(Change{Kind: "modified", Location: loc, Before: a.value, After: "=" + b.formula})
		return
	case a.formula != b.formula:
		r.change(Change{Kind: "modified", Location: loc, Before: "=" + a.formula, After: "=" + b.formula})
		formulaRules(r, loc, a, b)
		return
	}
	if a.value == b.value {
		return
	}
	if b.formula != "" {
		// Same formula, new result: the effect of a change made elsewhere.
		r.change(Change{Kind: "recomputed", Location: loc, Before: a.value, After: b.value})
		if b.isError && !a.isError {
			r.flag(Finding{Severity: High, Rule: "excel.error-appeared", Title: msg.M("A formula now returns an error"), Location: loc, Before: a.value, After: b.value})
		} else if pct, ok := relativeChange(a.value, b.value); ok && pct >= 0.10 {
			r.flag(Finding{Severity: Medium, Rule: "excel.result-moved", Title: fmt.Sprintf(msg.M("A computed result moved by %s"), formatPct(pct)), Location: loc, Before: a.value, After: b.value})
		}
		return
	}
	r.change(Change{Kind: "modified", Location: loc, Before: a.value, After: b.value})
	if a.isText || b.isText {
		textRules(r, loc, a.value, b.value)
	}
}

func formulaRules(r *reportBuilder, loc string, a, b xlCell) {
	for _, tok := range errorTokens {
		if strings.Contains(b.formula, tok) && !strings.Contains(a.formula, tok) {
			r.flag(Finding{Severity: High, Rule: "excel.broken-reference", Title: fmt.Sprintf(msg.M("The formula now contains a broken reference (%s)"), tok), Location: loc, Before: "=" + a.formula, After: "=" + b.formula})
			return
		}
	}
	if shrunk(a.formula, b.formula) {
		r.flag(Finding{Severity: High, Rule: "excel.range-reduced", Title: msg.M("A range in the formula was reduced: some cells are no longer counted"), Location: loc, Before: "=" + a.formula, After: "=" + b.formula})
		return
	}
	if constantsChanged(a.formula, b.formula) {
		r.flag(Finding{Severity: Medium, Rule: "excel.formula-constant-changed", Title: msg.M("A number typed inside a formula was changed"), Location: loc, Before: "=" + a.formula, After: "=" + b.formula})
		return
	}
	r.flag(Finding{Severity: Medium, Rule: "excel.formula-changed", Title: msg.M("A formula was changed"), Location: loc, Before: "=" + a.formula, After: "=" + b.formula})
}

type area struct{ c1, r1, c2, r2 int }

func (a area) size() int { return (a.c2 - a.c1 + 1) * (a.r2 - a.r1 + 1) }

func ranges(formula string) []area {
	var out []area
	for _, m := range rangePattern.FindAllStringSubmatch(formula, -1) {
		c1, r1, ok1 := splitRef(m[1] + m[2])
		c2, r2, ok2 := splitRef(m[3] + m[4])
		if ok1 && ok2 {
			out = append(out, area{c1, r1, c2, r2})
		}
	}
	return out
}

// shrunk reports whether a range of the new formula covers strictly fewer
// cells than the range at the same position in the old one while still
// starting or ending at the same place.
func shrunk(before, after string) bool {
	a, b := ranges(before), ranges(after)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		sameAnchor := (a[i].c1 == b[i].c1 && a[i].r1 == b[i].r1) || (a[i].c2 == b[i].c2 && a[i].r2 == b[i].r2)
		if sameAnchor && b[i].size() < a[i].size() {
			return true
		}
	}
	return false
}

var literalNumber = regexp.MustCompile(`(^|[^A-Za-z0-9_$.])(\d+(?:\.\d+)?)`)

func constantsChanged(before, after string) bool {
	strip := func(f string) (string, []string) {
		f = refToken.ReplaceAllString(f, "R")
		var nums []string
		for _, m := range literalNumber.FindAllStringSubmatch(f, -1) {
			nums = append(nums, m[2])
		}
		return literalNumber.ReplaceAllString(f, "${1}N"), nums
	}
	sa, na := strip(before)
	sb, nb := strip(after)
	return sa == sb && !sameMultiset(na, nb)
}

func relativeChange(before, after string) (float64, bool) {
	x, err1 := strconv.ParseFloat(before, 64)
	y, err2 := strconv.ParseFloat(after, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	if x == 0 {
		return math.Inf(1), y != 0
	}
	return math.Abs(y-x) / math.Abs(x), true
}

func formatPct(p float64) string {
	if math.IsInf(p, 1) {
		return "more than 100% (from zero)"
	}
	return fmt.Sprintf("%.0f%%", p*100)
}

func cellText(c xlCell) string {
	if c.formula != "" {
		return "=" + c.formula + " → " + c.value
	}
	return c.value
}

func visibility(state string) string {
	switch state {
	case "":
		return msg.M("visible")
	case "hidden":
		return msg.M("hidden")
	case "veryHidden":
		return msg.M("very hidden")
	}
	return state
}

func newlyHidden(old, cur map[int]bool) []int {
	var out []int
	for k := range cur {
		if !old[k] {
			out = append(out, k)
		}
	}
	sort.Ints(out)
	return out
}

func rowList(rows []int) string {
	var labels []string
	for _, r := range rows {
		labels = append(labels, strconv.Itoa(r))
	}
	return strings.Join(limitList(labels), ",")
}

func limitList(items []string) []string {
	if len(items) > 12 {
		return append(items[:12:12], "…")
	}
	return items
}

// refLess orders cells by row, then column, the way a reader scans a sheet.
func refLess(a, b string) bool {
	ca, ra, _ := splitRef(a)
	cb, rb, _ := splitRef(b)
	if ra != rb {
		return ra < rb
	}
	return ca < cb
}
