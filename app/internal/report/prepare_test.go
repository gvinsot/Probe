package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const (
	prepareCommit = "0123456789abcdef0123456789abcdef01234567"
	prepareBaseID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	prepareImage  = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func prepareRecord(status, reason string) *model.Prepare {
	p := &model.Prepare{
		Status: status, Reason: reason, SourceCommit: prepareCommit, Command: []string{"sh", "-c", "npm ci *not* `x` <b>#1</b>"},
		User: "sandbox", BaseImage: "golang:1.26-bookworm", BaseImageID: prepareBaseID,
		Inputs:     []model.PreparedInput{{Path: "go.sum", SHA256: strings.Repeat("a", 64), Size: 12}, {Path: "a_b[1].lock", SHA256: strings.Repeat("b", 64), Size: 3}},
		DurationMS: 1500, Note: model.PrepareNote,
	}
	if status == model.PrepareBuilt || status == model.PrepareReused {
		p.Key, p.ImageID, p.AddedBytes = strings.Repeat("c", 64), prepareImage, 20480
	}
	if status == model.PrepareBuilt || status == model.PrepareFailed {
		p.LogSHA256 = strings.Repeat("e", 64)
	}
	return p
}

func TestPrepareSectionRendersEachStatus(t *testing.T) {
	for status, want := range map[string]string{
		model.PrepareBuilt:        "Status: built. The base-branch prepare command ran in this review",
		model.PrepareReused:       "Status: reused. The prepare command did not run in this review",
		model.PrepareFailed:       "Status: failed. The prepare command exited 3; see the prepare\\_output log. No image was produced, so no check ran.",
		model.PrepareNotPermitted: "Status: not permitted. No local image was prepared",
		model.PrepareNotRun:       "Status: not run. Automated execution was explicitly disabled.",
	} {
		reason := map[string]string{
			model.PrepareFailed:       "the prepare command exited 3; see the prepare_output log",
			model.PrepareNotPermitted: "no local image was prepared for key cccc… and source commit 0123; building it needs --allow-prepare-network",
			model.PrepareNotRun:       "automated execution was explicitly disabled",
		}[status]
		r := &model.Report{Version: 1, Prepare: prepareRecord(status, reason)}
		Finalize(r, true)
		md := string(Markdown(r))
		body := section(t, md, "## Dependency Preparation")
		if !strings.Contains(body, want) {
			t.Errorf("%s: missing %q in:\n%s", status, want, body)
		}
		if strings.Index(md, "## Dependency Preparation") > strings.Index(md, "## Automated Checks") || strings.Count(md, "## Dependency Preparation") != 1 {
			t.Errorf("%s: section misplaced", status)
		}
		for _, fragment := range []string{
			"Command, one argument per line (user sandbox; network disabled):\n- sh\n- -c\n- " + inline("npm ci *not* `x` <b>#1</b>") + "\n\n",
			"Source commit: " + prepareCommit + ".",
			"- a\\_b\\[1\\].lock (sha256 " + strings.Repeat("b", 64) + "; 3 bytes)",
			model.PrepareNote,
		} {
			if !strings.Contains(body, fragment) {
				t.Errorf("%s: missing %q in:\n%s", status, fragment, body)
			}
		}
		imageLine := strings.Contains(body, "Image: "+prepareImage+", derived from golang:1.26-bookworm ("+prepareBaseID+"); adds 20480 bytes")
		if imageLine != (status == model.PrepareBuilt || status == model.PrepareReused) {
			t.Errorf("%s: image line %v:\n%s", status, imageLine, body)
		}
		if !imageLine && !strings.Contains(body, "Sandbox image: golang:1.26-bookworm ("+prepareBaseID+")") {
			t.Errorf("%s: no sandbox image line:\n%s", status, body)
		}
		if strings.Contains(body, "<b>") || strings.Contains(body, "\n#") {
			t.Errorf("%s: unescaped text:\n%s", status, body)
		}
		if stray := strayHeadings(md); len(stray) > 0 {
			t.Errorf("%s: stray headings %q", status, stray)
		}
	}
	md := string(Markdown(&model.Report{Version: 1}))
	if strings.Contains(md, "Dependency Preparation") {
		t.Fatal("section rendered without a prepare object")
	}
	for args, want := range map[string]string{
		"go\x00mod\x00download": "Command: go mod download (user root; network enabled)\n",
		"sh\x00\x00x":           "Command, one argument per line (user root; network enabled):\n- sh\n- (empty argument)\n- x\n",
		"":                      "Command, one argument per line (user root; network enabled):\n\n",
	} {
		p := prepareRecord(model.PrepareBuilt, "")
		p.Command, p.User, p.Network = nil, "root", true
		if args != "" {
			p.Command = strings.Split(args, "\x00")
		}
		body := section(t, string(Markdown(&model.Report{Version: 1, Prepare: p})), "## Dependency Preparation")
		if !strings.Contains(body, want) {
			t.Errorf("command %q: missing %q in:\n%s", args, want, body)
		}
	}
}

func TestPrepareSectionCapsInputsAndListsChangedInputs(t *testing.T) {
	p := prepareRecord(model.PrepareReused, "")
	p.Inputs = nil
	for i := 0; i < 53; i++ {
		p.Inputs = append(p.Inputs, model.PreparedInput{Path: fmt.Sprintf("dir%02d/go.sum", i), SHA256: strings.Repeat("a", 64), Size: 1})
	}
	r := &model.Report{Version: 1, Prepare: p}
	for i := 0; i < 22; i++ {
		r.Signals = append(r.Signals, model.Signal{ID: fmt.Sprintf("sig-%d", i), Kind: model.SignalPrepareInputChanged, Path: fmt.Sprintf("dir%02d/go.sum", i), Line: 1, Severity: "medium", Summary: "x", Evidence: "y"})
	}
	r.Signals = append(r.Signals, model.Signal{ID: "sig-x", Kind: "dependency_change", Path: "other.lock", Line: 1, Severity: "medium", Summary: "x", Evidence: "y"})
	body := section(t, string(Markdown(r)), "## Dependency Preparation")
	if !strings.Contains(body, "Inputs (53):") || !strings.Contains(body, "dir49/go.sum") || strings.Contains(body, "dir50/go.sum (") || !strings.Contains(body, "- … and 3 more in confidence-report.json") {
		t.Fatalf("inputs not capped:\n%s", body)
	}
	if !strings.Contains(body, "Candidate changes to declared inputs (never installed): dir00/go.sum,") || !strings.Contains(body, "dir19/go.sum, and 2 more") || strings.Contains(body, "other.lock") {
		t.Fatalf("changed inputs:\n%s", body)
	}
}

// Finalize never lets the prepare record change a status, a target, the
// unverified list or the exit code; finalizePrepare only restores an empty
// note.
func TestFinalizeIgnoresPrepare(t *testing.T) {
	for _, status := range []string{model.PrepareBuilt, model.PrepareReused, model.PrepareFailed, model.PrepareNotPermitted, model.PrepareNotRun} {
		for _, ci := range []bool{false, true} {
			with, without := proofReport(), proofReport()
			with.Prepare = prepareRecord(status, "reason")
			Finalize(with, ci)
			Finalize(without, ci)
			if with.ExitCode != without.ExitCode || !reflect.DeepEqual(with.ReproducedIssues, without.ReproducedIssues) || !reflect.DeepEqual(with.ReviewTargets, without.ReviewTargets) || !reflect.DeepEqual(with.Unverified, without.Unverified) || !reflect.DeepEqual(with.Hypotheses, without.Hypotheses) {
				t.Fatalf("%s ci=%v: prepare changed the finalized report", status, ci)
			}
		}
	}
	r := &model.Report{Version: 1, Prepare: &model.Prepare{Status: model.PrepareNotRun, Note: "  "}}
	Finalize(r, true)
	if r.Prepare.Note != model.PrepareNote || r.ExitCode != 0 {
		t.Fatalf("note %q exit %d", r.Prepare.Note, r.ExitCode)
	}
	custom := &model.Report{Version: 1, Prepare: &model.Prepare{Status: model.PrepareNotRun, Note: "kept"}}
	Finalize(custom, false)
	if custom.Prepare.Note != "kept" {
		t.Fatal("a non-empty note was replaced")
	}
}

func TestPrepareSurvivesRerender(t *testing.T) {
	r := proofReport()
	r.Prepare = prepareRecord(model.PrepareBuilt, "")
	r.Prepare.Command = []string{"sh", "-c", "npm ci --password=hunter2"}
	Finalize(r, true)
	first := t.TempDir()
	if err := Write(first, r, []string{FormatMarkdown, FormatJSON}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(first, "confidence-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("hunter2")) {
		t.Fatal("the prepare command was written unredacted")
	}
	var saved model.Report
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	Finalize(&saved, true)
	second := t.TempDir()
	if err := Write(second, &saved, []string{FormatMarkdown, FormatJSON}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"confidence-report.json", "CONFIDENCE_REPORT.md"} {
		a, _ := os.ReadFile(filepath.Join(first, name))
		b, _ := os.ReadFile(filepath.Join(second, name))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s differs after a re-render", name)
		}
	}
}
