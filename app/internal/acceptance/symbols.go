package acceptance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
)

// identifierPattern is the schema's identifier rule for referenced symbols.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]{0,127}$`)

// IsIdentifier reports whether s can be recorded as a referenced symbol: an
// ASCII identifier of at most 128 bytes (letters, digits, _ and $, not
// starting with a digit).
func IsIdentifier(s string) bool { return identifierPattern.MatchString(s) }

// overlaps reports whether any line of added lies in [start, end].
func overlaps(added []int, start, end int) bool {
	for _, n := range added {
		if n >= start && n <= end {
			return true
		}
	}
	return false
}

// GoChangedDeclarations returns the names of the top-level declarations of a
// Go source file (functions, methods, types, variables and constants) whose
// source range contains at least one of the added line numbers. A method is
// named by its method name. A file that does not parse yields nothing. Names
// that are not ASCII identifiers, and "_", are left out.
func GoChangedDeclarations(filename string, src []byte, added []int) []string {
	if len(added) == 0 {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	names := map[string]bool{}
	add := func(name string, start, end token.Pos) {
		if name != "_" && IsIdentifier(name) && overlaps(added, line(start), line(end)) {
			names[name] = true
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			add(d.Name.Name, d.Pos(), d.End())
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				start, end := spec.Pos(), spec.End()
				if !d.Lparen.IsValid() {
					start, end = d.Pos(), d.End()
				}
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add(s.Name.Name, start, end)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n.Name, start, end)
					}
				}
			}
		}
	}
	return sortedKeys(names)
}

// GoIdentifiers returns every identifier a Go source file names, including
// selector names (the Name of pkg.Name or value.Method), sorted and without
// duplicates. It is a static, name-based reading: identifiers are not
// resolved to declarations.
func GoIdentifiers(filename string, src []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && IsIdentifier(id.Name) && id.Name != "_" {
			names[id.Name] = true
		}
		return true
	})
	return sortedKeys(names), nil
}

// jsDeclaration matches a top-level JavaScript/TypeScript declaration at
// column 0 and captures its name.
var jsDeclaration = regexp.MustCompile(`^(?:export[ \t]+(?:default[ \t]+)?)?(?:declare[ \t]+)?(?:abstract[ \t]+)?(?:async[ \t]+)?(?:function\*?|class|const|let|var|interface|type|enum|namespace)[ \t]+\*?[ \t]*([A-Za-z_$][A-Za-z0-9_$]*)`)

// JSChangedDeclarations is the lexical counterpart of GoChangedDeclarations
// for JavaScript and TypeScript. A top-level declaration is a line at column
// 0 such as "export function name", "const name", "class Name" or
// "interface Name"; its range runs to the line before the next line that
// starts at column 0 with anything other than a closing }, ) or ]. It is an
// approximation: declarations are recognized by their text, not parsed.
func JSChangedDeclarations(src string, added []int) []string {
	if len(added) == 0 {
		return nil
	}
	lines := strings.Split(src, "\n")
	names := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		m := jsDeclaration.FindStringSubmatch(strings.TrimSuffix(lines[i], "\r"))
		if m == nil {
			continue
		}
		end := i + 1
		for end < len(lines) && !topLevelStart(strings.TrimSuffix(lines[end], "\r")) {
			end++
		}
		// Lines i+1 .. end (1-based) belong to the declaration.
		if IsIdentifier(m[1]) && overlaps(added, i+1, end) {
			names[m[1]] = true
		}
	}
	return sortedKeys(names)
}

// topLevelStart reports a line that starts a new top-level statement: its
// first byte is not a space, a tab, or a closing bracket.
func topLevelStart(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case ' ', '\t', '}', ')', ']':
		return false
	}
	return true
}

// JSIdentifiers returns the identifiers a JavaScript or TypeScript source
// names outside string literals, template literals and comments, sorted and
// without duplicates. It is lexical: a regular-expression literal is read as
// code, and nothing is resolved.
func JSIdentifiers(src string) []string {
	names := map[string]bool{}
	isStart := func(c byte) bool { return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	isPart := func(c byte) bool { return isStart(c) || c >= '0' && c <= '9' }
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
		case c == '"' || c == '\'' || c == '`':
			i++
			for i < len(src) && src[i] != c {
				if src[i] == '\\' {
					i++
				} else if src[i] == '\n' && c != '`' {
					break
				}
				i++
			}
			i++
		case isStart(c):
			j := i + 1
			for j < len(src) && isPart(src[j]) {
				j++
			}
			// A name glued to a preceding digit or non-ASCII letter is not an
			// identifier token of its own.
			if i == 0 || !isPart(src[i-1]) && src[i-1] < 0x80 {
				if name := src[i:j]; IsIdentifier(name) {
					names[name] = true
				}
			}
			i = j
		default:
			i++
		}
	}
	return sortedKeys(names)
}

// NamedOn reports whether symbol occurs in text as a whole word: the bytes
// around the occurrence are not ASCII letters, digits, _ or $.
func NamedOn(text, symbol string) bool {
	if symbol == "" {
		return false
	}
	part := func(c byte) bool {
		return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], symbol)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(symbol)
		if (i == 0 || !part(text[i-1])) && (end == len(text) || !part(text[end])) {
			return true
		}
		from = i + 1
	}
	return false
}

// Intersect returns the names present in both sorted lists, sorted.
func Intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, n := range b {
		set[n] = true
	}
	out := map[string]bool{}
	for _, n := range a {
		if set[n] {
			out[n] = true
		}
	}
	return sortedKeys(out)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
