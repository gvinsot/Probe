package i18n

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The desktop module, from this package directory.
const root = "../.."

// messages returns every message to translate, in a stable order: the Go
// strings marked msg.M, the t("…") and tn(n, "…", "…") strings of app.js,
// and the text and placeholder, title and aria-label attributes of
// index.html.
func extract(t *testing.T) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.Join(strings.Fields(s), " ")
		if s != "" && hasLetters(s) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	var goFiles []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	sort.Strings(goFiles)
	fset := token.NewFileSet()
	for _, path := range goFiles {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "M" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "msg" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: msg.M needs a string literal", fset.Position(call.Pos()))
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			add(s)
			return true
		})
	}

	js := read(t, "web/public/app.js")
	jsString := `"((?:[^"\\]|\\.)*)"`
	for _, m := range regexp.MustCompile(`\bt\(\s*`+jsString).FindAllStringSubmatch(js, -1) {
		add(unquoteJS(t, m[1]))
	}
	for _, m := range regexp.MustCompile(`\btn\([^,]+,\s*`+jsString+`,\s*`+jsString).FindAllStringSubmatch(js, -1) {
		add(unquoteJS(t, m[1]))
		add(unquoteJS(t, m[2]))
	}

	for _, s := range htmlMessages(read(t, "web/public/index.html")) {
		add(s)
	}
	return out
}

func read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func unquoteJS(t *testing.T, s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		t.Fatalf("JavaScript string %q: %v", s, err)
	}
	return out
}

var (
	tokenRe = regexp.MustCompile(`(?s)<!--.*?-->|<![^>]*>|<(/?)([a-zA-Z0-9]+)([^>]*)>|([^<]+)`)
	attrRe  = regexp.MustCompile(`\s(placeholder|title|aria-label)="([^"]*)"`)
	void    = map[string]bool{"meta": true, "link": true, "input": true, "br": true, "img": true, "hr": true, "source": true}
)

// htmlMessages returns the text and the translated attributes of a page, as
// i18n.js translates them: outside translate="no" elements, scripts and
// styles. The page is simple enough for a tokenizer.
func htmlMessages(page string) []string {
	var out []string
	type open struct {
		tag  string
		skip bool
	}
	var stack []open
	skipping := func() bool { return len(stack) > 0 && stack[len(stack)-1].skip }
	for _, m := range tokenRe.FindAllStringSubmatch(page, -1) {
		switch {
		case m[4] != "":
			if !skipping() {
				out = append(out, htmlUnescape(m[4]))
			}
		case m[2] != "" && m[1] == "/":
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].tag == strings.ToLower(m[2]) {
					stack = stack[:i]
					break
				}
			}
		case m[2] != "":
			tag := strings.ToLower(m[2])
			skip := skipping() || strings.Contains(m[3], `translate="no"`) || tag == "script" || tag == "style"
			if !skip {
				for _, a := range attrRe.FindAllStringSubmatch(m[3], -1) {
					out = append(out, htmlUnescape(a[2]))
				}
			}
			if !void[tag] && !strings.HasSuffix(m[3], "/") {
				stack = append(stack, open{tag, skip})
			}
		}
	}
	return out
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ").Replace(s)
}

func hasLetters(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			return true
		}
	}
	return false
}

var (
	goVerb = regexp.MustCompile(`%[dsqv]`)
	jsArg  = regexp.MustCompile(`\{\d+\}`)
)

// placeholders are the arguments a translation must keep: {0}…{n-1} for a Go
// format with n verbs, the same {n} for a string of app.js.
func placeholders(key string) []string {
	if n := len(goVerb.FindAllString(key, -1)); n > 0 {
		out := make([]string, n)
		for i := range out {
			out[i] = "{" + strconv.Itoa(i) + "}"
		}
		return out
	}
	return uniqueSorted(jsArg.FindAllString(key, -1))
}

func uniqueSorted(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func catalogPath(lang string) string { return filepath.Join(root, "web", Path(lang)) }

// TestCatalogs requires every message in every catalog, with its
// placeholders. PROBE_I18N_UPDATE=1 rewrites the catalogs instead: new
// messages are added with an empty translation, unused ones removed.
func TestCatalogs(t *testing.T) {
	keys := extract(t)
	if len(keys) < 100 {
		t.Fatalf("only %d messages found: is the extraction broken?", len(keys))
	}
	for _, l := range Languages[1:] {
		var c Catalog
		if data, err := os.ReadFile(catalogPath(l.Code)); err == nil {
			if err := json.Unmarshal(data, &c); err != nil {
				t.Fatalf("%s: %v", l.Code, err)
			}
		}
		if os.Getenv("PROBE_I18N_UPDATE") != "" {
			write(t, l.Code, keys, c.Messages)
			continue
		}
		want := map[string]bool{}
		var missing, empty, broken []string
		for _, k := range keys {
			want[k] = true
			v, ok := c.Messages[k]
			switch {
			case !ok:
				missing = append(missing, k)
			case strings.TrimSpace(v) == "":
				empty = append(empty, k)
			default:
				if p, got := placeholders(k), uniqueSorted(jsArg.FindAllString(v, -1)); strings.Join(p, "") != strings.Join(got, "") || goVerb.MatchString(v) {
					broken = append(broken, k)
				}
			}
		}
		var obsolete []string
		for k := range c.Messages {
			if !want[k] {
				obsolete = append(obsolete, k)
			}
		}
		report := func(what string, list []string) {
			if len(list) > 0 {
				t.Errorf("%s: %d %s (run PROBE_I18N_UPDATE=1 go test ./internal/i18n, then translate), e.g. %q", l.Code, len(list), what, list[0])
			}
		}
		report("missing messages", missing)
		report("empty translations", empty)
		report("translations with wrong placeholders", broken)
		report("unused messages", obsolete)
		if c.Language != l.Code {
			t.Errorf("%s: catalog declares language %q", l.Code, c.Language)
		}
	}
}

// write saves a catalog in the order of the messages, keeping the existing
// translations.
func write(t *testing.T, lang string, keys []string, old map[string]string) {
	var buf bytes.Buffer
	buf.WriteString("{\n  \"language\": " + quote(lang) + ",\n  \"messages\": {")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString("\n    " + quote(k) + ": " + quote(old[k]))
	}
	buf.WriteString("\n  }\n}\n")
	if err := os.WriteFile(catalogPath(lang), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSpace(buf.String())
}

func TestMatch(t *testing.T) {
	for in, want := range map[string]string{
		"fr_FR.UTF-8": "fr", "pt-BR": "pt", "de": "de", "it_IT@euro": "it", "es-419": "es",
		"en_US": "en", "ja_JP": "", "": "", "C": "",
	} {
		if got := Match(in); got != want {
			t.Errorf("Match(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestT(t *testing.T) {
	catalogs["xx"] = map[string]string{"Quit": "Beenden", "%d documents · %d to review": "{1} à revoir sur {0}"}
	defer delete(catalogs, "xx")
	if got := T("xx", "Quit"); got != "Beenden" {
		t.Errorf("T = %q", got)
	}
	if got := Tf("xx", "%d documents · %d to review", 5, 2); got != "2 à revoir sur 5" {
		t.Errorf("Tf reorders arguments: %q", got)
	}
	if got := T("xx", "Unknown"); got != "Unknown" {
		t.Errorf("missing message: %q", got)
	}
	if got := Tf("en", "%d row(s) hidden", 3); got != "3 row(s) hidden" {
		t.Errorf("English: %q", got)
	}
}
