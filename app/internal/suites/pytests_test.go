package suites

import (
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

const cartPyTests = `import pytest

from shop.cart import total


def test_total():
    assert total([10, 20], 0) == 30


@pytest.mark.parametrize("discount", [0, 0.5])
def test_discount(discount):
    assert total([10], discount) <= 10


class TestCart:
    def test_empty(self):
        assert total([], 0) == 0
`

func TestPlanPythonTests(t *testing.T) {
	const p = "tests/test_cart.py"
	all := func(change string) map[string]string {
		return map[string]string{"test_total": change, "test_discount": change, "TestCart::test_empty": change}
	}
	cases := []struct {
		name      string
		candidate string
		want      map[string]string
	}{
		{"comments only", strings.Replace(cartPyTests, "def test_total():\n", "def test_total():  # sum\n", 1), map[string]string{}},
		{"one test body", strings.Replace(cartPyTests, "== 30", "== 31", 1), map[string]string{"test_total": model.BaseTestModified}},
		{"a parametrize list", strings.Replace(cartPyTests, "[0, 0.5]", "[0]", 1), map[string]string{"test_discount": model.BaseTestModified}},
		{"a class test", strings.Replace(cartPyTests, "total([], 0) == 0", "total([], 0) == 1", 1), map[string]string{"TestCart::test_empty": model.BaseTestModified}},
		{"a test removed", strings.Replace(cartPyTests, "def test_total():\n    assert total([10, 20], 0) == 30\n", "", 1), map[string]string{"test_total": model.BaseTestRemoved}},
		{"a test added", cartPyTests + "\n\ndef test_new():\n    pass\n", map[string]string{}},
		{"an import", strings.Replace(cartPyTests, "from shop.cart import total", "from shop.cart2 import total", 1), all(model.BaseTestSharedCodeChanged)},
		{"a class renamed", strings.Replace(cartPyTests, "class TestCart:", "class TestBasket:", 1), map[string]string{"TestCart::test_empty": model.BaseTestRemoved, "test_total": model.BaseTestSharedCodeChanged, "test_discount": model.BaseTestSharedCodeChanged}},
		{"a name made ambiguous", cartPyTests + "\n\ndef test_total():\n    pass\n", map[string]string{"test_total": model.BaseTestModified}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := planScript(t, fakeRepo{base(p): cartPyTests, head(p): tc.candidate}, model.ChangedFile{Path: p, Status: "M"})
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

func TestPlanPythonFiles(t *testing.T) {
	const p = "tests/test_cart.py"
	sel := planScript(t, fakeRepo{base(p): cartPyTests}, model.ChangedFile{Path: p, Status: "D"})
	if len(sel.Tests) != 3 || sel.Tests[0].Change != model.BaseTestFileDeleted || sel.Tests[0].Name != "test_total" || sel.Tests[0].Line != 6 || sel.Tests[0].EndLine != 7 {
		t.Fatalf("deleted file: %+v", sel.Tests)
	}
	if sel.Tests[1].Name != "test_discount" || sel.Tests[1].Line != 10 {
		t.Fatalf("a decorated test starts at its decorator: %+v", sel.Tests[1])
	}
	if sel := planScript(t, fakeRepo{base(p): cartPyTests, head("tests/test_basket.py"): cartPyTests}, model.ChangedFile{Path: "tests/test_basket.py", OldPath: p, Status: "R"}); len(sel.Tests) != 0 {
		t.Fatalf("an unchanged rename selected %+v", sel.Tests)
	}
	// A rename to another language, or to a module pytest does not collect,
	// deletes the tests.
	for _, to := range []string{"tests/cart.py", "tests/test_cart.ts"} {
		if sel := planScript(t, fakeRepo{base(p): cartPyTests, head(to): cartPyTests}, model.ChangedFile{Path: to, OldPath: p, Status: "R"}); len(sel.Tests) != 3 || sel.Tests[0].Change != model.BaseTestFileDeleted {
			t.Fatalf("renamed to %s: %+v", to, sel.Tests)
		}
	}
	for _, path := range []string{"tests/conftest.py", "tests/helpers.py", "shop/cart.py"} {
		if sel := planScript(t, fakeRepo{base(path): cartPyTests, head(path): cartPyTests + "x = 1\n"}, model.ChangedFile{Path: path, Status: "M"}); len(sel.Tests) != 0 {
			t.Fatalf("%s selected %+v", path, sel.Tests)
		}
	}
}
