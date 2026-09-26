package symbols

import (
	"context"
	"time"
)

// budgetCheckEvery is how many work units a budget spends between two checks
// of its deadline and context.
const budgetCheckEvery = 1024

// budget bounds the work of one search over the index: every reference
// visited and every interface-implementation check is charged against a
// fixed number of units, and the deadline and the context are checked every
// budgetCheckEvery units, at the start of each search and before every
// implementation check (one check can cost far more than a unit). Once any of
// them is reached the budget stays stopped, and the search that noticed
// returns what it found so far as incomplete. A budget is used by one
// goroutine.
type budget struct {
	ctx        context.Context
	deadline   time.Time
	left       int64
	sinceCheck int

	stopped   bool // a bound was reached; nothing more is searched
	timedOut  bool // the deadline was reached
	cancelled bool // ctx was done

	// implCapped records that an implementers lookup met
	// maxImplementCandidates: interface methods of the same name beyond it
	// were not checked, and the search went on without them.
	implCapped bool
	// implCostly records that an implementers lookup skipped checks whose
	// estimated go/types work exceeded maxImplementSteps (a receiver type
	// with a wide embedding, or many fields or methods).
	implCostly bool
	// implGeneric records that an implementers lookup met an interface that a
	// generic receiver type may implement only once instantiated, which is
	// not checked.
	implGeneric bool
}

// resetImpl clears the implementers flags before the search of one changed
// function.
func (b *budget) resetImpl() {
	b.implCapped, b.implCostly, b.implGeneric = false, false, false
}

// implGap reports whether an implementers lookup left interface methods
// unchecked.
func (b *budget) implGap() bool {
	return b.implCapped || b.implCostly || b.implGeneric
}

func newBudget(ctx context.Context, deadline time.Time, units int64) *budget {
	return &budget{ctx: ctx, deadline: deadline, left: units}
}

// ok checks the deadline and the context now, and reports whether the search
// may go on.
func (b *budget) ok() bool {
	if b.stopped {
		return false
	}
	b.sinceCheck = 0
	switch {
	case b.ctx != nil && b.ctx.Err() != nil:
		b.stopped, b.cancelled = true, true
	case !b.deadline.IsZero() && time.Now().After(b.deadline):
		b.stopped, b.timedOut = true, true
	}
	return !b.stopped
}

// spend charges n units and reports whether the search may go on.
func (b *budget) spend(n int) bool {
	if b.stopped {
		return false
	}
	b.left -= int64(n)
	if b.left < 0 {
		b.stopped = true
		return false
	}
	b.sinceCheck += n
	if b.sinceCheck >= budgetCheckEvery {
		return b.ok()
	}
	return true
}
