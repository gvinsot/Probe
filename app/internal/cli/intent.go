package cli

// Intent parsing (F5). The intent is untrusted caller input: it must be UTF-8
// text without NUL, SwiftProof's own PR-comment output is removed from it, and
// acceptance criteria are extracted from its Markdown list items (criteria
// grammar v1). Nothing is executed and no provider is called.

import (
	"fmt"

	"github.com/gvinsot/SwiftProof/app/internal/acceptance"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// intentDoc is the parsed --intent / --intent-file text.
type intentDoc struct {
	Text     string                  // the intent recorded in the report
	SHA256   string                  // hex sha256 of Text; "" when not computed
	Criteria []model.IntentCriterion // acceptance criteria, in order
	Notes    []string                // Unverified entries about the parsing
}

// parseIntent parses the intent text. An error exits 3 ("intent: ..."): the
// text must be valid UTF-8 without NUL bytes. Every block from
// model.PRCommentBegin to model.PRCommentEnd is removed first (an unterminated
// begin marker removes the rest), with a fixed note, so that a SwiftProof PR
// comment copied into the intent never feeds criteria. Text is the stripped
// text, SHA256 hashes it, and Criteria are its acceptance criteria. Notes
// records the removal and any list items skipped by the extraction limits.
func parseIntent(text string) (intentDoc, error) {
	if err := acceptance.CheckEncoding(text); err != nil {
		return intentDoc{}, err
	}
	stripped, removed := acceptance.StripPRComments(text)
	doc, err := acceptance.Parse(stripped)
	if err != nil {
		return intentDoc{}, err
	}
	var notes []string
	if removed {
		notes = append(notes, acceptance.PRCommentNote)
	}
	notes = append(notes, doc.Notes()...)
	return intentDoc{Text: stripped, SHA256: doc.SHA256, Criteria: doc.Criteria, Notes: notes}, nil
}

// intentLine is the stdout line about acceptance criteria and intent-test
// failures; "" when the intent yielded no criteria. It counts what Finalize
// accepted and never presents a count as a ratio of criteria.
func intentLine(r *model.Report) string {
	n := len(r.IntentCriteria)
	if n == 0 {
		return ""
	}
	criteria := "acceptance criteria"
	if n == 1 {
		criteria = "acceptance criterion"
	}
	switch f := len(r.IntentTestFailures); f {
	case 0:
		return fmt.Sprintf("Intent: %d %s extracted; 0 intent-test failures.", n, criteria)
	case 1:
		return fmt.Sprintf("Intent: %d %s extracted; 1 intent-test failure (a model-written test failed on the candidate; no baseline control; weaker than a reproduced issue).", n, criteria)
	default:
		return fmt.Sprintf("Intent: %d %s extracted; %d intent-test failures (model-written tests failed on the candidate; no baseline control; weaker than a reproduced issue).", n, criteria, f)
	}
}
