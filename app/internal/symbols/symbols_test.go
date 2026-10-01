package symbols

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
)

// repoFixture is a real temporary Git repository.
type repoFixture struct {
	t   testing.TB
	dir string
}

func newRepo(t *testing.T) *repoFixture {
	t.Helper()
	f := &repoFixture{t: t, dir: t.TempDir()}
	f.git("init", "-b", "main")
	f.git("config", "core.autocrlf", "false")
	return f
}

func (f *repoFixture) git(args ...string) string {
	f.t.Helper()
	c := exec.Command("git", append([]string{"-C", f.dir, "-c", "user.name=Probe Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	b, err := c.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func (f *repoFixture) put(p, content string) {
	f.t.Helper()
	target := filepath.Join(f.dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *repoFixture) commit() string {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "--allow-empty", "-m", "test")
	return f.git("rev-parse", "HEAD")
}

// analyze runs Analyze on the exact comparison base..head.
func (f *repoFixture) analyze(base, head string, opts Options) (*Result, model.Change) {
	f.t.Helper()
	repo, err := gitrepo.Open(context.Background(), f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	change, err := repo.Analyze(context.Background(), base, head, false)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := Analyze(context.Background(), repo, change, opts)
	if err != nil {
		f.t.Fatal(err)
	}
	if res.Report() == nil {
		f.t.Fatal("Analyze returned no section")
	}
	return res, change
}

// The shop fixture of the F6 design: price.Total is called by api.Checkout,
// cart.Cart.Total may be reached through the Totaler interface from notify.
const (
	shopMod        = "module example.test/shop\n\ngo 1.23\n"
	shopPrice      = "package price\n\n// Total sums the items.\nfunc Total(items []int) int {\n\tsum := 0\n\tfor _, v := range items {\n\t\tsum += v\n\t}\n\treturn sum\n}\n"
	shopPriceNew   = "package price\n\n// Total sums the items.\nfunc Total(items []int) int {\n\tsum := 0\n\tfor _, v := range items[1:] {\n\t\tsum += v\n\t}\n\treturn sum\n}\n"
	shopPriceTest  = "package price\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif Total([]int{1, 2, 3}) != 6 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"
	shopHandler    = "package api\n\nimport \"example.test/shop/price\"\n\n// Checkout charges the items.\nfunc Checkout(items []int) int {\n\treturn price.Total(items) + 1\n}\n"
	shopHandlerTst = "package api\n\nimport \"testing\"\n\nfunc TestCheckout(t *testing.T) {\n\tif Checkout([]int{1}) != 2 {\n\t\tt.Fatal(\"checkout\")\n\t}\n}\n"
	shopCart       = "package cart\n\n// Cart holds items.\ntype Cart struct{ Items []int }\n\n// Total sums the cart.\nfunc (c *Cart) Total() int {\n\tn := 0\n\tfor _, v := range c.Items {\n\t\tn += v\n\t}\n\treturn n\n}\n\n// Totaler has a total.\ntype Totaler interface{ Total() int }\n"
	shopCartNew    = "package cart\n\n// Cart holds items.\ntype Cart struct{ Items []int }\n\n// Total sums the cart.\nfunc (c *Cart) Total() int {\n\tn := 1\n\tfor _, v := range c.Items {\n\t\tn += v\n\t}\n\treturn n\n}\n\n// Totaler has a total.\ntype Totaler interface{ Total() int }\n"
	shopNotify     = "package notify\n\nimport \"example.test/shop/cart\"\n\n// Message reports a total.\nfunc Message(t cart.Totaler) int {\n\treturn t.Total()\n}\n"
)

func shopRepo(t *testing.T) (*repoFixture, string, string) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	f.put("price/price_test.go", shopPriceTest)
	f.put("api/handler.go", shopHandler)
	f.put("api/handler_test.go", shopHandlerTst)
	f.put("cart/cart.go", shopCart)
	f.put("notify/notify.go", shopNotify)
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	f.put("cart/cart.go", shopCartNew)
	head := f.commit()
	return f, base, head
}

func findFunction(t *testing.T, impact *model.Impact, symbol string) model.ImpactFunction {
	t.Helper()
	for _, fn := range impact.ChangedFunctions {
		if fn.Symbol == symbol {
			return fn
		}
	}
	t.Fatalf("changed function %s not listed: %+v", symbol, impact.ChangedFunctions)
	return model.ImpactFunction{}
}

func signalsOf(signals []model.Signal, kind string) []model.Signal {
	var out []model.Signal
	for _, s := range signals {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

func TestShopImpact(t *testing.T) {
	f, base, head := shopRepo(t)
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactIndexed || impact.Reason != "" || impact.IndexedFiles != 6 {
		t.Fatalf("status %q reason %q files %d", impact.Status, impact.Reason, impact.IndexedFiles)
	}
	if len(impact.ChangedFunctions) != 2 {
		t.Fatalf("changed functions %+v", impact.ChangedFunctions)
	}
	total := findFunction(t, impact, "example.test/shop/price.Total")
	if total.Change != model.ChangeBodyChanged || !total.Indexed || total.Path != "price/price.go" || total.Line != 4 || total.EndLine != 10 {
		t.Fatalf("price.Total %+v", total)
	}
	if total.CallersTotal != 1 || len(total.Callers) != 1 || total.Callers[0] != (model.ImpactCaller{Path: "api/handler.go", Line: 7, Symbol: "example.test/shop/api.Checkout", Depth: 1, Resolution: model.ResolutionStatic}) {
		t.Fatalf("price.Total callers %+v", total.Callers)
	}
	if total.TestsTotal != 2 || len(total.Tests) != 2 {
		t.Fatalf("price.Total tests %+v", total.Tests)
	}
	first, second := total.Tests[0], total.Tests[1]
	if first.Name != "TestTotal" || first.Depth != 1 || first.Path != "price/price_test.go" || first.Line != 5 || first.Package != "example.test/shop/price" || first.Resolution != model.ResolutionStatic || first.FileChanged {
		t.Fatalf("TestTotal %+v", first)
	}
	if second.Name != "TestCheckout" || second.Depth != 2 || second.Package != "example.test/shop/api" || strings.Join(second.Via, " > ") != "example.test/shop/api.TestCheckout > example.test/shop/api.Checkout > example.test/shop/price.Total" {
		t.Fatalf("TestCheckout %+v", second)
	}
	cartTotal := findFunction(t, impact, "example.test/shop/cart.Cart.Total")
	if cartTotal.CallersTotal != 1 || cartTotal.Callers[0] != (model.ImpactCaller{Path: "notify/notify.go", Line: 7, Symbol: "example.test/shop/notify.Message", Depth: 1, Resolution: model.ResolutionInterface}) {
		t.Fatalf("Cart.Total callers %+v", cartTotal.Callers)
	}
	callers := signalsOf(res.Signals(), model.SignalImpactedCaller)
	if len(callers) != 2 || len(signalsOf(res.Signals(), model.SignalAnalysisLimited)) != 0 {
		t.Fatalf("signals %+v", res.Signals())
	}
	for _, s := range callers {
		if s.Severity != "low" || s.Side != "new" {
			t.Fatalf("signal %+v", s)
		}
		switch s.Path {
		case "api/handler.go":
			if s.Line != 7 || s.Symbol != "example.test/shop/price.Total" || s.Summary != "Unchanged caller of a changed Go function" {
				t.Fatalf("static signal %+v", s)
			}
		case "notify/notify.go":
			if s.Line != 7 || s.Symbol != "example.test/shop/cart.Cart.Total" || s.Summary != "Unchanged interface call that may dispatch to a changed Go method" || !strings.Contains(s.Evidence, "possible, not established") {
				t.Fatalf("interface signal %+v", s)
			}
		default:
			t.Fatalf("unexpected signal %+v", s)
		}
	}
	if res.Index() == nil {
		t.Fatal("no index for the tools")
	}
}

// Two analyses of the same change are identical, and the JSON is stable.
func TestDeterministicReport(t *testing.T) {
	f, base, head := shopRepo(t)
	a, _ := f.analyze(base, head, Options{})
	b, _ := f.analyze(base, head, Options{})
	ja, _ := json.Marshal(struct {
		I *model.Impact
		S []model.Signal
	}{a.Report(), a.Signals()})
	jb, _ := json.Marshal(struct {
		I *model.Impact
		S []model.Signal
	}{b.Report(), b.Signals()})
	if string(ja) != string(jb) {
		t.Fatalf("analyses differ:\n%s\n%s", ja, jb)
	}
}

func TestNoChangedGoFileIsNotApplicable(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	f.put("README.md", "a\n")
	base := f.commit()
	f.put("README.md", "b\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	if res.Report().Status != model.ImpactNotApplicable || len(res.Signals()) != 0 || res.Index() != nil || res.Report().ChangedFunctions == nil {
		t.Fatalf("%+v %+v", res.Report(), res.Signals())
	}
}

func TestNoGoModUnavailable(t *testing.T) {
	f := newRepo(t)
	f.put("price/price.go", shopPrice)
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactUnavailable || impact.Reason != "no go.mod file in the committed tree" || res.Index() != nil {
		t.Fatalf("%+v", impact)
	}
	// The changed function is still listed, as not searched.
	if len(impact.ChangedFunctions) != 1 || impact.ChangedFunctions[0].Indexed || impact.ChangedFunctions[0].Symbol != "price.Total" || impact.ChangedFunctions[0].Reason == "" {
		t.Fatalf("%+v", impact.ChangedFunctions)
	}
	limited := signalsOf(res.Signals(), model.SignalAnalysisLimited)
	if len(limited) != 1 || limited[0].Symbol != LimitedSymbol || limited[0].Severity != "medium" || limited[0].Path != "price/price.go" || limited[0].Line != 1 || !strings.Contains(limited[0].Evidence, "unavailable") {
		t.Fatalf("signals %+v", res.Signals())
	}
}

// The Probe layout: go.work at the root and the module in app/.
func TestNestedModules(t *testing.T) {
	f := newRepo(t)
	f.put("go.work", "go 1.23.0\n\nuse ./app\nuse ./hub\n")
	f.put("app/go.mod", "module example.test/app\n\ngo 1.23\n")
	f.put("app/internal/price/price.go", shopPrice)
	f.put("app/cmd/tool/main.go", "package main\n\nimport \"example.test/app/internal/price\"\n\nfunc main() { _ = price.Total(nil) }\n")
	f.put("hub/go.mod", "module example.test/hub\n\ngo 1.23\n")
	f.put("hub/hub.go", "package hub\n\nfunc Total() int { return 1 }\n")
	f.put("tools/stray.go", "package tools\n\nfunc Stray() {}\n")
	base := f.commit()
	f.put("app/internal/price/price.go", shopPriceNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactIndexed || impact.IndexedFiles != 3 {
		t.Fatalf("%+v", impact)
	}
	fn := findFunction(t, impact, "example.test/app/internal/price.Total")
	if fn.CallersTotal != 1 || fn.Callers[0].Path != "app/cmd/tool/main.go" || fn.Callers[0].Symbol != "example.test/app/cmd/tool.main" {
		t.Fatalf("%+v", fn)
	}
}

// Signals land only on unchanged code: a caller in a changed function, on an
// added line, or in a test file is part of the change or of the tests.
func TestImpactedCallerSignalsOnlyInUnchangedCode(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	f.put("use/unchanged.go", "package use\n\nimport \"example.test/shop/price\"\n\nfunc A() int { return price.Total(nil) }\n")
	f.put("use/mixed.go", "package use\n\nimport \"example.test/shop/price\"\n\nfunc B() int { return price.Total(nil) }\n\nfunc C() int { return price.Total(nil) + 1 }\n")
	f.put("use/use_test.go", "package use\n\nimport (\n\t\"testing\"\n\n\t\"example.test/shop/price\"\n)\n\nfunc TestUse(t *testing.T) { _ = price.Total(nil) }\n")
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	// C changes (a caller inside a changed function); a new function D adds a call.
	f.put("use/mixed.go", "package use\n\nimport \"example.test/shop/price\"\n\nfunc B() int { return price.Total(nil) }\n\nfunc C() int { return price.Total(nil) + 2 }\n\nfunc D() int { return price.Total(nil) }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	fn := findFunction(t, res.Report(), "example.test/shop/price.Total")
	var got []string
	for _, c := range fn.Callers {
		got = append(got, c.Path+":"+c.Symbol)
	}
	want := "use/mixed.go:example.test/shop/use.B use/unchanged.go:example.test/shop/use.A"
	if strings.Join(got, " ") != want || fn.CallersTotal != 2 {
		t.Fatalf("callers %q, want %q", got, want)
	}
	if n := len(signalsOf(res.Signals(), model.SignalImpactedCaller)); n != 2 {
		t.Fatalf("%d impacted_caller signals: %+v", n, res.Signals())
	}
	c := findFunction(t, res.Report(), "example.test/shop/use.C")
	if c.Change != model.ChangeBodyChanged {
		t.Fatalf("C %+v", c)
	}
	if fn.TestsTotal != 1 || fn.Tests[0].Name != "TestUse" {
		t.Fatalf("tests %+v", fn.Tests)
	}
}

// A method call on a value of an unresolved type is a name match: tool output
// only, never a signal or a caller.
func TestNameMatchNeverSignalled(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("cart/cart.go", shopCart)
	f.put("ext/ext.go", "package ext\n\nimport \"github.com/other/lib\"\n\nfunc Use() int { return lib.Open().Total() }\n")
	base := f.commit()
	f.put("cart/cart.go", shopCartNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	fn := findFunction(t, res.Report(), "example.test/shop/cart.Cart.Total")
	if fn.CallersTotal != 0 || len(res.Signals()) != 0 {
		t.Fatalf("%+v %+v", fn, res.Signals())
	}
	out, found, err := res.Index().Query(context.Background(), "find_references", "cart.Cart.Total", 0)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	m := out.(map[string]any)
	if m["name_matches_total"] != 1 {
		t.Fatalf("%+v", m)
	}
}

// 150 unchanged callers of 15 changed functions give 100 signals (10 per
// function) and one analysis_limited signal stating the remainder.
func TestCallerSignalCaps(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	var lib, lib2, use strings.Builder
	lib.WriteString("package lib\n\n")
	lib2.WriteString("package lib\n\n")
	use.WriteString("package use\n\nimport \"example.test/shop/lib\"\n\n")
	for i := 0; i < 15; i++ {
		name := string(rune('A'+i)) + "fn"
		lib.WriteString("func " + name + "() int { return 1 }\n")
		lib2.WriteString("func " + name + "() int { return 2 }\n")
		for j := 0; j < 10; j++ {
			use.WriteString("func Use" + name + string(rune('a'+j)) + "() int { return lib." + name + "() }\n")
		}
	}
	f.put("lib/lib.go", lib.String())
	f.put("use/use.go", use.String())
	base := f.commit()
	f.put("lib/lib.go", lib2.String())
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	callers := signalsOf(res.Signals(), model.SignalImpactedCaller)
	limited := signalsOf(res.Signals(), model.SignalAnalysisLimited)
	if len(callers) != 100 || len(limited) != 1 {
		t.Fatalf("%d impacted_caller, %d analysis_limited", len(callers), len(limited))
	}
	if limited[0].Symbol != LimitedSymbol || limited[0].Severity != "medium" || !strings.HasPrefix(limited[0].Evidence, "50 further caller sites") || limited[0].Path != "use/use.go" {
		t.Fatalf("limited %+v", limited[0])
	}
	if res.Report().Status != model.ImpactIndexed {
		t.Fatalf("caps do not limit the index itself: %+v", res.Report())
	}
	for _, fn := range res.Report().ChangedFunctions {
		if fn.CallersTotal != 10 || len(fn.Callers) != 10 {
			t.Fatalf("%s: %d callers, %d listed", fn.Symbol, fn.CallersTotal, len(fn.Callers))
		}
	}
}

// One function with 12 callers: 10 signals, 10 listed, callers_total 12, and
// the remainder is stated once.
func TestCallerSignalsPerFunctionCap(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	var use strings.Builder
	use.WriteString("package use\n\nimport \"example.test/shop/price\"\n\n")
	for j := 0; j < 12; j++ {
		use.WriteString("func U" + string(rune('a'+j)) + "() int { return price.Total(nil) }\n")
	}
	f.put("use/use.go", use.String())
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	fn := findFunction(t, res.Report(), "example.test/shop/price.Total")
	limited := signalsOf(res.Signals(), model.SignalAnalysisLimited)
	if fn.CallersTotal != 12 || len(fn.Callers) != 10 || len(signalsOf(res.Signals(), model.SignalImpactedCaller)) != 10 || len(limited) != 1 {
		t.Fatalf("%+v %+v", fn, res.Signals())
	}
	if limited[0].Line != 15 || !strings.HasPrefix(limited[0].Evidence, "2 further caller sites") {
		t.Fatalf("limited anchored at the first unlisted caller: %+v", limited[0])
	}
}

// Reaching tests: depth 1 to 3 are found, depth 4 is not; a test in a changed
// file is marked; the via chain is deterministic.
func TestReachingTestsDepth(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("chain/chain.go", "package chain\n\nfunc Core() int { return 1 }\n\nfunc L1() int { return Core() }\n\nfunc L2() int { return L1() }\n\nfunc L3() int { return L2() }\n")
	f.put("chain/chain_test.go", "package chain\n\nimport \"testing\"\n\nfunc TestD1(t *testing.T) { _ = Core() }\n\nfunc TestD2(t *testing.T) { _ = L1() }\n\nfunc TestD3(t *testing.T) { _ = L2() }\n\nfunc TestD4(t *testing.T) { _ = L3() }\n\nfunc helper() int { return Core() }\n\nfunc TestHelper(t *testing.T) { _ = helper() }\n")
	f.put("chain/ext_test.go", "package chain_test\n\nimport (\n\t\"testing\"\n\n\t\"example.test/shop/chain\"\n)\n\nfunc TestExternal(t *testing.T) { _ = chain.Core() }\n")
	base := f.commit()
	f.put("chain/chain.go", "package chain\n\nfunc Core() int { return 2 }\n\nfunc L1() int { return Core() }\n\nfunc L2() int { return L1() }\n\nfunc L3() int { return L2() }\n")
	f.put("chain/ext_test.go", "package chain_test\n\nimport (\n\t\"testing\"\n\n\t\"example.test/shop/chain\"\n)\n\nfunc TestExternal(t *testing.T) { _ = chain.Core() + 1 }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	fn := findFunction(t, res.Report(), "example.test/shop/chain.Core")
	var got []string
	for _, tst := range fn.Tests {
		got = append(got, tst.Name+"@"+string(rune('0'+tst.Depth)))
		if tst.Name == "TestExternal" && (!tst.FileChanged || tst.Package != "example.test/shop/chain") {
			t.Fatalf("external test %+v", tst)
		}
	}
	want := "TestD1@1 TestExternal@1 TestD2@2 TestHelper@2 TestD3@3"
	if strings.Join(got, " ") != want || fn.TestsTotal != 5 {
		t.Fatalf("tests %q, want %q", got, want)
	}
	// The callers of Core in unchanged non-test code are L1 only.
	if fn.CallersTotal != 1 || fn.Callers[0].Symbol != "example.test/shop/chain.L1" {
		t.Fatalf("%+v", fn.Callers)
	}
}

// Build constraints select linux/amd64 files: a function declared in both
// a_linux.go and a_windows.go is indexed from the linux file, and a change to
// the windows variant only is listed as not indexed.
func TestBuildConstraintsLinuxAmd64(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("osx/a_linux.go", "package osx\n\nfunc OS() string { return \"linux\" }\n")
	f.put("osx/a_windows.go", "package osx\n\nfunc OS() string { return \"windows\" }\n")
	f.put("osx/tagged.go", "//go:build ignore\n\npackage main\n\nfunc main() {}\n")
	f.put("use/use.go", "package use\n\nimport \"example.test/shop/osx\"\n\nfunc Name() string { return osx.OS() }\n")
	base := f.commit()
	f.put("osx/a_windows.go", "package osx\n\nfunc OS() string { return \"win\" }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactIndexed {
		t.Fatalf("%+v", impact)
	}
	fn := findFunction(t, impact, "example.test/shop/osx.OS")
	if fn.Indexed || fn.Path != "osx/a_windows.go" || fn.Reason != "the file is excluded by the linux/amd64 build constraints" || len(res.Signals()) != 0 {
		t.Fatalf("%+v %+v", fn, res.Signals())
	}
	// The linux variant is what the index resolves.
	out, found, _ := res.Index().Query(context.Background(), "find_callers", "osx.OS", 1)
	if !found || out.(map[string]any)["declaration"].(map[string]any)["path"] != "osx/a_linux.go" || out.(map[string]any)["callers_total"] != 1 {
		t.Fatalf("%v %+v", found, out)
	}
}

// Sensitive files and symlinks are neither read nor indexed, and snippets are
// redacted.
func TestSensitiveAndSymlinkNotIndexed(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	f.put(".aws/creds.go", "package aws\n\nimport \"example.test/shop/price\"\n\nfunc Secret() int { return price.Total(nil) }\n")
	f.put("config/credentials/load.go", "package credentials\n\nimport \"example.test/shop/price\"\n\nfunc Load() int { return price.Total(nil) }\n")
	f.put("use/use.go", "package use\n\nimport \"example.test/shop/price\"\n\nfunc Use() int { const password = \"hunter2hunter2\"; _ = password; return price.Total(nil) }\n")
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	f.git("add", "-A")
	// A symlink entry whose target text is not Go source: indexing it would
	// fail to parse and limit the index.
	blob := f.git("hash-object", "-w", "--stdin", "--path", "use/link.go")
	f.git("update-index", "--add", "--cacheinfo", "120000,"+blob+",use/link.go")
	f.git("commit", "-m", "symlink")
	head := f.git("rev-parse", "HEAD")
	res, _ := f.analyze(base, head, Options{Sensitive: func(p string) bool { return strings.Contains(p, "credentials") }})
	if res.Report().Status != model.ImpactIndexed || res.Report().IndexedFiles != 2 {
		t.Fatalf("%+v", res.Report())
	}
	fn := findFunction(t, res.Report(), "example.test/shop/price.Total")
	if fn.CallersTotal != 1 || fn.Callers[0].Path != "use/use.go" {
		t.Fatalf("%+v", fn.Callers)
	}
	out, _, _ := res.Index().Query(context.Background(), "find_references", "price.Total", 0)
	data, _ := json.Marshal(out)
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), ".aws") || strings.Contains(string(data), "credentials") || strings.Contains(string(data), "link.go") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("leaked or unredacted: %s", data)
	}
}

// Tiny limits give an unavailable or limited section with an analysis_limited
// signal, and never a panic.
func TestLimitsYieldLimitedOrUnavailable(t *testing.T) {
	f, base, head := shopRepo(t)
	for _, tc := range []struct {
		name   string
		limits Limits
		status string
		reason string
	}{
		{"file count", Limits{MaxFiles: 3}, model.ImpactUnavailable, "the candidate tree has more than 3 Go and go.mod files"},
		{"bytes", Limits{MaxBytes: 100}, model.ImpactUnavailable, "the Go sources of the candidate tree exceed 100 bytes"},
		{"packages", Limits{MaxPackages: 2}, model.ImpactUnavailable, "the candidate tree has more than 2 Go packages"},
		{"file size", Limits{MaxFileBytes: 150}, model.ImpactLimited, "Go files larger than 150 bytes were not indexed"},
		{"edges", Limits{MaxEdges: 1}, model.ImpactLimited, "the reference limit (1) was reached"},
		{"changed functions", Limits{MaxChangedFunctions: 1}, model.ImpactLimited, "1 further changed functions are not listed (at most 1)"},
		{"time", Limits{Timeout: 1}, model.ImpactLimited, "the index time limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := f.analyze(base, head, Options{Limits: tc.limits})
			impact := res.Report()
			if impact.Status != tc.status || !strings.Contains(impact.Reason, tc.reason) {
				t.Fatalf("status %q reason %q", impact.Status, impact.Reason)
			}
			limited := signalsOf(res.Signals(), model.SignalAnalysisLimited)
			if len(limited) != 1 || limited[0].Severity != "medium" || limited[0].Symbol != LimitedSymbol {
				t.Fatalf("signals %+v", res.Signals())
			}
			if tc.status == model.ImpactUnavailable && (res.Index() != nil || len(signalsOf(res.Signals(), model.SignalImpactedCaller)) != 0) {
				t.Fatal("an unavailable index answered")
			}
			if len(impact.ChangedFunctions) == 0 {
				t.Fatal("changed functions are listed even when the index is not usable")
			}
		})
	}
}

// A file that does not parse, a package clause that does not fit and a
// package that go/types rejects are contained: the rest is indexed.
func TestParseErrorsContained(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	f.put("broken/broken.go", "package broken\n\nfunc (\n")
	f.put("use/use.go", "package use\n\nimport \"example.test/shop/price\"\n\nfunc Use() int { return price.Total(nil) + undefined }\n")
	f.put("use/other.go", "package other\n\nfunc X() {}\n")
	f.put("use/doc.go", "package use\n")
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "1 Go files could not be parsed") || !strings.Contains(impact.Reason, "package clause") {
		t.Fatalf("%+v", impact)
	}
	fn := findFunction(t, impact, "example.test/shop/price.Total")
	if fn.CallersTotal != 1 || fn.Callers[0].Path != "use/use.go" {
		t.Fatalf("type errors lost the caller: %+v", fn)
	}
	if len(signalsOf(res.Signals(), model.SignalAnalysisLimited)) != 1 {
		t.Fatalf("%+v", res.Signals())
	}
}

// Words the impact output must never use about callers, tests or the index
// (contract §5).
var forbiddenWords = []string{"tested", "covers", "verified", "verifies", "safe", "unused", "no callers", "all callers", "complete", "affected", "broken", "regress", "bug", "correct", "%"}

func checkWording(t *testing.T, where, s string) {
	t.Helper()
	lower := strings.ToLower(s)
	for _, w := range forbiddenWords {
		if strings.Contains(lower, w) {
			t.Errorf("%s uses %q: %s", where, w, s)
		}
	}
}

func TestWordingMakesNoCompletenessClaim(t *testing.T) {
	checkWording(t, "note", model.ImpactNote)
	checkWording(t, "limitations", Limitations)
	f, base, head := shopRepo(t)
	for _, limits := range []Limits{{}, {MaxEdges: 1}, {MaxFiles: 2}} {
		res, _ := f.analyze(base, head, Options{Limits: limits})
		checkWording(t, "reason", res.Report().Reason)
		for _, fn := range res.Report().ChangedFunctions {
			checkWording(t, "function reason", fn.Reason)
		}
		for _, s := range res.Signals() {
			checkWording(t, "summary", s.Summary)
			checkWording(t, "evidence", s.Evidence)
		}
		if x := res.Index(); x != nil {
			for _, tool := range []string{"find_references", "find_callers", "inspect_symbol"} {
				out, _, _ := x.Query(context.Background(), tool, "price.Total", 2)
				data, _ := json.Marshal(out)
				// Snippets quote repository code; the fixed texts are checked here.
				m := out.(map[string]any)
				for k, v := range m {
					if s, ok := v.(string); ok && (k == "limitations" || strings.HasSuffix(k, "note")) {
						checkWording(t, tool+" "+k, s)
					}
				}
				if len(data) == 0 {
					t.Fatal("empty response")
				}
			}
		}
	}
}
