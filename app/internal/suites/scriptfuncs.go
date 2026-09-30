package suites

import (
	"errors"
	"unicode/utf8"

	"github.com/gvinsot/Probe/app/internal/symbols"
)

// parseScriptTestFile reads one TypeScript or JavaScript test file with the
// static index's lexical reader: its test() and it() calls in source order,
// named by their describe titles and title, and one digest of every token
// outside them (imports, helpers, describe titles and hooks) as the header,
// so any edit there selects every test of the file as shared_code_changed.
// A name declared more than once is ambiguous: no single test answers to it.
func parseScriptTestFile(filename string, src []byte) (testFile, error) {
	if !utf8.Valid(src) {
		return testFile{}, errors.New("not valid UTF-8")
	}
	read, ok := symbols.ReadScriptTests(filename, src)
	if !ok {
		return testFile{}, errors.New("not a TypeScript or JavaScript test file")
	}
	out := testFile{byName: map[string]int{}, shared: map[string]string{}, effects: map[string]string{}, header: "script " + read.Shared, ambiguous: map[string]bool{}}
	for _, t := range read.Tests {
		if _, seen := out.byName[t.Name]; seen {
			out.ambiguous[t.Name] = true
			continue
		}
		out.byName[t.Name] = len(out.tests)
		out.tests = append(out.tests, testFunc{name: t.Name, line: t.Line, endLine: t.EndLine, digest: t.Digest})
	}
	return out, nil
}
