package report

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// groupFileSignals combines overlapping path heuristics, retaining the source
// alerts. Other concerns (missing tests, dependencies, findings, etc.) stay
// independent. Different severities and AI judgments remain independently
// filterable; dismissed signals never enter this function.
func (r *Report) groupFileSignals(alerts []Alert) []Alert {
	type changeKey struct{ key, title string }
	files := make(map[string]ChangedFile, len(r.Change.Files))
	for _, f := range r.Change.Files {
		files[f.Path] = f
	}
	changes := map[string]changeKey{}
	groups := map[string]int{}
	out := make([]Alert, 0, len(alerts))
	for _, a := range alerts {
		if a.Kind != KindSignal || a.Scope != ScopeFile || a.Path == "" || len(a.Reasons) != 1 ||
			(a.Reasons[0] != "sensitive_path" && a.Reasons[0] != "infrastructure_change") {
			out = append(out, a)
			continue
		}
		change, computed := changes[a.Path]
		if !computed {
			change.key, change.title = fileChangeKey(files[a.Path])
			changes[a.Path] = change
		}
		key := "path:" + a.Path
		if change.key != "" {
			key = change.key
		}
		key = groupHash([]string{key, a.Severity, a.Status, a.Judgment, a.OriginalSeverity})
		if i, ok := groups[key]; ok {
			g := &out[i]
			if len(g.Members) == 0 {
				first := *g
				*g = Alert{ID: "group:" + key, Kind: KindSignal, Severity: a.Severity,
					Status: a.Status, Scope: ScopeFile, Path: first.Path,
					Judgment: a.Judgment, OriginalSeverity: a.OriginalSeverity,
					Title: "Changes in a sensitive or deployment file", Members: []Alert{first}}
				if change.title != "" {
					g.Title = change.title
				}
			}
			if g.Path != a.Path {
				g.Path = ""
				if change.title == "" {
					g.Title = "Same change in sensitive or deployment files"
				}
			}
			g.Members = append(g.Members, a)
		} else {
			groups[key] = len(out)
			out = append(out, a)
		}
	}
	return out
}

func groupHash(v any) string {
	data, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// fileChangeKey compares the complete edit, never the surrounding context or
// line numbers. Missing/truncated diffs, renames, binary and mode-only changes
// cannot establish that two files carry the same change.
func fileChangeKey(f ChangedFile) (string, string) {
	if f.Binary || f.OldPath != "" || f.Status != "M" {
		return "", ""
	}
	var removed, added []string
	var edits [][2]string
	for _, h := range f.Hunks {
		for _, line := range h.Lines {
			switch line.Kind {
			case "add":
				added = append(added, line.Content)
			case "delete":
				removed = append(removed, line.Content)
			default:
				continue
			}
			edits = append(edits, [2]string{line.Kind, line.Content})
		}
	}
	if len(edits) == 0 || len(removed) != f.Deletions || len(added) != f.Additions {
		return "", ""
	}
	if len(removed) == 1 && len(added) == 1 {
		oldKey, oldValue := configAssignment(f.Path, removed[0])
		newKey, newValue := configAssignment(f.Path, added[0])
		if oldKey != "" && oldKey == newKey && oldValue != newValue {
			return "config:" + groupHash([]string{oldKey, oldValue, newValue}), "Configuration value changed: " + oldKey
		}
	}
	return "edit:" + groupHash(edits), ""
}

// Only simple literal defaults are normalized. Expressions, quoting, comments
// and multi-line edits are deliberately not interpreted as equivalent intent.
var dockerArgument = regexp.MustCompile(`^ARG ([A-Za-z_][A-Za-z0-9_]*)=([A-Za-z0-9_./:@+\-]+)$`)
var composeDefault = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*): \$\{([A-Za-z_][A-Za-z0-9_]*):-([A-Za-z0-9_./:@+\-]+)\}$`)

func configAssignment(filename, line string) (string, string) {
	base := path.Base(filename)
	line = strings.TrimSpace(line)
	if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") {
		if m := dockerArgument.FindStringSubmatch(line); m != nil {
			return m[1], m[2]
		}
	}
	if strings.Contains(base, "compose") && (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) {
		if m := composeDefault.FindStringSubmatch(line); m != nil && m[1] == m[2] {
			return m[1], m[3]
		}
	}
	return "", ""
}
