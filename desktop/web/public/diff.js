// Inline difference of two excerpts, so the exact characters that changed
// stand out inside a modified paragraph or cell.
//
// The excerpts are first compared word by word, which keeps the result
// readable for prose. A replaced run that is short on both sides, like a
// figure or a single word, is then compared character by character when both
// sides are alike: "30 days" against "90 days" marks only the "3" and the "9".
"use strict";

const DIFF_MAX_CELLS = 250000; // bound of the comparison table
const DIFF_CHAR_MAX = 40; // longest run refined character by character

// Words (letters and digits of any script), runs of spaces, and any other
// character on its own.
function diffTokens(s) {
  return s.match(/[\p{L}\p{N}_]+|\s+|[^\p{L}\p{N}_\s]/gu) || [];
}

// diffOps returns the longest common subsequence of two token lists as
// operations: "=" kept, "-" removed from a, "+" added in b.
function diffOps(a, b) {
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  let endA = a.length, endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) { endA--; endB--; }
  const ops = [];
  for (let i = 0; i < start; i++) ops.push(["=", a[i]]);
  const midA = a.slice(start, endA), midB = b.slice(start, endB);
  const n = midA.length, m = midB.length;
  if (n && m && (n + 1) * (m + 1) <= DIFF_MAX_CELLS) {
    // lcs[i][j]: length of the common subsequence of midA[i:] and midB[j:].
    const lcs = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        lcs[i][j] = midA[i] === midB[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
      }
    }
    let i = 0, j = 0;
    while (i < n && j < m) {
      if (midA[i] === midB[j]) { ops.push(["=", midA[i]]); i++; j++; }
      else if (lcs[i + 1][j] >= lcs[i][j + 1]) ops.push(["-", midA[i++]]);
      else ops.push(["+", midB[j++]]);
    }
    while (i < n) ops.push(["-", midA[i++]]);
    while (j < m) ops.push(["+", midB[j++]]);
  } else {
    // Too long to compare finely: the whole middle part changed.
    for (const t of midA) ops.push(["-", t]);
    for (const t of midB) ops.push(["+", t]);
  }
  for (let i = endA; i < a.length; i++) ops.push(["=", a[i]]);
  return ops;
}

// inlineDiff returns the segments of both excerpts, each marked as changed
// or not.
function inlineDiff(before, after) {
  const out = { before: [], after: [] };
  const push = (side, text, changed) => {
    if (!text) return;
    const last = side[side.length - 1];
    if (last && last.changed === changed) last.text += text;
    else side.push({ text, changed });
  };
  const ops = diffOps(diffTokens(before), diffTokens(after));
  for (let k = 0; k < ops.length;) {
    if (ops[k][0] === "=") {
      push(out.before, ops[k][1], false);
      push(out.after, ops[k][1], false);
      k++;
      continue;
    }
    // A replaced run: what was removed and what was added in its place.
    let removed = "", added = "";
    for (; k < ops.length && ops[k][0] !== "="; k++) {
      if (ops[k][0] === "-") removed += ops[k][1];
      else added += ops[k][1];
    }
    const chars = removed && added && removed.length <= DIFF_CHAR_MAX && added.length <= DIFF_CHAR_MAX
      ? diffOps([...removed], [...added])
      : null;
    // Refined only when both sides share most of their characters: "shall"
    // against "may" reads better as a replaced word than as scattered letters.
    const kept = chars ? chars.filter(([op]) => op === "=").length : 0;
    if (chars && kept * 2 >= Math.min([...removed].length, [...added].length)) {
      for (const [op, ch] of chars) {
        if (op !== "+") push(out.before, ch, op === "-");
        if (op !== "-") push(out.after, ch, op === "+");
      }
    } else {
      push(out.before, removed, true);
      push(out.after, added, true);
    }
  }
  return out;
}

if (typeof module !== "undefined") module.exports = { inlineDiff };
