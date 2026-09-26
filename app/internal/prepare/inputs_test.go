package prepare

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/config"
	"github.com/gvinsot/SwiftProof/app/internal/linter"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

func lockfileHunk(start, lines int) model.Hunk {
	h := model.Hunk{OldStart: start, OldLines: 0, NewStart: start, NewLines: lines}
	for i := 0; i < lines; i++ {
		h.Lines = append(h.Lines, model.DiffLine{Kind: "add", NewLine: start + i, Content: "dep v1.0.0 h1:x="})
	}
	return h
}

func inputsChange() model.Change {
	return model.Change{Files: []model.ChangedFile{
		{Path: "go.sum", Status: "M", Hunks: []model.Hunk{lockfileHunk(4000, 5000)}},
		{Path: "app/go.mod", Status: "A", Hunks: []model.Hunk{lockfileHunk(1, 3)}},
		{Path: "old.lock", Status: "D", Hunks: []model.Hunk{{OldStart: 1, OldLines: 1, Lines: []model.DiffLine{{Kind: "delete", OldLine: 1, Content: "x"}}}}},
		{Path: "renamed.mod", OldPath: "go.mod", Status: "R"},
		{Path: "deps.bin", Status: "M", Binary: true},
		{Path: "main.go", Status: "M", Hunks: []model.Hunk{lockfileHunk(1, 1)}},
		{Path: "vendor/go.sum", Status: "M", Hunks: []model.Hunk{lockfileHunk(1, 1)}},
	}}
}

func TestChangedInputs(t *testing.T) {
	spec := &config.Prepare{Inputs: []string{"go.mod", "go.sum", "*/go.mod", "*.lock", "deps.bin"}}
	got := ChangedInputs(spec, inputsChange())
	if strings.Join(got, ",") != "app/go.mod,deps.bin,go.sum,old.lock,renamed.mod" {
		t.Fatalf("changed inputs %q", got)
	}
	if ChangedInputs(nil, inputsChange()) != nil || Signals(nil, inputsChange()) != nil {
		t.Fatal("a nil spec matched")
	}
	if Matching([]string{"[", "go.mod"}, "go.mod") != "go.mod" {
		t.Fatal("an invalid pattern stopped matching")
	}
}

// One medium signal per matching file, anchored at its first changed line
// (not at line 0 of a long lockfile hunk), with stable IDs through
// linter.Merge.
func TestSignalsAnchorAndIDs(t *testing.T) {
	spec := &config.Prepare{Inputs: []string{"go.mod", "go.sum", "*/go.mod", "*.lock", "deps.bin"}}
	signals := Signals(spec, inputsChange())
	want := map[string]string{"go.sum": "4000 new", "app/go.mod": "1 new", "old.lock": "1 old", "renamed.mod": "1 new", "deps.bin": "1 new"}
	if len(signals) != len(want) {
		t.Fatalf("%d signals: %+v", len(signals), signals)
	}
	for _, s := range signals {
		if s.Kind != model.SignalPrepareInputChanged || s.Severity != "medium" || s.Summary == "" || !strings.Contains(s.Evidence, "never installed") {
			t.Errorf("signal %+v", s)
		}
		if got := strings.Join([]string{strconv.Itoa(s.Line), s.Side}, " "); got != want[s.Path] {
			t.Errorf("%s anchored at %s, want %s", s.Path, got, want[s.Path])
		}
	}
	if !strings.Contains(signals[3].Evidence, "pattern go.mod") {
		t.Errorf("the renamed file names the pattern its old path matched: %q", signals[3].Evidence)
	}
	a, b := linter.Merge(nil, Signals(spec, inputsChange())), linter.Merge(nil, Signals(spec, inputsChange()))
	for i := range a {
		if a[i].ID == "" || a[i].ID != b[i].ID {
			t.Fatalf("unstable IDs %q %q", a[i].ID, b[i].ID)
		}
	}
}
