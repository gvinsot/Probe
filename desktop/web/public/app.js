// Probe Desktop interface. It only renders what the local engine computed:
// the list of watched documents and, for a changed one, the report of its
// differences with the reviewed version.
"use strict";

const SEVERITIES = ["low", "medium", "high", "critical"];
const RANK = { none: 0, low: 1, medium: 2, high: 3, critical: 4 };
const KIND_LETTER = { word: "W", excel: "X", powerpoint: "P" };
const STATUS_LABEL = {
  clean: "Reviewed",
  changed: "Changed",
  removed: "Deleted",
  "cloud-only": "Online only",
  "too-large": "Too large",
  error: "Unreadable",
};

const $ = (id) => document.getElementById(id);

const ui = {
  tab: "review",
  selected: null,
  state: null,
  detail: null,
  detailKey: "",
  settings: null,
  draftFolders: [],
  threshold: 1,
  busy: {},
};

try {
  ui.tab = localStorage.getItem("probe.tab") || "review";
  ui.threshold = Number(localStorage.getItem("probe.threshold") || 1);
} catch (_) { /* storage unavailable: keep defaults */ }

// A failure in an event handler must never be silent: show it in the header.
window.addEventListener("unhandledrejection", (e) => showError(e.reason));
window.addEventListener("error", (e) => showError(e.error || e.message));

function showError(err) {
  const s = $("status");
  s.textContent = `Error: ${(err && err.message) || err}`;
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
  s.folders = s.folders || [];
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
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  if (s < 86400 * 30) return `${Math.floor(s / 86400)} d ago`;
  return new Date(iso).toLocaleDateString();
}

function when(iso) {
  if (!iso || iso.startsWith("0001")) return "";
  return new Date(iso).toLocaleString();
}

function kindBadge(kind) {
  return el("span", { class: `kind ${kind}`, title: kind, text: KIND_LETTER[kind] || "?" });
}

function severityChip(sev) {
  if (!sev || sev === "none") return el("span", { class: "chip ok" }, el("span", { class: "dot" }), "no finding");
  return el("span", { class: `chip tone tone-${sev}` }, el("span", { class: "dot" }), sev);
}

function statusChip(doc) {
  if (doc.status === "changed") return severityChip(doc.severity);
  if (doc.status === "removed") return el("span", { class: "chip tone tone-high" }, el("span", { class: "dot" }), "deleted");
  if (doc.status === "clean") return el("span", { class: "chip ok" }, "reviewed");
  return el("span", { class: "chip" }, STATUS_LABEL[doc.status] || doc.status);
}

// detailSeverity mirrors the watcher: the AI explanation can raise the
// report severity, never lower it.
function detailSeverity(d) {
  const sev = d.report ? d.report.severity : "none";
  const raised = d.explanation && d.explanation.severity;
  return raised && RANK[raised] > RANK[sev] ? raised : sev;
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
    $("status").textContent = err.message;
    $("status").className = "status warn";
    return;
  }
  renderStatus();
  renderList();
  const doc = ui.state.documents.find((d) => d.id === ui.selected);
  const key = doc ? `${doc.id}|${doc.status}|${doc.changed_at}|${doc.mod_time}` : "";
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
    s.textContent = "Scanning…";
    s.className = "status busy";
  } else if (st.scan_error) {
    s.textContent = `Folder unavailable: ${st.scan_error}`;
    s.className = "status warn";
  } else if (st.folders === 0) {
    s.textContent = "No folder watched yet";
    s.className = "status";
  } else {
    const last = ago(st.last_scan);
    s.textContent = `${st.total} document${st.total === 1 ? "" : "s"} watched${last ? ` · scanned ${last}` : ""}`;
    s.className = "status";
  }
}

function renderList() {
  const docs = ui.state.documents;
  const review = docs.filter(needsReview);
  $("count-review").textContent = review.length;
  $("count-all").textContent = docs.length;
  $("tab-review").classList.toggle("on", ui.tab === "review");
  $("tab-all").classList.toggle("on", ui.tab === "all");
  $("tab-review").setAttribute("aria-selected", ui.tab === "review");
  $("tab-all").setAttribute("aria-selected", ui.tab === "all");

  const q = $("search").value.trim().toLowerCase();
  const shown = (ui.tab === "review" ? review : docs).filter(
    (d) => !q || d.name.toLowerCase().includes(q) || d.folder.toLowerCase().includes(q),
  );
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
        d.findings ? el("span", { text: `${d.findings} finding${d.findings === 1 ? "" : "s"}` }) : null,
        el("span", { text: d.folder }),
        d.changed_at && d.status !== "clean" ? el("span", { text: ago(d.changed_at) }) : null,
      )),
    ),
  );
  let empty = "";
  if (ui.state.folders === 0) empty = "Add a OneDrive or Google Drive folder in Settings to start.";
  else if (!shown.length && q) empty = "No document matches this filter.";
  else if (!shown.length && ui.tab === "review") empty = "Nothing to review: every watched document matches its reviewed version.";
  else if (!shown.length) empty = ui.state.scanning ? "First scan in progress…" : "No Word, Excel or PowerPoint document in the watched folders.";
  $("list-empty").textContent = empty;
  $("list-empty").classList.toggle("hidden", !empty);
}

function select(id) {
  ui.selected = id;
  ui.detailKey = "";
  renderList();
  refresh();
}

async function loadDetail(id, key) {
  try {
    const doc = await api("GET", `/api/documents/${id}`);
    if (ui.selected !== id) return;
    ui.detail = doc;
    ui.detailKey = key;
    renderDetail();
  } catch (err) {
    renderMessage(err.message, "error");
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
  if (st && st.folders > 0) {
    panel.replaceChildren(el("div", { class: "welcome" },
      el("h2", { text: st.to_review ? "Select a document to review" : "All caught up" }),
      el("p", { text: st.to_review
        ? "Each changed document shows what moved since its reviewed version, most risky first."
        : "Probe keeps watching in the background, even when this window is closed. The icon near the clock shows when something needs a look." }),
    ));
    return;
  }
  const box = el("div", { class: "welcome" },
    el("h2", { text: "Watch your shared documents" }),
    el("p", { text: "Probe follows the Word, Excel and PowerPoint files of a synchronized folder and tells you which modifications deserve a look: a formula replaced by a number, an amount changed in a contract, a softened obligation, a hidden sheet…" }),
  );
  panel.replaceChildren(box);
  try {
    const s = await getSettings();
    const buttons = s.cloud_folders.map((f) =>
      el("button", { class: "btn ghost small", title: f.path, onclick: () => quickAdd(f.path) }, `+ ${f.label}`));
    box.append(
      buttons.length
        ? el("div", { class: "cloud-folders" }, ...buttons)
        : el("p", { text: "No OneDrive or Google Drive folder was detected on this computer." }),
      el("div", { class: "actions" }, el("button", { class: "btn", onclick: openSettings }, "Open settings")),
    );
  } catch (_) { /* the settings dialog stays available */ }
}

async function quickAdd(path) {
  try {
    const s = await getSettings();
    if (!s.folders.some((f) => f.toLowerCase() === path.toLowerCase())) s.folders.push(path);
    await api("PUT", "/api/settings", s);
    ui.detail = null;
    await refresh();
  } catch (err) {
    alert(err.message);
  }
}

function renderDetail() {
  const d = ui.detail;
  const panel = $("detail");
  const name = d.path.split(/[\\/]/).pop();
  const head = el("div", { class: "detail-head" },
    el("div", { class: "detail-title" }, kindBadge(d.kind), el("h2", { text: name }), statusChip({ ...d, severity: detailSeverity(d) })),
    el("p", { class: "detail-sub mono", text: d.path }),
    el("p", { class: "detail-sub", text: metaLine(d) }),
    actions(d),
  );
  const parts = [head];

  if (d.error) parts.push(el("p", { class: "message error", text: d.error }));
  if (d.status === "removed") {
    parts.push(el("p", { class: "message warn", text: "This document was deleted, moved or renamed since its reviewed version. Acknowledge to stop tracking it; if it was moved inside a watched folder, it is already tracked under its new name." }));
  } else if (d.status === "clean") {
    parts.push(el("p", { class: "message", text: "This document matches its reviewed version." }));
  } else if (d.status === "cloud-only") {
    parts.push(el("p", { class: "message", text: "This file is only in the cloud: Probe will record its reviewed version once it is downloaded, or enable the download option in Settings." }));
  }

  if (d.explanation) {
    if (d.report && RANK[d.explanation.severity] > RANK[d.report.severity]) {
      const impacts = (d.explanation.impacts || []).join(" and ");
      parts.push(el("p", { class: `message tone tone-${d.explanation.severity}`, text: `Severity raised from ${d.report.severity} to ${d.explanation.severity}: the model states this modification may have ${impacts} consequences.` }));
    }
    // The model's own findings come first; its prose explanation follows,
    // collapsed when there are findings to look at.
    const extra = d.explanation.findings || [];
    if (extra.length) {
      parts.push(el("h3", { class: "section-title", text: `Raised by AI (${extra.length})` }));
      parts.push(el("ul", { class: "findings ai-findings" }, ...extra.map(findingItem)));
      parts.push(el("p", { class: "note", text: "Suggestions from the model, not rule results: check them in the document." }));
    }
    parts.push(el("details", { class: "explanation", open: !extra.length },
      el("summary", { text: d.explanation.outdated ? "AI analysis (earlier version)" : "AI analysis" }),
      d.explanation.outdated
        ? el("p", { class: "note", text: "Written for an earlier version of the modifications: the AI findings about elements modified again were removed and the latest changes are not covered. Explain again for a complete analysis." })
        : null,
      el("p", { text: d.explanation.text }),
      el("span", { class: "note", text: `Explanation by ${d.explanation.model} · ${ago(d.explanation.at)} · the findings below remain the reference` }),
    ));
  }

  const r = d.report;
  if (r) {
    parts.push(el("h3", { class: "section-title", text: r.findings.length ? `Findings (${r.findings.length})` : "No risky modification detected" }));
    if (r.findings.length) {
      parts.push(el("ul", { class: "findings" }, ...r.findings.map(findingItem)));
    }
    parts.push(changesTable(r));
  }
  panel.replaceChildren(...parts);
}

function metaLine(d) {
  const bits = [];
  if (d.changed_at && d.status !== "clean") bits.push(`Change detected ${ago(d.changed_at)}`);
  if (d.report && d.report.last_modified_by) bits.push(`last saved by ${d.report.last_modified_by}`);
  if (d.baseline_at) bits.push(`reviewed version from ${when(d.baseline_at)}`);
  return bits.join(" · ");
}

function actions(d) {
  const box = el("div", { class: "actions" });
  if (d.status !== "removed") {
    box.append(el("button", { class: "btn quiet small", onclick: () => run(d.id, "open") }, "Open document"));
  }
  if (d.report) {
    const configured = ui.state && ui.state.ai_configured;
    box.append(el("button", {
      class: "btn ghost small",
      disabled: ui.busy[d.id] === "explain",
      title: configured ? "Send the findings and changed excerpts to the AI provider" : "Configure an AI provider in Settings",
      onclick: () => (configured ? explain(d.id) : openSettings()),
    }, ui.busy[d.id] === "explain" ? "Explaining…" : d.explanation ? "Explain again" : "Explain with AI"));
  }
  if (d.status === "changed" || d.status === "removed") {
    box.append(el("button", {
      class: "btn small",
      title: d.status === "removed" ? "Stop tracking this document" : "This version becomes the reference for the next changes",
      onclick: () => accept(d.id),
    }, d.status === "removed" ? "Acknowledge" : "Mark as reviewed"));
  }
  return box;
}

function findingItem(f) {
  const item = el("li", { class: `finding tone-${f.severity}` },
    el("span", { class: "dot", title: f.severity }),
    el("span", { class: "finding-title", text: f.title }),
  );
  if (f.location) item.append(el("span", { class: "finding-where mono", text: `${f.location} · ${f.severity}` }));
  else item.append(el("span", { class: "finding-where", text: f.severity }));
  if (f.before || f.after) {
    item.append(el("div", { class: "compare" },
      f.before ? el("p", { class: "before", text: f.before }) : null,
      f.after ? el("p", { class: "after", text: f.after }) : null,
    ));
  }
  return item;
}

function changesTable(r) {
  const rows = r.changes.map((c) => el("tr", {},
    el("td", { class: "kind-cell", text: c.kind }),
    el("td", { class: "mono", text: c.location }),
    el("td", { class: c.before ? "b" : "", text: c.before || "" }),
    el("td", { class: c.after ? "a" : "", text: c.after || "" }),
  ));
  const label = r.truncated
    ? `All changes (${r.change_count}, first ${r.changes.length} shown)`
    : `All changes (${r.change_count})`;
  return el("details", { class: "changes" },
    el("summary", { text: label }),
    el("table", {},
      el("thead", {}, el("tr", {}, el("th", { text: "Change" }), el("th", { text: "Where" }), el("th", { text: "Before" }), el("th", { text: "After" }))),
      el("tbody", {}, ...rows),
    ),
  );
}

async function run(id, action) {
  try {
    await api("POST", `/api/documents/${id}/${action}`);
  } catch (err) {
    alert(err.message);
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
    alert(err.message);
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
    alert(err.message);
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
    alert(err.message);
    return;
  }
  const s = ui.settings;
  ui.draftFolders = [...s.folders];
  $("folder-path").value = "";
  $("download-cloud").checked = !!s.download_cloud_files;
  $("scan-seconds").value = String(s.scan_seconds);
  if (!$("scan-seconds").value) $("scan-seconds").value = "60";
  $("provider").value = s.provider || "";
  $("model").value = s.model || "";
  $("base-url").value = s.base_url || "";
  $("language").value = s.language || "en";
  $("api-key").value = "";
  $("clear-key").checked = false;
  renderFolders();
  renderProvider();
  $("settings").showModal();
}

function renderFolders() {
  $("folder-list").replaceChildren(...ui.draftFolders.map((f, i) =>
    el("li", {}, el("span", { class: "mono", text: f }),
      el("button", { class: "btn quiet small", type: "button", onclick: () => { ui.draftFolders.splice(i, 1); renderFolders(); } }, "Remove"))));
  const lower = ui.draftFolders.map((f) => f.toLowerCase());
  const candidates = ui.settings.cloud_folders.filter((c) => !lower.includes(c.path.toLowerCase()));
  $("cloud-folders").replaceChildren(...candidates.map((c) =>
    el("button", { class: "btn ghost small", type: "button", title: c.path, onclick: () => { ui.draftFolders.push(c.path); renderFolders(); } }, `+ ${c.label}`)));
}

function renderProvider() {
  const p = $("provider").value;
  document.querySelectorAll(".ai-only").forEach((n) => n.classList.toggle("hidden", !p));
  document.querySelectorAll(".openai-only").forEach((n) => n.classList.toggle("hidden", p !== "openai"));
  const s = ui.settings;
  $("model").placeholder = p && s.default_models[p] ? `Default: ${s.default_models[p]}` : "";
  const saved = p && s.keys[p];
  $("api-key").placeholder = saved ? "A key is saved; type a new one to replace it" : p === "openai" ? "sk-… (optional for a local server)" : "sk-ant-…";
  $("clear-key-label").classList.toggle("hidden", !saved);
}

async function saveSettings() {
  const path = $("folder-path").value.trim();
  if (path) {
    ui.draftFolders.push(path);
    $("folder-path").value = "";
  }
  const body = {
    folders: ui.draftFolders,
    provider: $("provider").value,
    model: $("model").value.trim(),
    base_url: $("base-url").value.trim(),
    language: $("language").value,
    scan_seconds: Number($("scan-seconds").value),
    max_file_mb: ui.settings.max_file_mb,
    download_cloud_files: $("download-cloud").checked,
    api_key: $("api-key").value,
    clear_key: $("clear-key").checked,
  };
  try {
    ui.settings = await api("PUT", "/api/settings", body);
    $("settings").close();
    await refresh();
  } catch (err) {
    if (path) ui.draftFolders.pop();
    renderFolders();
    $("settings-error").textContent = err.message;
  }
}

// ---------- wiring ----------

function setThreshold(value) {
  ui.threshold = Number(value) + 1;
  $("severity").value = String(ui.threshold - 1);
  $("severity-value").textContent = SEVERITIES[ui.threshold - 1];
  try { localStorage.setItem("probe.threshold", String(ui.threshold)); } catch (_) { /* optional */ }
}

function setTab(tab) {
  ui.tab = tab;
  try { localStorage.setItem("probe.tab", tab); } catch (_) { /* optional */ }
  if (ui.state) renderList();
}

$("tab-review").addEventListener("click", () => setTab("review"));
$("tab-all").addEventListener("click", () => setTab("all"));
$("search").addEventListener("input", () => ui.state && renderList());
$("severity").addEventListener("input", (e) => { setThreshold(e.target.value); if (ui.state) renderList(); });
$("scan").addEventListener("click", async () => { await api("POST", "/api/scan").catch((e) => alert(e.message)); setTimeout(refresh, 400); });
$("open-settings").addEventListener("click", openSettings);
$("provider").addEventListener("change", renderProvider);
$("save-settings").addEventListener("click", saveSettings);
$("add-folder").addEventListener("click", () => {
  const path = $("folder-path").value.trim();
  if (!path) return;
  ui.draftFolders.push(path);
  $("folder-path").value = "";
  renderFolders();
});

setThreshold(Math.min(Math.max(ui.threshold - 1, 0), 3));
refresh();
// Poll faster while a scan runs, so the first results appear quickly.
(function loop() {
  setTimeout(async () => {
    if (!document.hidden) await refresh();
    loop();
  }, ui.state && ui.state.scanning ? 1500 : 4000);
})();
