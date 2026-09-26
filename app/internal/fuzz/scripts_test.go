package fuzz

import (
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

const scriptBase = `// Pricing helpers.
import { clamp } from "./clamp";

export function discount(total: number, pct: number): number {
  return total - (total * pct) / 100;
}

export const round = (n: number): number => Math.round(n);

export async function load(id: string): Promise<string> {
  return id;
}

export default function (x: number) {
  return x + 1;
}

function helper(a: string) {
  return a.trim();
}

export function changedSig(a: number): number {
  return a;
}

export function unchanged(a: number) {
  return a * 2;
}

const internal = () => 1;

export const regex = (s: string) => /a\/b}/.test(s);

export function tpl(n: number) {
  return ` + "`n=${n}}`" + `;
}

export function* gen(n: number) {
  yield n;
}

export const typed: (a: number) => number = function named(a) {
  return a;
};

export function becomesAsync(a: number) {
  return a;
}

export { helper, internal as renamed };
`

const scriptCandidate = `// Pricing helpers.
import { clamp } from "./clamp";

export function discount(total: number, pct: number): number {
  return clamp(total - (total * pct) / 100);
}

export const round = (n: number): number => Math.floor(n);

export async function load(id: string): Promise<string> {
  return id.trim();
}

export default function (x: number) {
  return x + 2;
}

function helper(a: string) {
  return a.trimStart();
}

export function changedSig(a: number, b = 0): number {
  return a + b;
}

export function unchanged(a: number) {
  // A comment and new layout only.
  return a *
    2;
}

const internal = () => 2;

export const regex = (s: string) => /a\/c}/.test(s);

export function tpl(n: number) {
  return ` + "`n=${n + 1}}`" + `;
}

export function* gen(n: number) {
  yield n + 1;
}

export const typed: (a: number) => number = function named(a) {
  return a + 1;
};

export async function becomesAsync(a: number) {
  return a + 1;
}

export { helper, internal as renamed };
`

func TestSelectScriptsListsChangedExportsWithSameSignature(t *testing.T) {
	base, candidate := t.TempDir(), t.TempDir()
	writeTree(t, base, map[string]string{
		"web/price.ts":            scriptBase,
		"web/price.test.ts":       scriptBase,
		"web/types.d.ts":          "export declare function f(a: number): number;\n",
		"node_modules/x/index.js": "export function f() { return 1 }\n",
		"web/old.js":              "export function moved(a) { return a }\n",
		"web/hello.tsx":           "export function Hello({ name }: { name: string }) {\n  return <p>Don't {name}</p>;\n}\n",
		"web/added_base_only.ts":  "export function a() { return 1 }\n",
		"web/secrets.json":        "{}",
	})
	writeTree(t, candidate, map[string]string{
		"web/price.ts":            scriptCandidate,
		"web/price.test.ts":       scriptCandidate,
		"web/types.d.ts":          "export declare function f(a: number): string;\n",
		"node_modules/x/index.js": "export function f() { return 2 }\n",
		"web/new.js":              "export function moved(a) { return a + 1 }\n",
		"web/hello.tsx":           "export function Hello({ name }: { name: string }) {\n  return <p>Don't {name}!</p>;\n}\n",
		"web/fresh.ts":            "export function fresh() { return 1 }\n",
	})
	change := model.Change{Files: []model.ChangedFile{
		{Path: "web/price.ts", Status: "M"},
		{Path: "web/price.test.ts", Status: "M"},
		{Path: "web/types.d.ts", Status: "M"},
		{Path: "node_modules/x/index.js", Status: "M"},
		{Path: "web/new.js", OldPath: "web/old.js", Status: "R"},
		{Path: "web/hello.tsx", Status: "M"},
		{Path: "web/fresh.ts", Status: "A"},
		{Path: "web/price.go", Status: "M"},
	}}
	got := SelectScripts(base, candidate, change)
	var listed []string
	for _, s := range got {
		if s.Reason != ReasonScriptNotImplemented {
			t.Fatalf("reason %q", s.Reason)
		}
		listed = append(listed, s.Path+":"+s.Symbol)
	}
	// Not listed: the changed signature, the layout-only edit, the function
	// that became async, and every ignored file.
	want := "web/hello.tsx:Hello,web/new.js:moved,web/price.ts:discount,web/price.ts:round,web/price.ts:load,web/price.ts:default,web/price.ts:helper,web/price.ts:renamed,web/price.ts:regex,web/price.ts:tpl,web/price.ts:gen,web/price.ts:typed"
	if strings.Join(listed, ",") != want {
		t.Fatalf("listed %s\nwant   %s", strings.Join(listed, ","), want)
	}
	lines := map[string]int{}
	for _, s := range got {
		lines[s.Symbol] = s.Line
	}
	if lines["discount"] != 4 || lines["round"] != 8 || lines["default"] != 14 || lines["helper"] != 18 || lines["renamed"] != 32 {
		t.Fatalf("lines %v", lines)
	}
	// Deterministic.
	again := SelectScripts(base, candidate, change)
	if len(again) != len(got) {
		t.Fatal("two selections differ")
	}
	for i := range got {
		if again[i] != got[i] {
			t.Fatal("two selections differ")
		}
	}
}

func TestScriptTokensSkipCommentsStringsAndRegexps(t *testing.T) {
	src := "const a = 1 / 2; // tail } {\n/* block\n { */ const r = /[/}]{2}/g; const s = \"}\\\"{\"; const t = `x${ {a: '}'} }y`; if (a) { b }\n"
	toks := scriptTokens(src)
	depth := 0
	var texts []string
	for _, tk := range toks {
		texts = append(texts, tk.text)
		switch tk.text {
		case "{":
			depth++
		case "}":
			depth--
		}
	}
	if depth != 0 {
		t.Fatalf("unbalanced braces in %q", texts)
	}
	joined := strings.Join(texts, " ")
	for _, want := range []string{"1 / 2", "/[/}]{2}/g", `"}\"{"`, "`x${ {a: '}'} }y`"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tokens lack %q: %q", want, joined)
		}
	}
	if toks[len(toks)-1].line != 3 {
		t.Fatalf("line tracking: last token on line %d", toks[len(toks)-1].line)
	}
}

func TestScriptFunctionsIgnoreNestedAndUnexported(t *testing.T) {
	fns := scriptFunctions([]byte(`
function outer() {
  function inner() { return 1 }
  return inner
}
export class K { m() { return 1 } }
export const obj = { f() { return 1 } };
export const n = 5;
export function overload(a: string): string;
export function overload(a: any) { return a }
export default helperName;
function helperName(a) { return a }
export * from "./other";
export { outer as fromOther } from "./other";
`))
	var names []string
	for _, f := range sortedScriptFunctions(fns) {
		names = append(names, f.name)
	}
	if strings.Join(names, ",") != "overload,default" {
		t.Fatalf("functions %v", names)
	}
}
