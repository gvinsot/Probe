// Package knowledge reads, merges and writes the codebase knowledge base: a
// Markdown file (PROBE_KNOWLEDGE.md by default) committed in the repository,
// which the team edits by hand and which reviews propose to extend. Probe
// reads it at the tip of the base ref, like the policy, so a change never
// supplies the knowledge its own review receives; it never commits it.
//
// Format: an optional preamble, then one entry per level-2 heading. Metadata
// lines follow the heading, then free text:
//
//	## Refunds are validated by the gateway
//	- kind: architecture
//	- paths: pay/**, gateway/refund.go
//	- updated: 2026-09-30 (review of 1a2b3c4d)
//
//	The refund handler trusts the amount because ...
package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/linter"
	"github.com/gvinsot/Probe/app/internal/model"
)

// DefaultPath is the knowledge base read when --knowledge is not given.
const DefaultPath = "PROBE_KNOWLEDGE.md"

// Bounds of the knowledge base and of one entry.
const (
	MaxFileBytes = 256 * 1024
	MaxEntries   = 500
	MaxTitle     = 120
	MaxText      = 4000
	MaxPaths     = 10
	MaxPath      = 200
	MaxReason    = 500
)

const header = "# Probe knowledge base\n\nWhat the team and Probe reviews know about this codebase: how parts of it work, how they relate, known risks, architectural context, conventions and review knowledge. Edit it freely: one entry per `##` heading, then `- kind:` (" + "component, relationship, risk, architecture, convention, review or note), optional `- paths:` globs and `- updated:` lines, then the text. Probe reads it at the tip of the base branch and proposes updates in `.probe/knowledge-updates.json`; apply them with `probe knowledge apply`, then review and commit.\n"

// Base is a parsed knowledge base.
type Base struct {
	Preamble string
	Entries  []model.KnowledgeEntry
}

var metaLine = regexp.MustCompile(`^- (kind|paths|updated):\s*(.*)$`)

// Parse reads a knowledge base. A problem makes it an error, with its line,
// so that a manual edit is caught by probe knowledge check.
func Parse(data []byte) (*Base, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("knowledge base exceeds %d bytes", MaxFileBytes)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("knowledge base must be UTF-8 text")
	}
	b := &Base{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var preamble []string
	var current *model.KnowledgeEntry
	var text []string
	inMeta, inFence := false, false
	flush := func() {
		if current != nil {
			current.Text = strings.TrimSpace(strings.Join(text, "\n"))
			b.Entries = append(b.Entries, *current)
		}
		current, text = nil, nil
	}
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			flush()
			current = &model.KnowledgeEntry{Title: strings.TrimSpace(line[3:]), Kind: model.KnowledgeNote, Paths: []string{}}
			inMeta = true
			continue
		}
		if current == nil {
			preamble = append(preamble, line)
			continue
		}
		if inMeta {
			if m := metaLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				value := strings.TrimSpace(m[2])
				switch m[1] {
				case "kind":
					current.Kind = strings.ToLower(value)
				case "paths":
					current.Paths = splitPaths(value)
				case "updated":
					current.Updated = value
				}
				continue
			}
			if strings.TrimSpace(line) == "" && len(text) == 0 {
				continue
			}
			inMeta = false
		}
		text = append(text, line)
	}
	flush()
	b.Preamble = strings.TrimSpace(strings.Join(preamble, "\n"))
	if len(b.Entries) > MaxEntries {
		return nil, fmt.Errorf("knowledge base holds more than %d entries", MaxEntries)
	}
	seen := map[string]bool{}
	for _, e := range b.Entries {
		if err := validEntry(e.Title, e.Kind, e.Paths, e.Text); err != nil {
			return nil, fmt.Errorf("entry %q: %w", e.Title, err)
		}
		key := strings.ToLower(e.Title)
		if seen[key] {
			return nil, fmt.Errorf("entry %q appears twice; titles identify entries", e.Title)
		}
		seen[key] = true
	}
	return b, nil
}

func splitPaths(value string) []string {
	out := []string{}
	for _, p := range strings.Split(value, ",") {
		if p = strings.Trim(strings.TrimSpace(p), "`"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validEntry checks the fields every entry and update shares.
func validEntry(title, kind string, paths []string, text string) error {
	switch {
	case strings.TrimSpace(title) == "" || len(title) > MaxTitle || strings.ContainsAny(title, "\n\r"):
		return fmt.Errorf("a title is one line of at most %d bytes", MaxTitle)
	case !slices.Contains(model.KnowledgeKinds, kind):
		return fmt.Errorf("kind %q is not one of %s", kind, strings.Join(model.KnowledgeKinds, ", "))
	case len(paths) > MaxPaths:
		return fmt.Errorf("at most %d paths", MaxPaths)
	case len(text) > MaxText:
		return fmt.Errorf("text exceeds %d bytes", MaxText)
	}
	for _, p := range paths {
		if p == "" || len(p) > MaxPath || strings.ContainsAny(p, ",\n\r`") {
			return fmt.Errorf("invalid path glob %q", p)
		}
		if _, _, err := linter.MatchSensitive([]string{p}, "x"); err != nil {
			return fmt.Errorf("invalid path glob %q", p)
		}
	}
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "## ") {
			return errors.New("text cannot contain a level-2 heading outside a code fence: it starts a new entry")
		}
	}
	if fenced {
		return errors.New("text leaves a code fence open, which would swallow the next entries")
	}
	return nil
}

// ValidUpdate checks an update before it is recorded.
func ValidUpdate(u model.KnowledgeUpdate) error {
	if err := validEntry(u.Title, u.Kind, u.Paths, u.Text); err != nil {
		return err
	}
	if !u.Obsolete && strings.TrimSpace(u.Text) == "" {
		return errors.New("text is required unless the entry is obsolete")
	}
	if len(u.Reason) > MaxReason {
		return fmt.Errorf("reason exceeds %d bytes", MaxReason)
	}
	return nil
}

// Render writes a knowledge base back to Markdown.
func (b *Base) Render() []byte {
	var out strings.Builder
	if b.Preamble != "" {
		out.WriteString(b.Preamble + "\n")
	} else {
		out.WriteString(header)
	}
	for _, e := range b.Entries {
		out.WriteString("\n## " + e.Title + "\n")
		out.WriteString("- kind: " + e.Kind + "\n")
		if len(e.Paths) > 0 {
			out.WriteString("- paths: " + strings.Join(e.Paths, ", ") + "\n")
		}
		if e.Updated != "" {
			out.WriteString("- updated: " + e.Updated + "\n")
		}
		if e.Text != "" {
			out.WriteString("\n" + e.Text + "\n")
		}
	}
	return []byte(out.String())
}

// Merge applies updates in order: an update replaces the entry with the same
// title (case-insensitively), adds a new one, or removes it when obsolete.
// stamp is recorded as each changed entry's updated line. It returns how many
// entries were added, replaced and removed.
func (b *Base) Merge(updates []model.KnowledgeUpdate, stamp string) (added, replaced, removed int) {
	for _, u := range updates {
		i := slices.IndexFunc(b.Entries, func(e model.KnowledgeEntry) bool { return strings.EqualFold(e.Title, u.Title) })
		switch {
		case u.Obsolete && i >= 0:
			b.Entries = slices.Delete(b.Entries, i, i+1)
			removed++
		case u.Obsolete:
		case i >= 0:
			b.Entries[i] = entryOf(u, stamp)
			replaced++
		default:
			b.Entries = append(b.Entries, entryOf(u, stamp))
			added++
		}
	}
	return added, replaced, removed
}

func entryOf(u model.KnowledgeUpdate, stamp string) model.KnowledgeEntry {
	paths := append([]string{}, u.Paths...)
	return model.KnowledgeEntry{Title: strings.TrimSpace(u.Title), Kind: u.Kind, Paths: paths, Updated: stamp, Text: strings.TrimSpace(u.Text)}
}

// Relevant selects the entries given to a reviewer, within budget bytes of
// text: first the entries whose paths match a changed file, then the entries
// without paths (repository-wide context), each group in file order.
func (b *Base) Relevant(changed []string, budget int) []model.KnowledgeEntry {
	var matched, general []model.KnowledgeEntry
	for _, e := range b.Entries {
		if len(e.Paths) == 0 {
			general = append(general, e)
			continue
		}
		for _, p := range changed {
			if _, ok, _ := linter.MatchSensitive(e.Paths, p); ok {
				matched = append(matched, e)
				break
			}
		}
	}
	out := []model.KnowledgeEntry{}
	used := 0
	for _, e := range append(matched, general...) {
		size := len(e.Title) + len(e.Text) + 64
		if used+size > budget {
			continue
		}
		used += size
		out = append(out, e)
	}
	return out
}

// Read parses the knowledge base at a commit. A missing file is an empty base.
func Read(read func(path string) ([]byte, error), path string) (*Base, []byte, error) {
	data, err := read(path)
	if errors.Is(err, gitrepo.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &Base{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	b, err := Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, data, nil
}

// ValidPath checks a --knowledge path: repository-relative and portable.
func ValidPath(p string) error {
	if err := gitrepo.SafePath(p); err != nil {
		return fmt.Errorf("--knowledge: %w", err)
	}
	return nil
}

// Proposal file names in the output directory.
const (
	ProposalName = "knowledge-updates.json"
	PreviewName  = "KNOWLEDGE.md"
)

// WriteFile replaces path atomically.
func WriteFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".probe-knowledge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
