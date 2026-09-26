package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func mustParse(t *testing.T, text string) Document {
	t.Helper()
	doc, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func texts(doc Document) []string {
	out := []string{}
	for _, c := range doc.Criteria {
		out = append(out, c.Text)
	}
	return out
}

func TestParseExtractsListItemsVerbatim(t *testing.T) {
	text := "Intro prose is not a criterion.\n" +
		"- Orders of **100** or more get `10` off\n" +
		"* [ ] Orders of 50 or more ship free\n" +
		"+ [x] Refunds keep the [original](link) currency\n" +
		"1. First numbered\n" +
		"2) [X] Second numbered\n" +
		"  - nested item\n"
	doc := mustParse(t, text)
	want := []model.IntentCriterion{
		{ID: "AC-1", Text: "Orders of **100** or more get `10` off", Line: 2},
		{ID: "AC-2", Text: "Orders of 50 or more ship free", Line: 3},
		{ID: "AC-3", Text: "Refunds keep the [original](link) currency", Line: 4},
		{ID: "AC-4", Text: "First numbered", Line: 5},
		{ID: "AC-5", Text: "Second numbered", Line: 6},
		{ID: "AC-6", Text: "nested item", Line: 7},
	}
	if !reflect.DeepEqual(doc.Criteria, want) {
		t.Fatalf("criteria %+v", doc.Criteria)
	}
	sum := sha256.Sum256([]byte(text))
	if doc.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha %s", doc.SHA256)
	}
	if len(doc.Notes()) != 0 {
		t.Fatalf("notes %q", doc.Notes())
	}
}

func TestParseScopesToAcceptanceCriteriaHeading(t *testing.T) {
	text := strings.Join([]string{
		"# Change",
		"- outside before",
		"## Acceptance Criteria",
		"- in one",
		"### Details",
		"- deeper stays in",
		"## Notes",
		"- outside after",
		"### acceptance   CRITERION for export",
		"- in two",
		"#### Sub",
		"- in three",
		"### Other",
		"- outside again",
		"## Acceptance criteria",
		"### Acceptance criteria for the API",
		"- in four",
		"### Unrelated subsection",
		"- in five (still inside the level-2 section)",
		"# Top",
		"- outside at the end",
	}, "\n")
	got := texts(mustParse(t, text))
	want := []string{"in one", "deeper stays in", "in two", "in three", "in four", "in five (still inside the level-2 section)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	// Without a matching heading every list item counts.
	if got := texts(mustParse(t, "# Plan\n- a\n## Notes\n- b\n")); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("unscoped %q", got)
	}
}

func TestParseIgnoresFencesAndThematicBreaks(t *testing.T) {
	text := strings.Join([]string{
		"- before",
		"```go",
		"- inside backticks",
		"## Acceptance criteria",
		"```",
		"~~~~",
		"- inside tildes",
		"~~~",
		"- still inside: a shorter fence does not close",
		"~~~~",
		"---",
		"* * *",
		"_ _ _",
		"- after",
	}, "\n")
	got := texts(mustParse(t, text))
	if !reflect.DeepEqual(got, []string{"before", "after"}) {
		t.Fatalf("got %q", got)
	}
}

func TestParseContinuationLines(t *testing.T) {
	text := "- first line\n  continues here\n\tand here\nnot indented, not joined\n- second\n\n  after a blank line, not joined\n- third\n  - nested is its own item\n"
	doc := mustParse(t, text)
	want := []model.IntentCriterion{
		{ID: "AC-1", Text: "first line continues here and here", Line: 1},
		{ID: "AC-2", Text: "second", Line: 5},
		{ID: "AC-3", Text: "third", Line: 8},
		{ID: "AC-4", Text: "nested is its own item", Line: 9},
	}
	if !reflect.DeepEqual(doc.Criteria, want) {
		t.Fatalf("criteria %+v", doc.Criteria)
	}
}

func TestParseCRLFAndBOM(t *testing.T) {
	lf := "## Acceptance criteria\n- one\n  two\n- three\n"
	crlf := "\uFEFF" + strings.ReplaceAll(lf, "\n", "\r\n")
	a, b := mustParse(t, lf), mustParse(t, crlf)
	if !reflect.DeepEqual(a.Criteria, b.Criteria) {
		t.Fatalf("LF %+v, CRLF %+v", a.Criteria, b.Criteria)
	}
	sum := sha256.Sum256([]byte(crlf))
	if b.SHA256 != hex.EncodeToString(sum[:]) || a.SHA256 == b.SHA256 {
		t.Fatal("the hash must cover the exact bytes")
	}
}

func TestParseLimits(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxCriteria+1; i++ {
		fmt.Fprintf(&b, "- item %d\n", i)
	}
	doc := mustParse(t, b.String())
	if len(doc.Criteria) != MaxCriteria || doc.Omitted != 1 || doc.Criteria[MaxCriteria-1].ID != "AC-100" {
		t.Fatalf("%d criteria, omitted %d", len(doc.Criteria), doc.Omitted)
	}
	if notes := doc.Notes(); len(notes) != 1 || !strings.Contains(notes[0], "first 100") || !strings.Contains(notes[0], "1 further list item was ignored") {
		t.Fatalf("notes %q", notes)
	}
	exact := strings.Repeat("a", MaxCriterionBytes)
	doc = mustParse(t, "- "+exact+"a\n- "+exact+"\n- "+exact+"bb\n")
	if len(doc.Criteria) != 1 || doc.Criteria[0].ID != "AC-1" || doc.Criteria[0].Text != exact || doc.Criteria[0].Line != 2 || doc.Overlong != 2 {
		t.Fatalf("overlong handling: %d criteria, overlong %d", len(doc.Criteria), doc.Overlong)
	}
	if notes := doc.Notes(); len(notes) != 1 || !strings.Contains(notes[0], "2 intent list items longer than 1024 bytes were not taken") {
		t.Fatalf("notes %q", notes)
	}
}

func TestParseRejectsInvalidUTF8AndNUL(t *testing.T) {
	for _, text := range []string{"- ok\n\xff", "- a\x00b"} {
		if _, err := Parse(text); err != ErrEncoding {
			t.Fatalf("%q: %v", text, err)
		}
		if CheckEncoding(text) != ErrEncoding {
			t.Fatalf("CheckEncoding(%q)", text)
		}
	}
	if CheckEncoding("é - ok") != nil {
		t.Fatal("valid UTF-8 rejected")
	}
}

func TestParseEmptyAndProseOnly(t *testing.T) {
	doc := mustParse(t, "")
	if doc.SHA256 != "" || doc.Criteria == nil || len(doc.Criteria) != 0 {
		t.Fatalf("empty %+v", doc)
	}
	doc = mustParse(t, "Just prose.\nNo list here -- really.\n")
	if doc.SHA256 == "" || doc.Criteria == nil || len(doc.Criteria) != 0 {
		t.Fatalf("prose %+v", doc)
	}
	doc = mustParse(t, "-\n- \n- [ ] \n- [x]\n- [y] kept\n")
	if len(doc.Criteria) != 1 || doc.Criteria[0].Text != "[y] kept" || doc.Criteria[0].ID != "AC-1" {
		t.Fatalf("empty items %+v", doc.Criteria)
	}
}

func TestValidIDAndFind(t *testing.T) {
	for id, valid := range map[string]bool{"AC-1": true, "AC-10": true, "AC-999": true, "AC-0": false, "AC-01": false, "ac-1": false, "AC-1000": false, "AC-": false, "AC-1a": false, "": false} {
		if ValidID(id) != valid {
			t.Errorf("ValidID(%q) = %v", id, !valid)
		}
	}
	criteria := []model.IntentCriterion{{ID: "AC-1", Text: "a"}, {ID: "AC-2", Text: "b"}, {ID: "AC-2", Text: "c"}, {ID: "AC-01", Text: "d"}}
	if c, ok := Find(criteria, "AC-1"); !ok || c.Text != "a" {
		t.Fatal("AC-1 not found")
	}
	for _, id := range []string{"AC-2", "AC-01", "AC-3", ""} {
		if _, ok := Find(criteria, id); ok {
			t.Errorf("Find(%q) succeeded", id)
		}
	}
}

func TestStripPRComments(t *testing.T) {
	block := model.PRCommentBegin + "\n- Reproduced: 1 issue\n" + model.PRCommentEnd
	for _, tc := range []struct{ in, out string }{
		{"- keep\n" + block + "\n- after\n", "- keep\n\n- after\n"},
		{block + block, ""},
		{"- keep\n" + model.PRCommentBegin + "\n- unterminated\n", "- keep\n"},
		{"- keep " + model.PRCommentEnd + " stray\n", "- keep  stray\n"},
	} {
		got, removed := StripPRComments(tc.in)
		if got != tc.out || !removed {
			t.Errorf("StripPRComments(%q) = %q, %v", tc.in, got, removed)
		}
	}
	if got, removed := StripPRComments("- plain\n"); got != "- plain\n" || removed {
		t.Fatal("plain text changed")
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"", "- a\n- b\n", "## Acceptance criteria\n- [ ] x\n  y\n# End\n- z\n", "```\n- a\n```\n- b",
		"1. one\n2) two\n\t- three\n---\n* * *\n", "\uFEFF- bom\r\n- crlf\r\n", strings.Repeat("- x\n", 120),
		"- " + strings.Repeat("é", 600) + "\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		doc, err := Parse(text)
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			if err == nil {
				t.Fatal("invalid text accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if (doc.SHA256 == "") != (text == "") || len(doc.Criteria) > MaxCriteria {
			t.Fatalf("%d criteria", len(doc.Criteria))
		}
		lines := strings.Count(text, "\n") + 1
		for i, c := range doc.Criteria {
			if c.ID != "AC-"+strconv.Itoa(i+1) || !ValidID(c.ID) {
				t.Fatalf("ID %q at %d", c.ID, i)
			}
			if c.Text == "" || len(c.Text) > MaxCriterionBytes || strings.TrimSpace(c.Text) != c.Text || strings.Contains(c.Text, "\n") {
				t.Fatalf("text %q", c.Text)
			}
			if c.Line < 1 || c.Line > lines || i > 0 && c.Line <= doc.Criteria[i-1].Line {
				t.Fatalf("line %d", c.Line)
			}
		}
		again, _ := Parse(text)
		if !reflect.DeepEqual(doc, again) {
			t.Fatal("Parse is not deterministic")
		}
	})
}
