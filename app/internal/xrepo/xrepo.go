// Package xrepo gives the reviewer read access to other repositories declared
// as cross-repository context (policy "context"): shared types, SDKs, API
// clients, sibling services. Each repository is read at one commit, from Git
// objects only, never from a working tree; sensitive paths are never listed
// or read, and nothing is executed. A repository is found as a local checkout
// (--context-repo NAME=PATH, --context-dir DIR) or, with --fetch-context, by a
// shallow fetch of the url the trusted policy gives.
package xrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/linter"
	"github.com/gvinsot/Probe/app/internal/model"
)

// Bounds of what the reviewer can read.
const (
	MaxFileBytes     = 1 << 20  // files larger than this are not listed
	MaxFiles         = 20000    // listed files per repository
	MaxReadBytes     = 64 << 10 // returned by one read
	MaxSearchMatches = 50
	MaxSearchBytes   = 64 << 20 // scanned by one search
	fetchTimeout     = 3 * time.Minute
)

// Options says where to find the repositories.
type Options struct {
	// Paths maps a repository name to a local checkout (--context-repo).
	Paths map[string]string
	// Dir holds checkouts as DIR/<owner>/<repo> or DIR/<repo> (--context-dir).
	Dir string
	// Fetch allows a shallow fetch of the policy url when no checkout is found.
	Fetch bool
	// Temp receives fetched repositories; the caller removes it.
	Temp string
}

type source struct {
	info   model.ContextRepo
	repo   *gitrepo.Repository
	commit string
	files  []gitrepo.TreeEntry
	byPath map[string]gitrepo.TreeEntry
}

// Set is the context repositories of one run.
type Set struct {
	sources []*source
	byName  map[string]*source
}

// Open resolves every repository. A repository that cannot be found or read
// is recorded as unavailable with its reason; it never fails the run.
func Open(ctx context.Context, entries []config.ResolvedRepo, o Options) (*Set, []model.ContextRepo) {
	set := &Set{byName: map[string]*source{}}
	records := make([]model.ContextRepo, 0, len(entries))
	for i, e := range entries {
		info := model.ContextRepo{Name: e.Name, Role: e.Role, Clusters: append([]string{}, e.Clusters...), Ref: e.Ref, Status: model.ContextUnavailable}
		if info.Ref == "" {
			info.Ref = "HEAD"
		}
		s, err := open(ctx, e, &info, o, i)
		if err != nil {
			info.Reason = err.Error()
			records = append(records, info)
			continue
		}
		info.Status, info.Files = model.ContextAvailable, len(s.files)
		s.info = info
		set.sources = append(set.sources, s)
		set.byName[strings.ToLower(e.Name)] = s
		records = append(records, info)
	}
	return set, records
}

func open(ctx context.Context, e config.ResolvedRepo, info *model.ContextRepo, o Options, index int) (*source, error) {
	repo, err := local(ctx, e.Name, o)
	if err != nil {
		return nil, err
	}
	ref := info.Ref
	if repo != nil {
		info.Source = model.ContextLocal
	} else {
		if e.URL == "" {
			return nil, errors.New("no local checkout (use --context-repo NAME=PATH or --context-dir DIR) and no url to fetch")
		}
		if !o.Fetch {
			return nil, errors.New("no local checkout; its url is fetched only with --fetch-context")
		}
		if repo, err = fetch(ctx, e.URL, ref, filepath.Join(o.Temp, fmt.Sprintf("repo-%d", index))); err != nil {
			return nil, err
		}
		info.Source, ref = model.ContextFetched, "FETCH_HEAD"
	}
	commit, err := repo.ResolveCommit(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %v", info.Ref, err)
	}
	info.Commit = commit
	tree, err := repo.Tree(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("list files: %v", err)
	}
	s := &source{repo: repo, commit: commit, byPath: map[string]gitrepo.TreeEntry{}}
	for _, t := range tree {
		if t.Type != "blob" || t.Mode != "100644" && t.Mode != "100755" || t.Size > MaxFileBytes || gitrepo.SafePath(t.Path) != nil || harness.IsSensitivePath(t.Path) {
			continue
		}
		if len(e.Paths) > 0 {
			if _, ok, _ := linter.MatchSensitive(e.Paths, t.Path); !ok {
				continue
			}
		}
		if len(s.files) == MaxFiles {
			info.Reason = fmt.Sprintf("only the first %d files are listed; narrow it with paths", MaxFiles)
			break
		}
		s.files = append(s.files, t)
		s.byPath[t.Path] = t
	}
	return s, nil
}

// local finds a checkout: an explicit path, then DIR/<name>, then DIR/<repo>.
// A directory inside another repository is not a checkout of its own.
func local(ctx context.Context, name string, o Options) (*gitrepo.Repository, error) {
	var candidates []string
	for k, p := range o.Paths {
		if strings.EqualFold(k, name) {
			if !isRoot(ctx, p) {
				return nil, fmt.Errorf("--context-repo %s=%s is not the root of a Git repository", name, p)
			}
			candidates = append(candidates, p)
		}
	}
	if o.Dir != "" {
		candidates = append(candidates, filepath.Join(o.Dir, filepath.FromSlash(name)), filepath.Join(o.Dir, filepath.Base(filepath.FromSlash(name))))
	}
	for _, c := range candidates {
		if isRoot(ctx, c) {
			return gitrepo.Open(ctx, c)
		}
	}
	return nil, nil
}

func isRoot(ctx context.Context, dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	repo, err := gitrepo.Open(ctx, dir)
	if err != nil {
		return false
	}
	want, err1 := filepath.EvalSymlinks(dir)
	got, err2 := filepath.EvalSymlinks(repo.Root)
	return err1 == nil && err2 == nil && filepath.Clean(want) == filepath.Clean(got)
}

// fetch makes a shallow copy of one ref of url in dir. Git uses the caller's
// credential helpers; prompts, local and ext transports are refused.
func fetch(ctx context.Context, url, ref, dir string) (*gitrepo.Repository, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			message := strings.TrimSpace(stderr.String())
			if len(message) > 300 {
				message = message[:300]
			}
			return fmt.Errorf("fetch %s: %v %s", url, err, message)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := run("init", "-q", dir); err != nil {
		return nil, err
	}
	if err := run("-C", dir, "fetch", "-q", "--depth", "1", "--no-tags", "--", url, ref); err != nil {
		return nil, err
	}
	return gitrepo.Open(ctx, dir)
}

// Repos lists the available repositories.
func (s *Set) Repos() []model.ContextRepo {
	if s == nil {
		return nil
	}
	out := make([]model.ContextRepo, 0, len(s.sources))
	for _, src := range s.sources {
		out = append(out, src.info)
	}
	return out
}

func (s *Set) source(name string) (*source, error) {
	src, ok := s.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("unknown context repository %q", name)
	}
	return src, nil
}

// List returns the files under prefix, at most limit of them, and how many
// there are.
func (s *Set) List(name, prefix string, limit int) ([]string, int, error) {
	src, err := s.source(name)
	if err != nil {
		return nil, 0, err
	}
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "./")
	out := []string{}
	total := 0
	for _, f := range src.files {
		if strings.HasPrefix(f.Path, prefix) {
			total++
			if len(out) < limit {
				out = append(out, f.Path)
			}
		}
	}
	return out, total, nil
}

// Read returns lines start..end (1-based, inclusive; 0 means the file's
// bounds) of a listed file, numbered, within MaxReadBytes.
func (s *Set) Read(ctx context.Context, name, path string, start, end int) (string, int, bool, error) {
	src, err := s.source(name)
	if err != nil {
		return "", 0, false, err
	}
	if _, ok := src.byPath[path]; !ok {
		return "", 0, false, fmt.Errorf("%s is not a readable file of %s (unknown, sensitive, too large or outside its paths)", path, name)
	}
	data, err := src.repo.ReadFile(ctx, src.commit, path)
	if err != nil {
		return "", 0, false, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", 0, false, fmt.Errorf("%s is a binary file", path)
	}
	lines := strings.Split(string(data), "\n")
	if start < 1 {
		start = 1
	}
	if end < 1 || end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	truncated := false
	for i := start; i <= end; i++ {
		line := fmt.Sprintf("%d: %s\n", i, lines[i-1])
		if b.Len()+len(line) > MaxReadBytes {
			truncated = true
			break
		}
		b.WriteString(line)
	}
	return b.String(), len(lines), truncated, nil
}

// Match is one search result.
type Match struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Search finds query, literally, in the listed files of one repository or of
// all of them. It stops at MaxSearchMatches results or MaxSearchBytes read.
func (s *Set) Search(ctx context.Context, name, query string) ([]Match, bool, error) {
	if strings.TrimSpace(query) == "" || len(query) > 200 {
		return nil, false, errors.New("query must hold 1 to 200 bytes")
	}
	sources := s.sources
	if strings.TrimSpace(name) != "" {
		src, err := s.source(name)
		if err != nil {
			return nil, false, err
		}
		sources = []*source{src}
	}
	matches := []Match{}
	scanned := int64(0)
	needle := []byte(query)
	stop := errors.New("stop")
	for _, src := range sources {
		oids := make([]string, 0, len(src.files))
		paths := map[string][]string{}
		for _, f := range src.files {
			if scanned+f.Size > MaxSearchBytes {
				return matches, true, nil
			}
			scanned += f.Size
			if _, seen := paths[f.OID]; !seen {
				oids = append(oids, f.OID)
			}
			paths[f.OID] = append(paths[f.OID], f.Path)
		}
		err := src.repo.ReadBlobs(ctx, oids, MaxFileBytes, func(oid string, data []byte) error {
			if bytes.IndexByte(data, 0) >= 0 || !bytes.Contains(data, needle) {
				return nil
			}
			files := paths[oid]
			sort.Strings(files)
			for i, line := range bytes.Split(data, []byte("\n")) {
				if !bytes.Contains(line, needle) {
					continue
				}
				text := strings.TrimSpace(string(line))
				if len(text) > 300 {
					text = text[:300]
				}
				for _, p := range files {
					matches = append(matches, Match{Repo: src.info.Name, Path: p, Line: i + 1, Text: text})
					if len(matches) == MaxSearchMatches {
						return stop
					}
				}
			}
			return nil
		})
		if errors.Is(err, stop) {
			return matches, true, nil
		}
		if err != nil {
			return matches, false, err
		}
	}
	return matches, false, nil
}
