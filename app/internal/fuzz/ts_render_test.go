package fuzz

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/harness"
)

// jsTestDeclaration is the rule verifiable generated JS/TS tests follow
// (harness/jstests.go): a top-level test() or it() call at column 0 with a
// static title.
var jsTestDeclaration = regexp.MustCompile("(?m)^(?:test|it)\\(\\s*(?:'([^'\\\\\\n]*)'|\"([^\"\\\\\\n]*)\"|`([^`\\\\$\\n]*)`)\\s*,")

func scriptPlan(path string, targets ...Target) PackagePlan {
	module, ok := scriptModule(path)
	if !ok {
		panic("bad module " + path)
	}
	for i := range targets {
		targets[i].Dir, targets[i].Path = path, path
	}
	return PackagePlan{Dir: path, Targets: targets, Script: &module}
}

func scriptOptions(family string) RenderOptions {
	return RenderOptions{Suffix: "abcdef0123456789", ObservationsPath: "/tmp/probe-observations.jsonl", PayloadLimit: harness.PayloadLimit(32 * 1024), CallTimeout: 250 * time.Millisecond, Family: family}
}

func TestRenderScriptHarness(t *testing.T) {
	a := scriptTargetFor("discount", scalar("number"), scalar("number"))
	a.Inputs = 5
	b := scriptTargetFor("default", Param{Kind: ParamSlice, Basic: "string"}, Param{Kind: ParamVariadic, Basic: "number"})
	b.Inputs = 3
	h, err := RenderScript(scriptPlan("web/cart.ts", a, b), scriptOptions(FamilyVitest))
	if err != nil {
		t.Fatal(err)
	}
	if h.Path != "web/probe-fuzz-abcdef0123456789.test.ts" || h.Runner != harness.RunnerJest || h.EvidenceRunner() != harness.RunnerJest || h.Display != MaxDisplayBytes {
		t.Fatalf("harness %s %s %d", h.Path, h.Runner, h.Display)
	}
	var titles []string
	for _, m := range jsTestDeclaration.FindAllStringSubmatch(h.Content, -1) {
		titles = append(titles, m[1]+m[2]+m[3])
	}
	if strings.Join(titles, ",") != strings.Join(h.TestNames(), ",") || strings.Join(titles, ",") != "TestProbeFuzz_abcdef0123456789_1,TestProbeFuzz_abcdef0123456789_2" {
		t.Fatalf("test declarations %q, names %q", titles, h.TestNames())
	}
	for _, want := range []string{
		"// @ts-nocheck\n",
		"import { test } from \"vitest\";\n",
		"import * as probeFuzzabcdef0123456789_vm from \"node:vm\";\n",
		"import { discount as probeFuzzabcdef0123456789_f1, default as probeFuzzabcdef0123456789_f2 } from \"./cart\";\n",
		"const probeFuzzabcdef0123456789_path = \"/tmp/probe-observations.jsonl\";\n",
		"const probeFuzzabcdef0123456789_suffix = \"abcdef0123456789\";\n",
		"const probeFuzzabcdef0123456789_timeout = 250;\n",
		"    function () { return [0, 0]; },\n",
		"  ], [false, false]);\n}, 62500);\n",
		// The array parameter is encoded after the call; the rest parameter is not.
		"  ], [true]);\n}, 61500);\n",
		"    function () { return [[]]; },\n",
	} {
		if !strings.Contains(h.Content, want) {
			t.Errorf("harness lacks %q", want)
		}
	}
	if strings.Contains(h.Content, "ZSPFZ") || strings.Contains(h.Content, "`") {
		t.Fatal("a placeholder or a backtick is left in the harness")
	}
	// Every line that starts a statement at column 0 with test( is a harness test.
	for _, line := range strings.Split(h.Content, "\n") {
		if (strings.HasPrefix(line, "test(") || strings.HasPrefix(line, "it(")) && !jsTestDeclaration.MatchString(line) {
			t.Fatalf("undeclared test line %q", line)
		}
	}
	// Jest: the same harness without the vitest import; a JavaScript module
	// gets a .test.js harness; .mjs and .mts modules are imported by file name.
	jest, err := RenderScript(scriptPlan("web/cart.ts", a), scriptOptions(FamilyJest))
	if err != nil || strings.Contains(jest.Content, "vitest") {
		t.Fatalf("jest harness: %v", err)
	}
	for path, want := range map[string][2]string{
		"cart.js":      {"probe-fuzz-abcdef0123456789.test.js", `from "./cart";`},
		"web/cart.mjs": {"web/probe-fuzz-abcdef0123456789.test.js", `from "./cart.mjs";`},
		"web/cart.mts": {"web/probe-fuzz-abcdef0123456789.test.ts", `from "./cart.mts";`},
		"web/Cart.jsx": {"web/probe-fuzz-abcdef0123456789.test.js", `from "./Cart";`},
	} {
		h, err := RenderScript(scriptPlan(path, a), scriptOptions(FamilyJest))
		if err != nil || h.Path != want[0] || !strings.Contains(h.Content, want[1]) {
			t.Errorf("%s: %s %v", path, h.Path, err)
		}
	}
	// The rendering depends only on the plan and the options.
	again, _ := RenderScript(scriptPlan("web/cart.ts", a, b), scriptOptions(FamilyVitest))
	if again.Content != h.Content {
		t.Fatal("rendering is not deterministic")
	}
}

func TestRenderScriptRefusesUnsafeInputs(t *testing.T) {
	good := scriptTargetFor("discount", scalar("number"))
	good.Inputs = 2
	for name, edit := range map[string]func(*PackagePlan, *RenderOptions){
		"bad suffix":       func(_ *PackagePlan, o *RenderOptions) { o.Suffix = "XYZ" },
		"relative path":    func(_ *PackagePlan, o *RenderOptions) { o.ObservationsPath = "tmp/x" },
		"quote in path":    func(_ *PackagePlan, o *RenderOptions) { o.ObservationsPath = "/tmp/\"x" },
		"no timeout":       func(_ *PackagePlan, o *RenderOptions) { o.CallTimeout = 0 },
		"no payload":       func(_ *PackagePlan, o *RenderOptions) { o.PayloadLimit = 0 },
		"unknown family":   func(_ *PackagePlan, o *RenderOptions) { o.Family = "mocha" },
		"not a module":     func(p *PackagePlan, _ *RenderOptions) { p.Script = nil },
		"no targets":       func(p *PackagePlan, _ *RenderOptions) { p.Targets = nil },
		"module mismatch":  func(p *PackagePlan, _ *RenderOptions) { p.Script.Import = "./other" },
		"injected name":    func(p *PackagePlan, _ *RenderOptions) { p.Targets[0].Name = "a } from 'x'; //" },
		"unicode name":     func(p *PackagePlan, _ *RenderOptions) { p.Targets[0].Name = "café" },
		"go target":        func(p *PackagePlan, _ *RenderOptions) { p.Targets[0].Language = "" },
		"other module":     func(p *PackagePlan, _ *RenderOptions) { p.Targets[0].Path = "web/other.ts" },
		"unknown type":     func(p *PackagePlan, _ *RenderOptions) { p.Targets[0].Params = []Param{scalar("int")} },
		"array param kind": func(p *PackagePlan, _ *RenderOptions) { p.Targets[0].Params = []Param{array("number", 2)} },
		"bad file name":    func(p *PackagePlan, _ *RenderOptions) { p.Script.Path, p.Dir = "web/a b.ts", "web/a b.ts" },
	} {
		p := scriptPlan("web/cart.ts", good)
		o := scriptOptions(FamilyVitest)
		edit(&p, &o)
		if _, err := RenderScript(p, o); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

// Like the Go harness, a TS/JS harness halves its inputs until its
// worst-case stream fits the payload channel, and gives up below the
// smallest display bound.
func TestRenderScriptHalvesInputsToFitThePayload(t *testing.T) {
	tg := scriptTargetFor("f", scalar("string"))
	tg.Inputs = 256
	o := scriptOptions(FamilyVitest)
	o.PayloadLimit = 256 * 1024
	h, err := RenderScript(scriptPlan("web/f.ts", tg), o)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.Tests[0].Inputs); n != 256 || h.Display != MaxDisplayBytes {
		t.Fatalf("%d inputs, display %d", n, h.Display)
	}
	o.PayloadLimit = 40 * 1024
	h, err = RenderScript(scriptPlan("web/f.ts", tg), o)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.Tests[0].Inputs); n >= 256 || h.Display < minDisplayBytes {
		t.Fatalf("%d inputs, display %d", n, h.Display)
	}
	o.PayloadLimit = 1024
	if _, err := RenderScript(scriptPlan("web/f.ts", tg), o); err == nil {
		t.Fatal("a harness that cannot fit was rendered")
	}
}
