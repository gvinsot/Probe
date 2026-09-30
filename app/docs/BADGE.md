# "Verified by Probe" badge

A repository that reviews its changes with Probe can say so in its README:

[![verified by Probe](https://probe.technology/badge/verified-by-probe.svg)](https://probe.technology/)

The badge is a static image served by the Probe website, so any repository can
use it without an account. It links back to the website.

## What the badge means

The badge states that the project runs Probe on its changes, for example
`probe lint` or `probe review` in CI ([CI.md](CI.md)) or in the agent workflow
([AGENT_WORKFLOW.md](AGENT_WORKFLOW.md)). It does not mean that Probe approved
the code, and it is not a correctness guarantee: Probe produces a review plan
and evidence, and people still decide. Only add the badge when Probe actually
runs on the repository's pull requests.

For the result of the latest run instead of a static claim, use the
[live Hub badge](#live-result-from-probe-hub).

## Snippets

Markdown:

```markdown
[![verified by Probe](https://probe.technology/badge/verified-by-probe.svg)](https://probe.technology/)
```

HTML:

```html
<a href="https://probe.technology/"><img src="https://probe.technology/badge/verified-by-probe.svg" alt="verified by Probe" height="20"></a>
```

reStructuredText:

```rst
.. image:: https://probe.technology/badge/verified-by-probe.svg
   :target: https://probe.technology/
   :alt: verified by Probe
```

### shields.io styles

To match the other shields.io badges of a README (`flat-square`,
`for-the-badge`, `social`…), use the endpoint description the website also
serves:

```markdown
[![verified by Probe](https://img.shields.io/endpoint?url=https%3A%2F%2Fprobe.technology%2Fbadge%2Fverified-by-probe.json&style=flat-square)](https://probe.technology/)
```

Change `style=` to any shields.io style. This variant has no logo.

## Live result from Probe Hub

A repository monitored by [Probe Hub](../../hub/README.md) also has a live
badge showing the latest recorded result (`no blocker`, `3 to review`,
`reproduced issue`, `running`…). The Hub returns its URL, with a random badge
key, when the repository is monitored:

```markdown
[![probe latest](https://HUB_HOST/badge/BADGE_KEY.svg)](https://probe.technology/)
```

The key is separate from the webhook key and reveals nothing else about the
repository. A repository can show both badges: the static one says the project
uses Probe, the live one shows the latest run.

## Files

| URL | Source |
| --- | --- |
| `https://probe.technology/badge/verified-by-probe.svg` | [`web/public/badge/verified-by-probe.svg`](../../web/public/badge/verified-by-probe.svg) |
| `https://probe.technology/badge/verified-by-probe.json` | [`web/public/badge/verified-by-probe.json`](../../web/public/badge/verified-by-probe.json) (shields.io endpoint schema) |

Both URLs are stable: do not rename them, since external READMEs point to them.
The website serves them with `Cache-Control: no-cache` like its other assets.
GitHub proxies README images through its camo cache.
