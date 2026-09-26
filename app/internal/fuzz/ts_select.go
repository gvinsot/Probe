package fuzz

// Eligibility of changed TS/JS functions (F2c, Appendix D.13). A changed
// exported TS/JS function whose signature is unchanged is planned when its
// parameters can be generated: number, string, boolean and arrays of them,
// read from TypeScript annotations or, in a JavaScript module, from JSDoc
// @param types. Like the enumeration, this is lexical: no type checker runs
// and nothing is executed, so a type alias, a union or an inferred type is
// simply not recognized, and the function is skipped with a reason.

import (
	"path"
	"regexp"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// LanguageScript is the Target.Language of a TS/JS function.
const LanguageScript = "ts/js"

// Fixed skip reasons of TS/JS functions.
const (
	// ReasonScriptTemplate is the reason of an eligible TS/JS function when
	// the generated_test template cannot run a TS/JS harness.
	ReasonScriptTemplate = "TS/JS differential fuzzing needs a verifiable Vitest or Jest generated_test template (a {file} target and a {results_out} report)"
	// ReasonScriptStageNotRun is the reason of an eligible TS/JS function when
	// the stage did not run at all (fuzz.reason says why).
	ReasonScriptStageNotRun  = "not fuzzed: the fuzz stage did not run (see fuzz.reason)"
	ReasonScriptGenerator    = "generator functions are not fuzzed in this version"
	ReasonScriptCommonJS     = "CommonJS modules (.cjs, .cts) are not fuzzed in this version"
	ReasonScriptRenamed      = "module was renamed or moved; the harness imports one path on both revisions"
	ReasonScriptModuleName   = "the module file name is not supported by the fuzz harness (letters, digits, '.', '_' and '-' only)"
	ReasonScriptExportName   = "the exported name is not an ASCII identifier"
	ReasonScriptThis         = "functions with a this parameter are not fuzzed in this version"
	ReasonScriptDestructured = "destructured parameters are not fuzzed in this version"
	ReasonScriptJSDocDiffers = "JSDoc parameter types differ between revisions"
)

func reasonScriptUntyped(name string, js bool) string {
	if js {
		return "parameter " + shortText(name, 64) + " has no JSDoc @param type (number, string, boolean or an array of them)"
	}
	return "parameter " + shortText(name, 64) + " has no type annotation"
}

var (
	// scriptModuleName is the file name a harness can import: the specifier
	// is built from it.
	scriptModuleName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	// asciiIdentifier is an exported name the harness can import by name.
	asciiIdentifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]{0,127}$`)
	// jsdocParam matches one @param tag with a type and a name.
	jsdocParam = regexp.MustCompile(`@param\s*\{([^{}\n]*)\}\s*\[?\s*([A-Za-z_$][A-Za-z0-9_$]*)`)
	// Generated TS/JS types, after normalization (scriptTypeText).
	scriptScalarType = regexp.MustCompile(`^(number|string|boolean)$`)
	scriptArrayType  = regexp.MustCompile(`^(?:readonly (number|string|boolean)\[\]|(number|string|boolean)\[\]|(?:Readonly)?Array<(number|string|boolean)>|Array\.<(number|string|boolean)>)$`)
	scriptRestJSDoc  = regexp.MustCompile(`^\.\.\.(number|string|boolean)$`)
)

// isTypeScriptPath reports whether a module is TypeScript (its parameters
// carry annotations) rather than JavaScript (JSDoc types).
func isTypeScriptPath(p string) bool {
	switch path.Ext(p) {
	case ".ts", ".tsx", ".mts", ".cts":
		return true
	}
	return false
}

// scriptTarget classifies one changed exported function fn of module f (old
// is its baseline path) whose baseline declaration is b. It returns the
// target, or a fixed skip reason.
func scriptTarget(f model.ChangedFile, old string, fn, b scriptFunction) (Target, string) {
	t := Target{
		Dir: f.Path, Path: f.Path, Line: fn.line, EndLine: max(fn.line, fn.endLine), Name: fn.name, Symbol: fn.name,
		Signature: scriptSignature(fn), Exported: true, Language: LanguageScript,
	}
	ext := path.Ext(f.Path)
	switch {
	case ext == ".cjs" || ext == ".cts":
		return t, ReasonScriptCommonJS
	case old != f.Path:
		return t, ReasonScriptRenamed
	case !scriptModuleName.MatchString(path.Base(f.Path)):
		return t, ReasonScriptModuleName
	case fn.name != "default" && !asciiIdentifier.MatchString(fn.name):
		return t, ReasonScriptExportName
	case fn.generator || b.generator:
		return t, ReasonScriptGenerator
	case fn.typeParams || b.typeParams:
		return t, ReasonGeneric
	}
	js := !isTypeScriptPath(f.Path)
	params, reason := scriptParams(fn, js)
	if reason != "" {
		return t, reason
	}
	if js {
		baseParams, baseReason := scriptParams(b, true)
		if baseReason != "" || !equalParams(params, baseParams) {
			return t, ReasonScriptJSDocDiffers
		}
		t.Signature += " with JSDoc types (" + paramTypes(params) + ")"
	}
	t.Params = params
	t.Results = 1
	return t, ""
}

func equalParams(a, b []Param) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// paramTypes writes the TS/JS types of params, for example "number, string[]".
func paramTypes(params []Param) string {
	parts := make([]string, len(params))
	for i, p := range params {
		switch p.Kind {
		case ParamSlice:
			parts[i] = p.Basic + "[]"
		case ParamVariadic:
			parts[i] = "..." + p.Basic + "[]"
		default:
			parts[i] = p.Basic
		}
	}
	return strings.Join(parts, ", ")
}

// scriptSignature is the display form of a signature, for example
// "async (id: string): Promise<string>". It is also part of the seed.
func scriptSignature(fn scriptFunction) string {
	sig := prettyTokens(fn.sig)
	if fn.async {
		sig = "async " + sig
	}
	return sig
}

// prettyTokens joins signature tokens with the spacing people write.
func prettyTokens(toks []scriptToken) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && spaceBetween(toks[i-1].text, t.text) {
			b.WriteByte(' ')
		}
		b.WriteString(t.text)
	}
	return b.String()
}

func spaceBetween(prev, next string) bool {
	switch next {
	case ",", ")", "]", ":", ";", "?", ">", ".":
		return false
	case "(", "[", "<":
		if isScriptName(prev) || prev == ")" || prev == "]" || prev == ">" {
			return false
		}
	}
	switch prev {
	case "(", "[", "<", "...", ".":
		return false
	}
	return true
}

// scriptParams derives the generated parameters of fn. In a TypeScript
// module each parameter needs an annotation; in a JavaScript module each
// needs a JSDoc @param type in the comment right before the declaration.
func scriptParams(fn scriptFunction, js bool) ([]Param, string) {
	var docs map[string]string
	if js {
		docs = jsdocTypes(fn.doc)
	}
	if fn.bare {
		name := fn.params[0].text
		if !js {
			return nil, reasonScriptUntyped(name, false)
		}
		typ, ok := docs[name]
		if !ok {
			return nil, reasonScriptUntyped(name, true)
		}
		p, ok := scriptParamType(typ, false)
		if !ok {
			return nil, reasonParamType(typ)
		}
		return []Param{p}, ""
	}
	params := []Param{}
	for _, part := range splitScriptParams(fn.params) {
		rest := false
		if part[0].text == "..." {
			rest, part = true, part[1:]
			if len(part) == 0 {
				return nil, reasonParamType("...")
			}
		}
		first := part[0].text
		switch {
		case first == "{" || first == "[":
			return nil, ReasonScriptDestructured
		case first == "this":
			return nil, ReasonScriptThis
		case !isScriptName(first):
			return nil, reasonParamType(prettyTokens(part))
		}
		k := 1
		if k < len(part) && part[k].text == "?" {
			k++
		}
		var typ string
		switch {
		case k < len(part) && part[k].text == ":":
			if js {
				return nil, reasonParamType(prettyTokens(part))
			}
			end := k + 1
			for depth := 0; end < len(part); end++ {
				t := part[end].text
				if depth == 0 && t == "=" {
					break
				}
				switch t {
				case "(", "[", "{", "<":
					depth++
				case ")", "]", "}", ">":
					depth--
				}
			}
			typ = scriptTypeText(part[k+1 : end])
		case k < len(part) && part[k].text != "=":
			return nil, reasonParamType(prettyTokens(part))
		}
		if js {
			doc, ok := docs[first]
			if !ok {
				return nil, reasonScriptUntyped(first, true)
			}
			typ = doc
		} else if typ == "" {
			return nil, reasonScriptUntyped(first, false)
		}
		p, ok := scriptParamType(typ, rest)
		if !ok {
			return nil, reasonParamType(typ)
		}
		params = append(params, p)
	}
	return params, ""
}

// splitScriptParams splits the tokens of a parameter list at the commas of
// its own nesting level. Empty parts (a trailing comma) are dropped.
func splitScriptParams(toks []scriptToken) [][]scriptToken {
	var out [][]scriptToken
	depth, start := 0, 0
	for i, t := range toks {
		switch t.text {
		case "(", "[", "{", "<":
			depth++
		case ")", "]", "}", ">":
			depth--
		case ",":
			if depth == 0 {
				if i > start {
					out = append(out, toks[start:i])
				}
				start = i + 1
			}
		}
	}
	if start < len(toks) {
		out = append(out, toks[start:])
	}
	return out
}

// scriptTypeText normalizes the tokens of a type annotation: joined without
// spaces, except one space between two words ("readonly number[]").
func scriptTypeText(toks []scriptToken) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && isScriptName(toks[i-1].text) && isScriptName(t.text) {
			b.WriteByte(' ')
		}
		b.WriteString(t.text)
	}
	return b.String()
}

// scriptParamType maps a normalized type to a generated parameter. A rest
// parameter needs an array type (or the JSDoc form ...T) and becomes
// variadic.
func scriptParamType(typ string, rest bool) (Param, bool) {
	if m := scriptArrayType.FindStringSubmatch(typ); m != nil {
		basic := m[1] + m[2] + m[3] + m[4]
		if rest {
			return Param{Kind: ParamVariadic, Basic: basic}, true
		}
		return Param{Kind: ParamSlice, Basic: basic}, true
	}
	if m := scriptRestJSDoc.FindStringSubmatch(typ); m != nil && rest {
		return Param{Kind: ParamVariadic, Basic: m[1]}, true
	}
	if scriptScalarType.MatchString(typ) && !rest {
		return Param{Kind: ParamScalar, Basic: typ}, true
	}
	return Param{}, false
}

// jsdocTypes reads the @param tags of a /** ... */ comment: parameter name
// to its type with whitespace removed and an optional-parameter "=" suffix
// dropped. A name tagged twice with different types gets no type.
func jsdocTypes(doc string) map[string]string {
	out := map[string]string{}
	conflict := map[string]bool{}
	for _, m := range jsdocParam.FindAllStringSubmatch(doc, -1) {
		typ := strings.TrimSuffix(strings.Join(strings.Fields(m[1]), ""), "=")
		name := m[2]
		if prev, ok := out[name]; ok && prev != typ {
			conflict[name] = true
		}
		out[name] = typ
	}
	for name := range conflict {
		delete(out, name)
	}
	return out
}
