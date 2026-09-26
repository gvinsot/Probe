package fuzz

import (
	"context"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// selectOne runs SelectScripts on one module whose baseline and candidate
// sources are given, and returns the outcome of every changed function as
// "name: planned <params>" or "name: <reason>".
func selectOne(t *testing.T, name, base, candidate string, status string) map[string]string {
	t.Helper()
	b, c := t.TempDir(), t.TempDir()
	oldPath := name
	if status == "R" {
		oldPath = "web/old" + name[strings.LastIndex(name, "."):]
	}
	writeTree(t, b, map[string]string{oldPath: base})
	writeTree(t, c, map[string]string{name: candidate})
	f := model.ChangedFile{Path: name, Status: status}
	if status == "R" {
		f.OldPath = oldPath
	}
	sel := SelectScripts(context.Background(), b, c, model.Change{Files: []model.ChangedFile{f}})
	out := map[string]string{}
	for _, tg := range sel.Targets {
		out[tg.Symbol] = "planned " + paramTypes(tg.Params)
	}
	for _, s := range sel.Skipped {
		out[s.Symbol] = s.Reason
	}
	return out
}

// bump changes the body of every "return X;" of a module by appending "+ 0".
func bump(src string) string { return strings.ReplaceAll(src, ";\n}", " + 0;\n}") }

// TypeScript parameters are generated when annotated number, string, boolean
// or an array of them; everything else gets a specific reason.
func TestSelectScriptsTypeScriptParameters(t *testing.T) {
	src := `export function scalars(a: number, b: string, c: boolean): number {
  return a;
}
export function arrays(a: number[], b: Array<string>, c: readonly boolean[], d: ReadonlyArray<number>): number {
  return a.length;
}
export function rest(a: string, ...more: number[]): number {
  return more.length;
}
export function optional(a?: number, b: string = "x"): number {
  return 1;
}
export function union(a: number | string): number {
  return 1;
}
export function untyped(a, b: number): number {
  return 1;
}
export function destructured({ a }: { a: number }): number {
  return a;
}
export function withThis(this: Window, a: number): number {
  return a;
}
export function generic<T>(a: T): T {
  return a;
}
export function* gen(a: number) {
  yield a;
}
export function callback(f: (x: number) => number): number {
  return f(1);
}
export function nested(a: number[][]): number {
  return 1;
}
export function none(): string {
  return "x";
}
export const arrow = (a: number): number => {
  return a;
};
export async function later(a: string): Promise<string> {
  return a;
}
export default function (a: boolean): boolean {
  return a;
}
`
	got := selectOne(t, "web/mod.ts", src, bump(src), "M")
	want := map[string]string{
		"scalars":      "planned number, string, boolean",
		"arrays":       "planned number[], string[], boolean[], number[]",
		"rest":         "planned string, ...number[]",
		"optional":     "planned number, string",
		"union":        "parameter type number|string is not generated in this version",
		"untyped":      "parameter a has no type annotation",
		"destructured": ReasonScriptDestructured,
		"withThis":     ReasonScriptThis,
		"generic":      ReasonGeneric,
		"gen":          ReasonScriptGenerator,
		"callback":     "parameter type (x:number)=>number is not generated in this version",
		"nested":       "parameter type number[][] is not generated in this version",
		"none":         "planned ",
		"arrow":        "planned number",
		"later":        "planned string",
		"default":      "planned boolean",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("outcomes %v", got)
	}
}

// JavaScript parameters take their types from the JSDoc @param tags of the
// comment right before the declaration, and those types must be the same on
// both revisions.
func TestSelectScriptsJavaScriptJSDoc(t *testing.T) {
	base := `/**
 * Discount.
 * @param {number} total the total
 * @param {number=} pct optional percent
 * @returns {number}
 */
export function discount(total, pct = 10) {
  return total - pct;
}

/** @param {Array.<string>} parts */
export const join = (parts) => {
  return parts.join(",");
};

/** @param {string[]} parts */
export const bare = parts => {
  return parts.length;
};

/** @param {...number} xs */
export function sum(...xs) {
  return xs.length;
}

/** @param {number} a */
export function missing(a, b) {
  return a;
}

/** @param {number} a */
export function retyped(a) {
  return a;
}

/** @param {Object} o */
export function object(o) {
  return o;
}

// Not a JSDoc comment.
export function plain(a) {
  return a;
}
`
	candidate := strings.Replace(bump(base), "/** @param {number} a */\nexport function retyped", "/** @param {string} a */\nexport function retyped", 1)
	got := selectOne(t, "web/mod.js", base, candidate, "M")
	want := map[string]string{
		"discount": "planned number, number",
		"join":     "planned string[]",
		"bare":     "planned string[]",
		"sum":      "planned ...number[]",
		"missing":  "parameter b has no JSDoc @param type (number, string, boolean or an array of them)",
		"retyped":  ReasonScriptJSDocDiffers,
		"object":   "parameter type Object is not generated in this version",
		"plain":    "parameter a has no JSDoc @param type (number, string, boolean or an array of them)",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("outcomes %v", got)
	}
	// The JSDoc types are part of the displayed signature (and of the seed).
	b, c := t.TempDir(), t.TempDir()
	writeTree(t, b, map[string]string{"web/mod.js": base})
	writeTree(t, c, map[string]string{"web/mod.js": candidate})
	sel := SelectScripts(context.Background(), b, c, modified("web/mod.js"))
	for _, tg := range sel.Targets {
		if tg.Symbol == "discount" && tg.Signature != "(total, pct = 10) with JSDoc types (number, number)" {
			t.Fatalf("signature %q", tg.Signature)
		}
	}
}

// Module-level reasons: CommonJS files, renamed modules, file names the
// harness cannot import, and exported names that are not ASCII identifiers.
func TestSelectScriptsModuleReasons(t *testing.T) {
	src := "export function f(a: number): number {\n  return a;\n}\n"
	for name, want := range map[string]string{
		"web/mod.cts":      ReasonScriptCommonJS,
		"web/my mod.ts":    ReasonScriptModuleName,
		"web/mod.mts":      "planned number",
		"web/mod.tsx":      "planned number",
		"web/component.js": "parameter type a: number is not generated in this version", // an annotation in JavaScript
	} {
		if got := selectOne(t, name, src, bump(src), "M")["f"]; got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got := selectOne(t, "web/new.ts", src, bump(src), "R")["f"]; got != ReasonScriptRenamed {
		t.Errorf("renamed module: %q", got)
	}
	unicode := "export function café(a: number): number {\n  return a;\n}\n"
	if got := selectOne(t, "web/mod.ts", unicode, bump(unicode), "M")["café"]; got != ReasonScriptExportName {
		t.Errorf("non-ASCII export: %q", got)
	}
}

// SelectAll plans TS/JS modules like packages under the one budget, and
// skips eligible TS/JS functions when the template cannot run them.
func TestSelectAllSharesTheBudgetAndNeedsATemplate(t *testing.T) {
	base, candidate := calcTrees(t)
	ts := "export function price(n: number): number {\n  return n;\n}\n\nexport function tax(n: number): number {\n  return n;\n}\n"
	writeTree(t, base, map[string]string{"web/price.ts": ts})
	writeTree(t, candidate, map[string]string{"web/price.ts": bump(ts)})
	change := modified("calc/calc.go", "web/price.ts")
	// A Go template: the TS/JS functions are listed with the template reason.
	plan, err := SelectAll(context.Background(), base, candidate, change, nil, defaultLimits(), "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.GoTargets() != plan.Targets() || plan.Targets() == 0 {
		t.Fatalf("plan %+v", plan)
	}
	templateSkips := 0
	for _, s := range plan.Skipped {
		if s.Reason == ReasonScriptTemplate {
			templateSkips++
		}
	}
	if templateSkips != 2 {
		t.Fatalf("skipped %+v", plan.Skipped)
	}
	goTargets := plan.GoTargets()
	// A Vitest template: the module is planned as its own package, after the
	// Go package (both have exported functions without signals: path order).
	plan, err = SelectAll(context.Background(), base, candidate, change, nil, defaultLimits(), FamilyVitest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GoTargets() != goTargets || plan.Targets() != goTargets+2 {
		t.Fatalf("plan %+v", plan.Packages)
	}
	last := plan.Packages[len(plan.Packages)-1]
	if last.Script == nil || *last.Script != (ScriptModule{Path: "web/price.ts", Import: "./price", Ext: "ts"}) || len(last.Targets) != 2 || last.Dir != "web/price.ts" {
		t.Fatalf("module plan %+v", last)
	}
	for _, tg := range last.Targets {
		if tg.Inputs != len(scriptCorpus(tg, defaultLimits().MaxInputs)) || tg.Inputs == 0 {
			t.Fatalf("inputs of %s: %d", tg.Name, tg.Inputs)
		}
	}
	if skips := plan.ScriptSkips("x"); len(skips) != 2 || skips[0].Symbol != "price" {
		t.Fatalf("script skips %+v", skips)
	}
	// A high signal on the module puts it first; max_packages 1 then cuts the
	// Go package, and max_functions counts both languages.
	signals := []model.Signal{{Path: "web/price.ts", Side: "new", Line: 5, EndLine: 5, Severity: "high"}}
	limits := defaultLimits()
	limits.MaxPackages = 1
	plan, err = SelectAll(context.Background(), base, candidate, change, signals, limits, FamilyJest)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packages) != 1 || plan.Packages[0].Script == nil || plan.Packages[0].Targets[0].Name != "tax" {
		t.Fatalf("plan %+v", plan.Packages)
	}
	budgetSkips := 0
	for _, s := range plan.Skipped {
		if s.Reason == ReasonBudgetPackages {
			budgetSkips++
			if !strings.HasPrefix(s.Path, "calc/") {
				t.Fatalf("skip %+v", s)
			}
		}
	}
	if budgetSkips != goTargets || plan.BudgetSkipped != goTargets {
		t.Fatalf("%d budget skips, want %d: %+v", budgetSkips, goTargets, plan.Skipped)
	}
	limits = defaultLimits()
	limits.MaxFunctions = 1
	plan, err = SelectAll(context.Background(), base, candidate, change, signals, limits, FamilyJest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Targets() != 1 || plan.Packages[0].Targets[0].Name != "tax" {
		t.Fatalf("plan %+v", plan.Packages)
	}
}

func TestScriptFamily(t *testing.T) {
	for want, cmds := range map[string][][]string{
		FamilyVitest: {
			{"vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"},
			{"npx", "vitest", "run", "{file}", "--reporter=json", "--outputFile={results_out}"},
			{"node", "node_modules/vitest/vitest.mjs", "run", "{file}", "--outputFile={results_out}"},
		},
		FamilyJest: {
			{"jest", "{file}", "--json", "--outputFile={results_out}"},
			{"/usr/local/bin/jest", "{file}", "--json", "--outputFile={results_out}"},
		},
		"": {
			{"go", "test", "{package}"},
			{"vitest", "run", "{file}"},                                   // no report: not verifiable
			{"npm", "test", "--", "{file}", "--outputFile={results_out}"}, // a package script
			{"vitest", "run", "{file}", "{file}", "--outputFile={results_out}"},
			{"node", "vitest", "jest", "{file}", "--outputFile={results_out}"}, // both
			{"mocha", "{file}", "--reporter-option", "output={results_out}"},
		},
	} {
		for _, cmd := range cmds {
			if got := ScriptFamily(cmd); got != want {
				t.Errorf("%q: %q, want %q", cmd, got, want)
			}
		}
	}
}

func TestScriptSignatureSpacing(t *testing.T) {
	for src, want := range map[string]string{
		"export function f(total: number, pct: number): number {\n  return 1;\n}\n":               "(total: number, pct: number): number",
		"export const f = async (xs: readonly number[]): Promise<number> => {\n  return 1;\n};\n": "async (xs: readonly number[]): Promise<number>",
		"export function f(...xs: Array<string>) {\n  return 1;\n}\n":                             "(...xs: Array<string>)",
	} {
		fns, _ := scriptFunctions([]byte(src))
		if got := scriptSignature(fns["f"]); got != want {
			t.Errorf("%q: %q, want %q", src, got, want)
		}
	}
}
