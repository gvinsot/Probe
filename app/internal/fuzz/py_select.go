package fuzz

// Selection of changed Python functions. Changed public top-level functions
// of modified Python modules whose signature tokens are identical on both
// revisions are found lexically on the host, with the static index's Python
// tokenizer; nothing is executed. A function becomes a target when its
// parameters can be generated from their annotations: int, float, str, bool,
// and lists of them. Like the TS/JS reading, this is lexical and best-effort:
// a construct it does not recognize is either skipped with a reason or not
// listed at all.

import (
	"context"
	"errors"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gvinsot/Probe/app/internal/coverage"
	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/symbols"
)

// LanguagePython is the Target.Language of a Python function.
const LanguagePython = "python"

// FamilyPytest is the runner family of a verifiable pytest generated_test
// template.
const FamilyPytest = "pytest"

// Fixed skip reasons of Python functions.
const (
	// ReasonPythonTemplate is the reason of an eligible Python function when
	// the generated_test template cannot run a Python harness.
	ReasonPythonTemplate = "Python differential fuzzing needs a verifiable pytest generated_test template (a {file} target and --junitxml={results_out})"
	// ReasonGoTemplatePytest and ReasonScriptTemplatePytest are the reasons
	// of an eligible Go or TS/JS function when the template is a pytest one.
	ReasonGoTemplatePytest     = "Go differential fuzzing needs a single-package go test generated_test template with the {package} target; this template is a pytest template"
	ReasonScriptTemplatePytest = "TS/JS differential fuzzing needs a verifiable Vitest or Jest generated_test template; this template is a pytest template"
	ReasonPythonRenamed        = "module was renamed or moved; the harness imports one path on both revisions"
	ReasonPythonModuleName     = "the module file name is not a Python identifier"
	ReasonPythonDecorated      = "decorated functions are not fuzzed in this version"
	ReasonPythonGenerator      = "generator functions are not fuzzed in this version"
	ReasonPythonKwargs         = "functions with a **kwargs parameter are not fuzzed in this version"
	ReasonPythonRedefined      = "the module defines this name more than once"
	ReasonPythonTooLarge       = "Python functions of this file were not enumerated: the file exceeds 2 MiB on one revision"
	ReasonPythonTotalBound     = "Python functions of this file were not enumerated: 32 MiB of Python source had already been read"
	ReasonPythonTimeLimit      = "Python functions of this file were not enumerated: the time limit of the fuzz stage (fuzz.max_runtime_seconds or --deadline) was reached"
	ReasonPythonScanBound      = "Python functions of this file were not enumerated: its lexical scan reached a bound (1,048,576 tokens)"
)

func reasonPythonUntyped(name string) string {
	return "parameter " + shortText(name, 64) + " has no type annotation"
}

var (
	// pythonIdentifier is an ASCII Python identifier.
	pythonIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// Generated Python types, after normalization (tokens joined without
	// spaces).
	pythonScalarType = regexp.MustCompile(`^(int|float|str|bool)$`)
	pythonListType   = regexp.MustCompile(`^(?:list|List|typing\.List|Sequence|typing\.Sequence|collections\.abc\.Sequence)\[(int|float|str|bool)\]$`)
)

// PytestFamily returns FamilyPytest for a verifiable pytest generated_test
// template, and "" otherwise.
func PytestFamily(cmd []string) string {
	if harness.VerifiablePytestTemplate(cmd) {
		return FamilyPytest
	}
	return ""
}

// PythonSelection is the outcome of SelectPython: the eligible Python
// functions and the changed ones that are not planned, with their reasons.
type PythonSelection struct {
	Targets []Target
	Skipped []model.FuzzSkip
}

// SelectPython finds the changed public top-level functions of modified
// Python modules (coverage.PythonSource: not a test module, conftest.py or a
// file under tests/) whose body tokens differ and whose signature tokens
// (parameters, return annotation and the async marker) are identical on both
// revisions. Each one becomes a target when its parameters can be generated
// (pythonTarget), and an entry of Skipped with a specific reason otherwise.
// Files are read from the two snapshots, at most 2 MiB each and 32 MiB in
// total; a file that is too large, whose scan reaches the token bound, or
// that comes after the total bound or after ctx is done gets one file-level
// entry instead. Both lists are sorted by path and line.
func SelectPython(ctx context.Context, baseDir, candidateDir string, change model.Change) PythonSelection {
	var out PythonSelection
	seen := map[string]bool{}
	read := 0
	for _, f := range change.Files {
		if f.Binary || f.Status != "M" && f.Status != "R" || !pythonModulePath(f.Path) || seen[f.Path] || harness.IsSensitivePath(f.Path) {
			continue
		}
		seen[f.Path] = true
		old := f.Path
		if f.Status == "R" && f.OldPath != "" {
			old = f.OldPath
			if !pythonModulePath(old) || harness.IsSensitivePath(old) {
				continue
			}
		}
		fileSkip := func(reason string) {
			out.Skipped = append(out.Skipped, model.FuzzSkip{Path: f.Path, Reason: reason})
		}
		switch {
		case ctx.Err() != nil:
			fileSkip(ReasonPythonTimeLimit)
			continue
		case read >= scriptSourceBudget:
			fileSkip(ReasonPythonTotalBound)
			continue
		}
		baseSrc, err := readSnapshotFile(baseDir, old)
		if err != nil {
			if errors.Is(err, errSourceTooLarge) {
				fileSkip(ReasonPythonTooLarge)
			}
			continue
		}
		candSrc, err := readSnapshotFile(candidateDir, f.Path)
		if err != nil {
			if errors.Is(err, errSourceTooLarge) {
				fileSkip(ReasonPythonTooLarge)
			}
			continue
		}
		read += len(baseSrc) + len(candSrc)
		base, baseOK := pythonFunctions(f.Path, baseSrc)
		candidate, candOK := pythonFunctions(f.Path, candSrc)
		if !baseOK || !candOK {
			fileSkip(ReasonPythonScanBound)
			continue
		}
		for _, fn := range sortedPythonFunctions(candidate) {
			b, ok := base[fn.name]
			if !ok || b.signature != fn.signature || b.body == fn.body && b.decorators == fn.decorators {
				continue
			}
			t, reason := pythonTarget(f, old, fn, b)
			if reason == "" && len(pythonCorpus(t, 1)) == 0 {
				reason = ReasonNoInput
			}
			if reason != "" {
				out.Skipped = append(out.Skipped, model.FuzzSkip{Path: f.Path, Line: fn.line, Symbol: shortText(fn.name, 128), Reason: reason})
				continue
			}
			out.Targets = append(out.Targets, t)
		}
	}
	sort.SliceStable(out.Skipped, func(i, j int) bool {
		if out.Skipped[i].Path != out.Skipped[j].Path {
			return out.Skipped[i].Path < out.Skipped[j].Path
		}
		return out.Skipped[i].Line < out.Skipped[j].Line
	})
	sort.SliceStable(out.Targets, func(i, j int) bool {
		if out.Targets[i].Path != out.Targets[j].Path {
			return out.Targets[i].Path < out.Targets[j].Path
		}
		return out.Targets[i].Line < out.Targets[j].Line
	})
	return out
}

// pythonModulePath reports whether p is a Python source that SelectPython
// reads: a clean relative path that coverage.PythonSource measures.
func pythonModulePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p || strings.Contains(p, "..") {
		return false
	}
	return coverage.PythonSource(p)
}

// pythonFunction is one top-level function of a Python module.
type pythonFunction struct {
	name       string
	line       int    // line of the def (or its first decorator)
	endLine    int    // line of the body's last token
	signature  string // tokens of the parameters and the return annotation, with the async marker
	body       string // tokens of the body, each logical line with its indentation
	decorators string // tokens of the decorator lines, "" without decorators
	params     []symbols.PythonToken
	async      bool
	generator  bool
	defined    int // number of top-level definitions of this name
}

// maxPythonTokens bounds the tokens read from one Python file.
const maxPythonTokens = 1 << 20

// pythonFunctions reads the top-level function definitions of a Python
// module: def and async def at column 0, with the decorator lines right
// above them. A name defined more than once is kept with its count. It
// returns false when the file has more than maxPythonTokens tokens.
func pythonFunctions(p string, src []byte) (map[string]pythonFunction, bool) {
	read, ok := symbols.ReadPythonSource(p, src)
	if !ok || len(read.Tokens) > maxPythonTokens {
		return nil, false
	}
	t := read.Tokens
	var starts []int
	for i := range t {
		if t[i].First && t[i].Indent == 0 {
			starts = append(starts, i)
		}
	}
	out := map[string]pythonFunction{}
	for k := 0; k < len(starts); k++ {
		first := starts[k]
		j := k
		for j < len(starts) && t[starts[j]].Text == "@" {
			j++
		}
		if j == len(starts) {
			break
		}
		i := starts[j]
		end := len(t) - 1
		if j+1 < len(starts) {
			end = starts[j+1] - 1
		}
		k = j
		fn := pythonFunction{line: t[first].Line, endLine: t[end].Line}
		if t[i].Text == "async" && i+1 <= end && t[i+1].Text == "def" {
			fn.async = true
			i++
		}
		if t[i].Text != "def" || i+2 > end || t[i+1].Kind != 'i' || t[i+2].Text != "(" || t[i+2].Match <= i+2 || t[i+2].Match > end {
			continue
		}
		fn.name = t[i+1].Text
		open, close := i+2, t[i+2].Match
		fn.params = t[open+1 : close]
		colon := -1
		for c := close + 1; c <= end; c++ {
			if t[c].Kind == 'p' && t[c].Match > c {
				c = t[c].Match
				continue
			}
			if t[c].Text == ":" {
				colon = c
				break
			}
		}
		if colon < 0 {
			continue
		}
		var sig, body, decorators strings.Builder
		if fn.async {
			sig.WriteString("async ")
		}
		for c := open; c < colon; c++ {
			sig.WriteString(t[c].Text)
			sig.WriteByte(' ')
		}
		for c := first; c < starts[j]; c++ {
			pythonTokenText(&decorators, t[c])
		}
		for c := colon + 1; c <= end; c++ {
			pythonTokenText(&body, t[c])
			if t[c].Kind == 'i' && (t[c].Text == "yield") {
				fn.generator = true
			}
		}
		fn.signature, fn.body, fn.decorators = strings.TrimSpace(sig.String()), body.String(), decorators.String()
		if prev, seen := out[fn.name]; seen {
			fn.defined = prev.defined
		}
		fn.defined++
		out[fn.name] = fn
	}
	return out, true
}

// pythonTokenText appends one token with its kind, and, for the first token
// of a logical line, the line's indentation: indentation is Python syntax.
func pythonTokenText(b *strings.Builder, t symbols.PythonToken) {
	if t.First {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(t.Indent))
		b.WriteByte(' ')
	}
	b.WriteByte(t.Kind)
	b.WriteString(strconv.Itoa(len(t.Text)))
	b.WriteByte(':')
	b.WriteString(t.Text)
	b.WriteByte(' ')
}

func sortedPythonFunctions(m map[string]pythonFunction) []pythonFunction {
	out := make([]pythonFunction, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].name < out[j].name
	})
	return out
}

// pythonTarget classifies one changed public function fn of module f (old is
// its baseline path) whose baseline definition is b. It returns the target,
// or a fixed skip reason. A name that starts with "_" is private and is not
// listed.
func pythonTarget(f model.ChangedFile, old string, fn, b pythonFunction) (Target, string) {
	t := Target{
		Dir: f.Path, Path: f.Path, Line: fn.line, EndLine: max(fn.line, fn.endLine), Name: fn.name, Symbol: fn.name,
		Signature: pythonSignature(fn), Exported: !strings.HasPrefix(fn.name, "_"), Language: LanguagePython,
	}
	stem := strings.TrimSuffix(path.Base(f.Path), ".py")
	switch {
	case old != f.Path:
		return t, ReasonPythonRenamed
	case !pythonIdentifier.MatchString(stem):
		return t, ReasonPythonModuleName
	case !pythonIdentifier.MatchString(fn.name):
		return t, ReasonScriptExportName
	case fn.defined > 1 || b.defined > 1:
		return t, ReasonPythonRedefined
	case fn.decorators != "" || b.decorators != "":
		return t, ReasonPythonDecorated
	case fn.generator || b.generator:
		return t, ReasonPythonGenerator
	}
	params, reason := pythonParams(fn.params)
	if reason != "" {
		return t, reason
	}
	t.Params = params
	t.Results = 1
	return t, ""
}

// pythonSignature is the display form of a signature, for example
// "(prices: list[float], discount: float = 0.0) -> float". It is also part
// of the seed.
func pythonSignature(fn pythonFunction) string {
	toks := make([]scriptToken, 0, len(fn.params)+2)
	toks = append(toks, scriptToken{text: "("})
	for _, p := range fn.params {
		toks = append(toks, scriptToken{text: p.Text, line: p.Line})
	}
	toks = append(toks, scriptToken{text: ")"})
	sig := prettyTokens(toks)
	// A star that opens a parameter stays with its name: *args, **kwargs.
	sig = strings.NewReplacer("(* ", "(*", ", * ", ", *", "(** ", "(**", ", ** ", ", **").Replace(sig)
	if i := strings.Index(fn.signature, ") ->"); i >= 0 {
		sig += " -> " + strings.Join(strings.Fields(fn.signature[i+len(") ->"):]), "")
	}
	if fn.async {
		sig = "async " + sig
	}
	return sig
}

// pythonParams derives the generated parameters of a function from the
// tokens of its parameter list. Every parameter needs an annotation of a
// generated type; "/" and "*" markers are read, a parameter after "*" (or
// after *args) is passed by keyword, *args: T is variadic over T, and
// **kwargs is refused. Defaults are read past: every parameter gets a value
// on every call.
func pythonParams(toks []symbols.PythonToken) ([]Param, string) {
	params := []Param{}
	keyword := false
	for _, part := range splitPythonParams(toks) {
		switch {
		case len(part) == 1 && part[0].Text == "/":
			continue
		case len(part) == 1 && part[0].Text == "*":
			keyword = true
			continue
		case part[0].Text == "**":
			return nil, ReasonPythonKwargs
		}
		variadic := false
		if part[0].Text == "*" {
			variadic, keyword, part = true, true, part[1:]
		}
		if len(part) == 0 || part[0].Kind != 'i' || !pythonIdentifier.MatchString(part[0].Text) {
			return nil, reasonParamType(pythonTypeText(part))
		}
		name := part[0].Text
		if len(part) < 2 || part[1].Text != ":" {
			if len(part) >= 2 && part[1].Text != "=" {
				return nil, reasonParamType(pythonTypeText(part))
			}
			return nil, reasonPythonUntyped(name)
		}
		end := 2
		for end < len(part) && part[end].Text != "=" {
			if part[end].Kind == 'p' && part[end].Match > end {
				end = part[end].Match
			}
			end++
		}
		typ := pythonTypeText(part[2:min(end, len(part))])
		p, ok := pythonParamType(typ, variadic)
		if !ok {
			return nil, reasonParamType(typ)
		}
		if keyword && !variadic {
			p.Keyword = name
		}
		params = append(params, p)
	}
	return params, ""
}

// splitPythonParams splits a parameter list at the commas of its own nesting
// level; a trailing comma is dropped. Match indexes stay those of the whole
// file, so they are compared with the part's own positions by text instead.
func splitPythonParams(toks []symbols.PythonToken) [][]symbols.PythonToken {
	var out [][]symbols.PythonToken
	depth, start := 0, 0
	for i, t := range toks {
		switch t.Text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		case ",":
			if depth == 0 {
				if i > start {
					out = append(out, rebase(toks[start:i]))
				}
				start = i + 1
			}
		}
	}
	if start < len(toks) {
		out = append(out, rebase(toks[start:]))
	}
	return out
}

// rebase rewrites the bracket matches of a slice of tokens relative to the
// slice, -1 for a bracket matched outside it.
func rebase(part []symbols.PythonToken) []symbols.PythonToken {
	out := append([]symbols.PythonToken(nil), part...)
	var stack []int
	for i := range out {
		out[i].Match = -1
		switch out[i].Text {
		case "(", "[", "{":
			stack = append(stack, i)
		case ")", "]", "}":
			if n := len(stack); n > 0 {
				out[stack[n-1]].Match, out[i].Match = i, stack[n-1]
				stack = stack[:n-1]
			}
		}
	}
	return out
}

// pythonTypeText normalizes the tokens of an annotation: joined without
// spaces, except one space between two words.
func pythonTypeText(toks []symbols.PythonToken) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && toks[i-1].Kind == 'i' && t.Kind == 'i' {
			b.WriteByte(' ')
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// pythonParamType maps a normalized annotation to a generated parameter. The
// annotation of *args is the type of each element.
func pythonParamType(typ string, variadic bool) (Param, bool) {
	if variadic {
		if pythonScalarType.MatchString(typ) {
			return Param{Kind: ParamVariadic, Basic: typ}, true
		}
		return Param{}, false
	}
	if m := pythonListType.FindStringSubmatch(typ); m != nil {
		return Param{Kind: ParamSlice, Basic: m[1]}, true
	}
	if pythonScalarType.MatchString(typ) {
		return Param{Kind: ParamScalar, Basic: typ}, true
	}
	return Param{}, false
}

// IsPythonPath reports whether a fuzz function or skip path is a Python
// module rather than a Go file or a TS/JS module.
func IsPythonPath(p string) bool { return path.Ext(p) == ".py" }
