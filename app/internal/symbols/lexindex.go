package symbols

// Building the lexical part of the index (TypeScript/JavaScript, Python and
// Rust) into the same builder as the Go part, and listing the changed
// functions of changed lexical files.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// MethodLexical is the method of a tool response about a lexical declaration.
const MethodLexical = "lexical_name_index"

// LimitationsLexical is the caveat carried by every tool response about a
// lexical declaration.
const LimitationsLexical = "Approximate lexical analysis of committed TypeScript/JavaScript, Python and Rust source: declarations and calls are found by scanning tokens, without type checking or loading imports. A call is linked by name to the indexed functions or methods of that name in the same language, preferring the same file, the caller's own class and a qualifier naming the declaring module or type, so a link may be wrong. Calls through variables, aliases, re-exports, decorators, macros, dynamic dispatch or generated code are missed, calls of common method names on values of unknown type are not linked, and nested functions and closures belong to their enclosing declaration. A short or empty result is not proof that no other reference exists."

// maxLexCandidates is the number of same-named declarations a call may be
// linked to; a call matching more is recorded as a name site only.
const maxLexCandidates = 3

// lexCommonMethods are method names so common on built-in and library values
// (collections, strings, promises, options) that a call of one on a value of
// unknown type is recorded as a name site, never linked.
var lexCommonMethods = set(
	"get", "set", "add", "remove", "delete", "clear", "has", "keys", "values", "items", "entries", "update", "push", "pop",
	"shift", "unshift", "append", "extend", "insert", "map", "filter", "reduce", "forEach", "find", "some", "every",
	"join", "split", "replace", "format", "toString", "to_string", "as_str", "clone", "unwrap", "expect", "iter",
	"into", "collect", "len", "next", "send", "write", "read", "close", "open", "then", "catch", "finally", "emit",
	"on", "off", "log", "info", "debug", "warn", "error", "copy", "sort", "reverse", "index", "count", "slice",
	"splice", "includes", "indexOf", "startsWith", "endsWith", "trim", "strip", "lower", "upper", "encode", "decode",
	"json", "parse", "stringify", "is_some", "is_none", "is_ok", "is_err", "ok", "err", "map_err", "and_then",
	"unwrap_or", "unwrap_or_default", "as_ref", "borrow", "borrow_mut", "lock", "resolve", "reject",
)

// moduleName is the name under which other files refer to a lexical
// source: its file stem, or its directory's name for a package or module
// entry file (index.ts, __init__.py, mod.rs, lib.rs, main.rs).
func moduleName(p string) string {
	base := path.Base(p)
	stem := strings.TrimSuffix(base, path.Ext(base))
	switch stem {
	case "index", "__init__", "mod", "lib", "main":
		if dir := path.Base(path.Dir(p)); dir != "." && dir != "/" {
			if dir == "src" && (stem == "lib" || stem == "main") {
				return stem
			}
			return dir
		}
	}
	return stem
}

// lexKey is the index key of a lexical declaration.
func lexKey(p, name string) string { return p + "." + name }

// moduleDeclName names the pseudo-declaration of a file's top-level code.
const moduleDeclName = "(module)"

// changedLexicalFiles reports whether any changed file, on either side, is a
// lexical source the index may read.
func changedLexicalFiles(change model.Change, sensitive func(string) bool) bool {
	for _, f := range change.Files {
		for _, p := range []string{f.Path, f.OldPath} {
			if p != "" && lexIndexable(p, sensitive) {
				return true
			}
		}
	}
	return false
}

// lexSource reports a non-test lexical source the index may read.
func lexSource(p string, sensitive func(string) bool) bool {
	return lexIndexable(p, sensitive) && !lexTestFile(lexLanguage(p), p)
}

// lexBuild is the outcome of buildLexical.
type lexBuild struct {
	indexed   int
	languages map[string]bool
}

// buildLexical reads the lexical sources of the candidate tree, lists the
// changed lexical functions, and records the declarations and call edges of
// every source into b. Content of indexed files is added to content. It
// returns an error only when ctx is cancelled.
func (a *analysis) buildLexical(ctx context.Context, repo *gitrepo.Repository, headTree []gitrepo.TreeEntry, b *builder, content map[string][]byte, deadline time.Time, sensitive func(string) bool) (lexBuild, error) {
	out := lexBuild{languages: map[string]bool{}}
	var entries []gitrepo.TreeEntry
	for _, e := range headTree {
		if regularBlob(e) && lexIndexable(e.Path, sensitive) {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	byPath := map[string]gitrepo.TreeEntry{}
	var readable []gitrepo.TreeEntry
	var total int64
	large := 0
	for _, e := range entries {
		byPath[e.Path] = e
		if e.Size > a.lim.MaxFileBytes {
			large++
			a.fileReason[e.Path] = fmt.Sprintf("the file is larger than %d bytes and was not indexed", a.lim.MaxFileBytes)
			continue
		}
		readable = append(readable, e)
		total += e.Size
	}
	tooBig := ""
	switch {
	case len(readable) > a.lim.MaxFiles:
		tooBig = fmt.Sprintf("the candidate tree has more than %d TypeScript/JavaScript, Python and Rust files, so the lexical index was not built", a.lim.MaxFiles)
	case total > a.lim.MaxBytes:
		tooBig = fmt.Sprintf("the TypeScript/JavaScript, Python and Rust sources of the candidate tree exceed %d bytes, so the lexical index was not built", a.lim.MaxBytes)
	}
	toRead := readable
	if tooBig != "" {
		// Only the changed files are read, to list the changed functions.
		wanted := map[string]bool{}
		for _, f := range a.change.Files {
			wanted[f.Path] = true
		}
		toRead = nil
		for _, e := range readable {
			if wanted[e.Path] {
				toRead = append(toRead, e)
			}
		}
	}
	head, err := readEntries(ctx, repo, toRead, a.lim.MaxFileBytes)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		a.unknown = true
		a.limit("the candidate TypeScript/JavaScript, Python and Rust sources could not be read: " + shortError(err))
		a.lexUnindexed("the lexical index could not read the candidate sources")
		return out, nil
	}
	if err := a.lexChangedFunctions(ctx, repo, head, byPath, sensitive); err != nil {
		return out, err
	}
	if tooBig != "" {
		a.limit(tooBig)
		a.lexUnindexed(tooBig)
		return out, nil
	}
	if large > 0 {
		a.limit(fmt.Sprintf("%d TypeScript/JavaScript, Python or Rust files larger than %d bytes were not indexed", large, a.lim.MaxFileBytes))
	}

	paths := make([]string, 0, len(head))
	for p := range head {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	type parsedFile struct {
		f   *lexFile
		ids []int32 // builder ids of f.decls
	}
	var files []parsedFile
	timedOut, failed := 0, 0
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if time.Now().After(deadline) {
			timedOut++
			a.fileReason[p] = "the index time limit was reached before the file"
			continue
		}
		f, ok := safeParseLexical(p, head[p])
		if !ok {
			failed++
			a.fileReason[p] = "the lexical scan of the file stopped unexpectedly"
			continue
		}
		files = append(files, parsedFile{f: f})
	}
	if timedOut > 0 {
		a.limit(fmt.Sprintf("the index time limit (%s) was reached; %d TypeScript/JavaScript, Python or Rust files were not indexed", a.lim.Timeout, timedOut))
	}
	if failed > 0 {
		a.limit(fmt.Sprintf("the lexical scan of %d files stopped unexpectedly; their declarations are missing", failed))
	}

	// Declarations of every file first, so that calls resolve across files.
	resolvers := map[string]*lexResolver{}
	for i := range files {
		f := files[i].f
		r := resolvers[f.lang]
		if r == nil {
			r = newLexResolver()
			resolvers[f.lang] = r
		}
		fileID := b.file(f.path)
		_ = fileID
		for _, d := range f.decls {
			decl := Decl{
				Key: lexKey(f.path, d.name), Package: dirOf(f.path), PkgName: moduleName(f.path), Name: d.name,
				Path: f.path, Line: int(f.toks[d.start].line), EndLine: int(f.toks[d.end].line), Kind: d.kind,
				Test: d.test, Signature: tokenText(f.toks, d.start, d.bodyStart), Language: f.lang, testCode: d.testCode || lexTestFile(f.lang, f.path),
			}
			id := b.declare(decl)
			files[i].ids = append(files[i].ids, id)
			if !d.test {
				r.add(id, decl)
			}
		}
	}
	// Then the edges.
	for _, pf := range files {
		f := pf.f
		r := resolvers[f.lang]
		fileID := b.file(f.path)
		owner := make([]int32, len(f.toks))
		for i := range owner {
			owner[i] = -1
		}
		for k, d := range f.decls {
			for i := d.start; i <= d.end && i < len(owner); i++ {
				owner[i] = pf.ids[k]
			}
		}
		module := int32(-1)
		for _, c := range f.calls {
			caller := owner[c.tok]
			if caller < 0 {
				if module < 0 {
					module = b.declare(Decl{Key: lexKey(f.path, moduleDeclName), Package: dirOf(f.path), PkgName: moduleName(f.path), Name: moduleDeclName, Path: f.path, Line: 1, EndLine: int(f.toks[len(f.toks)-1].line), Kind: KindPackageInit, Language: f.lang, testCode: lexTestFile(f.lang, f.path)})
				}
				caller = module
			}
			tk := f.toks[c.tok]
			ids, unresolved := r.resolve(c, b.decls[caller], f.path)
			if unresolved && !b.nameCap {
				if len(b.names) >= b.limits.MaxNameSites {
					b.nameCap = true
				} else {
					b.names = append(b.names, nameSite{caller: caller, name: c.name, file: fileID, line: tk.line})
				}
			}
			for _, callee := range ids {
				if b.edgeCap {
					break
				}
				if len(b.edges) >= b.limits.MaxEdges {
					b.edgeCap = true
					break
				}
				b.edges = append(b.edges, edge{caller: caller, callee: callee, file: fileID, line: tk.line, col: tk.col, call: true})
			}
		}
		content[f.path] = head[f.path]
		out.indexed++
		out.languages[f.lang] = true
	}
	return out, nil
}

// lexUnindexed gives every changed lexical function without a reason the
// reason why the lexical index holds none.
func (a *analysis) lexUnindexed(reason string) {
	for _, cf := range a.changed {
		if cf.key != "" && a.fileReason[cf.path] == "" {
			a.fileReason[cf.path] = reason
		}
	}
}

// safeParseLexical parses one source, turning a panic of the scanner into a
// failure of that file alone.
func safeParseLexical(p string, src []byte) (f *lexFile, ok bool) {
	defer func() {
		if recover() != nil {
			f, ok = nil, false
		}
	}()
	return parseLexical(p, src), true
}

// tokenText joins the texts of tokens [from, to) with single spaces, cut to
// 300 bytes: the signature of a declaration.
func tokenText(t []ltok, from, to int) string {
	var b strings.Builder
	for i := from; i < to && i < len(t) && b.Len() < 400; i++ {
		if i > from {
			b.WriteByte(' ')
		}
		b.WriteString(t[i].text)
	}
	return b.String()
}

// tokenDigest is the hex SHA-256 of the texts of tokens [from, to]:
// comments and formatting are ignored.
func tokenDigest(t []ltok, from, to int) string {
	h := sha256.New()
	for i := from; i <= to && i < len(t); i++ {
		h.Write([]byte(t[i].text))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- resolution --------------------------------------------------------------

// lexResolver links calls of one language to its declarations by name.
type lexResolver struct {
	funcs   map[string][]int32            // function name -> ids
	methods map[string][]int32            // method name -> ids
	byClass map[string]map[string][]int32 // class or type -> method name -> ids
	module  map[int32]string              // id -> module name of its file
	path    map[int32]string              // id -> file
}

func newLexResolver() *lexResolver {
	return &lexResolver{funcs: map[string][]int32{}, methods: map[string][]int32{}, byClass: map[string]map[string][]int32{}, module: map[int32]string{}, path: map[int32]string{}}
}

func appendUnique(list []int32, id int32) []int32 {
	for _, v := range list {
		if v == id {
			return list
		}
	}
	return append(list, id)
}

func (r *lexResolver) add(id int32, d Decl) {
	r.module[id], r.path[id] = d.PkgName, d.Path
	switch d.Kind {
	case KindFunc:
		r.funcs[d.Name] = appendUnique(r.funcs[d.Name], id)
	case KindMethod:
		dot := strings.LastIndex(d.Name, ".")
		class, name := d.Name[:dot], d.Name[dot+1:]
		if i := strings.LastIndex(class, "."); i >= 0 {
			class = class[i+1:]
		}
		r.methods[name] = appendUnique(r.methods[name], id)
		if r.byClass[class] == nil {
			r.byClass[class] = map[string][]int32{}
		}
		r.byClass[class][name] = appendUnique(r.byClass[class][name], id)
	}
}

// classOf is the class (or Rust type) of a method declaration, or "".
func classOf(d Decl) string {
	if d.Kind != KindMethod {
		return ""
	}
	class := d.Name[:strings.LastIndex(d.Name, ".")]
	if i := strings.LastIndex(class, "."); i >= 0 {
		class = class[i+1:]
	}
	return class
}

// inModule keeps the ids whose file has the module name q.
func (r *lexResolver) inModule(ids []int32, q string) []int32 {
	var out []int32
	for _, id := range ids {
		if r.module[id] == q {
			out = append(out, id)
		}
	}
	return out
}

// resolve returns the declarations a call may refer to. unresolved is true
// when the call is recorded as a name site instead: a method call on a value
// of unknown type with a common name, or a name with too many candidates.
func (r *lexResolver) resolve(c lexCall, caller Decl, file string) (ids []int32, unresolved bool) {
	class := classOf(caller)
	cap := func(ids []int32) ([]int32, bool) {
		if len(ids) > maxLexCandidates {
			return nil, true
		}
		return ids, false
	}
	switch {
	case c.ctor:
		init := r.byClass[c.name]["constructor"]
		return init, false
	case c.member && (c.qual == "this" || c.qual == "self" || c.qual == "cls") && class != "" && len(r.byClass[class][c.name]) > 0:
		return r.byClass[class][c.name], false
	case c.path && (c.qual == "super" || c.qual == "crate" || c.qual == "self" && class == ""):
		// A path through the crate's own modules: linked as a plain call.
	case c.path:
		q := c.qual
		if q == "Self" || q == "self" && class != "" {
			q = class
		}
		if ids := r.byClass[q][c.name]; len(ids) > 0 {
			return cap(ids)
		}
		if ids := r.inModule(r.funcs[c.name], q); len(ids) > 0 {
			return cap(ids)
		}
		// A path through a type or crate outside the index (Vec::new).
		return nil, false
	case c.member:
		if c.qual != "" {
			if ids := r.byClass[c.qual][c.name]; len(ids) > 0 {
				return cap(ids)
			}
			if ids := r.inModule(r.funcs[c.name], c.qual); len(ids) > 0 {
				return cap(ids)
			}
		}
		ids := r.methods[c.name]
		if len(ids) == 0 {
			return nil, false
		}
		if lexCommonMethods[c.name] {
			return nil, true
		}
		return cap(ids)
	}
	var local []int32
	for _, id := range r.funcs[c.name] {
		if r.path[id] == file {
			local = append(local, id)
		}
	}
	if len(local) > 0 {
		return cap(local)
	}
	ids = append([]int32(nil), r.funcs[c.name]...)
	for _, id := range r.byClass[c.name]["__init__"] {
		ids = appendUnique(ids, id)
	}
	return cap(ids)
}

// --- changed functions -------------------------------------------------------

// lexPrint is the fingerprint of one lexical function or method.
type lexPrint struct {
	name      string
	line, end int
	sig, body string
}

// lexPrints lists the fingerprints of the non-test declarations of a source.
func lexPrints(p string, src []byte) ([]lexPrint, bool) {
	f, ok := safeParseLexical(p, src)
	if !ok {
		return nil, false
	}
	var out []lexPrint
	for _, d := range f.decls {
		if d.testCode {
			continue
		}
		out = append(out, lexPrint{
			name: d.name, line: int(f.toks[d.start].line), end: int(f.toks[d.end].line),
			sig: tokenDigest(f.toks, d.start, d.bodyStart-1), body: tokenDigest(f.toks, d.bodyStart, d.end),
		})
	}
	return out, true
}

// lexChangedFunctions compares the functions of the changed non-test lexical
// files with their baseline versions, as changedFunctions does for Go: a
// function is identified by its file (following a rename) and its name, and
// several declarations of one name are compared as multisets.
func (a *analysis) lexChangedFunctions(ctx context.Context, repo *gitrepo.Repository, head map[string][]byte, headEntries map[string]gitrepo.TreeEntry, sensitive func(string) bool) error {
	var pairs []sourcePair
	needBase := false
	for _, f := range a.change.Files {
		oldPath := f.Path
		if f.OldPath != "" {
			oldPath = f.OldPath
		}
		var p sourcePair
		if f.Status != "D" && lexSource(f.Path, sensitive) {
			p.headPath = f.Path
		}
		if f.Status != "A" && lexSource(oldPath, sensitive) {
			p.basePath = oldPath
			needBase = true
		}
		if p.headPath != "" {
			pairs = append(pairs, p)
		}
	}
	baseEntries := map[string]gitrepo.TreeEntry{}
	if needBase {
		tree, err := repo.Tree(ctx, a.change.BaseCommit)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.unknown = true
			a.limit("the baseline versions of changed TypeScript/JavaScript, Python and Rust files could not be listed: " + shortError(err))
			return nil
		}
		for _, e := range tree {
			if regularBlob(e) && lexIndexable(e.Path, sensitive) {
				baseEntries[e.Path] = e
			}
		}
	}
	failed := 0
	var kept []sourcePair
	var read []gitrepo.TreeEntry
	for _, p := range pairs {
		headEntry, inHead := headEntries[p.headPath]
		baseEntry, inBase := baseEntries[p.basePath]
		data, ok := head[p.headPath]
		if !inHead || !ok || headEntry.Size > a.lim.MaxFileBytes || inBase && baseEntry.Size > a.lim.MaxFileBytes {
			if inHead && headEntry.Size <= a.lim.MaxFileBytes && !ok {
				continue // not read: the tree is too large, and its reason is recorded
			}
			if inHead {
				failed++
			}
			continue
		}
		p.head = data
		if inBase {
			read = append(read, baseEntry)
		} else {
			p.basePath = ""
		}
		kept = append(kept, p)
	}
	sort.Slice(read, func(i, j int) bool { return read[i].Path < read[j].Path })
	baseSources, err := readEntries(ctx, repo, read, a.lim.MaxFileBytes)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.unknown = true
		a.limit("the baseline versions of changed TypeScript/JavaScript, Python and Rust files could not be read: " + shortError(err))
		return nil
	}
	var changed []changedFunction
	for _, p := range kept {
		after, ok := lexPrints(p.headPath, p.head)
		if !ok {
			failed++
			continue
		}
		var before []lexPrint
		if p.basePath != "" {
			if before, ok = lexPrints(p.basePath, baseSources[p.basePath]); !ok {
				failed++
				continue
			}
		}
		same, sigs, known := map[[3]string]int{}, map[[2]string]bool{}, map[string]bool{}
		for _, b := range before {
			same[[3]string{b.name, b.sig, b.body}]++
			sigs[[2]string{b.name, b.sig}] = true
			known[b.name] = true
		}
		for _, v := range after {
			key := lexKey(p.headPath, v.name)
			if !known[v.name] {
				a.touchedKeys[key] = true // added
				continue
			}
			k := [3]string{v.name, v.sig, v.body}
			if same[k] > 0 {
				same[k]--
				continue
			}
			class := model.ChangeSignatureChanged
			if sigs[[2]string{v.name, v.sig}] {
				class = model.ChangeBodyChanged
			}
			a.touchedKeys[key] = true
			changed = append(changed, changedFunction{path: p.headPath, dir: dirOf(p.headPath), pkgName: moduleName(p.headPath), name: v.name, line: v.line, end: v.end, change: class, key: key})
		}
	}
	if failed > 0 {
		a.unknown = true
		a.limit(fmt.Sprintf("%d changed TypeScript/JavaScript, Python or Rust files could not be compared (too large or not scannable); their functions are not listed", failed))
	}
	a.changed = append(a.changed, changed...)
	return nil
}
