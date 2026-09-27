package symbols

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// declNames lists "name kind [test]" for each declaration of a parsed source.
func declNames(f *lexFile) []string {
	var out []string
	for _, d := range f.decls {
		s := d.name + " " + d.kind
		if d.test {
			s += " test"
		}
		out = append(out, s)
	}
	return out
}

func callNames(f *lexFile) []string {
	var out []string
	for _, c := range f.calls {
		s := c.name
		switch {
		case c.ctor:
			s = "new " + s
		case c.member:
			s = c.qual + "." + s
		case c.path:
			s = c.qual + "::" + s
		}
		out = append(out, s)
	}
	return out
}

func equalLists(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
}

func TestLexLanguageAndPaths(t *testing.T) {
	for p, want := range map[string]string{
		"src/a.ts": LangTypeScript, "src/a.tsx": LangTypeScript, "a.mjs": LangTypeScript, "types/a.d.ts": "", "a.min.js": "",
		"pkg/a.py": LangPython, "src/lib.rs": LangRust, "main.go": "", "README.md": "",
	} {
		if got := lexLanguage(p); got != want {
			t.Errorf("lexLanguage(%q) = %q, want %q", p, got, want)
		}
	}
	for p, want := range map[string]bool{
		"src/a.ts": true, "node_modules/x/a.js": false, "target/debug/a.rs": false, ".venv/a.py": false,
		"web/dist/app.js": false, "pkg/_private.py": true,
	} {
		if got := lexIndexable(p, nil); got != want {
			t.Errorf("lexIndexable(%q) = %v, want %v", p, got, want)
		}
	}
	for p, want := range map[string]bool{
		"src/cart.test.ts": true, "src/__tests__/cart.ts": true, "src/cart.ts": false,
		"tests/test_cart.py": true, "shop/cart_test.py": true, "shop/cart.py": false,
		"tests/integration.rs": true, "src/lib.rs": false,
	} {
		if got := testFile(p); got != want {
			t.Errorf("testFile(%q) = %v, want %v", p, got, want)
		}
	}
	for p, want := range map[string]string{"src/cart.ts": "cart", "src/cart/index.ts": "cart", "shop/__init__.py": "shop", "src/util/mod.rs": "util", "src/lib.rs": "lib"} {
		if got := moduleName(p); got != want {
			t.Errorf("moduleName(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestTokenizeDropsCommentsAndKeepsStrings(t *testing.T) {
	src := "// call(a)\nconst s = 'x(y)'; /* z(w) */ const r = /a(b)/g; const t = `k${f(1)}v(2)`;\n"
	var texts []string
	for _, tk := range tokenize(LangTypeScript, []byte(src)) {
		texts = append(texts, tk.text)
	}
	equalLists(t, "tokens", texts, []string{"const", "s", "=", "'x(y)'", ";", "const", "r", "=", "/a(b)/g", ";", "const", "t", "=", "`k${", "f", "(", "1", ")", "}v(2)`", ";"})

	py := "def f():\n    s = \"\"\"doc\n g(x)\n\"\"\"  # h(y)\n    return r'\\d(' + k(1)\n"
	f := parseLexical("m.py", []byte(py))
	equalLists(t, "python calls", callNames(f), []string{"k"})

	rs := "fn a<'a>(x: &'a str) -> char { let c = '('; let s = r#\"b(\"#; d(x) }\n"
	f = parseLexical("src/a.rs", []byte(rs))
	equalLists(t, "rust calls", callNames(f), []string{"d"})
}

func TestParseTypeScript(t *testing.T) {
	src := `import { tax } from './tax';

export function total(items: number[]): { sum: number } {
  return { sum: items.reduce((a, b) => a + b, 0) + tax(1) };
}

export const discount = async (n: number): Promise<number> => {
  return helper(n);
};

const double = (n: number) => n * 2;

function helper(n: number) { return double(n); }

interface Totaler { total(): number; }

export class Cart<T> extends Base implements Totaler {
  private items: number[] = [];
  static create(): Cart<number> { return new Cart(); }
  constructor() { super(); }
  total(): number {
    return total(this.items).sum + this.fee();
  }
  fee = () => helper(1);
  abstractish(): void;
}
`
	f := parseLexical("src/cart.ts", []byte(src))
	equalLists(t, "declarations", declNames(f), []string{
		"total func", "discount func", "double func", "helper func", "Cart.create method", "Cart.constructor method", "Cart.total method", "Cart.fee method",
	})
	equalLists(t, "calls", callNames(f), []string{"items.reduce", "tax", "helper", "double", "new Cart", "total", "this.fee", "helper"})
}

func TestParseTypeScriptTests(t *testing.T) {
	src := `import { total } from '../src/cart';
describe('cart', () => {
  it('sums items', () => {
    expect(total([1, 2]).sum).toBe(3);
  });
  test.skip('ignored', () => {});
});
test("top level", async () => { await total([]); });
`
	f := parseLexical("test/cart.test.ts", []byte(src))
	equalLists(t, "declarations", declNames(f), []string{"cart > sums items func test", "top level func test"})
}

func TestParsePython(t *testing.T) {
	src := `import helpers

def total(items):
    """Sum (items)."""
    def inner(x):
        return x
    return sum(items) + helpers.tax(1) + inner(2)


class Cart(Base):
    def __init__(self, items):
        self.items = items

    @property
    def size(self) -> int:
        return len(self.items)

    async def checkout(self, *, fee=lambda: compute(1)):
        return total(self.items) + self.size()

    class Line:
        def cost(self): return 1

def after():
    return Cart([1]).checkout()
`
	f := parseLexical("shop/cart.py", []byte(src))
	equalLists(t, "declarations", declNames(f), []string{
		"total func", "Cart.__init__ method", "Cart.size method", "Cart.checkout method", "Cart.Line.cost method", "after func",
	})
	byName := map[string]lexDecl{}
	for _, d := range f.decls {
		byName[d.name] = d
	}
	if got := f.toks[byName["total"].end].line; got != 7 {
		t.Errorf("total ends on line %d, want 7", got)
	}
	if got := f.toks[byName["Cart.checkout"].end].line; got != 19 {
		t.Errorf("Cart.checkout ends on line %d, want 19", got)
	}
	equalLists(t, "calls", callNames(f), []string{"sum", "helpers.tax", "inner", "len", "compute", "total", "self.size", "Cart", ".checkout"})

	tests := parseLexical("tests/test_cart.py", []byte("import unittest\n\ndef test_total():\n    assert total([1]) == 1\n\ndef helper():\n    pass\n\nclass CartTest(unittest.TestCase):\n    def test_size(self):\n        pass\n    def setUp(self):\n        pass\n"))
	equalLists(t, "python tests", declNames(tests), []string{"test_total func test", "helper func", "CartTest.test_size method test", "CartTest.setUp method"})
}

func TestParseRust(t *testing.T) {
	src := `use crate::util;

pub struct Cart { items: Vec<u32> }

impl Cart {
    pub fn new() -> Self { Cart { items: Vec::new() } }
    pub(crate) fn total(&self) -> u32 {
        let f = |x: u32| x + 1;
        self.items.iter().sum::<u32>() + util::tax(1) + Self::fee() + f(1)
    }
    fn fee() -> u32 { helper() }
}

impl<T: Clone> Totaler for Wrapper<T> where T: Fn() -> u32 {
    fn total(&self) -> u32 { 0 }
}

trait Totaler {
    fn total(&self) -> u32;
    fn doubled(&self) -> u32 { self.total() * 2 }
}

fn helper() -> u32 { println!("x"); 1 }

extern "C" { fn ext(); }

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Cart { Cart::new() }

    #[test]
    fn totals() { assert_eq!(fixture().total(), 0); }

    #[tokio::test(flavor = "multi_thread")]
    async fn async_totals() {}
}
`
	f := parseLexical("src/cart.rs", []byte(src))
	var got []string
	for _, d := range f.decls {
		s := d.name + " " + d.kind
		if d.test {
			s += " test"
		} else if d.testCode {
			s += " testcode"
		}
		got = append(got, s)
	}
	equalLists(t, "declarations", got, []string{
		"Cart.new method", "Cart.total method", "Cart.fee method", "Wrapper.total method", "Totaler.doubled method",
		"helper func", "fixture func testcode", "totals func test", "async_totals func test",
	})
	equalLists(t, "calls", callNames(f), []string{"Vec::new", "items.iter", ".sum", "util::tax", "Self::fee", "f", "helper", "self.total", "Cart::new", "fixture", ".total"})
}

// polyRepo commits a base with one source per language and a head that
// changes one function of each.
func polyRepo(t *testing.T) (*repoFixture, string, string) {
	f := newRepo(t)
	f.put("web/src/price.ts", "export function total(items: number[]): number {\n  return items.reduce((a, b) => a + b, 0);\n}\n")
	f.put("web/src/checkout.ts", "import { total } from './price';\n\nexport function checkout(items: number[]) {\n  return total(items) + 1;\n}\n")
	f.put("web/src/checkout.test.ts", "import { checkout } from './checkout';\n\ntest('checkout adds a fee', () => {\n  expect(checkout([1])).toBe(2);\n});\n")
	f.put("py/shop/cart.py", "class Cart:\n    def __init__(self, items):\n        self.items = items\n\n    def total(self):\n        return sum(self.items)\n")
	f.put("py/shop/api.py", "from shop.cart import Cart\n\ndef checkout(items):\n    return Cart(items).total() + 1\n")
	f.put("py/tests/test_api.py", "from shop.api import checkout\n\ndef test_checkout():\n    assert checkout([1]) == 2\n")
	f.put("rs/src/lib.rs", "pub mod price;\n\npub fn checkout(items: &[u32]) -> u32 {\n    price::total(items) + 1\n}\n\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn checks_out() { assert_eq!(super::checkout(&[1]), 2); }\n}\n")
	f.put("rs/src/price.rs", "pub fn total(items: &[u32]) -> u32 {\n    items.iter().sum()\n}\n")
	base := f.commit()
	f.put("web/src/price.ts", "export function total(items: number[]): number {\n  return items.reduce((a, b) => a + b, 1);\n}\n")
	f.put("py/shop/cart.py", "class Cart:\n    def __init__(self, items):\n        self.items = items\n\n    def total(self):\n        return sum(self.items) - 1\n")
	f.put("rs/src/price.rs", "pub fn total(items: &[u32], fee: u32) -> u32 {\n    items.iter().sum::<u32>() + fee\n}\n")
	head := f.commit()
	return f, base, head
}

func TestAnalyzeLexicalLanguages(t *testing.T) {
	f, base, head := polyRepo(t)
	res, _ := f.analyze(base, head, Options{})
	im := res.Report()
	if im.Status != model.ImpactIndexed {
		t.Fatalf("status = %s (%s), want indexed", im.Status, im.Reason)
	}
	equalLists(t, "languages", im.Languages, []string{LangTypeScript, LangPython, LangRust})
	if !strings.Contains(im.Note, model.ImpactNoteLexical) {
		t.Errorf("the note does not describe the lexical index: %q", im.Note)
	}
	if im.IndexedFiles != 8 {
		t.Errorf("indexed files = %d, want 8", im.IndexedFiles)
	}
	type want struct {
		symbol, change, caller, test string
	}
	for _, w := range []want{
		{"web/src/price.ts.total", model.ChangeBodyChanged, "web/src/checkout.ts.checkout", "checkout adds a fee"},
		{"py/shop/cart.py.Cart.total", model.ChangeBodyChanged, "py/shop/api.py.checkout", "test_checkout"},
		{"rs/src/price.rs.total", model.ChangeSignatureChanged, "rs/src/lib.rs.checkout", "checks_out"},
	} {
		fn := findFunction(t, im, w.symbol)
		if !fn.Indexed || fn.Change != w.change {
			t.Errorf("%s: indexed %v change %s (%s)", w.symbol, fn.Indexed, fn.Change, fn.Reason)
			continue
		}
		if len(fn.Callers) != 1 || fn.Callers[0].Symbol != w.caller || fn.Callers[0].Resolution != model.ResolutionName {
			t.Errorf("%s: callers %+v, want %s by name", w.symbol, fn.Callers, w.caller)
		}
		if len(fn.Tests) != 1 || fn.Tests[0].Name != w.test || fn.Tests[0].Depth != 2 || fn.Tests[0].Resolution != model.ResolutionName {
			t.Errorf("%s: tests %+v, want %s at depth 2", w.symbol, fn.Tests, w.test)
		}
	}
	var kinds []string
	for _, s := range res.Signals() {
		kinds = append(kinds, s.Kind+" "+s.Path)
		if s.Kind == model.SignalImpactedCaller && !strings.Contains(s.Evidence, "Lexical index") {
			t.Errorf("signal evidence does not name the lexical index: %s", s.Evidence)
		}
	}
	sort.Strings(kinds)
	equalLists(t, "signals", kinds, []string{"impacted_caller py/shop/api.py", "impacted_caller rs/src/lib.rs", "impacted_caller web/src/checkout.ts"})

	// The reviewer tools answer from the same index.
	out, found, err := res.Index().Query(context.Background(), "find_callers", "Cart.total", 2)
	if err != nil || !found {
		t.Fatalf("find_callers Cart.total: found %v, %v", found, err)
	}
	m := out.(map[string]any)
	if m["method"] != MethodLexical || m["limitations"] != LimitationsLexical {
		t.Errorf("method %v", m["method"])
	}
	callers := m["callers"].([]map[string]any)
	if len(callers) != 2 || callers[0]["caller"] != "py/shop/api.py.checkout" || callers[1]["caller"] != "py/tests/test_api.py.test_checkout" || callers[1]["test"] != true || callers[0]["resolution"] != "name" {
		t.Errorf("callers = %v", callers)
	}
}

func TestAnalyzeMixedGoAndLexical(t *testing.T) {
	f, base, head := shopRepo(t)
	f.put("tools/report.py", "def render(total):\n    return str(total)\n\ndef main():\n    return render(1)\n")
	head2base := f.commit()
	f.put("tools/report.py", "def render(total):\n    return repr(total)\n\ndef main():\n    return render(1)\n")
	f.put("price/price.go", shopPrice)
	head2 := f.commit()
	_ = head
	res, _ := f.analyze(head2base, head2, Options{})
	im := res.Report()
	if im.Status != model.ImpactIndexed {
		t.Fatalf("status = %s (%s)", im.Status, im.Reason)
	}
	equalLists(t, "languages", im.Languages, []string{LangGo, LangPython})
	goFn := findFunction(t, im, "example.test/shop/price.Total")
	if !goFn.Indexed || len(goFn.Callers) != 1 || goFn.Callers[0].Resolution != model.ResolutionStatic {
		t.Errorf("Go function: %+v", goFn)
	}
	pyFn := findFunction(t, im, "tools/report.py.render")
	if !pyFn.Indexed || len(pyFn.Callers) != 1 || pyFn.Callers[0].Symbol != "tools/report.py.main" {
		t.Errorf("Python function: %+v", pyFn)
	}
	_ = base
}

func TestAnalyzeLexicalWithoutGoModule(t *testing.T) {
	// A Go file without go.mod leaves the Go index unavailable; the lexical
	// part is still built and the section is limited.
	f := newRepo(t)
	f.put("scripts/gen.go", "package main\n\nfunc gen() int { return 1 }\n")
	f.put("app/util.py", "def f():\n    return 1\n\ndef g():\n    return f()\n")
	base := f.commit()
	f.put("scripts/gen.go", "package main\n\nfunc gen() int { return 2 }\n")
	f.put("app/util.py", "def f():\n    return 2\n\ndef g():\n    return f()\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	im := res.Report()
	if im.Status != model.ImpactLimited || !strings.Contains(im.Reason, "the static Go index is unavailable: no go.mod file") {
		t.Fatalf("status = %s (%s), want limited by the Go index", im.Status, im.Reason)
	}
	goFn := findFunction(t, im, "main.gen")
	if goFn.Indexed || !strings.Contains(goFn.Reason, "static Go index is unavailable") {
		t.Errorf("Go function: %+v", goFn)
	}
	pyFn := findFunction(t, im, "app/util.py.f")
	if !pyFn.Indexed || pyFn.CallersTotal != 1 {
		t.Errorf("Python function: %+v", pyFn)
	}
}

func TestLexicalResolutionRules(t *testing.T) {
	f := newRepo(t)
	// Two classes with a method "total": self.total() resolves to the own
	// class only, and a common method name on an unknown value is not linked.
	f.put("m/a.py", "class A:\n    def total(self):\n        return 1\n    def twice(self):\n        return self.total() * 2\n\n    def get(self):\n        return 0\n")
	f.put("m/b.py", "class B:\n    def total(self):\n        return 2\n\ndef use(x, d):\n    return x.total() + d.get('k')\n")
	base := f.commit()
	f.put("m/a.py", "class A:\n    def total(self):\n        return 3\n    def twice(self):\n        return self.total() * 2\n\n    def get(self):\n        return 1\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	im := res.Report()
	fn := findFunction(t, im, "m/a.py.A.total")
	var callers []string
	for _, c := range fn.Callers {
		callers = append(callers, c.Symbol)
	}
	// twice is part of the change? No: only total and get changed.
	equalLists(t, "A.total callers", callers, []string{"m/a.py.A.twice", "m/b.py.use"})
	get := findFunction(t, im, "m/a.py.A.get")
	if get.CallersTotal != 0 {
		t.Errorf("a common method name was linked: %+v", get.Callers)
	}
	out, found, _ := res.Index().Query(context.Background(), "find_references", "m/a.py.A.get", 0)
	if !found || out.(map[string]any)["name_matches_total"] != 1 {
		t.Errorf("the unlinked call is not a name match: %v", out)
	}
}

func TestLexicalOnlyTestFileChangeIsIndexed(t *testing.T) {
	f := newRepo(t)
	f.put("src/a.ts", "export function a() { return 1; }\n")
	f.put("src/a.test.ts", "test('a', () => { a(); });\n")
	base := f.commit()
	f.put("src/a.test.ts", "test('a', () => { a(); a(); });\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	if im := res.Report(); im.Status != model.ImpactIndexed || len(im.ChangedFunctions) != 0 || im.IndexedFiles != 2 {
		t.Errorf("section = %+v", im)
	}
}

func TestNotApplicableWithoutIndexableSource(t *testing.T) {
	f := newRepo(t)
	f.put("README.md", "a\n")
	f.put("node_modules/x/index.js", "function x() {}\n")
	base := f.commit()
	f.put("README.md", "b\n")
	f.put("node_modules/x/index.js", "function x() { return 1; }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	if im := res.Report(); im.Status != model.ImpactNotApplicable || im.Reason != NotApplicableReason {
		t.Errorf("section = %+v", im)
	}
}

func FuzzTokenize(f *testing.F) {
	for _, s := range []string{"a(`${b(`c${d}`)}`)", "r#\"x\"# 'a' '\\u{1}' b'x'", "'''x\n''' f\"{y}\" \\\n z", "/[/]/.test(x) / 2", "/* /* */ */"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		for _, p := range []string{"a.ts", "a.py", "src/a.rs"} {
			lf := parseLexical(p, []byte(src))
			for _, d := range lf.decls {
				if d.start > d.bodyStart || d.bodyStart > d.end || d.end >= len(lf.toks) {
					t.Fatalf("%s: bad span %+v", p, d)
				}
			}
		}
	})
}
