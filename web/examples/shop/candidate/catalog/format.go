// Package catalog formats products for the storefront.
package catalog

import (
	"fmt"
	"strings"
	"unicode"
)

// FormatPrice renders cents as a price, such as "12.50 EUR".
func FormatPrice(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, cents/100, cents%100, currency)
}

// Slug turns a product name into a URL path segment: "Blue Mug (L)" becomes
// "blue-mug-l".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
