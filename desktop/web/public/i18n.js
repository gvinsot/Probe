// Translation of the interface. The catalogs, i18n/<lang>.json, map each
// English message to its translation; English has no catalog. Three kinds of
// messages share them:
//   - the strings of app.js, written t("…") with {0}, {1}… for arguments;
//   - the text and the placeholder, title and aria-label attributes of
//     index.html, translated once the catalog is loaded;
//   - the text the engine wrote into reports and errors, marked msg.M in the
//     Go sources. It is translated when displayed (tr), so a report saved in
//     English or by an earlier version is shown in the current language.
// internal/i18n checks that every message has a translation in each catalog.
"use strict";

const I18N = { lang: "en", messages: {}, patterns: [] };

// t translates a string of the interface; {n} are replaced by the arguments.
function t(english, ...args) {
  const s = I18N.messages[english] || english;
  return args.length ? s.replace(/\{(\d+)\}/g, (m, i) => (i < args.length ? String(args[i]) : m)) : s;
}

// tn picks the singular or the plural message for a count, with {0} = n.
function tn(n, one, other) {
  return n === 1 ? t(one, n) : t(other, n);
}

const VERB = /%[dsqv]/g;

// compile turns the Go formats of the catalog into patterns: %d matches a
// number, %q a quoted string, %s and %v any text.
function compile(messages) {
  const out = [];
  for (const [key, value] of Object.entries(messages)) {
    const verbs = key.match(VERB);
    if (!verbs || !value) continue;
    const parts = key.split(VERB).map((p) => p.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
    let src = "^" + parts[0];
    verbs.forEach((v, i) => {
      src += (v === "%d" ? "(-?\\d+)" : v === "%q" ? '("(?:[^"\\\\]|\\\\.)*")' : "(.+?)") + parts[i + 1];
    });
    out.push({ re: new RegExp(src + "$"), verbs, value, weight: key.replace(VERB, "").length });
  }
  // The most specific formats first: "%s %d (baseline)" before "%s %d".
  return out.sort((a, b) => b.weight - a.weight);
}

// tr translates a text the engine produced: an exact message, or a Go format
// whose text arguments are translated in turn ("Paragraph" in "Paragraph 3").
// Anything else, such as document content, is returned unchanged.
function tr(text, depth = 0) {
  if (!text || I18N.lang === "en") return text;
  const exact = I18N.messages[text];
  if (exact) return exact;
  if (depth > 3) return text;
  for (const p of I18N.patterns) {
    const m = p.re.exec(text);
    if (!m) continue;
    return p.value.replace(/\{(\d+)\}/g, (all, i) => {
      const arg = m[+i + 1];
      if (arg === undefined) return all;
      const verb = p.verbs[+i];
      return verb === "%s" || verb === "%v" ? trList(arg, depth + 1) : arg;
    });
  }
  return text;
}

// trList also translates a list of labels, such as "Paragraph 1, Slide 2",
// but only when every item is a message: document text stays as it is.
function trList(text, depth) {
  const whole = tr(text, depth);
  if (whole !== text || !text.includes(", ")) return whole;
  const items = text.split(", ");
  const done = items.map((s) => tr(s, depth));
  return done.every((s, i) => s !== items[i]) ? done.join(", ") : text;
}

// translateDom translates the static text of the page, except inside
// translate="no" elements.
function translateDom(root) {
  const skip = (node) => node.closest && node.closest('[translate="no"], script, style');
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const texts = [];
  while (walker.nextNode()) texts.push(walker.currentNode);
  for (const node of texts) {
    if (skip(node.parentElement)) continue;
    const raw = node.nodeValue;
    const key = raw.replace(/\s+/g, " ").trim();
    const value = key && I18N.messages[key];
    if (value) node.nodeValue = raw.slice(0, raw.length - raw.trimStart().length) + value + raw.slice(raw.trimEnd().length);
  }
  for (const el of root.querySelectorAll("[placeholder], [title], [aria-label]")) {
    if (skip(el)) continue;
    for (const attr of ["placeholder", "title", "aria-label"]) {
      const v = el.getAttribute(attr);
      const key = v && v.replace(/\s+/g, " ").trim();
      if (key && I18N.messages[key]) el.setAttribute(attr, I18N.messages[key]);
    }
  }
}

// loadLanguage loads a catalog and translates the page. A missing catalog
// leaves the interface in English.
async function loadLanguage(lang) {
  if (!lang || lang === "en") return;
  try {
    const res = await fetch(`i18n/${encodeURIComponent(lang)}.json`);
    if (!res.ok) return;
    const catalog = await res.json();
    I18N.lang = lang;
    I18N.messages = catalog.messages || {};
    I18N.patterns = compile(I18N.messages);
  } catch (_) {
    return;
  }
  document.documentElement.lang = lang;
  translateDom(document.body);
}
