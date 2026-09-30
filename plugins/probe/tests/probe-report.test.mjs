// Tests of the report helper: node --test plugins/probe/tests/*.test.mjs
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, copyFileSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { list, loadReport, main, parseArgs, show, status, summary, targets } from "../scripts/probe-report.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const FIXTURE = join(here, "fixtures", "review-report.json");
const SCRIPT = join(here, "..", "scripts", "probe-report.mjs");
const report = () => loadReport(FIXTURE);

test("review targets are numbered most severe first, keeping report order on ties", () => {
  const t = targets(report());
  assert.deepEqual(t.map((x) => [x.id, x.path, x.severity]), [
    ["T1", "auth/auth.go", "high"],
    ["T2", "payment/refund.go", "high"],
    ["T3", "payment/refund.go", "low"],
  ]);
});

test("summary keeps every handoff category separate and hides the dismissed ones", () => {
  const s = summary(report());
  assert.match(s, /Exit code: 1 — a high\/critical issue was reproduced/);
  assert.match(s, /Focused review: 11 \/ 52 changed lines/);
  assert.match(s, /## Reproduced issues \(1\)\n- H1 \[high\] Negative refunds are accepted payment\/refund.go:30/);
  assert.match(s, /## Behavior divergences \(1\)/);
  assert.match(s, /## Intent-test failures .*\(1\)\n- H4 AC-1/);
  assert.match(s, /## Unresolved hypotheses \(1\)\n- H2/);
  assert.match(s, /## Checks that did not pass \(1\)\n- chk-build build FAIL \(exit 1\)/);
  assert.match(s, /## Unverified areas \(1\)/);
  assert.match(s, /## Plan conformance: drifted, gate: human_review_required/);
  assert.doesNotMatch(s, /H3|Rounding/, "NOT_REPRODUCED hypotheses are not open findings");
  assert.match(s, /1111111111\S*\.\.2222222222/);
});

test("summary lists empty categories on one line", () => {
  const r = report();
  r.divergences = [];
  r.unverified = [];
  assert.match(summary(r), /Nothing in: Behavior divergences, Unverified areas\./);
});

test("summary respects the limit and points to the full list", () => {
  const s = summary(report(), { limit: 1 });
  assert.match(s, /## Review targets \(3\)\n- T1 .*\n… 2 more/);
});

test("list filters by severity and path", () => {
  const all = list(report(), {});
  assert.match(all, /^H1\tREPRODUCED\thigh/m);
  assert.match(all, /^chk-build\tCHECK_FAIL/m);
  assert.doesNotMatch(all, /^sig-/m, "raw signals only with --all");
  const high = list(report(), { severity: "high" });
  assert.doesNotMatch(high, /^T3\t/m);
  assert.match(high, /^T1\tTARGET\thigh\tauth\/auth.go:12-20/m);
  const path = list(report(), { path: "auth/" });
  assert.deepEqual(path.split("\n").map((l) => l.split("\t")[0]), ["H2", "T1"]);
  assert.match(list(report(), { all: true }), /^sig-branch\tSIGNAL\tlow/m);
  assert.equal(list(report(), { path: "nowhere" }), "no finding matches");
});

test("show explains a target with its signals, a hypothesis with its evidence, a check with its output", () => {
  const t = show(report(), "T2");
  assert.match(t, /T2 review target \[high\] payment\/refund.go:30/);
  assert.match(t, /sig-valid \[high\] validation_removed/);
  assert.match(t, /removed: if amount <= 0/);
  const h = show(report(), "H1");
  assert.match(h, /H1 hypothesis REPRODUCED/);
  assert.match(h, /E1 differential_test REPRODUCED: Refund\(-5\) fails on candidate/);
  const k = show(report(), "chk-build");
  assert.match(k, /Command: go build \.\/\.\.\./);
  assert.match(k, /undefined: ErrInvalid/);
  assert.match(show(report(), "sig-auth"), /Part of: T1/);
  assert.throws(() => show(report(), "T9"), /no finding T9/);
  assert.throws(() => show(report(), undefined), /needs an id/);
});

test("parseArgs accepts --key value, --key=value and --all", () => {
  assert.deepEqual(parseArgs(["list", "--severity", "high", "--path=a b", "--all"]),
    { _: ["list"], severity: "high", path: "a b", all: true });
  assert.throws(() => parseArgs(["--report"]), /missing value/);
});

test("a missing report tells the user to run a review", () => {
  assert.throws(() => loadReport("/nonexistent/confidence-report.json"), /run \/probe:review first/);
});

function repoWithReport(headMatches) {
  const dir = mkdtempSync(join(tmpdir(), "probe-plugin-"));
  const git = (...a) => execFileSync("git", a, { cwd: dir, stdio: "pipe" }).toString().trim();
  git("init", "-q");
  git("-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init");
  mkdirSync(join(dir, ".probe"));
  const r = JSON.parse(readFileSync(FIXTURE, "utf8"));
  if (headMatches) r.change.head_commit = git("rev-parse", "HEAD");
  writeFileSync(join(dir, ".probe", "confidence-report.json"), JSON.stringify(r));
  writeFileSync(join(dir, ".probe", "claude-context.json"), JSON.stringify({ base: "origin/release" }));
  writeFileSync(join(dir, ".probe", "intent.md"), "# x\n");
  return dir;
}

test("status reports the saved context and whether the report is stale", () => {
  const fresh = repoWithReport(true);
  const s = status(join(fresh, ".probe", "confidence-report.json"), fresh);
  assert.match(s, /Probe binary: /);
  assert.match(s, /Base: origin\/release/);
  assert.match(s, /Intent: \.probe\/intent.md/);
  assert.match(s, /Plan: none/);
  assert.match(s, /matches HEAD/);
  const stale = repoWithReport(false);
  assert.match(status(join(stale, ".probe", "confidence-report.json"), stale), /STALE/);
});

test("status never fails, even outside a repository or on a broken report", () => {
  const dir = mkdtempSync(join(tmpdir(), "probe-plugin-"));
  assert.match(main(["status"], dir), /Not a Git repository/);
  const repo = repoWithReport(true);
  writeFileSync(join(repo, ".probe", "confidence-report.json"), "{broken");
  assert.match(main(["status"], repo), /Report: unreadable/);
});

test("the script runs as a command and exits 1 with a message on errors", () => {
  const dir = repoWithReport(true);
  const out = execFileSync("node", [SCRIPT, "list", "--severity", "high"], { cwd: dir }).toString();
  assert.match(out, /^H1\tREPRODUCED/m);
  let failed;
  try {
    execFileSync("node", [SCRIPT, "show", "nope"], { cwd: dir, stdio: "pipe" });
  } catch (err) {
    failed = err;
  }
  assert.equal(failed?.status, 1);
  assert.match(failed.stderr.toString(), /probe-report: no finding nope/);
  copyFileSync(FIXTURE, join(dir, "other.json"));
  assert.match(execFileSync("node", [SCRIPT, "summary", "--report", join(dir, "other.json")]).toString(), /Probe report v0.5.16/);
});
