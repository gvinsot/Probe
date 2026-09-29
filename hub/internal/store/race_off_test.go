//go:build !race

package store

// timeBudgetFactor scales the wall-clock budgets of the tests; see race_on_test.go.
const timeBudgetFactor = 1
