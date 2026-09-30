package issuetext

import (
	"testing"

	"github.com/gvinsot/Probe/app/internal/acceptance"
)

func TestProseLists(t *testing.T) {
	in := "- context\n1. step\n\n## Acceptance criteria\n\n- [ ] kept\n  - nested kept\n\n### Detail\n\n- still in scope\n\n## Design\n\n- [x] prose\n```\n- code\n```"
	want := "• context\n(1) step\n\n## Acceptance criteria\n\n- [ ] kept\n  - nested kept\n\n### Detail\n\n- still in scope\n\n## Design\n\n• prose\n```\n- code\n```"
	got := ProseLists(in)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	doc, err := acceptance.Parse(got)
	if err != nil || len(doc.Criteria) != 3 {
		t.Fatalf("criteria %+v %v", doc.Criteria, err)
	}
	if doc, _ := acceptance.Parse(ProseLists("- a\n- b")); len(doc.Criteria) != 0 {
		t.Fatalf("unscoped items stayed criteria: %+v", doc.Criteria)
	}
}
