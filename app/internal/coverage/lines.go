package coverage

// NotExecuted returns, in ascending order, the added new-side lines of path
// that the measured run reported as not executed (execution count 0 for every
// instrumented block containing them). It returns nil unless the result is
// measured and the file was measured, so missing data never yields a line.
//
// Mutation of added lines (F4) uses it to skip candidate mutants on lines the
// package's own tests did not execute: those lines are already reported as
// uncovered_change signals. It never adds, removes or rewords a signal.
func (r Result) NotExecuted(path string) []int {
	if r.coverage.Status != StatusMeasured {
		return nil
	}
	if file, ok := r.byPath[path]; !ok || file.Status != StatusMeasured {
		return nil
	}
	return append([]int(nil), r.notExecuted[path]...)
}
