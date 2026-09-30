// Package i18n holds the languages of Probe Desktop and translates the
// messages the engine renders itself, such as the tray menu.
//
// The catalogs are web/public/i18n/<lang>.json, shared with the interface:
// each maps an English message to its translation. A message is the text
// marked with msg.M in the Go sources, or with t("…") in app.js, or the text
// of index.html. A Go format ("%d row(s) hidden") is translated with numbered
// placeholders ("{0} ligne(s) masquée(s)"), so the interface and the engine
// substitute the arguments the same way. English has no catalog.
package i18n

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/gvinsot/Probe/desktop/web"
)

// Language is a supported language, named in itself.
type Language struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Languages are the supported languages, English first: it is the source
// and the fallback.
var Languages = []Language{
	{"en", "English"},
	{"fr", "Français"},
	{"es", "Español"},
	{"de", "Deutsch"},
	{"pt", "Português"},
	{"it", "Italiano"},
}

// Supported reports whether code is a supported language.
func Supported(code string) bool {
	for _, l := range Languages {
		if l.Code == code {
			return true
		}
	}
	return false
}

// Match returns the supported language of a locale name such as "fr_FR.UTF-8",
// "pt-BR" or "de", or "" when none matches.
func Match(locale string) string {
	code := strings.ToLower(strings.TrimSpace(locale))
	if i := strings.IndexAny(code, "_-.@"); i >= 0 {
		code = code[:i]
	}
	if Supported(code) {
		return code
	}
	return ""
}

// Catalog is the content of a web/public/i18n/<lang>.json file.
type Catalog struct {
	Language string            `json:"language"`
	Messages map[string]string `json:"messages"`
}

var (
	mu       sync.Mutex
	catalogs = map[string]map[string]string{}
)

// Path is the location of a catalog inside web.Assets.
func Path(lang string) string { return "public/i18n/" + lang + ".json" }

func messages(lang string) map[string]string {
	mu.Lock()
	defer mu.Unlock()
	if m, ok := catalogs[lang]; ok {
		return m
	}
	var c Catalog
	if data, err := fs.ReadFile(web.Assets, Path(lang)); err == nil {
		json.Unmarshal(data, &c)
	}
	catalogs[lang] = c.Messages
	return c.Messages
}

var placeholder = regexp.MustCompile(`\{(\d+)\}`)

// T renders a message in a language: its translation, else the English
// message.
func T(lang, english string) string {
	if lang != "en" {
		if tr := messages(lang)[english]; tr != "" {
			return tr
		}
	}
	return english
}

// Tf renders a fmt format in a language: the translation with its {n}
// placeholders replaced by the arguments, else the English format applied
// to them.
func Tf(lang, format string, args ...any) string {
	if lang != "en" {
		if tr := messages(lang)[format]; tr != "" {
			return placeholder.ReplaceAllStringFunc(tr, func(m string) string {
				i, _ := strconv.Atoi(m[1 : len(m)-1])
				if i < len(args) {
					return fmt.Sprint(args[i])
				}
				return m
			})
		}
	}
	return fmt.Sprintf(format, args...)
}
