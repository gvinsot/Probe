package symbols

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/gitrepo"
	"github.com/gvinsot/Probe/app/internal/model"
)

// open returns the repository and the exact comparison base..head.
func (f *repoFixture) open(base, head string) (*gitrepo.Repository, model.Change) {
	f.t.Helper()
	repo, err := gitrepo.Open(context.Background(), f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	change, err := repo.Analyze(context.Background(), base, head, false)
	if err != nil {
		f.t.Fatal(err)
	}
	return repo, change
}

// //line directives written by the candidate never move a recorded position:
// sites, declarations and changed functions keep their real repository path
// and line. A directive naming an absolute path, a relative path or a _test.go
// file does not relocate or hide a caller, a comment-only directive added
// above an unchanged caller does not hide it, and a reference on added lines
// stays excluded even when a directive names another file.
func TestLineDirectivesDoNotMovePositions(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", "module example.test/ld\n\ngo 1.23\n")
	f.put("lib/lib.go", "package lib\n\nfunc F() int { return 1 }\n")
	f.put("use/gen.go", "package use\n\nimport \"example.test/ld/lib\"\n\n//line /etc/passwd:1\nfunc Gen() int { return lib.F() }\n")
	f.put("use/hide.go", "package use\n\nimport \"example.test/ld/lib\"\n\n//line hidden_test.go:1\nfunc Hide() int { return lib.F() }\n")
	f.put("use/use.go", "package use\n\nimport \"example.test/ld/lib\"\n\n// Genuine is an unchanged caller.\nfunc Genuine() int {\n\treturn lib.F()\n}\n")
	f.put("other/other.go", "package other\n\nfunc Nothing() {}\n")
	base := f.commit()
	// The changed function sits under a directive naming line 900 of another
	// file; an added init calls it under a directive naming other/other.go.
	f.put("lib/lib.go", "package lib\n\n//line price.y:900\nfunc F() int { return 2 }\n\nfunc init() {\n//line other/other.go:3\n\t_ = F()\n}\n")
	// Only a directive comment is added above the unchanged caller.
	f.put("use/use.go", "package use\n\nimport \"example.test/ld/lib\"\n\n//line use/hidden_test.go:1\n// Genuine is an unchanged caller.\nfunc Genuine() int {\n\treturn lib.F()\n}\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactIndexed || len(impact.ChangedFunctions) != 1 {
		t.Fatalf("%+v", impact)
	}
	fn := findFunction(t, impact, "example.test/ld/lib.F")
	if fn.Path != "lib/lib.go" || fn.Line != 4 || fn.EndLine != 4 {
		t.Fatalf("changed function moved: %+v", fn)
	}
	var got []string
	for _, c := range fn.Callers {
		got = append(got, fmt.Sprintf("%s:%d:%s", c.Path, c.Line, c.Symbol))
	}
	want := "use/gen.go:6:example.test/ld/use.Gen use/hide.go:6:example.test/ld/use.Hide use/use.go:8:example.test/ld/use.Genuine"
	if strings.Join(got, " ") != want || fn.CallersTotal != 3 {
		t.Fatalf("callers %q (total %d), want %q", got, fn.CallersTotal, want)
	}
	var signalled []string
	for _, s := range signalsOf(res.Signals(), model.SignalImpactedCaller) {
		signalled = append(signalled, fmt.Sprintf("%s:%d", s.Path, s.Line))
	}
	if strings.Join(signalled, " ") != "use/gen.go:6 use/hide.go:6 use/use.go:8" {
		t.Fatalf("signals %q", signalled)
	}
	x := res.Index()
	if d, ok := x.Lookup("example.test/ld/use.Gen"); !ok || d.Path != "use/gen.go" || d.Line != 6 {
		t.Fatalf("declaration moved: %+v", d)
	}
	for _, p := range x.files {
		if p != "lib/lib.go" && p != "other/other.go" && !strings.HasPrefix(p, "use/") {
			t.Fatalf("index records a file named by a directive: %q in %q", p, x.files)
		}
	}
	out, _, err := x.Query(context.Background(), "find_references", "lib.F", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(out)
	for _, bad := range []string{"passwd", "hidden_test", "price.y", "other/other.go"} {
		if strings.Contains(string(data), bad) {
			t.Fatalf("find_references names %q: %s", bad, data)
		}
	}
	// The init reference on the added lines is recorded where it is.
	if m := out.(map[string]any); m["references_total"] != 4 {
		t.Fatalf("references %s", data)
	}
}

// A method declared on an alias receiver is keyed by the aliased type; the
// changed method is found at its position and searched, and a reference on an
// unchanged line inside it belongs to the change, not to its callers.
func TestAliasReceiverChangedMethod(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("cart/cart.go", "package cart\n\ntype Cart struct{}\n\ntype C = Cart\n\nfunc (c *C) Total() int {\n\tx := 1\n\treturn x + Helper()\n}\n\nfunc Helper() int { return 1 }\n")
	f.put("use/use.go", "package use\n\nimport \"example.test/shop/cart\"\n\nfunc U() int { return (&cart.Cart{}).Total() }\n\nfunc V() int { return cart.Helper() }\n")
	base := f.commit()
	f.put("cart/cart.go", "package cart\n\ntype Cart struct{}\n\ntype C = Cart\n\nfunc (c *C) Total() int {\n\tx := 2\n\treturn x + Helper()\n}\n\nfunc Helper() int { return 2 }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	fn := findFunction(t, res.Report(), "example.test/shop/cart.Cart.Total")
	if !fn.Indexed || fn.Reason != "" || fn.Line != 7 || fn.CallersTotal != 1 || fn.Callers[0].Path != "use/use.go" {
		t.Fatalf("%+v", fn)
	}
	helper := findFunction(t, res.Report(), "example.test/shop/cart.Helper")
	if helper.CallersTotal != 1 || helper.Callers[0].Path != "use/use.go" || helper.Callers[0].Line != 7 {
		t.Fatalf("a reference inside the changed alias-receiver method is listed as a caller: %+v", helper)
	}
}

// adversarialRepo is the shape that made the impact phase unbounded: n types
// whose Do method calls every changed function, one interface I{Do()} called
// calls times from functions U0..U(k) (100 calls each), and a test reaching
// U0. Every changed function then reaches I.Do through n implementing methods.
// Types and callers are spread over files of 100 declarations.
func adversarialRepo(tb testing.TB, types, changed, calls int) (*repoFixture, string, string) {
	tb.Helper()
	f := &repoFixture{t: tb, dir: tb.TempDir()}
	f.git("init", "-b", "main")
	f.git("config", "core.autocrlf", "false")
	f.put("go.mod", "module example.test/adv\n\ngo 1.23\n")
	var fb, fh strings.Builder
	fb.WriteString("package f\n\n")
	fh.WriteString("package f\n\n")
	for j := 0; j < changed; j++ {
		fmt.Fprintf(&fb, "func F%d() int { return 1 }\n", j)
		fmt.Fprintf(&fh, "func F%d() int { return 2 }\n", j)
	}
	f.put("f/f.go", fb.String())
	f.put("iface/iface.go", "package iface\n\ntype I interface{ Do() }\n")
	for file := 0; file*100 < types; file++ {
		var impl strings.Builder
		impl.WriteString("package impl\n\nimport \"example.test/adv/f\"\n\n")
		for i := file * 100; i < (file+1)*100 && i < types; i++ {
			fmt.Fprintf(&impl, "type T%d struct{}\n\nfunc (T%d) Do() {\n", i, i)
			for j := 0; j < changed; j++ {
				fmt.Fprintf(&impl, "\t_ = f.F%d()\n", j)
			}
			impl.WriteString("}\n\n")
		}
		f.put(fmt.Sprintf("impl/impl%d.go", file), impl.String())
	}
	funcs := (calls + 99) / 100
	for file := 0; file*100 < funcs; file++ {
		var use strings.Builder
		use.WriteString("package use\n\nimport \"example.test/adv/iface\"\n\n")
		for n := file * 100; n < (file+1)*100 && n < funcs; n++ {
			fmt.Fprintf(&use, "func U%d(x iface.I) {\n", n)
			for c := n * 100; c < (n+1)*100 && c < calls; c++ {
				use.WriteString("\tx.Do()\n")
			}
			use.WriteString("}\n\n")
		}
		f.put(fmt.Sprintf("use/use%d.go", file), use.String())
	}
	f.put("use/use_test.go", "package use\n\nimport \"testing\"\n\nfunc TestU(t *testing.T) { U0(nil) }\n")
	base := f.commit()
	f.put("f/f.go", fh.String())
	head := f.commit()
	return f, base, head
}

// BenchmarkImpactSearchAdversarial measures Analyze on the adversarial shape
// at scale: 1,000 implementing types and 200,000 interface calls. Up to 10
// changed functions are searched within the budgets; 200 reach the
// reaching-test visit budget, so the section is limited.
func BenchmarkImpactSearchAdversarial(b *testing.B) {
	for _, changed := range []int{1, 10, 200} {
		b.Run(fmt.Sprintf("changed=%d", changed), func(b *testing.B) {
			f, base, head := adversarialRepo(b, 1000, changed, 200000)
			repo, change := f.open(base, head)
			want := model.ImpactIndexed
			if changed == 200 {
				want = model.ImpactLimited
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := Analyze(context.Background(), repo, change, Options{})
				if err != nil || res.Report().Status != want {
					b.Fatalf("%v %s %q", err, res.Report().Status, res.Report().Reason)
				}
			}
		})
	}
}

// The references of an interface method are followed once per search, so the
// work of one search is bounded by the index size, not by the number of
// implementing declarations times the interface's references. The results are
// those of a search that expanded the interface method from every
// implementing declaration.
func TestInterfaceReferencesFollowedOncePerSearch(t *testing.T) {
	f, base, head := adversarialRepo(t, 50, 5, 2000)
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactIndexed || impact.Reason != "" || len(impact.ChangedFunctions) != 5 {
		t.Fatalf("%+v", impact)
	}
	for _, fn := range impact.ChangedFunctions {
		if fn.Reason != "" || fn.CallersTotal != 50 || fn.TestsTotal != 1 || fn.Tests[0].Name != "TestU" || fn.Tests[0].Depth != 3 || fn.Tests[0].Resolution != model.ResolutionInterface {
			t.Fatalf("%+v", fn)
		}
	}
	x := res.Index()
	id := x.byKey["example.test/adv/f.F0"]
	const plenty = int64(1) << 40
	bud := newBudget(context.Background(), time.Time{}, plenty)
	tests, total, ok, capped := x.reachingTests(id, modules{}, nil, bud)
	spent := plenty - bud.left
	// 50 references of F0, 2,000 of I.Do, one of U0 and 50 interface checks:
	// expanding I.Do from each of the 50 methods would cost over 100,000.
	if !ok || capped || total != 1 || len(tests) != 1 || spent > 2200 {
		t.Fatalf("ok %v capped %v total %d spent %d", ok, capped, total, spent)
	}
	// find_callers follows I.Do once too: 50 + 2,000 + 1 sites, not truncated
	// by the visit bound.
	m := query(t, x, "find_callers", "f.F0", 3)
	if m["callers_total"] != float64(2051) {
		t.Fatalf("callers_total %v", m["callers_total"])
	}
}

// A search that reaches its visit budget stops, keeps what it found, and
// makes the section limited with a reason and the analysis_limited signal.
func TestImpactSearchVisitLimit(t *testing.T) {
	f, base, head := adversarialRepo(t, 50, 5, 2000)
	res, _ := f.analyze(base, head, Options{Limits: Limits{MaxSearchVisits: 3000}})
	impact := res.Report()
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "the reaching-test search stopped at its limit of 3000 visits; reaching tests of 4 changed functions may be missing") || strings.Contains(impact.Reason, "caller search") {
		t.Fatalf("status %q reason %q", impact.Status, impact.Reason)
	}
	first := impact.ChangedFunctions[0]
	if first.Reason != "" || first.TestsTotal != 1 || first.CallersTotal != 50 {
		t.Fatalf("the first function was searched within the budget: %+v", first)
	}
	for _, fn := range impact.ChangedFunctions[1:] {
		if !fn.Indexed || fn.Reason != reasonTestsStopped || fn.CallersTotal != 50 {
			t.Fatalf("%+v", fn)
		}
	}
	limited := signalsOf(res.Signals(), model.SignalAnalysisLimited)
	if len(limited) != 1 || !strings.Contains(limited[0].Evidence, "is limited") {
		t.Fatalf("signals %+v", res.Signals())
	}
	checkWording(t, "reason", impact.Reason)
	checkWording(t, "function reason", impact.ChangedFunctions[1].Reason)

	// A budget too small for the callers stops the caller search as well.
	res, _ = f.analyze(base, head, Options{Limits: Limits{MaxSearchVisits: 120}})
	// 50 references for each of the first two functions; the third stops.
	if r := res.Report().Reason; !strings.Contains(r, "the caller search stopped at its limit of 120 visits; callers of 3 changed functions may be missing") {
		t.Fatalf("reason %q", r)
	}
}

// The analysis deadline also stops the impact searches: functions not
// searched in time are reported, and a cancelled context ends Analyze with
// its error.
func TestImpactSearchDeadlineAndCancel(t *testing.T) {
	f, base, head := shopRepo(t)
	searchDeadlineHook = func(time.Time) time.Time { return time.Now().Add(-time.Second) }
	defer func() { searchDeadlineHook = nil }()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "the caller search stopped at the index time limit (2m0s); callers of 2 changed functions may be missing") || !strings.Contains(impact.Reason, "the reaching-test search stopped at the index time limit (2m0s)") {
		t.Fatalf("status %q reason %q", impact.Status, impact.Reason)
	}
	for _, fn := range impact.ChangedFunctions {
		if !fn.Indexed || fn.CallersTotal != 0 || fn.TestsTotal != 0 || !strings.Contains(fn.Reason, reasonCallersStopped) || !strings.Contains(fn.Reason, reasonTestsStopped) {
			t.Fatalf("%+v", fn)
		}
	}
	if len(signalsOf(res.Signals(), model.SignalImpactedCaller)) != 0 || len(signalsOf(res.Signals(), model.SignalAnalysisLimited)) != 1 {
		t.Fatalf("signals %+v", res.Signals())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	searchDeadlineHook = func(d time.Time) time.Time { cancel(); return d }
	repo, change := f.open(base, head)
	if _, err := Analyze(ctx, repo, change, Options{}); err != context.Canceled {
		t.Fatalf("a search cancelled by its context returned %v", err)
	}
}

// Budgets stop at their deadline, at their context and at their units.
func TestBudget(t *testing.T) {
	if b := newBudget(context.Background(), time.Now().Add(-time.Second), 100); b.ok() || !b.timedOut || b.spend(1) {
		t.Fatalf("%+v", b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if b := newBudget(ctx, time.Time{}, 100); b.ok() || !b.cancelled {
		t.Fatalf("%+v", b)
	}
	b := newBudget(context.Background(), time.Time{}, 10)
	if !b.spend(10) || b.spend(1) || !b.stopped || b.timedOut || b.cancelled {
		t.Fatalf("%+v", b)
	}
	// The deadline is noticed within budgetCheckEvery units.
	b = newBudget(context.Background(), time.Now().Add(-time.Second), 1<<20)
	b.sinceCheck = 0
	stoppedAt := -1
	for i := 0; i < 2*budgetCheckEvery; i++ {
		if !b.spend(1) {
			stoppedAt = i
			break
		}
	}
	if stoppedAt < 0 || stoppedAt >= budgetCheckEvery || !b.timedOut {
		t.Fatalf("stopped at %d: %+v", stoppedAt, b)
	}
}

// More interface methods of one name than are checked: the lookup reports it,
// the section is limited, and tool answers are marked truncated.
func TestImplementersCapReported(t *testing.T) {
	defer func(n int) { maxImplementCandidates = n }(maxImplementCandidates)
	maxImplementCandidates = 2
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("cart/cart.go", shopCart)
	var b strings.Builder
	b.WriteString("package ifaces\n\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, "type A%d interface{ Total(x%d) int }\ntype x%d struct{}\n", i, i, i)
	}
	f.put("ifaces/ifaces.go", b.String())
	f.put("zz/zz.go", "package zz\n\ntype ZTotaler interface{ Total() int }\n\nfunc Use(t ZTotaler) int { return t.Total() }\n")
	base := f.commit()
	f.put("cart/cart.go", shopCartNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	fn := findFunction(t, impact, "example.test/shop/cart.Cart.Total")
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "more than 2 interface methods share a method name the search met") || !strings.Contains(fn.Reason, reasonImplCapped) {
		t.Fatalf("status %q reason %q function %+v", impact.Status, impact.Reason, fn)
	}
	if len(signalsOf(res.Signals(), model.SignalAnalysisLimited)) != 1 {
		t.Fatalf("signals %+v", res.Signals())
	}
	m := query(t, res.Index(), "find_references", "cart.Cart.Total", 0)
	if m["truncated"] != true || m["truncated_note"] != truncatedNote {
		t.Fatalf("%+v", m)
	}
	checkWording(t, "truncated note", truncatedNote)

	// With every candidate checked, the interface call in zz is found.
	maxImplementCandidates = 200
	res, _ = f.analyze(base, head, Options{})
	fn = findFunction(t, res.Report(), "example.test/shop/cart.Cart.Total")
	if res.Report().Status != model.ImpactIndexed || fn.CallersTotal != 1 || fn.Callers[0].Path != "zz/zz.go" {
		t.Fatalf("%+v %+v", res.Report(), fn)
	}
}

// The reaching-test search reports its declaration cap.
func TestReachCapReported(t *testing.T) {
	defer func(n int) { maxReachVisits = n }(maxReachVisits)
	maxReachVisits = 3
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("chain/chain.go", "package chain\n\nfunc Core() int { return 1 }\n\nfunc L1() int { return Core() }\n\nfunc L2() int { return L1() }\n\nfunc L3() int { return L2() }\n")
	f.put("chain/chain_test.go", "package chain\n\nimport \"testing\"\n\nfunc TestD3(t *testing.T) { _ = L2() }\n")
	base := f.commit()
	f.put("chain/chain.go", "package chain\n\nfunc Core() int { return 2 }\n\nfunc L1() int { return Core() }\n\nfunc L2() int { return L1() }\n\nfunc L3() int { return L2() }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	impact := res.Report()
	fn := findFunction(t, impact, "example.test/shop/chain.Core")
	if impact.Status != model.ImpactLimited || !strings.Contains(impact.Reason, "the reaching-test search of 1 changed functions stopped after 3 declarations") || fn.Reason != reasonReachCapped || fn.TestsTotal != 0 {
		t.Fatalf("status %q reason %q function %+v", impact.Status, impact.Reason, fn)
	}
	m := query(t, res.Index(), "inspect_symbol", "chain.Core", 0)
	if m["truncated"] != true {
		t.Fatalf("%+v", m)
	}
}

// Snippets come from the whole-file redaction, as read_file and search show
// files: a credential whose key and value sit on different lines is masked.
func TestSnippetsUseWholeFileRedaction(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", "module example.test/red\n\ngo 1.23\n")
	f.put("lib/lib.go", "package lib\n\nfunc Suffix() string { return \"a\" }\n")
	f.put("cfg/cfg.go", "package cfg\n\nimport \"example.test/red/lib\"\n\ntype Config struct{ Password string }\n\nvar C = Config{\n\tPassword:\n\t\t\"hunter2hunter2\" + lib.Suffix(),\n}\n")
	base := f.commit()
	f.put("lib/lib.go", "package lib\n\nfunc Suffix() string { return \"b\" }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	m := query(t, res.Index(), "find_references", "lib.Suffix", 0)
	refs := m["references"].([]any)
	if len(refs) != 1 {
		t.Fatalf("%+v", m)
	}
	ref := refs[0].(map[string]any)
	if ref["path"] != "cfg/cfg.go" || ref["line"] != float64(9) || ref["content"] != "\" + lib.Suffix()," {
		t.Fatalf("snippet %+v", ref)
	}
	data, _ := json.Marshal(m)
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("leaked: %s", data)
	}
}

// Many go.mod files repeating one module path give one counted reason, and
// impact.reason stays bounded.
func TestDuplicateModulesOneReason(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	for i := 0; i < 6; i++ {
		f.put(fmt.Sprintf("zcopy%d/go.mod", i), shopMod)
	}
	base := f.commit()
	f.put("price/price.go", shopPriceNew)
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	want := `6 go.mod files repeat the module path of another go.mod and were not indexed (first: "zcopy0", "zcopy1", "zcopy2")`
	if r := res.Report().Reason; res.Report().Status != model.ImpactLimited || r != want {
		t.Fatalf("status %q reason %q", res.Report().Status, r)
	}
	long := make([]string, 2000)
	for i := range long {
		long[i] = fmt.Sprintf("reason %d with some words", i)
	}
	if n := len(joinReasons(long)); n > maxReasonBytes {
		t.Fatalf("joined reasons %d bytes", n)
	}
}

// A change touching only Go files the index never reads is not_applicable,
// and the reason says which files those are.
func TestOnlyUnindexedGoFileChangedIsNotApplicable(t *testing.T) {
	f := newRepo(t)
	f.put("go.mod", shopMod)
	f.put("price/price.go", shopPrice)
	f.put("price/testdata/fixture.go", "package fixture\n\nfunc F() int { return 1 }\n")
	base := f.commit()
	f.put("price/testdata/fixture.go", "package fixture\n\nfunc F() int { return 2 }\n")
	head := f.commit()
	res, _ := f.analyze(base, head, Options{})
	if res.Report().Status != model.ImpactNotApplicable || res.Report().Reason != NotApplicableReason || res.Index() != nil {
		t.Fatalf("%+v", res.Report())
	}
	checkWording(t, "reason", NotApplicableReason)
}
