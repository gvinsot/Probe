package office

import (
	"strings"
	"testing"
)

func TestReplacedNameStillUsedElsewhere(t *testing.T) {
	parties := "Entre M. Jean Dupont, ci-après « le Bailleur », et Mme Claire Petit, ci-après « le Preneur »."
	before := docx(t, parties,
		"Le loyer est fixé à 900 euros par mois.",
		"Les clés sont remises par M. Jean Dupont au Preneur.",
		"M. Jean Dupont s'engage à effectuer les grosses réparations.")
	after := docx(t, parties,
		"Le loyer est fixé à 900 euros par mois.",
		"Les clés sont remises par M. Paul Martin au Preneur.",
		"M. Jean Dupont s'engage à effectuer les grosses réparations.")
	r := mustCompare(t, Word, before, after)

	f, ok := rules(r)["text.inconsistent-mention"]
	if !ok {
		t.Fatalf("no consistency finding: %+v", r.Findings)
	}
	if f.Consistency != Inconsistent || f.Location != "Paragraph 3" || f.Before != "Jean Dupont" || f.After != "Paul Martin" {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(f.Title, "Paragraph 1") || !strings.Contains(f.Title, "Paragraph 4") {
		t.Fatalf("title does not say where the old name is used: %q", f.Title)
	}
	if !strings.Contains(f.Note, "le Bailleur") {
		t.Fatalf("note lacks the context naming the role: %q", f.Note)
	}
	if len(r.Mentions) != 1 || r.Mentions[0].Count != 2 || len(r.Mentions[0].Elsewhere) != 2 {
		t.Fatalf("mentions = %+v", r.Mentions)
	}
}

func TestConsistentReplacementIsNotFlagged(t *testing.T) {
	// Replaced everywhere: the document agrees with itself.
	before := docx(t, "Le Bailleur, M. Jean Dupont, loue le bien.", "M. Jean Dupont remet les clés.")
	after := docx(t, "Le Bailleur, M. Paul Martin, loue le bien.", "M. Paul Martin remet les clés.")
	r := mustCompare(t, Word, before, after)
	if f, ok := rules(r)["text.inconsistent-mention"]; ok || len(r.Mentions) != 0 {
		t.Fatalf("consistent replacement flagged: %+v", f)
	}
}

func TestFiguresAndCommonWordsAreLeftToOtherRules(t *testing.T) {
	before := docx(t, "Le paiement intervient sous 30 jours.", "Le délai de 30 jours court à compter de la facture.", "Il est payé par virement.")
	after := docx(t, "Le paiement intervient sous 60 jours.", "Le délai de 30 jours court à compter de la facture.", "Il sera payé par virement.")
	r := mustCompare(t, Word, before, after)
	if len(r.Mentions) != 0 {
		t.Fatalf("figures or common words reported: %+v", r.Mentions)
	}
}

func TestMovedTermIsNotRemoved(t *testing.T) {
	got := replacedTerms("Signé par Jean Dupont à Paris.", "À Paris, signé par Jean Dupont.")
	if len(got) != 0 {
		t.Fatalf("moved words reported as replaced: %+v", got)
	}
}

func TestSlideTitleStillUsedInNotes(t *testing.T) {
	before := pptx(t, []string{"Projet Atlas", "Budget 2026"}, []string{"Planning du projet Atlas"})
	after := pptx(t, []string{"Projet Orion", "Budget 2026"}, []string{"Planning du projet Atlas"})
	r := mustCompare(t, PowerPoint, before, after)
	f, ok := rules(r)["text.inconsistent-mention"]
	if !ok || f.Before != "Atlas" || f.After != "Orion" || !strings.Contains(f.Title, "Slide 2") {
		t.Fatalf("finding = %+v, all = %+v", f, r.Findings)
	}
}
