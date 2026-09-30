// Package office compares two versions of an Office document and flags the
// modifications that deserve a human look.
//
// The comparison is deterministic: it reads the OOXML parts directly (Word,
// Excel and PowerPoint, which macOS Office writes in the same formats) and
// applies fixed rules. A language model may explain a report and raise extra
// findings kept apart, but it
// never changes what the rules flagged.
package office

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/desktop/internal/msg"
)

// Kind is the family of a supported document.
type Kind string

const (
	Word       Kind = "word"
	Excel      Kind = "excel"
	PowerPoint Kind = "powerpoint"
)

// KindOf returns the document family for a file name, or "" when the format
// is not supported.
func KindOf(name string) Kind {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".docx", ".docm":
		return Word
	case ".xlsx", ".xlsm":
		return Excel
	case ".pptx", ".pptm":
		return PowerPoint
	}
	return ""
}

// Severity levels, ordered, shared with the CLI and the hub.
const (
	None     = "none"
	Low      = "low"
	Medium   = "medium"
	High     = "high"
	Critical = "critical"
)

var severityRank = map[string]int{None: 0, Low: 1, Medium: 2, High: 3, Critical: 4}

// Rank orders severities; unknown values rank as none.
func Rank(severity string) int { return severityRank[severity] }

// Finding is a modification a rule considers risky.
type Finding struct {
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Title    string `json:"title"`
	Location string `json:"location,omitempty"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
	// Consistency tells whether the change still agrees with the rest of
	// the document (Inconsistent, Consistent), empty when unknown; Note
	// says why.
	Consistency string `json:"consistency,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Change is one raw modification, flagged or not.
type Change struct {
	Kind     string `json:"kind"` // added, removed, modified, recomputed
	Location string `json:"location"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
}

// Report is the result of comparing a baseline with the current version.
type Report struct {
	Kind           Kind      `json:"kind"`
	Severity       string    `json:"severity"`
	Findings       []Finding `json:"findings"`
	Changes        []Change  `json:"changes"`
	ChangeCount    int       `json:"change_count"`
	Truncated      bool      `json:"truncated,omitempty"`
	LastModifiedBy string    `json:"last_modified_by,omitempty"`
	// Mentions are the replaced or removed terms that the unchanged
	// passages still use, with those passages.
	Mentions []Mention `json:"mentions,omitempty"`
}

// Limits keep a report readable and bounded in size whatever the document.
const (
	maxChanges         = 400
	maxFindingsPerRule = 40
	maxExcerpt         = 600
)

// ErrUnreadable reports a file that is not a valid package yet, typically
// because it is still being written or synchronized.
var ErrUnreadable = errors.New(msg.M("document is not a readable Office package"))

// Compare diffs two versions of a document of the given kind.
func Compare(kind Kind, before, after []byte) (*Report, error) {
	oldPkg, err := openPackage(before)
	if err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	newPkg, err := openPackage(after)
	if err != nil {
		return nil, fmt.Errorf("current version: %w", err)
	}
	r := &reportBuilder{counts: map[string]int{}}
	switch kind {
	case Word:
		err = compareWord(r, oldPkg, newPkg)
	case Excel:
		err = compareExcel(r, oldPkg, newPkg)
	case PowerPoint:
		err = comparePowerPoint(r, oldPkg, newPkg)
	default:
		return nil, fmt.Errorf("unsupported document kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	comparePackage(r, oldPkg, newPkg)
	return r.build(kind, newPkg.lastModifiedBy()), nil
}

// reportBuilder accumulates findings and changes with the size limits applied.
type reportBuilder struct {
	findings    []Finding
	counts      map[string]int
	changes     []Change
	changeCount int
	// For the consistency check: the modified passages and every passage
	// of the current version.
	edits    []edit
	passages []passage
}

// edited records a modified passage for the consistency check.
func (r *reportBuilder) edited(location, before, after string) {
	r.edits = append(r.edits, edit{location, before, after})
}

// current records the passages of the current version.
func (r *reportBuilder) current(location string, texts ...string) {
	for _, t := range texts {
		r.passages = append(r.passages, passage{location, t})
	}
}

func (r *reportBuilder) flag(f Finding) {
	r.counts[f.Rule]++
	if r.counts[f.Rule] > maxFindingsPerRule {
		return
	}
	f.Before, f.After = excerpt(f.Before), excerpt(f.After)
	r.findings = append(r.findings, f)
}

func (r *reportBuilder) change(c Change) {
	r.changeCount++
	if len(r.changes) >= maxChanges {
		return
	}
	c.Before, c.After = excerpt(c.Before), excerpt(c.After)
	r.changes = append(r.changes, c)
}

func (r *reportBuilder) build(kind Kind, modifiedBy string) *Report {
	mentions := r.consistency()
	// A rule that fired more often than shown gets one summary line, so the
	// reader knows the list is partial without wading through every cell.
	for rule, n := range r.counts {
		if n <= maxFindingsPerRule {
			continue
		}
		sev, title := Low, ""
		for _, f := range r.findings {
			if f.Rule == rule {
				sev, title = f.Severity, f.Title
				break
			}
		}
		r.findings = append(r.findings, Finding{
			Severity: sev,
			Rule:     rule,
			Title:    fmt.Sprintf(msg.M("%s: %d more occurrences not listed"), title, n-maxFindingsPerRule),
		})
	}
	sort.SliceStable(r.findings, func(i, j int) bool {
		return Rank(r.findings[i].Severity) > Rank(r.findings[j].Severity)
	})
	severity := None
	for _, f := range r.findings {
		if Rank(f.Severity) > Rank(severity) {
			severity = f.Severity
		}
	}
	if severity == None && r.changeCount > 0 {
		severity = Low
	}
	if r.findings == nil {
		r.findings = []Finding{}
	}
	if r.changes == nil {
		r.changes = []Change{}
	}
	return &Report{
		Kind:           kind,
		Severity:       severity,
		Findings:       r.findings,
		Changes:        r.changes,
		ChangeCount:    r.changeCount,
		Truncated:      r.changeCount > len(r.changes),
		LastModifiedBy: modifiedBy,
		Mentions:       mentions,
	}
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= maxExcerpt {
		return s
	}
	return string([]rune(s)[:maxExcerpt]) + "…"
}

// pkg is an opened OOXML package: a zip archive of XML parts.
type pkg struct {
	files map[string]*zip.File
}

func openPackage(data []byte) (*pkg, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrUnreadable
	}
	p := &pkg{files: map[string]*zip.File{}}
	for _, f := range zr.File {
		p.files[strings.TrimPrefix(f.Name, "/")] = f
	}
	if _, ok := p.files["[Content_Types].xml"]; !ok {
		return nil, ErrUnreadable
	}
	return p, nil
}

// maxPartSize bounds a single decompressed XML part (zip bomb guard).
const maxPartSize = 256 << 20

func (p *pkg) read(name string) ([]byte, bool) {
	f, ok := p.files[name]
	if !ok {
		return nil, false
	}
	if f.UncompressedSize64 > maxPartSize {
		return nil, false
	}
	rc, err := f.Open()
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(ioLimit(rc, maxPartSize)); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

func (p *pkg) has(name string) bool {
	_, ok := p.files[name]
	return ok
}

// list returns the part names under a prefix, sorted.
func (p *pkg) list(prefix string) []string {
	var names []string
	for name := range p.files {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (p *pkg) lastModifiedBy() string {
	data, ok := p.read("docProps/core.xml")
	if !ok {
		return ""
	}
	var who string
	walkXML(data, func(el xmlElement) {
		if el.name == "lastModifiedBy" {
			who = el.text()
		}
	})
	return strings.TrimSpace(who)
}

// comparePackage applies the rules common to every format: macros, external
// links and embedded objects.
func comparePackage(r *reportBuilder, before, after *pkg) {
	macro := func(p *pkg) bool {
		for name := range p.files {
			if strings.HasSuffix(strings.ToLower(name), "vbaproject.bin") {
				return true
			}
		}
		return false
	}
	if !macro(before) && macro(after) {
		r.flag(Finding{Severity: High, Rule: "package.macro-added", Title: msg.M("Macros (VBA code) were added to the document")})
		r.change(Change{Kind: "added", Location: msg.M("Macros"), After: "vbaProject.bin"})
	} else if macro(before) && !macro(after) {
		r.change(Change{Kind: "removed", Location: msg.M("Macros"), Before: "vbaProject.bin"})
	}

	oldLinks, newLinks := externalTargets(before), externalTargets(after)
	for target := range newLinks {
		if !oldLinks[target] {
			r.flag(Finding{Severity: Medium, Rule: "package.external-link-added", Title: msg.M("A link to an external file or template was added"), After: target})
			r.change(Change{Kind: "added", Location: msg.M("External links"), After: target})
		}
	}

	oldEmbeds := map[string]bool{}
	for _, name := range embeddedParts(before) {
		oldEmbeds[name] = true
	}
	for _, name := range embeddedParts(after) {
		if !oldEmbeds[name] {
			r.flag(Finding{Severity: Medium, Rule: "package.embedded-object-added", Title: msg.M("An embedded object (file or OLE object) was added"), After: filepath.Base(name)})
			r.change(Change{Kind: "added", Location: msg.M("Embedded objects"), After: filepath.Base(name)})
		}
	}
}

func embeddedParts(p *pkg) []string {
	var out []string
	for _, prefix := range []string{"word/embeddings/", "xl/embeddings/", "ppt/embeddings/"} {
		out = append(out, p.list(prefix)...)
	}
	return out
}

// externalTargets collects the external targets of relationships that pull
// data or code from elsewhere. Plain hyperlinks are ignored: they are content,
// not a dependency.
func externalTargets(p *pkg) map[string]bool {
	out := map[string]bool{}
	for name := range p.files {
		if !strings.HasSuffix(name, ".rels") {
			continue
		}
		data, ok := p.read(name)
		if !ok {
			continue
		}
		walkXML(data, func(el xmlElement) {
			if el.name != "Relationship" || el.attr("TargetMode") != "External" {
				return
			}
			typ := el.attr("Type")
			if strings.HasSuffix(typ, "/hyperlink") {
				return
			}
			out[el.attr("Target")] = true
		})
	}
	return out
}
