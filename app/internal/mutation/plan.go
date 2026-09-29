package mutation

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Source is the read-only view of the candidate snapshot that planning needs.
// The harness's mutation workspace implements it over its private copy.
type Source interface {
	// ReadSource returns a repository-relative regular text file of at most
	// 1 MiB. Excluded (secret-bearing) paths, symlinks and binaries fail.
	ReadSource(rel string) ([]byte, error)
	// HasTestFile reports whether the repository-relative directory directly
	// contains a regular *_test.go file ("." is the root).
	HasTestFile(dir string) (bool, error)
}

// maxReportedFiles caps the files listed in the section; counts still cover
// every file.
const maxReportedFiles = 500

// Fixed file skip reasons produced by planning.
const (
	skipNoAddedLines = "no added lines"
	skipIgnoredPath  = "the path has a component that starts with an underscore or a dot, or a testdata or vendor directory, which the go command ignores or treats specially"
	skipOSArchName   = "the file name has a GOOS or GOARCH suffix; files a build may exclude are not mutated"
	skipUnreadable   = "the file could not be read from the candidate snapshot (an excluded path, not a regular text file, or larger than 1 MiB)"
	skipNoTestFile   = "no _test.go file in this package directory"
	skipTotalCap     = "the total limit of 20000 candidate mutants was reached before this file"
)

// Plan is the deterministic outcome of planning: which files were considered,
// every candidate mutant (generated) and which of them were selected.
type Plan struct {
	Files           []model.MutationFile
	Generated       int
	CoverageSkipped int
	Selected        []Site            // in execution order
	Sources         map[string][]byte // source of every file with a selected site
}

// NewPlan enumerates the candidate mutants of the change, applies the
// optional coverage skip and selects at most maxMutants of them breadth-first.
// notExecuted may be nil; otherwise it returns the added lines a passing,
// measured coverage run reported as not executed.
func NewPlan(src Source, change model.Change, maxMutants int, notExecuted func(string) []int) Plan {
	files := append([]model.ChangedFile(nil), change.Files...)
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var p Plan
	var perFile [][]Site
	sources := map[string][]byte{}
	total := 0
	for _, f := range files {
		if !inScope(f) {
			continue
		}
		added := addedLines(f)
		mf := model.MutationFile{Path: f.Path, Status: model.MutationFileSkipped, AddedLines: len(added)}
		reason := ""
		switch {
		case len(added) == 0:
			reason = skipNoAddedLines
		case ignoredPath(f.Path):
			reason = skipIgnoredPath
		case osArchConstrained(f.Path):
			reason = skipOSArchName
		case total >= maxSitesTotal:
			reason = skipTotalCap
		}
		var data []byte
		if reason == "" {
			var err error
			if data, err = src.ReadSource(f.Path); err != nil {
				reason = skipUnreadable
			}
		}
		if reason == "" {
			if ok, err := src.HasTestFile(path.Dir(f.Path)); err != nil || !ok {
				reason = skipNoTestFile
			}
		}
		var sites []Site
		if reason == "" {
			addedSet := make(map[int]bool, len(added))
			for _, line := range added {
				addedSet[line] = true
			}
			var capped bool
			var skip string
			sites, capped, skip = FileSites(f.Path, data, addedSet)
			switch {
			case skip != "":
				reason = skip
			case capped:
				mf.Reason = fmt.Sprintf("candidate mutant enumeration stopped at %d for this file", maxSitesPerFile)
			}
		}
		if reason != "" {
			mf.Reason = reason
			p.Files = append(p.Files, mf)
			continue
		}
		mf.Status = model.MutationFileEligible
		if notExecuted != nil {
			skipped := map[int]bool{}
			for _, line := range notExecuted(f.Path) {
				skipped[line] = true
			}
			kept := sites[:0]
			dropped := 0
			for _, s := range sites {
				if skipped[s.Line] {
					dropped++
					continue
				}
				kept = append(kept, s)
			}
			sites = kept
			if dropped > 0 {
				p.CoverageSkipped += dropped
				mf.Reason = joinReasons(mf.Reason, fmt.Sprintf("%d candidate mutants on lines the coverage run did not execute were skipped", dropped))
			}
		}
		if len(sites) > maxSitesTotal-total {
			sites = sites[:maxSitesTotal-total]
			mf.Reason = joinReasons(mf.Reason, fmt.Sprintf("the total limit of %d candidate mutants was reached in this file", maxSitesTotal))
		}
		if len(sites) == 0 && mf.Reason == "" {
			mf.Reason = "no mutation site on the added lines"
		}
		total += len(sites)
		p.Files = append(p.Files, mf)
		if len(sites) > 0 {
			perFile = append(perFile, sites)
			sources[f.Path] = data
		}
	}
	p.Generated = total
	p.Selected = executionOrder(selectSites(perFile, maxMutants))
	p.Sources = map[string][]byte{}
	lines := map[string]map[int]bool{}
	for _, s := range p.Selected {
		p.Sources[s.Path] = sources[s.Path]
		if lines[s.Path] == nil {
			lines[s.Path] = map[int]bool{}
		}
		lines[s.Path][s.Line] = true
	}
	for i := range p.Files {
		p.Files[i].MutatedLines = len(lines[p.Files[i].Path])
	}
	if len(p.Files) > maxReportedFiles {
		p.Files = p.Files[:maxReportedFiles]
	}
	return p
}

// selectSites picks at most max sites breadth-first: the first site of the
// first line of every file, then of the second line of every file, and so
// on, then the second site of each line. Within a file sites are already in
// (line, operator rank, column) order, and files are in path order.
func selectSites(perFile [][]Site, max int) []Site {
	type keyed struct {
		site                   Site
		depth, lineIndex, file int
	}
	var all []keyed
	for fi, sites := range perFile {
		lineIndex, depth := -1, 0
		for i, s := range sites {
			if i == 0 || s.Line != sites[i-1].Line {
				lineIndex++
				depth = 0
			} else {
				depth++
			}
			all = append(all, keyed{s, depth, lineIndex, fi})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		if a.lineIndex != b.lineIndex {
			return a.lineIndex < b.lineIndex
		}
		return a.file < b.file
	})
	if max < 0 {
		max = 0
	}
	if len(all) > max {
		all = all[:max]
	}
	out := make([]Site, len(all))
	for i, k := range all {
		out[i] = k.site
	}
	return out
}

// executionOrder groups the selected sites by package so that one control run
// serves every mutant of a package: package argument, path, line, column,
// operator rank.
func executionOrder(sites []Site) []Site {
	out := append([]Site(nil), sites...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if pa, pb := PackageArg(a.Path), PackageArg(b.Path); pa != pb {
			return pa < pb
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.rank() < b.rank()
	})
	return out
}

// PackageArg is the {package} expansion of a repository-relative file path,
// exactly as for generated_test: "./" plus its directory, or "." at the root.
// Planning only admits paths without "." or "_" components, so the argument
// always starts with "./" or is "." and can never be read as a flag.
func PackageArg(file string) string {
	dir := path.Dir(file)
	if dir == "." {
		return "."
	}
	return "./" + dir
}

// ExpandCommand replaces the one standalone {package} argument; nothing else
// is added, so the executed argv is the reviewed argv plus that expansion.
func ExpandCommand(command []string, pkg string) []string {
	out := append([]string(nil), command...)
	for i, arg := range out {
		if arg == PackagePlaceholder {
			out[i] = pkg
		}
	}
	return out
}

// PackagePlaceholder mirrors config.PackagePlaceholder (config is not a
// dependency of this package).
const PackagePlaceholder = "{package}"

// inScope selects changed, non-deleted, non-binary, non-test Go files, as the
// coverage measurement does.
func inScope(f model.ChangedFile) bool {
	return f.Status != "D" && !f.Binary && strings.HasSuffix(f.Path, ".go") && !strings.HasSuffix(f.Path, "_test.go")
}

// addedLines returns the new-side line numbers of added lines, ascending.
func addedLines(f model.ChangedFile) []int {
	var added []int
	for _, h := range f.Hunks {
		for _, d := range h.Lines {
			if d.Kind == "add" && d.NewLine > 0 {
				added = append(added, d.NewLine)
			}
		}
	}
	sort.Ints(added)
	return added
}

// ignoredPath reports a path the go command ignores or treats specially: a
// component starting with "_" or ".", a testdata or vendor directory, or
// "..." anywhere.
func ignoredPath(p string) bool {
	if strings.Contains(p, "...") {
		return true
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || strings.HasPrefix(part, "_") || strings.HasPrefix(part, ".") || part == "testdata" || part == "vendor" {
			return true
		}
	}
	return false
}

// osArchConstrained applies the go/build file name rule (goodOSArchFile): the
// base name is cut at its first "." (so x_windows.impl.go counts as x_windows),
// then, after the first "_", a trailing GOOS, GOARCH or GOOS_GOARCH element
// (before an optional "_test") restricts the file to that platform. Such files
// are skipped whatever the sandbox platform: a file a build excludes would let
// every mutant survive.
func osArchConstrained(file string) bool {
	name, _, _ := strings.Cut(path.Base(file), ".")
	i := strings.Index(name, "_")
	if i < 0 {
		return false
	}
	l := strings.Split(name[i:], "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
		return true
	}
	return n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]])
}

// knownOS and knownArch are the go/build file name lists (syslist.go).
var knownOS = setOf("aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos")

var knownArch = setOf("386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390", "s390x", "sparc", "sparc64", "wasm")

func setOf(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

func joinReasons(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
