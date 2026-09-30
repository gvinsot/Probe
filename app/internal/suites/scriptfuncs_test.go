package suites

import (
	"context"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const priceTests = `import { describe, test, expect } from "vitest";
import { price } from "./price";

describe("price", () => {
  test("discounts", () => {
    expect(price(100, 10)).toBe(90);
  });
  it("rounds", () => {
    expect(price(5, 33)).toBe(4);
  });
});

test("top", () => { expect(price(1, 0)).toBe(1); });
`

func planScript(t *testing.T, repo fakeRepo, files ...model.ChangedFile) Selection {
	t.Helper()
	sel, err := Plan(context.Background(), repo.reader(nil), model.Change{BaseCommit: baseCommit, HeadCommit: headCommit, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func names(sel Selection) map[string]string {
	out := map[string]string{}
	for _, bt := range sel.Tests {
		out[bt.Name] = bt.Change
	}
	return out
}

func TestPlanScriptTests(t *testing.T) {
	const p = "web/price.test.ts"
	cases := []struct {
		name      string
		candidate string
		want      map[string]string
	}{
		{"layout and comments only", strings.ReplaceAll(priceTests, "  test(\"discounts\", () => {", "  // a note\n  test(\"discounts\",   () => {"), map[string]string{}},
		{"one test body", strings.Replace(priceTests, "toBe(90)", "toBe(91)", 1), map[string]string{"price > discounts": model.BaseTestModified}},
		{"a test removed", strings.Replace(priceTests, "test(\"top\", () => { expect(price(1, 0)).toBe(1); });\n", "", 1), map[string]string{"top": model.BaseTestRemoved}},
		{"a test added", priceTests + "test(\"new\", () => {});\n", map[string]string{}},
		{"shared code", strings.Replace(priceTests, "import { price } from \"./price\";", "import { price } from \"./price2\";", 1), map[string]string{
			"price > discounts": model.BaseTestSharedCodeChanged, "price > rounds": model.BaseTestSharedCodeChanged, "top": model.BaseTestSharedCodeChanged,
		}},
		{"describe renamed", strings.Replace(priceTests, "describe(\"price\"", "describe(\"cost\"", 1), map[string]string{
			"price > discounts": model.BaseTestRemoved, "price > rounds": model.BaseTestRemoved, "top": model.BaseTestSharedCodeChanged,
		}},
		{"a name made ambiguous", priceTests + "test(\"top\", () => {});\n", map[string]string{"top": model.BaseTestModified}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := planScript(t, fakeRepo{base(p): priceTests, head(p): tc.candidate}, model.ChangedFile{Path: p, Status: "M"})
			got := names(sel)
			if len(got) != len(tc.want) {
				t.Fatalf("selected %v, want %v", got, tc.want)
			}
			for n, change := range tc.want {
				if got[n] != change {
					t.Fatalf("selected %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPlanScriptFiles(t *testing.T) {
	const p = "web/price.test.ts"
	sel := planScript(t, fakeRepo{base(p): priceTests}, model.ChangedFile{Path: p, Status: "D"})
	if len(sel.Tests) != 3 || sel.Tests[0].Change != model.BaseTestFileDeleted || sel.Tests[0].Name != "price > discounts" || sel.Tests[0].Line != 5 || sel.Tests[0].EndLine != 7 {
		t.Fatalf("deleted file: %+v", sel.Tests)
	}
	moved := planScript(t, fakeRepo{base(p): priceTests, head("api/price.test.ts"): priceTests}, model.ChangedFile{Path: "api/price.test.ts", OldPath: p, Status: "R"})
	if got := names(moved); len(got) != 3 || got["top"] != model.BaseTestSharedCodeChanged {
		t.Fatalf("moved to another directory: %v", got)
	}
	if sel := planScript(t, fakeRepo{base(p): priceTests, head("web/price.ts"): "x"}, model.ChangedFile{Path: "web/price.ts", OldPath: p, Status: "R"}); len(sel.Tests) != 3 || sel.Tests[0].Change != model.BaseTestFileDeleted {
		t.Fatalf("renamed to a source file: %+v", sel.Tests)
	}
	for _, path := range []string{"web/price.ts", "node_modules/x/a.test.js", "web/types.d.ts"} {
		if sel := planScript(t, fakeRepo{base(path): priceTests, head(path): priceTests + "x"}, model.ChangedFile{Path: path, Status: "M"}); len(sel.Tests) != 0 {
			t.Fatalf("%s selected %+v", path, sel.Tests)
		}
	}
	dup := priceTests + "test(\"top\", () => {});\n"
	sel = planScript(t, fakeRepo{base(p): dup, head(p): strings.Replace(dup, "toBe(90)", "toBe(91)", 1)}, model.ChangedFile{Path: p, Status: "M"})
	if got := names(sel); len(got) != 1 || got["price > discounts"] != model.BaseTestModified || len(sel.Notes) != 1 || !strings.Contains(sel.Notes[0], `more than one test named "top"`) {
		t.Fatalf("ambiguous baseline name: %v %v", got, sel.Notes)
	}
}
