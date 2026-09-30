#!/usr/bin/env node
// Reads a Probe report (.probe/confidence-report.json) and prints compact
// views of it for Claude Code, so a large report never has to be loaded into
// the conversation whole. It only reads: it never reruns Probe, and it never
// changes a report, a policy or a baseline.
//
//   probe-report.mjs summary [--report FILE] [--limit N]
//   probe-report.mjs list    [--report FILE] [--severity LEVEL] [--path TEXT] [--all]
//   probe-report.mjs show ID [--report FILE]
//   probe-report.mjs status  [--report FILE]
//
// Review targets are numbered T1, T2… most severe first, as `list` prints
// them; checks, hypotheses and signals keep the ids of the report.
// No dependency: Claude Code always ships with Node.

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

const RANK = { critical: 4, high: 3, medium: 2, low: 1, info: 0 };
const EXIT = {
  0: "0 — completed without a reproduced high/critical issue (not a correctness claim)",
  1: "1 — a high/critical issue was reproduced by a differential experiment",
  2: "2 — human review required (with --ci): high-risk signals, unverified areas or incomplete checks",
  3: "3 — invalid arguments, Git comparison or trusted configuration; nothing ran",
  4: "4 — operational error in the harness, the analysis or the report",
};
// Hypotheses the reader must see: everything but the ones the experiments
// dismissed or did not reproduce.
const OPEN_HYPOTHESES = new Set(["REPRODUCED", "DIVERGED", "INTENT_TEST_FAILED", "UNVERIFIED"]);

export function parseArgs(argv) {
  const args = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--all") args.all = true;
    else if (a.startsWith("--")) {
      const [k, v] = a.includes("=") ? a.slice(2).split(/=(.*)/s) : [a.slice(2), argv[++i]];
      if (v === undefined) throw new Error(`missing value for --${k}`);
      args[k] = v;
    } else args._.push(a);
  }
  return args;
}

function git(args, cwd) {
  try {
    return execFileSync("git", args, { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
  } catch {
    return "";
  }
}

export function repoRoot(cwd = process.cwd()) {
  return git(["rev-parse", "--show-toplevel"], cwd) || cwd;
}

export function defaultReport(cwd) {
  return join(repoRoot(cwd), ".probe", "confidence-report.json");
}

export function loadReport(file) {
  if (!existsSync(file)) {
    throw new Error(`no report at ${file}: run /probe:review first (or pass --report FILE)`);
  }
  const report = JSON.parse(readFileSync(file, "utf8"));
  for (const key of ["linter", "checks", "hypotheses", "review_targets", "unverified"]) {
    if (!Array.isArray(report[key])) report[key] = [];
  }
  return report;
}

const rank = (sev) => RANK[String(sev || "").toLowerCase()] ?? 0;
const bySeverity = (a, b) => rank(b.severity) - rank(a.severity);
const place = (path, start, end) =>
  path ? `${path}${start ? `:${start}${end && end !== start ? `-${end}` : ""}` : ""}` : "";

/** Review targets, most severe first, numbered T1, T2… (stable for a report). */
export function targets(report) {
  return report.review_targets
    .map((t, i) => ({ ...t, order: i }))
    .sort((a, b) => bySeverity(a, b) || a.order - b.order)
    .map((t, i) => ({ ...t, id: `T${i + 1}` }));
}

export function openHypotheses(report) {
  return report.hypotheses.filter((h) => OPEN_HYPOTHESES.has(h.status)).sort(bySeverity);
}

export function failedChecks(report) {
  return report.checks.filter((c) => c.status && c.status !== "PASS");
}

function short(text, n = 160) {
  const s = String(text ?? "").replace(/\s+/g, " ").trim();
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

export function summary(report, { limit = 12 } = {}) {
  const out = [];
  const c = report.change || {};
  const range = c.base_commit ? `${c.base_commit.slice(0, 12)}..${String(c.head_commit || "").slice(0, 12)}` : "";
  out.push(`Probe report ${report.tool_version || ""} · ${report.generated_at || ""}`.trim());
  if (c.base_ref || range) {
    out.push(`Change: ${c.base_ref || "?"} → ${c.head_ref || "?"} (${range}), ${(c.files || []).length} files, +${c.additions ?? "?"}/-${c.deletions ?? "?"}`);
  }
  if (report.analysis_mode) out.push(`Mode: ${report.analysis_mode}`);
  out.push(`Exit code: ${EXIT[report.exit_code] || report.exit_code}`);
  const s = report.review_surface;
  if (s) out.push(`Focused review: ${s.focused_lines} / ${s.changed_lines} changed lines (a prioritization aid, not a guarantee)`);

  // Empty sections are listed on one line at the end, to keep the summary short.
  const empty = [];
  const section = (title, items, fmt) => {
    if (!items.length) { empty.push(title); return; }
    out.push("", `## ${title} (${items.length})`);
    items.slice(0, limit).forEach((x) => out.push(fmt(x)));
    if (items.length > limit) out.push(`… ${items.length - limit} more: probe-report.mjs list`);
  };

  const hyps = openHypotheses(report);
  section("Reproduced issues", hyps.filter((h) => h.status === "REPRODUCED"),
    (h) => `- ${h.id} [${h.severity}] ${short(h.title)} ${place(h.path, h.line)}`);
  section("Behavior divergences", report.divergences || [],
    (d) => `- ${d.evidence_id} ${d.kind} ${place(d.path, d.line)} ${short(d.note, 120)}`);
  section("Intent-test failures (model-written tests, candidate only)", report.intent_test_failures || [],
    (h) => `- ${h.id} ${h.criterion_id || ""} ${short(h.title)}`);
  // The categories the handoff reports separately (app/docs/AGENT_WORKFLOW.md).
  section("Unresolved hypotheses", hyps.filter((h) => h.status === "UNVERIFIED"),
    (h) => `- ${h.id} [${h.severity}] ${short(h.title)} ${place(h.path, h.line)}`);
  section("Checks that did not pass", failedChecks(report),
    (k) => `- ${k.id} ${k.kind} ${k.status} (exit ${k.exit_code})`);
  section("Unverified areas", report.unverified, (u) => `- ${short(u, 200)}`);
  section("Review targets", targets(report),
    (t) => `- ${t.id} [${t.severity}] ${place(t.path, t.start_line, t.end_line)} — ${short((t.reasons || []).join("; "), 140)}`);
  if (report.plan_drift) {
    const p = report.plan_drift;
    out.push("", `## Plan conformance: ${p.status}, gate: ${p.decision}`);
    (p.decision_reasons || []).slice(0, limit).forEach((r) => out.push(`- ${short(r, 200)}`));
  }
  if (empty.length) out.push("", `Nothing in: ${empty.join(", ")}.`);
  out.push("", "Details: probe-report.mjs show <ID>. Signals are heuristics to review, not confirmed bugs.");
  return out.join("\n");
}

export function list(report, { severity, path, all } = {}) {
  const min = severity ? rank(severity) : 0;
  const keep = (x) => rank(x.severity) >= min && (!path || String(x.path || "").includes(path));
  const out = [];
  for (const h of openHypotheses(report).filter(keep)) {
    out.push(`${h.id}\t${h.status}\t${h.severity}\t${place(h.path, h.line)}\t${short(h.title, 120)}`);
  }
  for (const k of failedChecks(report)) {
    if (!path) out.push(`${k.id}\tCHECK_${k.status}\t-\t${k.kind}\texit ${k.exit_code}`);
  }
  for (const t of targets(report).filter(keep)) {
    out.push(`${t.id}\tTARGET\t${t.severity}\t${place(t.path, t.start_line, t.end_line)}\t${short((t.reasons || []).join("; "), 120)}`);
  }
  if (all) {
    for (const s of [...report.linter].sort(bySeverity).filter(keep)) {
      out.push(`${s.id}\tSIGNAL\t${s.severity}\t${place(s.path, s.line, s.end_line)}\t${s.kind}: ${short(s.summary, 100)}`);
    }
  }
  return out.length ? out.join("\n") : "no finding matches";
}

export function show(report, id) {
  if (!id) throw new Error("show needs an id, such as T1, a check id, a hypothesis id or a signal id");
  const signals = new Map(report.linter.map((s) => [s.id, s]));
  const evidence = new Map((report.evidence || []).map((e) => [e.id, e]));
  const sig = (s) => [
    `  ${s.id} [${s.severity}] ${s.kind} at ${place(s.path, s.line, s.end_line)}${s.symbol ? ` (${s.symbol})` : ""}`,
    `    ${s.summary}`,
    `    evidence: ${s.evidence}`,
  ].join("\n");

  const t = targets(report).find((x) => x.id === id);
  if (t) {
    return [
      `${t.id} review target [${t.severity}] ${place(t.path, t.start_line, t.end_line)} (${t.side} side)`,
      `Reasons: ${(t.reasons || []).join("; ")}`,
      "Signals:",
      ...(t.signal_ids || []).map((i) => (signals.has(i) ? sig(signals.get(i)) : `  ${i}`)),
    ].join("\n");
  }
  if (signals.has(id)) {
    const s = signals.get(id);
    const inTargets = targets(report).filter((x) => (x.signal_ids || []).includes(id)).map((x) => x.id);
    return [`Signal`, sig(s), inTargets.length ? `Part of: ${inTargets.join(", ")}` : ""].join("\n").trim();
  }
  const h = report.hypotheses.find((x) => x.id === id);
  if (h) {
    return [
      `${h.id} hypothesis ${h.status} [${h.severity}] ${place(h.path, h.line)}`,
      `Title: ${h.title}`,
      `Rationale: ${h.rationale}`,
      h.criterion_id ? `Criterion: ${h.criterion_id}` : "",
      "Evidence:",
      ...(h.evidence_ids || []).map((i) => {
        const e = evidence.get(i);
        return e ? `  ${e.id} ${e.kind} ${e.status}: ${e.description}${e.output ? `\n    output: ${short(e.output, 600)}` : ""}` : `  ${i}`;
      }),
    ].filter(Boolean).join("\n");
  }
  const k = report.checks.find((x) => x.id === id);
  if (k) {
    return [
      `${k.id} check ${k.kind}: ${k.status} (exit ${k.exit_code}, ${k.duration_ms} ms)`,
      k.command ? `Command: ${k.command.join(" ")}` : "",
      `Output${k.truncated ? " (truncated by Probe)" : ""}:`,
      String(k.output || "").slice(-4000),
    ].filter(Boolean).join("\n");
  }
  throw new Error(`no finding ${id} in this report: see probe-report.mjs list --all`);
}

function probeVersion() {
  try {
    const v = execFileSync("probe", ["version"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
    return v.split("\n")[0].trim();
  } catch {
    return "";
  }
}

/** The binary, the saved context and whether the report still describes
 * HEAD. Never fails: skills inject it before Claude reads them, and a failed
 * injection would abort the skill. */
export function status(file, cwd = process.cwd()) {
  const root = repoRoot(cwd);
  const out = [];
  const version = probeVersion();
  out.push(`Probe binary: ${version || "NOT INSTALLED (see /probe:setup)"}`);
  if (!git(["rev-parse", "--git-dir"], root)) {
    out.push("Not a Git repository: Probe reviews Git commits.");
    return out.join("\n");
  }
  const def = git(["symbolic-ref", "--short", "refs/remotes/origin/HEAD"], root);
  out.push(`Default branch: ${def || "unknown (no origin/HEAD); main assumed"}`);
  out.push(`Current branch: ${git(["rev-parse", "--abbrev-ref", "HEAD"], root) || "?"}`);
  const ctxFile = join(root, ".probe", "claude-context.json");
  let ctx = {};
  if (existsSync(ctxFile)) {
    try { ctx = JSON.parse(readFileSync(ctxFile, "utf8")); } catch { out.push(`unreadable ${ctxFile}`); }
  }
  out.push(`Base: ${ctx.base || "(default: the repository's default branch)"}`);
  const intent = join(root, ".probe", "intent.md");
  out.push(`Intent: ${existsSync(intent) ? ".probe/intent.md" : "none"}`);
  const plan = join(root, ".probe", "PLAN.json");
  out.push(`Plan: ${existsSync(plan) ? ".probe/PLAN.json" : "none"}`);
  out.push(`Policy: ${git(["ls-files", "--error-unmatch", ".probe.json"], root) ? ".probe.json (committed)" : "no committed .probe.json"}`);
  const head = git(["rev-parse", "HEAD"], root);
  const dirty = git(["status", "--porcelain", "--untracked-files=no"], root);
  if (dirty) out.push(`Uncommitted changes: ${dirty.split("\n").length} file(s) — Probe only analyzes committed files`);
  if (!existsSync(file)) {
    out.push("Report: none yet");
  } else {
    try {
      const r = loadReport(file);
      const reviewed = r.change?.head_commit || "";
      const fresh = reviewed && head && reviewed === head;
      out.push(`Report: ${r.generated_at || "?"}, exit ${r.exit_code}, head ${reviewed.slice(0, 12)} — ${fresh ? "matches HEAD" : "STALE: HEAD moved since this report, run it again"}`);
    } catch (err) {
      out.push(`Report: unreadable (${err.message})`);
    }
  }
  return out.join("\n");
}

export function main(argv, cwd = process.cwd()) {
  const args = parseArgs(argv);
  const [cmd, id] = args._;
  const file = args.report || defaultReport(cwd);
  switch (cmd) {
    case "summary": return summary(loadReport(file), { limit: Number(args.limit) || 12 });
    case "list": return list(loadReport(file), args);
    case "show": return show(loadReport(file), id);
    case "status":
      try { return status(file, cwd); } catch (err) { return `status unavailable: ${err.message}`; }
    default: throw new Error("usage: probe-report.mjs summary|list|show ID|status [--report FILE]");
  }
}

if (import.meta.url === `file://${process.argv[1]}` || process.argv[1]?.endsWith("probe-report.mjs")) {
  try {
    console.log(main(process.argv.slice(2)));
  } catch (err) {
    console.error(`probe-report: ${err.message}`);
    process.exit(1);
  }
}
