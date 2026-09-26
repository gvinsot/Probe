package mutation

// Test-only access for the external test package, which also imports harness
// to check the single-pass outcome against harness.GoTestOutcome.

// OnePassOutcome is the outcome of name in a log read once by readLog.
func OnePassOutcome(output, name string) (action, pkg string) {
	return readLog(output).outcome(name)
}

// LogTestNames returns every test name readLog counted an event for, and the
// top-level ones in first-event order.
func LogTestNames(output string) (all, topLevel []string) {
	l := readLog(output)
	for name := range l.tests {
		all = append(all, name)
	}
	return all, l.names
}
