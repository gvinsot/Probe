package fuzz

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
	got := SelectScripts(context.Background(), base, candidate, change)
	var listed []string
	lines := map[string]int{}
	for _, tg := range got.Targets {
		listed = append(listed, tg.Path+":"+tg.Symbol)
		lines[tg.Symbol] = tg.Line
		if tg.Language != LanguageScript || tg.Dir != tg.Path || !tg.Exported || tg.Results != 1 {
			t.Fatalf("target %+v", tg)
		}
	}
	for _, s := range got.Skipped {
		listed = append(listed, s.Path+":"+s.Symbol+": "+s.Reason)
		lines[s.Symbol] = s.Line
	}
	// Not listed: the changed signature, the layout-only edit, the function
	// that became async, and every ignored file. Planned: every changed
	// function whose parameters are annotated number, string or boolean (or
	// that has none); skipped with a reason: a destructured parameter, a
	// renamed module, a generator and a parameter without an annotation.
	want := strings.Join([]string{
		"web/price.ts:discount", "web/price.ts:round", "web/price.ts:load", "web/price.ts:default", "web/price.ts:helper",
		"web/price.ts:renamed", "web/price.ts:regex", "web/price.ts:tpl",
		"web/hello.tsx:Hello: " + ReasonScriptDestructured, "web/new.js:moved: " + ReasonScriptRenamed,
		"web/price.ts:gen: " + ReasonScriptGenerator, "web/price.ts:typed: parameter a has no type annotation",
	}, ",")
	if strings.Join(listed, ",") != want {
		t.Fatalf("listed %s\nwant   %s", strings.Join(listed, ","), want)
	}
	if lines["discount"] != 4 || lines["round"] != 8 || lines["default"] != 14 || lines["helper"] != 18 || lines["renamed"] != 32 {
		t.Fatalf("lines %v", lines)
	}
	sigs := map[string]string{}
	for _, tg := range got.Targets {
		sigs[tg.Symbol] = tg.Signature
	}
	if sigs["discount"] != "(total: number, pct: number): number" || sigs["load"] != "async (id: string): Promise<string>" || sigs["renamed"] != "()" {
		t.Fatalf("signatures %q", sigs)
	}
	// Deterministic.
	again := SelectScripts(context.Background(), base, candidate, change)
	if fmt.Sprint(again) != fmt.Sprint(got) {
		t.Fatal("two selections differ")
	}
}

func TestScriptTokensSkipCommentsStringsAndRegexps(t *testing.T) {
	src := "const a = 1 / 2; // tail } {\n/* block\n { */ const r = /[/}]{2}/g; const s = \"}\\\"{\"; const t = `x${ {a: '}'} }y`; if (a) { b }\n"
	toks, cut := scriptTokens(src)
	if cut {
		t.Fatal("a short module was cut")
	}
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
	fns, cut := scriptFunctions([]byte(`
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
	if cut {
		t.Fatal("a short module was cut")
	}
	var names []string
	for _, f := range sortedScriptFunctions(fns) {
		names = append(names, f.name)
	}
	if strings.Join(names, ",") != "overload,default" {
		t.Fatalf("functions %v", names)
	}
}

// adversarialScripts are TS/JS modules of just under 2 MiB that made the
// enumeration quadratic or deeply recursive: every failed parse attempt
// scanned to the end of the module, or template literals nested without end.
func adversarialScripts() map[string]string {
	out := map[string]string{}
	for name, unit := range map[string]string{
		"typed binding without an initializer": "const a: b ",
		"typed binding per line":               "const a: b\n",
		"type parameters that never close":     "function f < ",
		"return type without a body":           "function f(): b ",
		"arrow return type without an arrow":   "const a = (): b ",
		"nested template literals":             "`${",
	} {
		out[name] = strings.Repeat(unit, (2<<20-1)/len(unit))
	}
	return out
}

// The enumeration is linear in the size of candidate-controlled modules: each
// adversarial module ends well within a generous time bound (the quadratic
// scan took over a minute on 2 MiB), and a module that a bound stops is
// reported as cut rather than partially listed. Code that omits semicolons
// after typed bindings is not cut.
func TestScriptEnumerationIsBoundedOnAdversarialInput(t *testing.T) {
	for name, src := range adversarialScripts() {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			_, cut := scriptFunctions([]byte(src))
			if d := time.Since(started); d > 20*time.Second {
				t.Fatalf("%d bytes took %s", len(src), d)
			}
			if name == "typed binding per line" && cut {
				t.Fatal("typed bindings on separate lines reached a bound")
			}
			if name == "nested template literals" && !cut {
				t.Fatal("template literals nested beyond the depth bound were not reported as cut")
			}
		})
	}
	// Ordinary code far larger than any test fixture is enumerated completely
	// within the work bound.
	var b strings.Builder
	for i := 0; b.Len() < 1<<20; i++ {
		fmt.Fprintf(&b, "export function f%d(a: number, b: string): number {\n  const x: number = a + %d\n  let y: string\n  return x + b.length\n}\nexport const g%d = (n: number): number => n * %d\n", i, i, i, i)
	}
	fns, cut := scriptFunctions([]byte(b.String()))
	if cut || len(fns) < 1000 {
		t.Fatalf("ordinary module: cut %t, %d functions", cut, len(fns))
	}
}

// SelectScripts gives a file that a bound stops one file-level entry (line 0,
// no symbol) instead of its functions: a file over 2 MiB, a file whose scan
// is cut, files after the total source bound, and files after the context is
// done.
func TestSelectScriptsFileLevelBounds(t *testing.T) {
	base, candidate := t.TempDir(), t.TempDir()
	big := "export function big() { return 1 }\n" + strings.Repeat("//", 1<<20) + "\n"
	cut := strings.Repeat("const a: b ", 100000)
	writeTree(t, base, map[string]string{
		"web/a.ts":   "export function a() { return 1 }\n",
		"web/big.ts": big,
		"web/cut.ts": "export function c() { return 1 }\n" + cut,
		"web/z.ts":   "export function z() { return 1 }\n",
	})
	writeTree(t, candidate, map[string]string{
		"web/a.ts":   "export function a() { return 2 }\n",
		"web/big.ts": big,
		"web/cut.ts": "export function c() { return 2 }\n" + cut,
		"web/z.ts":   "export function z() { return 2 }\n",
	})
	change := model.Change{Files: []model.ChangedFile{{Path: "web/a.ts", Status: "M"}, {Path: "web/big.ts", Status: "M"}, {Path: "web/cut.ts", Status: "M"}, {Path: "web/z.ts", Status: "M"}}}
	render := func(sel ScriptSelection) string {
		var parts []string
		for _, tg := range sel.Targets {
			parts = append(parts, fmt.Sprintf("%s:%d:%s:planned", tg.Path, tg.Line, tg.Symbol))
		}
		for _, s := range sel.Skipped {
			parts = append(parts, fmt.Sprintf("%s:%d:%s:%s", s.Path, s.Line, s.Symbol, s.Reason))
		}
		return strings.Join(parts, "\n")
	}
	want := strings.Join([]string{
		"web/a.ts:1:a:planned",
		"web/z.ts:1:z:planned",
		"web/big.ts:0::" + ReasonScriptTooLarge,
		"web/cut.ts:0::" + ReasonScriptScanBound,
	}, "\n")
	if got := render(SelectScripts(context.Background(), base, candidate, change)); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Once the total bound is reached, no further file is read.
	saved := scriptSourceBudget
	scriptSourceBudget = 1
	defer func() { scriptSourceBudget = saved }()
	want = strings.Join([]string{
		"web/a.ts:1:a:planned",
		"web/big.ts:0::" + ReasonScriptTotalBound,
		"web/cut.ts:0::" + ReasonScriptTotalBound,
		"web/z.ts:0::" + ReasonScriptTotalBound,
	}, "\n")
	if got := render(SelectScripts(context.Background(), base, candidate, change)); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	scriptSourceBudget = saved
	// A done context (the fuzz sub-cap or --deadline) stops the enumeration.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := SelectScripts(ctx, base, candidate, change)
	if len(done.Targets) != 0 || len(done.Skipped) != 4 {
		t.Fatalf("selection after the context ended: %+v", done)
	}
	for _, s := range done.Skipped {
		if s.Line != 0 || s.Symbol != "" || s.Reason != ReasonScriptTimeLimit {
			t.Fatalf("entry %+v", s)
		}
	}
}
