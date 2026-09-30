package office

import (
	"regexp"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/desktop/internal/msg"
)

// Text rules apply to prose, whatever the format: Word paragraphs, slide
// text, and text cells of a workbook. They look for the edits that change the
// meaning or the stakes of a sentence rather than its wording.

var (
	datePattern = regexp.MustCompile(`(?i)\b(\d{1,2}[/.\-]\d{1,2}[/.\-]\d{2,4}|\d{4}-\d{2}-\d{2}|\d{1,2}(?:er)?\s+(?:janv|févr|fevr|mars|avr|mai|juin|juil|août|aout|sept|oct|nov|déc|dec)[a-zéû]*\.?\s+\d{4}|(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+\d{1,2},?\s+\d{4}|\d{1,2}\s+(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+\d{4})\b`)
	// A figure, with thousands separators (space, no-break space, dot, comma).
	numberPattern = regexp.MustCompile(`\d(?:[\d\x{00A0}\x{202F} .,']*\d)?`)
	currencyNear  = regexp.MustCompile(`(?i)^\s*(?:€|\$|£|¥|eur\b|euros?\b|usd\b|gbp\b|chf\b|k€|m€|%|pour ?cent|percent)`)
	currencyAfter = regexp.MustCompile(`(?i)(?:€|\$|£|¥|eur|usd|gbp|chf)\s*$`)

	strongModal = regexp.MustCompile(`(?i)(^|[^\p{L}])(shall|must|is required to|are required to|will be required|doit|doivent|devra|devront|est tenue?s? de|sont tenue?s? de|obligatoire(?:ment)?|impérativement)([^\p{L}]|$)`)
	weakModal   = regexp.MustCompile(`(?i)(^|[^\p{L}])(may|can|might|should|could|peut|peuvent|pourra|pourront|devrait|devraient|facultati(?:f|ve)|le cas échéant|si possible)([^\p{L}]|$)`)
	negation    = regexp.MustCompile(`(?i)(^|[^\p{L}'’])(not|no|never|none|nor|without|ne|pas|jamais|aucun|aucune|sans|ni|nul|nulle)([^\p{L}]|$)`)
	elision     = regexp.MustCompile(`(?i)(n['’]t([^\p{L}]|$)|(^|[^\p{L}])n['’]\p{L})`)

	sensitiveTopic = regexp.MustCompile(`(?i)(^|[^\p{L}])(liabilit|indemn|penalt|terminat|warrant|guarantee|exclusiv|confidential|non-compet|non-solicit|governing law|jurisdiction|arbitrat|payment|price|fee|fees|interest rate|late payment|deadline|notice period|force majeure|intellectual property|ownership|assignment|renewal|damages|royalt|invoice|salary|compensation|responsabilit|indemni|pénalit|penalit|résiliation|resiliation|garantie|exclusivit|confidentialit|non-concurrence|droit applicable|juridiction|tribunal|arbitrage|paiement|prix|honoraires|taux d'intérêt|intérêts de retard|délai|préavis|propriété intellectuelle|cession|reconduction|dommages|plafond|redevance|tarif|facturation|rémunération|salaire)`)
)

// textRules flags one modified, added or removed piece of prose. Before is
// empty for an addition, after is empty for a removal.
func textRules(r *reportBuilder, location, before, after string) {
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)
	if before == after {
		return
	}
	specific := false
	if before != "" && after != "" {
		specific = modifiedTextRules(r, location, before, after)
	}
	if specific {
		return
	}
	topic := sensitiveTopic.MatchString(before) || sensitiveTopic.MatchString(after)
	if !topic {
		return
	}
	switch {
	case before == "":
		r.flag(Finding{Severity: Medium, Rule: "text.sensitive-clause-added", Title: msg.M("Text added about a sensitive topic (payment, liability, termination…)"), Location: location, After: after})
	case after == "":
		r.flag(Finding{Severity: High, Rule: "text.sensitive-clause-removed", Title: msg.M("Text removed about a sensitive topic (payment, liability, termination…)"), Location: location, Before: before})
	default:
		r.flag(Finding{Severity: Medium, Rule: "text.sensitive-clause-modified", Title: msg.M("Wording changed in a sensitive clause (payment, liability, termination…)"), Location: location, Before: before, After: after})
	}
}

// modifiedTextRules runs the rules that need both versions, and reports
// whether one of them fired.
func modifiedTextRules(r *reportBuilder, location, before, after string) bool {
	fired := false
	flag := func(sev, rule, title string) {
		fired = true
		r.flag(Finding{Severity: sev, Rule: rule, Title: title, Location: location, Before: before, After: after})
	}

	oldDates, newDates := datePattern.FindAllString(before, -1), datePattern.FindAllString(after, -1)
	if !sameMultiset(normalizeAll(oldDates), normalizeAll(newDates)) {
		flag(Medium, "text.date-changed", msg.M("A date was changed"))
	}

	oldAmounts, oldFigures := figures(datePattern.ReplaceAllString(before, " "))
	newAmounts, newFigures := figures(datePattern.ReplaceAllString(after, " "))
	switch {
	case !sameMultiset(oldAmounts, newAmounts):
		flag(High, "text.amount-changed", msg.M("An amount or a percentage was changed"))
	case !sameMultiset(oldFigures, newFigures):
		flag(Medium, "text.figure-changed", msg.M("A figure was changed"))
	}

	if strongModal.MatchString(before) && !strongModal.MatchString(after) && weakModal.MatchString(after) {
		flag(High, "text.obligation-softened", msg.M("An obligation was softened (e.g. \"shall\" became \"may\")"))
	} else if !strongModal.MatchString(before) && strongModal.MatchString(after) {
		flag(Medium, "text.obligation-added", msg.M("The text now states an obligation"))
	}

	if negations(before) != negations(after) {
		flag(High, "text.negation-changed", msg.M("A negation was added or removed, which may invert the meaning"))
	}
	return fired
}

// negations counts the negative words of a text. Matches are counted on a
// spaced copy so that adjacent words ("ne pas") do not share a boundary.
func negations(s string) int {
	spaced := strings.Join(strings.Fields(s), "  ")
	return len(negation.FindAllString(spaced, -1)) + len(elision.FindAllString(spaced, -1))
}

// figures extracts the numbers of a text, split between amounts (next to a
// currency or a percent sign) and plain figures.
func figures(s string) (amounts, plain []string) {
	for _, loc := range numberPattern.FindAllStringIndex(s, -1) {
		num := canonicalNumber(s[loc[0]:loc[1]])
		if num == "" {
			continue
		}
		if currencyNear.MatchString(s[loc[1]:]) || currencyAfter.MatchString(s[:loc[0]]) {
			amounts = append(amounts, num)
		} else {
			plain = append(plain, num)
		}
	}
	return amounts, plain
}

// canonicalNumber strips grouping separators so "1 200,50" and "1200,50"
// compare equal. It keeps the last separator as the decimal mark.
func canonicalNumber(s string) string {
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' || r == '.' || r == ',' {
			digits.WriteRune(r)
		}
	}
	n := strings.Trim(digits.String(), ".,")
	last := strings.LastIndexAny(n, ".,")
	if last < 0 {
		return n
	}
	intPart := strings.NewReplacer(".", "", ",", "").Replace(n[:last])
	frac := n[last+1:]
	// Three digits after the only separator is a thousands group: "1,200".
	if len(frac) == 3 && !strings.ContainsAny(n[:last], ".,") && strings.Count(n, string(n[last])) == 1 && intPart != "0" {
		return intPart + frac
	}
	return intPart + "." + frac
}

func normalizeAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(normalize(s))
	}
	return out
}

func sameMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
