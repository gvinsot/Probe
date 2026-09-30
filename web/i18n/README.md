# Website translations

The website is published in English, French, Spanish, German, Portuguese
(Brazil) and Italian. English pages live in `web/public/` and are the only
source; the other languages are generated at image build by
`web/scripts/i18n.py` into `/fr/`, `/es/`, `/de/`, `/pt/` and `/it/`.

Each catalog, `<lang>.json`, maps an English segment to its translation:

```json
{
  "language": "fr",
  "segments": { "<English segment>": "<translation>" },
  "js": { "Download for {0}": "Télécharger pour {0}" }
}
```

A segment is the content of a paragraph, list item, table cell, heading, link
or button, inline markup included, so a sentence is translated whole. The
`<title>`, the meta description and sharing cards, and the `alt`, `title`,
`placeholder` and `aria-label` attributes are segments too. `js` holds the
strings `site.js` passes to `t("…")`; `{0}`, `{1}`… are its arguments.

## After editing an English page

```sh
python3 web/scripts/i18n.py extract   # new segments appear with "" in every catalog
# translate the empty values
python3 web/scripts/i18n.py check     # nothing missing, markup and placeholders kept
python3 web/scripts/i18n.py build --out /tmp/site && sh web/scripts/check-seo.sh /tmp/site
```

`extract` keeps the existing translations and drops the segments no page uses
any more; an edited English sentence is a new segment. A segment left empty is
published in English, with a warning in the build log, so a copy edit never
blocks a deployment, but `check` fails until every language is complete.

Rules for a translation:

- keep every tag and its attributes, and every `<code>…</code>` verbatim; tags
  may move within the sentence (`check` compares them);
- keep product and brand names, commands, flags, file names and policy keys;
- page titles stay within 60 characters and meta descriptions within 160
  (`check-seo.sh` fails the image build otherwise);
- register: French and German address the reader formally (vous, Sie);
  Spanish, Portuguese and Italian informally (tú, você, tu).

## What the build adds

For every language, English included: the `lang` attribute, the canonical and
`og:url` of the page in that language, `hreflang` alternates for all languages
plus `x-default` (English), `og:locale` and its alternates, the language menu
next to the main navigation, and `/<lang>/i18n.js` loaded before `site.js`.
Page links stay relative, so a visitor stays in their language; assets are
referenced from the root (`/styles.css`), so every language shares them.
