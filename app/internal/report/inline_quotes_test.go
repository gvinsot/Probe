package report

import (
	"strings"
	"testing"
)

// inline() escapes "&", "<" and ">" and the Markdown punctuation, but leaves
// quotes and apostrophes alone: numeric entities followed by an escaped "#"
// (&\#39;) showed as literal entity text in CommonMark renderers.
func TestInlineKeepsQuotesReadable(t *testing.T) {
	cases := map[string]string{
		`Guest's "admin" role`:              `Guest's "admin" role`,
		`lookup("")`:                        `lookup\(""\)`,
		`&#39; and &#34;`:                   `&amp;\#39; and &amp;\#34;`,
		`<b>x</b> & y`:                      `&lt;b&gt;x&lt;/b&gt; &amp; y`,
		"a" + string(rune(0x202e)) + "b\nc": "a b c",
		"# heading [l](u) `c` *e* _u_ |!":   `\# heading \[l\]\(u\) \` + "`c\\`" + ` \*e\* \_u\_ \|\!`,
	}
	for in, want := range cases {
		got := inline(in)
		if got != want {
			t.Errorf("inline(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "&#") || strings.Contains(got, `&\#3`) && !strings.Contains(got, `&amp;\#3`) {
			t.Errorf("inline(%q) = %q keeps a live or escaped numeric entity", in, got)
		}
		if strings.ContainsAny(got, "<>\n") {
			t.Errorf("inline(%q) = %q keeps markup or a line break", in, got)
		}
	}
}
