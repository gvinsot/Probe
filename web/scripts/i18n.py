#!/usr/bin/env python3
"""Builds the translated website from the English pages and the catalogs.

The English pages of web/public/ are the only source. Their text is cut into
segments: the content of a paragraph, a list item, a table cell, a heading, a
link or a button, inline markup included (<a>, <code>, <strong>…), so a
sentence is always translated whole. The <title>, the meta descriptions and
sharing cards, and the alt, title, placeholder and aria-label attributes are
segments too, and so are the strings site.js passes to t("…").

web/i18n/<lang>.json maps each English segment to its translation:

  {"language": "fr", "segments": {"<english>": "<translation>"}, "js": {…}}

Commands:

  i18n.py extract            add the new segments to every catalog (empty),
                             drop the ones no page uses any more
  i18n.py build --out DIR    write the whole site to DIR: public/ as is, the
                             English pages with the language menu and the
                             hreflang links, and one directory per language
  i18n.py check              fail when a catalog misses a translation or a
                             translation lost or changed the markup

A segment without translation is published in English, with a warning, so an
edit of the English copy never blocks a deployment; `check` lists them.
"""

import argparse
import html
import json
import os
import re
import shutil
import sys
from html.parser import HTMLParser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PUBLIC = os.path.join(ROOT, "public")
CATALOGS = os.path.join(ROOT, "i18n")
SITE = "https://probe.technology"

# English first: it is the source and the x-default.
LANGUAGES = [
    # code, name in the language, Open Graph locale
    ("en", "English", "en_US"),
    ("fr", "Français", "fr_FR"),
    ("es", "Español", "es_ES"),
    ("de", "Deutsch", "de_DE"),
    ("pt", "Português", "pt_BR"),
    ("it", "Italiano", "it_IT"),
]
TRANSLATED = [code for code, _, _ in LANGUAGES[1:]]

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"}
INLINE = {"a", "abbr", "b", "bdi", "br", "cite", "code", "data", "dfn", "em", "i", "img", "kbd", "mark", "q",
          "s", "samp", "small", "span", "strong", "sub", "sup", "time", "u", "var", "wbr"}
SKIP = {"script", "style", "pre", "svg", "template", "noscript", "textarea"}
ATTRS = ("alt", "title", "placeholder", "aria-label")
META = {("name", "description"), ("property", "og:title"), ("property", "og:description"),
        ("property", "og:image:alt"), ("name", "twitter:title"), ("name", "twitter:description"),
        ("name", "twitter:image:alt")}


class Node:
    def __init__(self, tag, attrs, start, start_end, parent):
        self.tag, self.attrs, self.parent = tag, dict(attrs), parent
        self.start, self.start_end = start, start_end  # the start tag
        self.end_start = self.end = start_end          # the end tag
        self.children = []                              # Node or (start, end) text spans


class Tree(HTMLParser):
    """Parses a page into nodes that keep their offsets in the source."""

    def __init__(self, source):
        super().__init__(convert_charrefs=False)
        self.source = source
        self.lines = [0]
        for i, c in enumerate(source):
            if c == "\n":
                self.lines.append(i + 1)
        self.root = Node("#root", [], 0, 0, None)
        self.cur = self.root
        self.feed(source)
        self.close()

    def at(self):
        line, col = self.getpos()
        return self.lines[line - 1] + col

    def handle_starttag(self, tag, attrs):
        start = self.at()
        end = start + len(self.get_starttag_text())
        node = Node(tag, attrs, start, end, self.cur)
        self.cur.children.append(node)
        if tag not in VOID:
            self.cur = node

    def handle_startendtag(self, tag, attrs):
        start = self.at()
        end = start + len(self.get_starttag_text())
        self.cur.children.append(Node(tag, attrs, start, end, self.cur))

    def handle_endtag(self, tag):
        start = self.at()
        end = self.source.index(">", start) + 1
        node = self.cur
        while node is not self.root and node.tag != tag:
            node = node.parent
        if node is self.root:
            return  # stray end tag
        node.end_start, node.end = start, end
        self.cur = node.parent

    def _text(self, n):
        start = self.at()
        self.cur.children.append((start, start + n))

    def handle_data(self, data):
        self._text(len(data))

    def handle_entityref(self, name):
        self._text(len(name) + 2)

    def handle_charref(self, name):
        self._text(len(name) + 3)


def normalize(s):
    return re.sub(r"\s+", " ", s).strip()


def has_words(fragment):
    """A fragment is worth translating when letters remain outside <code>."""
    text = re.sub(r"<code\b.*?</code>", "", fragment, flags=re.S)
    text = html.unescape(re.sub(r"<[^>]+>", "", text))
    return re.search(r"[^\W\d_]{2,}", text) is not None


class Segment:
    def __init__(self, start, end, key):
        self.start, self.end, self.key = start, end, key


def segments(source):
    """Returns the translatable spans of a page: text segments and attribute
    values, as (start, end, english) with the offsets of the raw source."""
    tree = Tree(source)
    out = []

    def span_of(child):
        return (child.start, child.end) if isinstance(child, Node) else child

    def is_inline(child):
        return not isinstance(child, Node) or (child.tag in INLINE and all(is_inline(c) for c in child.children))

    def add_run(run):
        start, end = span_of(run[0])[0], span_of(run[-1])[1]
        raw = source[start:end]
        # Keep the surrounding whitespace in place: only the words change.
        lead = len(raw) - len(raw.lstrip())
        trail = len(raw) - len(raw.rstrip())
        start, end = start + lead, end - trail
        frag = source[start:end]
        if frag and has_words(frag):
            out.append(Segment(start, end, normalize(frag)))

    def visit(node):
        if node.tag in SKIP:
            return
        runs, run = [], []
        for child in node.children:
            if is_inline(child) and not (isinstance(child, Node) and child.tag in SKIP):
                run.append(child)
            else:
                if run:
                    runs.append(run)
                run = []
                if isinstance(child, Node):
                    visit(child)
        if run:
            runs.append(run)
        for r in runs:
            direct_text = any(not isinstance(c, Node) and source[c[0]:c[1]].strip() for c in r)
            elements = [c for c in r if isinstance(c, Node)]
            if not direct_text and len(elements) > 1:
                # Separate links or buttons: one segment each.
                for e in elements:
                    if e.children and e.tag != "code":
                        add_run(e.children)
            elif not direct_text and len(elements) == 1 and elements[0].children and elements[0].tag != "code":
                add_run(elements[0].children)
            else:
                add_run(r)

    def attributes(node):
        for child in node.children:
            if not isinstance(child, Node):
                continue
            tag_src = source[child.start:child.start_end]
            if child.tag == "meta" and any(child.attrs.get(k) == v for k, v in META):
                keys = ["content"]
            elif child.tag in ("script", "style"):
                keys = []
            else:
                keys = [a for a in ATTRS if child.attrs.get(a)]
            for a in keys:
                m = re.search(r'\s%s="([^"]*)"' % re.escape(a), tag_src)
                if m and has_words(m.group(1)):
                    out.append(Segment(child.start + m.start(1), child.start + m.end(1), normalize(m.group(1))))
            attributes(child)

    visit(tree.root)
    attributes(tree.root)
    out.sort(key=lambda s: s.start)
    return out


JS_CALL = re.compile(r'\bt\("((?:[^"\\]|\\.)*)"')


def js_strings(path):
    with open(path, encoding="utf-8") as f:
        return [json.loads('"%s"' % m) for m in JS_CALL.findall(f.read())]


def pages():
    return sorted(f for f in os.listdir(PUBLIC) if f.endswith(".html"))


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def load_catalog(code):
    path = os.path.join(CATALOGS, code + ".json")
    if not os.path.exists(path):
        return {"language": code, "segments": {}, "js": {}}
    cat = json.loads(read(path))
    cat.setdefault("segments", {})
    cat.setdefault("js", {})
    return cat


def save_catalog(code, cat):
    os.makedirs(CATALOGS, exist_ok=True)
    with open(os.path.join(CATALOGS, code + ".json"), "w", encoding="utf-8") as f:
        json.dump(cat, f, ensure_ascii=False, indent=2)
        f.write("\n")


def all_keys():
    keys = []
    for page in pages():
        for s in segments(read(os.path.join(PUBLIC, page))):
            if s.key not in keys:
                keys.append(s.key)
    js = []
    for s in js_strings(os.path.join(PUBLIC, "site.js")):
        if s not in js:
            js.append(s)
    return keys, js


def cmd_extract(_):
    keys, js = all_keys()
    for code in TRANSLATED:
        cat = load_catalog(code)
        cat["segments"] = {k: cat["segments"].get(k, "") for k in keys}
        cat["js"] = {k: cat["js"].get(k, "") for k in js}
        save_catalog(code, cat)
        missing = sum(1 for v in list(cat["segments"].values()) + list(cat["js"].values()) if not v)
        print("%s: %d segments, %d strings, %d to translate" % (code, len(keys), len(js), missing))


# The markup a translation must keep: its tags and their attributes, and the
# <code> spans verbatim. Placeholders like {0} in site.js strings too.
TAG = re.compile(r"<(/?)([a-zA-Z0-9]+)([^>]*)>")


def markup(s):
    tags = sorted(m.group(0) for m in TAG.finditer(s))
    codes = sorted(re.findall(r"<code\b[^>]*>.*?</code>", s, flags=re.S))
    return tags, codes


def problems(code):
    cat = load_catalog(code)
    keys, js = all_keys()
    out = []
    for k in keys:
        v = cat["segments"].get(k, "")
        if not v:
            out.append("missing: " + k[:100])
        elif markup(k) != markup(v):
            out.append("markup differs: " + k[:100])
    for k in js:
        v = cat["js"].get(k, "")
        if not v:
            out.append("missing js: " + k[:100])
        elif sorted(re.findall(r"\{\d\}", k)) != sorted(re.findall(r"\{\d\}", v)):
            out.append("placeholders differ: " + k[:100])
    return out


def cmd_check(args):
    failed = False
    for code in args.lang or TRANSLATED:
        found = problems(code)
        for p in found:
            print("%s: %s" % (code, p))
        print("%s: %s" % (code, "complete" if not found else "%d problem(s)" % len(found)))
        failed = failed or bool(found)
    return 1 if failed else 0


def page_url(code, page):
    path = "" if page == "index.html" else page
    return "%s/%s%s" % (SITE, "" if code == "en" else code + "/", path)


# The label of the language menu, for screen readers.
MENU_LABEL = {"en": "Language", "fr": "Langue", "es": "Idioma", "de": "Sprache", "pt": "Idioma", "it": "Lingua"}


def language_menu(code, page):
    current = next(name for c, name, _ in LANGUAGES if c == code)
    links = []
    for c, name, _ in LANGUAGES:
        href = "/" + ("" if c == "en" else c + "/") + ("" if page == "index.html" else page)
        attrs = ' aria-current="true"' if c == code else ""
        links.append('<li><a href="%s" hreflang="%s" lang="%s"%s>%s</a></li>' % (href, c, c, attrs, name))
    return ('<details class="lang-menu"><summary aria-label="%s: %s">%s</summary>'
            '<ul>%s</ul></details>' % (MENU_LABEL[code], current, code.upper(), "".join(links)))


def localize(source, code, page, cat, warn):
    """Returns the page in a language: translated segments, then the head
    and the navigation adapted to that language."""
    out = source
    if code != "en":
        missing = 0
        for s in reversed(segments(source)):
            t = cat["segments"].get(s.key)
            if not t:
                missing += 1
                continue
            out = out[:s.start] + t + out[s.end:]
        if missing:
            warn("%s/%s: %d segment(s) left in English" % (code, page, missing))

    locale = next(loc for c, _, loc in LANGUAGES if c == code)
    out = re.sub(r'<html lang="[^"]*"', '<html lang="%s"' % code, out, count=1)
    if "noindex" not in out:
        url = page_url(code, page)
        out = re.sub(r'(<link rel="canonical" href=")[^"]*"', r'\g<1>%s"' % url, out, count=1)
        out = re.sub(r'(<meta property="og:url" content=")[^"]*"', r'\g<1>%s"' % url, out, count=1)
        alternates = "".join('\n  <link rel="alternate" hreflang="%s" href="%s">' % (c, page_url(c, page)) for c, _, _ in LANGUAGES)
        alternates += '\n  <link rel="alternate" hreflang="x-default" href="%s">' % page_url("en", page)
        others = "".join('\n  <meta property="og:locale:alternate" content="%s">' % loc for c, _, loc in LANGUAGES if c != code)
        out = re.sub(r'(<meta property="og:locale" content=")[^"]*(">)', r'\g<1>%s\g<2>%s' % (locale, others), out, count=1)
        out = re.sub(r'(<link rel="canonical" href="[^"]*">)', lambda m: m.group(1) + alternates, out, count=1)
        if code != "en":
            # Page addresses in the structured data follow the language; the
            # organization and the site keep the root address.
            def ld(m):
                block = re.sub(r'"%s/([a-z-]+\.html)"' % re.escape(SITE),
                               lambda u: '"%s/%s/%s"' % (SITE, code, u.group(1)), m.group(0))
                return block.replace('"item":"%s/"' % SITE, '"item":"%s/%s/"' % (SITE, code))
            out = re.sub(r'<script type="application/ld\+json">.*?</script>', ld, out, flags=re.S)
    else:
        # The 404 page is served for any missing path of its language.
        out = out.replace('<base href="/">', '<base href="/%s">' % ("" if code == "en" else code + "/"))

    # Next to the main navigation, not inside it: on a phone the navigation
    # scrolls sideways and would clip the menu.
    # The header is found by its class: the label of the navigation is
    # translated.
    out, placed = re.subn(r'(<header class="nav">.*?</nav>)', lambda m: m.group(1) + "\n      " + language_menu(code, page),
                          out, count=1, flags=re.S)
    if not placed:
        raise SystemExit("%s/%s: no <header class=\"nav\"> with a <nav> for the language menu" % (code, page))
    if code != "en":
        out = out.replace('<script src="/site.js" defer></script>',
                          '<script src="/%s/i18n.js" defer></script>\n  <script src="/site.js" defer></script>' % code)
    return out


def cmd_build(args):
    dest = args.out
    if os.path.exists(dest):
        shutil.rmtree(dest)
    shutil.copytree(PUBLIC, dest)
    warnings = []
    for code, _, _ in LANGUAGES:
        cat = load_catalog(code) if code != "en" else None
        target = dest if code == "en" else os.path.join(dest, code)
        os.makedirs(target, exist_ok=True)
        for page in pages():
            html_out = localize(read(os.path.join(PUBLIC, page)), code, page, cat, warnings.append)
            with open(os.path.join(target, page), "w", encoding="utf-8") as f:
                f.write(html_out)
        if code != "en":
            strings = {k: v for k, v in cat["js"].items() if v}
            with open(os.path.join(target, "i18n.js"), "w", encoding="utf-8") as f:
                f.write("window.PROBE_I18N = %s;\n" % json.dumps(strings, ensure_ascii=False, sort_keys=True))
    for w in warnings:
        print("warning: " + w, file=sys.stderr)
    print("site written to %s (%d languages)" % (dest, len(LANGUAGES)))
    return 0


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("extract")
    b = sub.add_parser("build")
    b.add_argument("--out", required=True)
    c = sub.add_parser("check")
    c.add_argument("--lang", action="append", choices=TRANSLATED)
    args = p.parse_args()
    return {"extract": cmd_extract, "build": cmd_build, "check": cmd_check}[args.cmd](args) or 0


if __name__ == "__main__":
    sys.exit(main())
