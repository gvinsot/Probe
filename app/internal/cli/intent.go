package cli

// Intent parsing (F5). The intent is untrusted caller input: it must be UTF-8
// text without NUL, SwiftProof's own PR-comment output is removed from it, and
// acceptance criteria are extracted from its Markdown list items (criteria
// grammar v1). Nothing is executed and no provider is called.

import (
	"fmt"

	"github.com/gvinsot/SwiftProof/app/internal/acceptance"
	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/redact"
)

// intentDoc is the parsed --intent / --intent-file text.
type intentDoc struct {
	Text     string                  // the intent recorded in the report
	SHA256   string                  // hex sha256 of Text; "" when Text is empty
	Criteria []model.IntentCriterion // acceptance criteria of Text, in order
	Notes    []string                // Unverified entries about the parsing
}

// parseIntent parses the intent text. An error exits 3 ("intent: ..."): the
// text must be valid UTF-8 without NUL bytes. Every block from
// model.PRCommentBegin to model.PRCommentEnd is removed first (an unterminated
// begin marker removes the rest), with a fixed note, so that a SwiftProof PR
// comment copied into the intent never feeds criteria. The stripped text is
// then redacted, as report sanitizing would redact it: Text is exactly the
// intent the report records, SHA256 hashes it, and Criteria are its acceptance
// criteria. The hash therefore never covers a secret that redaction hides from
// the report, and anyone can recompute it and the criteria from the recorded
// intent. Notes records the removal and any list items skipped by the
// extraction limits.
func parseIntent(text string) (intentDoc, error) {
	if err := acceptance.CheckEncoding(text); err != nil {
		return intentDoc{}, err
	}
	stripped, removed := acceptance.StripPRComments(text)
	// A redaction marker contains no character of a PR-comment marker, so
	// redacting cannot rebuild one.
	recorded := redact.Redact(stripped)
	doc, err := acceptance.Parse(recorded)
	if err != nil {
		return intentDoc{}, err
	}
	var notes []string
	if removed {
		notes = append(notes, acceptance.PRCommentNote)
	}
	notes = append(notes, doc.Notes()...)
	return intentDoc{Text: recorded, SHA256: doc.SHA256, Criteria: doc.Criteria, Notes: notes}, nil
}

// intentLine is the stdout line about acceptance criteria and intent-test
// failures; "" when the intent yielded no criteria. It counts what Finalize
// accepted and never presents a count as a ratio of criteria: without an
// accepted failure it prints the extraction count alone when no intent test
// ran (always the case for lint), and otherwise says that the absence of a
// failure says nothing about the criteria.
func intentLine(r *model.Report) string {
	n := len(r.IntentCriteria)
	if n == 0 {
		return ""
	}
	criteria, hold := "acceptance criteria", "the criteria hold"
	if n == 1 {
		criteria, hold = "acceptance criterion", "the criterion holds"
	}
	switch f := len(r.IntentTestFailures); f {
	case 0:
		if !intentTestRan(r) {
			return fmt.Sprintf("Intent: %d %s extracted.", n, criteria)
		}
		return fmt.Sprintf("Intent: %d %s extracted; no intent-test failure was accepted, which says nothing about whether %s.", n, criteria, hold)
	case 1:
		return fmt.Sprintf("Intent: %d %s extracted; 1 intent-test failure (a model-written test failed on the candidate; no baseline control; weaker than a reproduced issue).", n, criteria)
	default:
		return fmt.Sprintf("Intent: %d %s extracted; %d intent-test failures (model-written tests failed on the candidate; no baseline control; weaker than a reproduced issue).", n, criteria, f)
	}
}

// intentTestRan reports whether the report records a generated_test_intent
// check.
func intentTestRan(r *model.Report) bool {
	for _, c := range r.Checks {
		if c.Kind == model.CheckGeneratedIntent {
			return true
		}
	}
	return false
}
