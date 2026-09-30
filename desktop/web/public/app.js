// Probe Desktop interface. It only renders what the local engine computed:
// the list of watched documents and, for a changed one, the report of its
// differences with the reviewed version.
"use strict";

const SEVERITIES = ["low", "medium", "high", "critical"];
const RANK = { none: 0, low: 1, medium: 2, high: 3, critical: 4 };
const KIND_LETTER = { word: "W", excel: "X", powerpoint: "P" };
const STATUS_LABEL = {
  clean: () => t("Reviewed"),
  changed: () => t("Changed"),
  removed: () => t("Deleted"),
  "cloud-only": () => t("Online only"),
  "too-large": () => t("Too large"),
  error: () => t("Unreadable"),
};
// Words the engine sends as identifiers, shown translated.
const SEVERITY_LABEL = { low: () => t("low"), medium: () => t("medium"), high: () => t("high"), critical: () => t("critical") };
const CHANGE_LABEL = { added: () => t("added"), removed: () => t("removed"), modified: () => t("modified"), recomputed: () => t("recomputed") };
const IMPACT_LABEL = { legal: () => t("legal"), financial: () => t("financial") };
const label = (table, key) => (table[key] ? table[key]() : key);

const TABS = ["review", "all", "reviewed"];

const $ = (id) => document.getElementById(id);

const ui = {
  tab: "review",
  selected: null,
  // The Reviewed tab: the latest reviews and the one shown.
  history: [],
  selectedReview: null,
  state: null,
  detail: null,
  detailKey: "",
  settings: null,
  draftSources: [],
  drives: {},
  threshold: 1,
  busy: {},
  // List filters and sort, kept between sessions.
  list: { kind: "", status: "", sort: "risk" },
};

try {
  ui.tab = localStorage.getItem("probe.tab") || "review";
  if (!TABS.includes(ui.tab)) ui.tab = "review";
  ui.threshold = Number(localStorage.getItem("probe.threshold") || 1);
  Object.assign(ui.list, JSON.parse(localStorage.getItem("probe.list") || "{}"));
} catch (_) { /* storage unavailable: keep defaults */ }

// A failure in an event handler must never be silent: show it in the header.
window.addEventListener("unhandledrejection", (e) => showError(e.reason));
window.addEventListener("error", (e) => showError(e.error || e.message));

function showError(err) {
  const s = $("status");
  s.textContent = t("Error: {0}", tr((err && err.message) || String(err)));
  s.className = "status warn";
}

// ---------- HTTP ----------

async function api(method, path, body) {
  const opts = { method, headers: { "X-Probe-Request": "1" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 204 || res.status === 202) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// getSettings tolerates an older engine that sends null for empty lists.
async function getSettings() {
  const s = await api("GET", "/api/settings");
  s.sources = s.sources || [];
  s.google_accounts = s.google_accounts || [];
  s.google = s.google || {};
  s.cloud_folders = s.cloud_folders || [];
  s.keys = s.keys || {};
  s.default_models = s.default_models || {};
  return s;
}

// ---------- helpers ----------

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function ago(iso) {
  if (!iso || iso.startsWith("0001")) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return t("just now");
  if (s < 3600) return t("{0} min ago", Math.floor(s / 60));
  if (s < 86400) return t("{0} h ago", Math.floor(s / 3600));
  if (s < 86400 * 30) return t("{0} d ago", Math.floor(s / 86400));
  return new Date(iso).toLocaleDateString(I18N.lang);
}

function when(iso) {
  if (!iso || iso.startsWith("0001")) return "";
  return new Date(iso).toLocaleString(I18N.lang);
}

function kindBadge(kind) {
  return el("span", { class: `kind ${kind}`, title: kind, text: KIND_LETTER[kind] || "?" });
}

function severityChip(sev) {
  if (!sev || sev === "none") return el("span", { class: "chip ok" }, el("span", { class: "dot" }), t("no finding"));
  return el("span", { class: `chip tone tone-${sev}` }, el("span", { class: "dot" }), label(SEVERITY_LABEL, sev));
}

function statusChip(doc) {
  if (doc.status === "changed") return severityChip(doc.severity);
  if (doc.status === "removed") return el("span", { class: "chip tone tone-high" }, el("span", { class: "dot" }), t("deleted"));
  if (doc.status === "clean") return el("span", { class: "chip ok" }, t("reviewed"));
  return el("span", { class: "chip" }, label(STATUS_LABEL, doc.status));
}

// detailSeverity mirrors the watcher: the AI explanation can raise the
// report severity, never lower it.
function detailSeverity(d) {
  const sev = d.report ? d.report.severity : "none";
  const raised = d.explanation && d.explanation.severity;
  return raised && RANK[raised] > RANK[sev] ? raised : sev;
}

// sourceLabel mirrors config.Source.Label of the engine.
function sourceLabel(src) {
  if (src.type !== "gdrive") return src.path;
  let name = src.drive_name || t("My Drive");
  if (src.folder_id) name += ` › ${src.folder_name || src.folder_id}`;
  return `Google Drive · ${src.account} · ${name}`;
}

function sameFolder(a, b) {
  return a.toLowerCase() === b.toLowerCase();
}

// isExplaining reports an explanation in progress, asked from this window or
// started automatically by the engine when it detected the change.
function isExplaining(id) {
  return ui.busy[id] === "explain" || Boolean(ui.state && (ui.state.explaining || []).includes(id));
}

function needsReview(doc) {
  if (doc.status === "removed") return true;
  return doc.status === "changed" && RANK[doc.severity] >= ui.threshold;
}

// ---------- state ----------

async function refresh() {
  try {
    ui.state = await api("GET", "/api/state");
  } catch (err) {
    $("status").textContent = tr(err.message);
    $("status").className = "status warn";
    return;
  }
  if (ui.tab === "reviewed") {
    try {
      ui.history = await api("GET", "/api/reviewed");
    } catch (err) {
      showError(err);
    }
  }
  renderStatus();
  renderList();
  if (ui.tab === "reviewed") {
    refreshReviewDetail();
    return;
  }
  const doc = ui.state.documents.find((d) => d.id === ui.selected);
  const key = doc ? `${doc.id}|${doc.status}|${doc.changed_at}|${doc.mod_time}|${doc.explained_at}|${isExplaining(doc.id)}` : "";
  if (!doc) {
    if (ui.selected || !ui.detail) renderWelcome();
    ui.selected = null;
  } else if (key !== ui.detailKey) {
    loadDetail(doc.id, key);
  }
}

function renderStatus() {
  const st = ui.state;
  const s = $("status");
  if (st.scanning) {
    s.textContent = t("Scanning…");
    s.className = "status busy";
  } else if (st.scan_error) {
    s.textContent = t("Source unavailable: {0}", st.scan_error);
    s.className = "status warn";
  } else if (st.sources === 0) {
    s.textContent = t("Nothing watched yet");
    s.className = "status";
  } else {
    const last = ago(st.last_scan);
    s.textContent = tn(st.total, "{0} document watched", "{0} documents watched") + (last ? " · " + t("scanned {0}", last) : "");
    s.className = "status";
  }
}

function renderList() {
  const docs = ui.state.documents;
  const review = docs.filter(needsReview);
  $("count-review").textContent = review.length;
  $("count-all").textContent = docs.length;
  $("count-reviewed").textContent = ui.state.reviewed || 0;
  for (const tab of TABS) {
    $(`tab-${tab}`).classList.toggle("on", ui.tab === tab);
    $(`tab-${tab}`).setAttribute("aria-selected", ui.tab === tab);
  }
  // Status and sort do not apply to the history, always newest first.
  $("filter-status").classList.toggle("hidden", ui.tab === "reviewed");
  $("sort").classList.toggle("hidden", ui.tab === "reviewed");
  if (ui.tab === "reviewed") {
    renderHistory();
    return;
  }

  const q = $("search").value.trim().toLowerCase();
  const shown = sortDocs((ui.tab === "review" ? review : docs).filter(
    (d) => (!q || d.name.toLowerCase().includes(q) || d.folder.toLowerCase().includes(q)) && matchesFilters(d),
  ));
  const filtered = Boolean(q || ui.list.kind || ui.list.status);
  const list = $("doc-list");
  list.replaceChildren(
    ...shown.slice(0, 1000).map((d) =>
      el("li", {
        class: `doc${d.id === ui.selected ? " active" : ""}`,
        tabindex: "0",
        onclick: () => select(d.id),
        onkeydown: (e) => { if (e.key === "Enter") select(d.id); },
      },
      el("div", { class: "doc-name" }, kindBadge(d.kind), el("span", { text: d.name })),
      el("div", { class: "doc-meta" },
        statusChip(d),
        d.findings ? el("span", { text: tn(d.findings, "{0} finding", "{0} findings") }) : null,
        el("span", { text: d.folder }),
        d.changed_at && d.status !== "clean" ? el("span", { text: ago(d.changed_at) }) : null,
      )),
    ),
  );
  let empty = "";
  if (ui.state.sources === 0) empty = t("Add a folder or a Google Drive in Settings to start.");
  else if (!shown.length && q) empty = t("No document matches this filter.");
  else if (!shown.length && ui.tab === "review") empty = t("Nothing to review: every watched document matches its reviewed version.");
  else if (!shown.length) empty = ui.state.scanning ? t("First scan in progress…") : t("No Word, Excel or PowerPoint document in the watched sources.");
  $("list-empty").textContent = empty;
  $("list-empty").classList.toggle("hidden", !empty);
}

// renderHistory lists the latest reviews, most recent first.
function renderHistory() {
  const q = $("search").value.trim().toLowerCase();
  const shown = ui.history.filter((r) =>
    (!q || r.name.toLowerCase().includes(q) || r.folder.toLowerCase().includes(q)) && (!ui.list.kind || r.kind === ui.list.kind));
  $("doc-list").replaceChildren(
    ...shown.map((r) =>
      el("li", {
        class: `doc${r.id === ui.selectedReview ? " active" : ""}`,
        tabindex: "0",
        onclick: () => selectReview(r.id),
        onkeydown: (e) => { if (e.key === "Enter") selectReview(r.id); },
      },
      el("div", { class: "doc-name" }, kindBadge(r.kind), el("span", { text: r.name })),
      el("div", { class: "doc-meta" },
        reviewChip(r),
        r.findings ? el("span", { text: tn(r.findings, "{0} finding", "{0} findings") }) : null,
        el("span", { text: r.folder }),
        el("span", { text: t("reviewed {0}", ago(r.reviewed_at)) }),
      )),
    ),
  );
  let empty = "";
  if (!shown.length && (q || ui.list.kind)) empty = t("No review matches this filter.");
  else if (!shown.length) empty = t("No change reviewed yet. Once you mark a change as reviewed, its report stays available here.");
  $("list-empty").textContent = empty;
  $("list-empty").classList.toggle("hidden", !empty);
}

// reviewChip shows what was approved: the severity of the change, or a
// deletion acknowledged.
function reviewChip(r) {
  if (r.status === "removed") return el("span", { class: "chip tone tone-high" }, el("span", { class: "dot" }), t("deletion"));
  return severityChip(r.severity);
}

const OTHER_STATUSES = ["cloud-only", "too-large", "error"];

function matchesFilters(d) {
  const f = ui.list;
  if (f.kind && d.kind !== f.kind) return false;
  if (f.status === "other") return OTHER_STATUSES.includes(d.status);
  return !f.status || d.status === f.status;
}

const byText = (a, b) => a.localeCompare(b, undefined, { sensitivity: "base", numeric: true });
const time = (iso) => (iso && !iso.startsWith("0001") ? new Date(iso).getTime() : 0);

// sortDocs orders a copy of the list. The engine already sends it most risky
// first, which the stable sort keeps as the tie-breaker of every other order.
function sortDocs(docs) {
  const cmp = {
    recent: (a, b) => time(b.changed_at) - time(a.changed_at),
    modified: (a, b) => time(b.mod_time) - time(a.mod_time),
    findings: (a, b) => (b.findings || 0) - (a.findings || 0),
    name: (a, b) => byText(a.name, b.name),
    folder: (a, b) => byText(a.folder, b.folder) || byText(a.name, b.name),
  }[ui.list.sort];
  return cmp ? [...docs].sort(cmp) : docs;
}

function renderFilters() {
  // A value saved by another version of the application falls back to the default.
  for (const [key, id, def] of [["kind", "filter-kind", ""], ["status", "filter-status", ""], ["sort", "sort", "risk"]]) {
    if (![...$(id).options].some((o) => o.value === ui.list[key])) ui.list[key] = def;
  }
  $("filter-kind").value = ui.list.kind;
  $("filter-status").value = ui.list.status;
  $("sort").value = ui.list.sort;
  const f = ui.list;
  $("reset-filters").classList.toggle("hidden", !f.kind && !f.status && f.sort === "risk");
}

function setListOption(key, value) {
  ui.list[key] = value;
  try { localStorage.setItem("probe.list", JSON.stringify(ui.list)); } catch (_) { /* optional */ }
  renderFilters();
  if (ui.state) renderList();
}

function select(id) {
  ui.selected = id;
  ui.detailKey = "";
  renderList();
  refresh();
}

function selectReview(id) {
  ui.selectedReview = id;
  ui.detailKey = "";
  renderList();
  refreshReviewDetail();
}

// refreshReviewDetail shows the selected review, or a hint when none is.
function refreshReviewDetail() {
  const r = ui.history.find((h) => h.id === ui.selectedReview);
  if (!r) {
    ui.selectedReview = null;
    if (ui.detailKey !== "reviewed") {
      ui.detail = null;
      ui.detailKey = "reviewed";
      $("detail").replaceChildren(el("div", { class: "welcome" },
        el("h2", { text: t("Read a review again") }),
        el("p", { text: t("Probe keeps the report of the latest changes you marked as reviewed (up to 50), with the AI explanation you saw. Select one to read it again: this changes nothing in the documents or their reviewed versions.") }),
      ));
    }
    return;
  }
  const key = `review|${r.id}`;
  if (key !== ui.detailKey) loadReview(r.id, key);
}

async function loadReview(id, key) {
  try {
    const r = await api("GET", `/api/reviewed/${encodeURIComponent(id)}`);
    if (ui.selectedReview !== id || ui.tab !== "reviewed") return;
    ui.detail = null;
    ui.detailKey = key;
    renderReview(r);
  } catch (err) {
    renderMessage(tr(err.message), "error");
  }
}

async function loadDetail(id, key) {
  try {
    const doc = await api("GET", `/api/documents/${id}`);
    if (ui.selected !== id) return;
    ui.detail = doc;
    ui.detailKey = key;
    renderDetail();
  } catch (err) {
    renderMessage(tr(err.message), "error");
  }
}

// ---------- detail ----------

function renderMessage(text, kind) {
  $("detail").replaceChildren(el("p", { class: `message ${kind || ""}`, text }));
}

async function renderWelcome() {
  ui.detail = null;
  ui.detailKey = "";
  const panel = $("detail");
  const st = ui.state;
  if (st && st.sources > 0) {
    panel.replaceChildren(el("div", { class: "welcome" },
      el("h2", { text: st.to_review ? t("Select a document to review") : t("All caught up") }),
      el("p", { text: st.to_review
        ? t("Each changed document shows what moved since its reviewed version, most risky first.")
        : t("Probe keeps watching in the background, even when this window is closed. The icon near the clock shows when something needs a look.") }),
    ));
    return;
  }
  const box = el("div", { class: "welcome" },
    el("h2", { text: t("Watch your shared documents") }),
    el("p", { text: t("Probe follows the Word, Excel and PowerPoint files of a folder or of a Google Drive and tells you which modifications deserve a look: a formula replaced by a number, an amount changed in a contract, a softened obligation, a hidden sheet…") }),
  );
  panel.replaceChildren(box);
  try {
    const s = await getSettings();
    const buttons = s.cloud_folders.map((f) =>
      el("button", { class: "btn ghost small", title: f.path, onclick: () => quickAdd(f.path) }, `+ ${f.label}`));
    box.append(
      buttons.length
        ? el("div", { class: "cloud-folders" }, ...buttons)
        : el("p", { text: t("No synchronized folder was detected on this computer. Choose any folder, or connect a Google Drive, in the settings.") }),
      el("div", { class: "actions" }, el("button", { class: "btn", onclick: openSettings }, t("Open settings"))),
    );
  } catch (_) { /* the settings dialog stays available */ }
}

async function quickAdd(path) {
  try {
    const s = await getSettings();
    if (!s.sources.some((src) => src.type === "folder" && sameFolder(src.path, path))) s.sources.push({ type: "folder", path });
    await api("PUT", "/api/settings", s);
    ui.detail = null;
    await refresh();
  } catch (err) {
    alert(tr(err.message));
  }
}

function renderDetail() {
  const d = ui.detail;
  const panel = $("detail");
  const name = d.name || d.path.split(/[\\/]/).pop();
  const head = el("div", { class: "detail-head" },
    el("div", { class: "detail-title" }, kindBadge(d.kind), el("h2", { text: name }), statusChip({ ...d, severity: detailSeverity(d) })),
    el("p", { class: "detail-sub mono", text: d.path }),
    el("p", { class: "detail-sub", text: metaLine(d) }),
    actions(d),
  );
  const parts = [head];

  if (d.error) parts.push(el("p", { class: "message error", text: tr(d.error) }));
  if (d.status === "removed") {
    parts.push(el("p", { class: "message warn", text: t("This document was deleted, moved or renamed since its reviewed version. Acknowledge to stop tracking it; if it was moved inside a watched folder, it is already tracked under its new name.") }));
  } else if (d.status === "clean") {
    parts.push(el("p", { class: "message", text: t("This document matches its reviewed version.") }));
  } else if (d.status === "cloud-only") {
    parts.push(el("p", { class: "message", text: t("This file is only in the cloud: Probe will record its reviewed version once it is downloaded, or enable the download option in Settings.") }));
  }

  parts.push(...reportParts(d));
  panel.replaceChildren(...parts);
}

// reportParts renders the AI explanation, the findings and the changes of a
// report, for a pending change or a past review.
function reportParts(d) {
  const parts = [];
  if (d.explanation) {
    if (d.report && RANK[d.explanation.severity] > RANK[d.report.severity]) {
      const impacts = (d.explanation.impacts || []).map((i) => label(IMPACT_LABEL, i));
      const message = impacts.length > 1
        ? t("Severity raised from {0} to {1}: the model states this modification may have {2} and {3} consequences.", label(SEVERITY_LABEL, d.report.severity), label(SEVERITY_LABEL, d.explanation.severity), impacts[0], impacts[1])
        : t("Severity raised from {0} to {1}: the model states this modification may have {2} consequences.", label(SEVERITY_LABEL, d.report.severity), label(SEVERITY_LABEL, d.explanation.severity), impacts[0] || "");
      parts.push(el("p", { class: `message tone tone-${d.explanation.severity}`, text: message }));
    }
    // The model's own findings come first; its prose explanation follows,
    // collapsed when there are findings to look at.
    const extra = d.explanation.findings || [];
    if (extra.length) {
      parts.push(el("h3", { class: "section-title", text: t("Raised by AI ({0})", extra.length) }));
      parts.push(el("ul", { class: "findings ai-findings" }, ...extra.map((f) => findingItem(f))));
      parts.push(el("p", { class: "note", text: t("Suggestions from the model, not rule results: check them in the document.") }));
    }
    parts.push(el("details", { class: "explanation", open: !extra.length },
      el("summary", { text: d.explanation.outdated ? t("AI analysis (earlier version)") : t("AI analysis") }),
      d.explanation.outdated
        ? el("p", { class: "note", text: t("Written for an earlier version of the modifications: the AI findings about elements modified again were removed and the latest changes are not covered. Explain again for a complete analysis.") })
        : null,
      el("p", { text: d.explanation.text }),
      el("span", { class: "note", text: t("Explanation by {0} · {1} · the findings below remain the reference", d.explanation.model, ago(d.explanation.at)) }),
    ));
  }

  const r = d.report;
  if (r) {
    parts.push(el("h3", { class: "section-title", text: r.findings.length ? t("Findings ({0})", r.findings.length) : t("No risky modification detected") }));
    if (r.findings.length) {
      parts.push(el("ul", { class: "findings" }, ...r.findings.map((f, i) => findingItem(f, readingFor(d.explanation, f, i)))));
    }
    parts.push(changesTable(r));
  }
  return parts;
}

// renderReview shows a past review read-only: the report as it was when the
// change was marked as reviewed.
function renderReview(r) {
  const doc = ui.state && ui.state.documents.find((d) => d.id === r.doc_id);
  const box = el("div", { class: "actions" });
  if (doc && doc.status !== "removed") {
    box.append(el("button", { class: "btn quiet small", onclick: () => run(doc.id, "open") }, r.link ? t("Open in Google Drive") : t("Open document")));
    if (doc.status !== "clean") {
      box.append(el("button", { class: "btn ghost small", onclick: () => { setTab("review"); select(doc.id); } }, t("See its new changes")));
    }
  }
  const bits = [t("Reviewed {0}", when(r.reviewed_at))];
  if (r.changed_at) bits.push(t("change detected {0}", when(r.changed_at)));
  if (r.report && r.report.last_modified_by) bits.push(t("saved by {0}", r.report.last_modified_by));
  if (r.baseline_at) bits.push(t("compared with the version from {0}", when(r.baseline_at)));
  const parts = [
    el("div", { class: "detail-head" },
      el("div", { class: "detail-title" }, kindBadge(r.kind), el("h2", { text: r.name }), reviewChip(r), el("span", { class: "chip ok" }, t("reviewed"))),
      el("p", { class: "detail-sub mono", text: r.path }),
      el("p", { class: "detail-sub", text: bits.join(" · ") }),
      box,
    ),
    el("p", { class: "message", text: r.status === "removed"
      ? t("You acknowledged that this document was deleted, moved or renamed.")
      : t("Read-only: the report as it was when you marked this change as reviewed. The reviewed version has been the reference for the next changes since then.") }),
  ];
  parts.push(...reportParts(r));
  $("detail").replaceChildren(...parts);
}

function metaLine(d) {
  const bits = [];
  if (d.changed_at && d.status !== "clean") bits.push(t("Change detected {0}", ago(d.changed_at)));
  if (d.report && d.report.last_modified_by) bits.push(t("last saved by {0}", d.report.last_modified_by));
  if (d.baseline_at) bits.push(t("reviewed version from {0}", when(d.baseline_at)));
  return bits.join(" · ");
}

function actions(d) {
  const box = el("div", { class: "actions" });
  if (d.status !== "removed") {
    box.append(el("button", { class: "btn quiet small", onclick: () => run(d.id, "open") }, d.link ? t("Open in Google Drive") : t("Open document")));
  }
  if (d.report) {
    const configured = ui.state && ui.state.ai_configured;
    const explaining = isExplaining(d.id);
    box.append(el("button", {
      class: "btn ghost small",
      disabled: explaining,
      title: configured ? t("Send the findings and changed excerpts to the AI provider") : t("Configure an AI provider in Settings"),
      onclick: () => (configured ? explain(d.id) : openSettings()),
    }, explaining ? t("Explaining…") : d.explanation ? t("Explain again") : t("Explain with AI")));
  }
  if (d.status === "changed" || d.status === "removed") {
    box.append(el("button", {
      class: "btn small",
      title: d.status === "removed" ? t("Stop tracking this document") : t("This version becomes the reference for the next changes"),
      onclick: () => accept(d.id),
    }, d.status === "removed" ? t("Acknowledge") : t("Mark as reviewed")));
  }
  return box;
}

// readingFor returns the AI reading of the rule finding at index i, if the
// explanation has one for that very finding.
function readingFor(explanation, f, i) {
  const readings = (explanation && explanation.readings) || [];
  return readings.find((r) => r.finding === i && r.rule === f.rule && (r.location || "") === (f.location || ""));
}

const CONSISTENCY = {
  inconsistent: () => t("No longer consistent with the rest of the document"),
  consistent: () => t("Consistent with the rest of the document"),
};

// findingItem renders a finding; reading is the AI reading of a rule
// finding: a title in the words of the document and its consistency.
function findingItem(f, reading) {
  const item = el("li", { class: `finding tone-${f.severity}` },
    el("span", { class: "dot", title: label(SEVERITY_LABEL, f.severity) }),
    el("span", { class: "finding-title", text: tr(f.title) }),
  );
  if (reading && reading.title) {
    item.append(el("p", { class: "finding-reading", title: t("AI reading of this finding") }, el("span", { class: "ai-tag", text: t("AI") }), reading.title));
  }
  if (f.location) item.append(el("span", { class: "finding-where mono", text: `${tr(f.location)} · ${label(SEVERITY_LABEL, f.severity)}` }));
  else item.append(el("span", { class: "finding-where", text: label(SEVERITY_LABEL, f.severity) }));
  // The AI note, in the reader's language, takes precedence over the rule's.
  const verdict = (reading && reading.consistency) || f.consistency;
  const note = (reading && reading.note) || tr(f.note);
  if (verdict || note) {
    item.append(el("p", { class: `consistency ${verdict || ""}` },
      verdict ? el("strong", { text: `${label(CONSISTENCY, verdict)}.` }) : null,
      note ? ` ${note}` : null,
    ));
  }
  if (f.before || f.after) {
    const d = highlighted(f.before, f.after);
    item.append(el("div", { class: "compare" },
      f.before ? el("p", { class: "before" }, d.before) : null,
      f.after ? el("p", { class: "after" }, d.after) : null,
    ));
  }
  return item;
}

// highlighted returns both excerpts as nodes where the exact characters that
// changed are marked. An excerpt without counterpart is shown as is: all of
// it was added or removed.
function highlighted(before, after) {
  before = before || "";
  after = after || "";
  if (!before || !after) return { before: [before], after: [after] };
  const d = inlineDiff(before, after);
  const nodes = (segments, cls) => segments.map((s) =>
    (s.changed ? el("mark", { class: cls, text: s.text }) : document.createTextNode(s.text)));
  return { before: nodes(d.before, "del"), after: nodes(d.after, "ins") };
}

function changesTable(r) {
  const rows = r.changes.map((c) => {
    // Values the engine wrote itself ("Track changes on") are translated;
    // document content is not, since it matches no message.
    const d = highlighted(tr(c.before), tr(c.after));
    return el("tr", {},
      el("td", { class: "kind-cell", text: label(CHANGE_LABEL, c.kind) }),
      el("td", { class: "mono", text: tr(c.location) }),
      el("td", { class: c.before ? "b" : "" }, d.before),
      el("td", { class: c.after ? "a" : "" }, d.after),
    );
  });
  const summary = r.truncated
    ? t("All changes ({0}, first {1} shown)", r.change_count, r.changes.length)
    : t("All changes ({0})", r.change_count);
  return el("details", { class: "changes" },
    el("summary", { text: summary }),
    el("table", {},
      el("thead", {}, el("tr", {}, el("th", { text: t("Change") }), el("th", { text: t("Where") }), el("th", { text: t("Before") }), el("th", { text: t("After") }))),
      el("tbody", {}, ...rows),
    ),
  );
}

async function run(id, action) {
  try {
    await api("POST", `/api/documents/${id}/${action}`);
  } catch (err) {
    alert(tr(err.message));
  }
}

async function accept(id) {
  try {
    await api("POST", `/api/documents/${id}/accept`);
    ui.detailKey = "";
    // Move on to the next document to review, like a queue.
    const next = ui.state.documents.find((d) => d.id !== id && needsReview(d));
    ui.selected = next ? next.id : null;
  } catch (err) {
    alert(tr(err.message));
  }
  await refresh();
}

async function explain(id) {
  ui.busy[id] = "explain";
  renderDetail();
  try {
    const e = await api("POST", `/api/documents/${id}/explain`);
    if (ui.detail && ui.detail.id === id) ui.detail.explanation = e;
  } catch (err) {
    alert(tr(err.message));
  } finally {
    delete ui.busy[id];
    if (ui.detail && ui.detail.id === id) renderDetail();
  }
}

// ---------- settings ----------

async function openSettings() {
  $("settings-error").textContent = "";
  try {
    ui.settings = await getSettings();
  } catch (err) {
    alert(tr(err.message));
    return;
  }
  const s = ui.settings;
  ui.draftSources = s.sources.map((src) => ({ ...src }));
  $("folder-path").value = "";
  $("google-folder").value = "";
  $("google-client-id").value = s.google_client_id || "";
  $("google-client-secret").value = "";
  $("google-status").textContent = "";
  $("download-cloud").checked = !!s.download_cloud_files;
  $("scan-seconds").value = String(s.scan_seconds);
  if (!$("scan-seconds").value) $("scan-seconds").value = "60";
  $("provider").value = s.provider || "";
  $("model").value = s.model || "";
  $("base-url").value = s.base_url || "";
  $("language").value = s.language || I18N.lang;
  $("api-key").value = "";
  $("clear-key").checked = false;
  $("browse-folder").classList.toggle("hidden", typeof window.probePickFolder !== "function");
  renderSources();
  renderGoogle();
  renderProvider();
  $("settings").showModal();
}

const INTERVALS = [[0, () => t("Default pace")], [60, () => t("Every minute")], [300, () => t("Every 5 min")], [900, () => t("Every 15 min")], [3600, () => t("Every hour")]];

function renderSources() {
  $("source-list").replaceChildren(...ui.draftSources.map((src, i) => {
    const pace = el("select", {
      title: t("How often this source is scanned. A network share or a large drive deserves a slower pace."),
      onchange: (e) => { src.scan_seconds = Number(e.target.value); },
    }, ...INTERVALS.map(([v, name]) => el("option", { value: String(v), selected: (src.scan_seconds || 0) === v }, name())));
    return el("li", {},
      el("span", { class: "source-kind", text: src.type === "gdrive" ? t("API") : t("Folder") }),
      el("span", { class: src.type === "gdrive" ? "" : "mono", text: sourceLabel(src) }),
      pace,
      el("button", { class: "btn quiet small", type: "button", onclick: () => { ui.draftSources.splice(i, 1); renderSources(); } }, t("Remove")));
  }));
  const candidates = ui.settings.cloud_folders.filter((c) => !hasFolder(c.path));
  $("cloud-folders").replaceChildren(...candidates.map((c) =>
    el("button", { class: "btn ghost small", type: "button", title: c.path, onclick: () => addFolder(c.path) }, `+ ${c.label}`)));
}

function hasFolder(path) {
  return ui.draftSources.some((src) => src.type === "folder" && sameFolder(src.path, path));
}

function addFolder(path) {
  path = path.trim();
  if (!path || hasFolder(path)) return;
  ui.draftSources.push({ type: "folder", path });
  renderSources();
}

async function browseFolder() {
  try {
    const path = await window.probePickFolder();
    if (path) addFolder(path);
  } catch (err) {
    $("settings-error").textContent = tr(String((err && err.message) || err));
  }
}

// ---------- Google Drive ----------

function renderGoogle() {
  const s = ui.settings;
  $("google-section").classList.toggle("hidden", !s.google.available);
  if (!s.google.available) return;
  $("google-accounts").replaceChildren(...s.google_accounts.map((a) =>
    el("li", {}, el("span", { text: a }),
      el("button", { class: "btn quiet small", type: "button", onclick: () => disconnectGoogle(a) }, t("Disconnect")))));
  const needsClient = !s.google.builtin_client && !s.google_client_id;
  $("google-client").open = needsClient;
  $("google-client-secret").placeholder = s.google.secret_saved ? t("A secret is saved; type a new one to replace it") : "";
  $("google-add").classList.toggle("hidden", !s.google_accounts.length);
  const select = $("google-account");
  const current = select.value;
  select.replaceChildren(...s.google_accounts.map((a) => el("option", { value: a, selected: a === current }, a)));
  if (s.google_accounts.length) loadDrives(select.value);
}

async function loadDrives(account) {
  const select = $("google-drive");
  if (!ui.drives[account]) {
    select.replaceChildren(el("option", { value: "" }, t("Loading…")));
    try {
      ui.drives[account] = await api("GET", `/api/google/drives?account=${encodeURIComponent(account)}`);
    } catch (err) {
      select.replaceChildren(el("option", { value: "" }, t("My Drive")));
      $("google-status").textContent = tr(err.message);
      return;
    }
  }
  if ($("google-account").value !== account) return;
  select.replaceChildren(...ui.drives[account].map((d) => el("option", { value: d.id }, d.name)));
}

async function connectGoogle() {
  const status = $("google-status");
  $("settings-error").textContent = "";
  try {
    await api("POST", "/api/google/connect", {
      client_id: $("google-client-id").value.trim(),
      client_secret: $("google-client-secret").value,
    });
  } catch (err) {
    status.textContent = tr(err.message);
    return;
  }
  $("google-client-secret").value = "";
  status.textContent = t("Finish in the browser window that just opened…");
  const started = Date.now();
  while (Date.now() - started < 5 * 60 * 1000) {
    await new Promise((r) => setTimeout(r, 1500));
    let st;
    try {
      st = await api("GET", "/api/google/connect");
    } catch (err) {
      status.textContent = tr(err.message);
      return;
    }
    if (st.state === "pending") continue;
    if (st.state === "error") {
      status.textContent = st.error;
      return;
    }
    status.textContent = t("Connected: {0}", st.account);
    await reloadGoogle();
    $("google-account").value = st.account;
    loadDrives(st.account);
    return;
  }
  status.textContent = t("The connection was not completed.");
}

// reloadGoogle refreshes the accounts without losing the sources being edited.
async function reloadGoogle() {
  const fresh = await getSettings();
  ui.settings.google_accounts = fresh.google_accounts;
  ui.settings.google_client_id = fresh.google_client_id;
  ui.settings.google = fresh.google;
  renderGoogle();
}

async function disconnectGoogle(account) {
  if (!confirm(t("Disconnect {0}? Probe forgets its Google token.", account))) return;
  try {
    await api("DELETE", `/api/google/accounts/${encodeURIComponent(account)}`);
    delete ui.drives[account];
    await reloadGoogle();
  } catch (err) {
    $("settings-error").textContent = tr(err.message);
  }
}

async function addGoogleSource() {
  const account = $("google-account").value;
  const driveSelect = $("google-drive");
  const drive = driveSelect.selectedOptions[0];
  const src = { type: "gdrive", account, drive_id: driveSelect.value, drive_name: drive && driveSelect.value ? drive.textContent : "" };
  const ref = $("google-folder").value.trim();
  $("settings-error").textContent = "";
  if (ref) {
    try {
      const f = await api("GET", `/api/google/folder?account=${encodeURIComponent(account)}&ref=${encodeURIComponent(ref)}`);
      Object.assign(src, { folder_id: f.id, folder_name: f.name, drive_id: f.drive_id, drive_name: f.drive_name });
    } catch (err) {
      $("settings-error").textContent = tr(err.message);
      return;
    }
  }
  const exists = ui.draftSources.some((o) => o.type === "gdrive" && o.account === src.account &&
    (o.drive_id || "") === (src.drive_id || "") && (o.folder_id || "") === (src.folder_id || ""));
  if (!exists) ui.draftSources.push(src);
  $("google-folder").value = "";
  renderSources();
}

function renderProvider() {
  const p = $("provider").value;
  document.querySelectorAll(".ai-only").forEach((n) => n.classList.toggle("hidden", !p));
  document.querySelectorAll(".openai-only").forEach((n) => n.classList.toggle("hidden", p !== "openai"));
  const s = ui.settings;
  $("model").placeholder = p && s.default_models[p] ? t("Default: {0}", s.default_models[p]) : "";
  const saved = p && s.keys[p];
  $("api-key").placeholder = saved ? t("A key is saved; type a new one to replace it") : p === "openai" ? t("sk-… (optional for a local server)") : "sk-ant-…";
  $("clear-key-label").classList.toggle("hidden", !saved);
}

async function saveSettings() {
  const path = $("folder-path").value.trim();
  const typed = path && !hasFolder(path);
  if (typed) {
    ui.draftSources.push({ type: "folder", path });
    $("folder-path").value = "";
  }
  const body = {
    sources: ui.draftSources,
    provider: $("provider").value,
    model: $("model").value.trim(),
    base_url: $("base-url").value.trim(),
    language: $("language").value,
    scan_seconds: Number($("scan-seconds").value),
    max_file_mb: ui.settings.max_file_mb,
    download_cloud_files: $("download-cloud").checked,
    api_key: $("api-key").value,
    clear_key: $("clear-key").checked,
    google_client_id: $("google-client-id").value.trim(),
    google_client_secret: $("google-client-secret").value,
  };
  try {
    ui.settings = await api("PUT", "/api/settings", body);
    $("settings").close();
    // A new language needs the page in that language: reload it.
    if (ui.settings.language && ui.settings.language !== I18N.lang) {
      location.reload();
      return;
    }
    await refresh();
  } catch (err) {
    if (typed) ui.draftSources.pop();
    renderSources();
    $("settings-error").textContent = tr(err.message);
  }
}

// ---------- wiring ----------

function setThreshold(value) {
  ui.threshold = Number(value) + 1;
  $("severity").value = String(ui.threshold - 1);
  $("severity-value").textContent = label(SEVERITY_LABEL, SEVERITIES[ui.threshold - 1]);
  try { localStorage.setItem("probe.threshold", String(ui.threshold)); } catch (_) { /* optional */ }
}

function setTab(tab) {
  const was = ui.tab;
  ui.tab = tab;
  try { localStorage.setItem("probe.tab", tab); } catch (_) { /* optional */ }
  // Entering or leaving the history changes what the detail panel shows.
  if ((was === "reviewed") !== (tab === "reviewed")) {
    ui.detailKey = "";
    refresh();
  } else if (ui.state) {
    renderList();
  }
}

$("tab-review").addEventListener("click", () => setTab("review"));
$("tab-all").addEventListener("click", () => setTab("all"));
$("tab-reviewed").addEventListener("click", () => setTab("reviewed"));
$("search").addEventListener("input", () => ui.state && renderList());
$("filter-kind").addEventListener("change", (e) => setListOption("kind", e.target.value));
$("filter-status").addEventListener("change", (e) => setListOption("status", e.target.value));
$("sort").addEventListener("change", (e) => setListOption("sort", e.target.value));
$("reset-filters").addEventListener("click", () => {
  ui.list = { kind: "", status: "", sort: "risk" };
  $("search").value = "";
  setListOption("sort", "risk");
});
$("severity").addEventListener("input", (e) => { setThreshold(e.target.value); if (ui.state) renderList(); });
$("scan").addEventListener("click", async () => { await api("POST", "/api/scan").catch((e) => alert(tr(e.message))); setTimeout(refresh, 400); });
$("open-settings").addEventListener("click", openSettings);
$("provider").addEventListener("change", renderProvider);
$("save-settings").addEventListener("click", saveSettings);
$("add-folder").addEventListener("click", () => {
  addFolder($("folder-path").value);
  $("folder-path").value = "";
});
$("browse-folder").addEventListener("click", browseFolder);
$("google-connect").addEventListener("click", connectGoogle);
$("google-account").addEventListener("change", (e) => loadDrives(e.target.value));
$("add-google").addEventListener("click", addGoogleSource);

// Start in the language of the settings, then poll: faster while a scan
// runs, so the first results appear quickly.
(async function start() {
  try {
    const st = await api("GET", "/api/state");
    await loadLanguage(st.language);
  } catch (_) { /* refresh reports the engine error */ }
  setThreshold(Math.min(Math.max(ui.threshold - 1, 0), 3));
  renderFilters();
  refresh();
  (function loop() {
    setTimeout(async () => {
      if (!document.hidden) await refresh();
      loop();
    }, ui.state && ui.state.scanning ? 1500 : 4000);
  })();
})();
