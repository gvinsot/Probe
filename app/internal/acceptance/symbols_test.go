package acceptance

import (
	"reflect"
	"testing"
)

const cartSource = `package cart

// Total sums the items.
func Total(xs []int) int {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return sum
}

func Discount(total int) int {
	if total > 100 {
		return total - 10
	}
	return total
}

type Cart struct{ Items []int }

func (c Cart) Sum() int { return Total(c.Items) }

var (
	Rate    = 3
	Minimum = 50
)

const Limit, Other = 10, 20

var _ = Rate
`

func TestGoChangedDeclarations(t *testing.T) {
	for _, tc := range []struct {
		added []int
		want  []string
	}{
		{nil, nil},
		{[]int{3}, []string{}},                        // the doc comment is not part of the declaration
		{[]int{14}, []string{"Discount"}},             // a body line
		{[]int{12, 19}, []string{"Cart", "Discount"}}, // signature line and a type
		{[]int{21}, []string{"Sum"}},                  // a method is named by its method name
		{[]int{25}, []string{"Minimum"}},              // one spec of a grouped var
		{[]int{28}, []string{"Limit", "Other"}},       // both names of one spec
		{[]int{30}, []string{}},                       // the blank identifier is never recorded
		{[]int{100}, []string{}},
	} {
		got := GoChangedDeclarations("cart.go", []byte(cartSource), tc.added)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("added %v: %q, want %q", tc.added, got, tc.want)
		}
	}
	if got := GoChangedDeclarations("bad.go", []byte("package x\nfunc {"), []int{1, 2}); len(got) != 0 {
		t.Fatalf("unparsable source gave %q", got)
	}
}

func TestGoIdentifiers(t *testing.T) {
	src := `package cart
import "testing"
func TestIntentAC1(t *testing.T) {
	if got := Discount(100); got != 90 {
		t.Errorf("Discount(100) = %d, want 90", got)
	}
	_ = Cart{}.Sum
}
`
	got, err := GoIdentifiers("cart_intent_test.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Cart", "Discount", "Errorf", "Sum", "T", "TestIntentAC1", "cart", "got", "t", "testing"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if _, err := GoIdentifiers("x_test.go", []byte("package")); err == nil {
		t.Fatal("parse error hidden")
	}
}

func TestJSChangedDeclarations(t *testing.T) {
	src := "import { x } from './x';\n" + // 1
		"export function discount(total: number): number {\n" + // 2
		"  if (total >= 100) {\n" + // 3
		"    return total - 10;\n" + // 4
		"  }\n" + // 5
		"  return total;\n" + // 6
		"}\n" + // 7
		"\n" + // 8
		"export const freeShipping = (total: number) =>\n" + // 9
		"  total >= 50;\n" + // 10
		"export default class Cart {}\n" + // 11
		"interface Line { qty: number }\n" + // 12
		"export async function* stream() {}\n" // 13
	for _, tc := range []struct {
		added []int
		want  []string
	}{
		{[]int{4}, []string{"discount"}},
		{[]int{10}, []string{"freeShipping"}},
		{[]int{8}, []string{"discount"}}, // a blank line belongs to the declaration above it
		{[]int{11, 12, 13}, []string{"Cart", "Line", "stream"}},
		{[]int{1}, nil},
	} {
		got := JSChangedDeclarations(src, tc.added)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("added %v: %q, want %q", tc.added, got, tc.want)
		}
	}
}

func TestJSIdentifiers(t *testing.T) {
	src := "import { expect, test } from \"vitest\";\n" +
		"// discountInComment is ignored\n" +
		"/* blockComment */\n" +
		"test(\"applies the 10 off\", () => {\n" +
		"  expect(discount(100)).toBe(90); // trailing\n" +
		"  const s = `template ${notParsed}`;\n" +
		"  const $x = 'single quoted';\n" +
		"});\n"
	got := JSIdentifiers(src)
	want := []string{"$x", "const", "discount", "expect", "from", "import", "s", "test", "toBe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestNamedOnAndIntersect(t *testing.T) {
	for _, tc := range []struct {
		text, symbol string
		want         bool
	}{
		{"func Discount(total int) int {", "Discount", true},
		{"x := DiscountRate", "Discount", false},
		{"return applyDiscount(x)", "Discount", false},
		{"a.Discount()", "Discount", true},
		{"$Discount", "Discount", false},
		{"Discount", "Discount", true},
		{"DiscountDiscount Discount", "Discount", true},
		{"anything", "", false},
	} {
		if got := NamedOn(tc.text, tc.symbol); got != tc.want {
			t.Errorf("NamedOn(%q, %q) = %v", tc.text, tc.symbol, got)
		}
	}
	if got := Intersect([]string{"b", "a", "c", "a"}, []string{"c", "a", "z"}); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("Intersect %q", got)
	}
	for s, want := range map[string]bool{"Discount": true, "$x": true, "_y1": true, "1abc": false, "a-b": false, "é": false, "": false} {
		if IsIdentifier(s) != want {
			t.Errorf("IsIdentifier(%q)", s)
		}
	}
}
