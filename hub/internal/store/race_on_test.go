//go:build race

package store

// timeBudgetFactor scales the wall-clock budgets of the tests: the race
// detector makes them several times slower without changing what they check.
const timeBudgetFactor = 8
