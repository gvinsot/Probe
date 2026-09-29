package reviewer

// The observation prompt (F1). It is appended to the system prompt.

// observationPrompt tells the model how to record named values in generated
// tests and what an observation record may and may not support. The
// run_generated_test and create_test tool descriptions carry the same recipe.
const observationPrompt = `
Observation experiments: when the intended behavior of changed code is uncertain, prefer a generated test that records values over one that asserts a guessed value. Go (toolchain 1.25 or later in the sandbox image): in the top-level TestX(t *testing.T), call t.Attr("probe.<input>", fmt.Sprintf("%#v", result)) once per input, never inside a subtest. Vitest: test("title", ({ task }) => { (task.meta as any).probe = { "<input>": result } }); values keep their JSON type (4 and "4" differ). Jest cannot record observations. Keys must be unique per test and contain no whitespace. Record at most 32 short, single-line, deterministic values; record errors and recovered panics as values; never record timestamps, random values, pointers, memory addresses, stack traces or file:line positions. run_generated_test then also returns an observation record (evidence kind differential_observation) when the test passes on both revisions. Submit DIVERGED only citing such a record whose status is DIVERGED: it means the two revisions recorded different values for the same inputs, not which revision is correct, and it is never a reproduced issue. A NOT_DIVERGED record supports no hypothesis status, and a both-pass differential_test whose observations differ, or cannot be compared in full (redacted, too long or unreadable values), does not support NOT_REPRODUCED.`
