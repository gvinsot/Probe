package symbols

import (
	"strings"
	"testing"
)

const cartTests = `import pytest
import unittest

from shop.cart import total


@pytest.fixture
def prices():
    return [10, 20]


def test_total(prices):
    assert total(prices, 0) == 30


@pytest.mark.parametrize("discount", [0, 0.5])
def test_discount(prices, discount):
    if discount:
        assert total(prices, discount) == 15
    assert total(prices, 0) == 30


class TestCart:
    def test_empty(self):
        assert total([], 0) == 0

    def helper(self):
        pass

    class TestNested:
        async def test_inner(self):
            pass


class CartCase(unittest.TestCase):
    def test_case(self):
        self.assertEqual(total([1], 0), 1)


class Helper:
    def test_not_collected(self):
        pass


def helper():
    def test_nested_in_function():
        pass
`

func pythonTests(t *testing.T, src string) (map[string]ScriptTest, string) {
	t.Helper()
	read, ok := ReadPythonTests("tests/test_cart.py", []byte(src))
	if !ok {
		t.Fatal("not read as a Python test module")
	}
	out := map[string]ScriptTest{}
	for _, test := range read.Tests {
		out[test.Name] = test
	}
	return out, read.Shared
}

func TestReadPythonTestsNamesAsPytest(t *testing.T) {
	tests, _ := pythonTests(t, cartTests)
	want := map[string][2]int{
		"test_total":                       {12, 13},
		"test_discount":                    {16, 20}, // from its decorator
		"TestCart::test_empty":             {24, 25},
		"TestCart::TestNested::test_inner": {31, 32},
		"CartCase::test_case":              {36, 37},
	}
	if len(tests) != len(want) {
		t.Fatalf("tests %v", tests)
	}
	for name, lines := range want {
		got, ok := tests[name]
		if !ok || got.Line != lines[0] || got.EndLine != lines[1] {
			t.Errorf("%s: %+v, want lines %v", name, got, lines)
		}
	}
}

func TestReadPythonTestsDigests(t *testing.T) {
	base, baseShared := pythonTests(t, cartTests)
	for _, tc := range []struct {
		name    string
		edit    func(string) string
		changed []string // tests whose digest changes
		shared  bool     // whether the shared digest changes
	}{
		{"comments and blank lines", func(s string) string {
			return strings.Replace(s, "def test_total(prices):\n", "def test_total(prices):  # sum\n\n", 1)
		}, nil, false},
		{"a test body", func(s string) string { return strings.Replace(s, "== 30\n\n\n@pytest", "== 31\n\n\n@pytest", 1) }, []string{"test_total"}, false},
		{"a decorator", func(s string) string { return strings.Replace(s, "[0, 0.5]", "[0, 0.25]", 1) }, []string{"test_discount"}, false},
		{"a statement moved out of its block", func(s string) string {
			return strings.Replace(s, "        assert total(prices, discount) == 15\n    assert total(prices, 0) == 30", "        assert total(prices, discount) == 15\n        assert total(prices, 0) == 30", 1)
		}, []string{"test_discount"}, false},
		{"a fixture", func(s string) string { return strings.Replace(s, "[10, 20]", "[10, 21]", 1) }, nil, true},
		{"an import", func(s string) string {
			return strings.Replace(s, "from shop.cart import total", "from shop.cart2 import total", 1)
		}, nil, true},
		{"a class helper", func(s string) string {
			return strings.Replace(s, "    def helper(self):\n        pass", "    def helper(self):\n        return 1", 1)
		}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edited, shared := pythonTests(t, tc.edit(cartTests))
			changed := map[string]bool{}
			for _, n := range tc.changed {
				changed[n] = true
			}
			for name, test := range base {
				if (edited[name].Digest != test.Digest) != changed[name] {
					t.Errorf("%s: digest changed = %v, want %v", name, edited[name].Digest != test.Digest, changed[name])
				}
			}
			if (shared != baseShared) != tc.shared {
				t.Errorf("shared changed = %v, want %v", shared != baseShared, tc.shared)
			}
		})
	}
}

func TestIsPythonTestPath(t *testing.T) {
	for p, want := range map[string]bool{
		"tests/test_cart.py": true, "cart_test.py": true, "src/pkg/test_x.py": true,
		"tests/conftest.py": false, "tests/helpers.py": false, "tests/test_cart.ts": false, ".venv/lib/test_x.py": false,
	} {
		if got := IsPythonTestPath(p); got != want {
			t.Errorf("IsPythonTestPath(%q) = %v", p, got)
		}
	}
	if PythonTestName("TestCart.TestNested.test_inner") != "TestCart::TestNested::test_inner" || PythonTestName("test_total") != "test_total" {
		t.Fatal("PythonTestName")
	}
}

const cartModule = `"""Cart helpers."""
import math

RATE = 0.2
LIMIT: int = 100


def total(prices):
    return sum(prices)


@cache
def discount(amount):
    if amount > LIMIT:
        return amount - 10
    return amount


class Cart:
    def add(self, x):
        pass


if __name__ == "__main__":
    print(total([1]))
`

func TestPythonChangedDeclarations(t *testing.T) {
	for _, tc := range []struct {
		added []int
		want  []string
	}{
		{[]int{4}, []string{"RATE"}},
		{[]int{5}, []string{"LIMIT"}},
		{[]int{9}, []string{"total"}},
		{[]int{12}, []string{"discount"}}, // its decorator
		{[]int{15}, []string{"discount"}},
		{[]int{21}, []string{"Cart"}}, // a method belongs to its class
		{[]int{24, 25}, nil},          // an if block declares nothing
		{[]int{1, 2}, nil},            // docstring and import
		{[]int{4, 9, 21}, []string{"Cart", "RATE", "total"}},
		{nil, nil},
	} {
		got := PythonChangedDeclarations("shop/cart.py", []byte(cartModule), tc.added)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("added %v: %q, want %q", tc.added, got, tc.want)
		}
	}
}

func TestPythonIdentifiers(t *testing.T) {
	got := PythonIdentifiers([]byte("from shop.cart import discount  # total\n\ndef test_x():\n    assert discount(100) == 90, \"total\"\n    '''RATE'''\n    f\"{LIMIT}\"\n"))
	want := map[string]bool{"from": true, "shop": true, "cart": true, "import": true, "discount": true, "def": true, "test_x": true, "assert": true}
	seen := map[string]bool{}
	for _, name := range got {
		seen[name] = true
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("missing %q in %q", name, got)
		}
	}
	for _, name := range []string{"total", "RATE"} {
		if seen[name] {
			t.Errorf("%q from a comment or a string read as an identifier: %q", name, got)
		}
	}
}
