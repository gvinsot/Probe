// Run with: node --test desktop/web/diff.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const { inlineDiff } = require("./public/diff.js");

const show = (segments) => segments.map((s) => (s.changed ? `[${s.text}]` : s.text)).join("");

test("marks the exact characters of a changed figure", () => {
  const d = inlineDiff("Payment is due within 30 days.", "Payment is due within 90 days.");
  assert.strictEqual(show(d.before), "Payment is due within [3]0 days.");
  assert.strictEqual(show(d.after), "Payment is due within [9]0 days.");
});

test("marks a replaced word whole rather than scattered letters", () => {
  const d = inlineDiff("The supplier shall deliver.", "The supplier may deliver, if possible.");
  assert.strictEqual(show(d.before), "The supplier [shall] deliver.");
  assert.strictEqual(show(d.after), "The supplier [may] deliver[, if possible].");
});

test("handles accents, identical and empty excerpts", () => {
  assert.strictEqual(show(inlineDiff("Café", "Cafe").before), "Caf[é]");
  assert.strictEqual(show(inlineDiff("same", "same").after), "same");
  assert.strictEqual(show(inlineDiff("", "new").after), "[new]");
});

test("keeps long excerpts intact", () => {
  const a = "word ".repeat(2000), b = "other ".repeat(2000);
  const d = inlineDiff(a, b);
  assert.strictEqual(d.before.map((s) => s.text).join(""), a);
  assert.strictEqual(d.after.map((s) => s.text).join(""), b);
});
