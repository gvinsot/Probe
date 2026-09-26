// Package symbols builds a static index of a repository's own Go packages from
// committed Git objects, and derives the impact analysis of a change from it:
// the functions and methods the change modified, the places in unchanged code
// that reference them, and the existing Go tests that reach them within a few
// calls.
//
// The index is an observation from static analysis. Files are read only
// through gitrepo.Tree and gitrepo.ReadBlobs, parsed and type-checked
// in-process with the Go standard library, and never executed: no go command,
// cgo, code generation or network access is involved. Imports from outside the
// repository are not loaded, so every answer is approximate. The index never
// creates evidence, never removes or lowers a signal and never supports a
// hypothesis status; it only adds low-severity review targets and tool
// observations.
//
// The package satisfies harness.SymbolIndex structurally and must not import
// harness (contract §0.7).
package symbols

import (
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/gitrepo"
)

// Method names the resolution method in every tool response.
const Method = "go_static_index"

// Limitations is the fixed caveat carried by every index-backed tool response.
const Limitations = "Approximate static analysis of committed Go source: imports from outside the repository are not loaded, files are selected with linux/amd64 build constraints, and calls through function values, reflection, go:linkname, assembly or generated code are not resolved. Interface edges are possible dispatch only. A short or empty result is not proof that no other reference exists."

// Symbol of the analysis_limited signal the impact analysis adds.
const LimitedSymbol = "impact_index"

// NotApplicableReason is the reason of a not_applicable section: the change
// touches no Go file the index may read. Go files can still have changed in
// the paths the index never reads.
const NotApplicableReason = "no indexable Go file changed (files under testdata or vendor, in directories whose name starts with _ or ., and sensitive paths are not indexed)"

// Analysis caps (contract §2 F6a and Appendix B).
const (
	MaxDepth              = 3   // calls followed from a test or caller to a changed function
	MaxCallersListed      = 10  // callers listed per changed function in the report
	MaxSignalsPerFunction = 10  // impacted_caller signals per changed function
	MaxSignalsTotal       = 100 // impacted_caller signals per run
	MaxTestsListed        = 20  // reaching tests listed per changed function
)

// Search caps. They are variables only so that tests can lower them; a search
// that meets one reports it (the section is then limited, or a tool answer is
// marked truncated).
var (
	maxReachVisits         = 50000 // declarations visited per reaching-test search
	maxImplementCandidates = 200   // interface methods of one name checked for implementation
)

// Limits bounds what the index reads and builds. The zero value of a field
// means its default (DefaultLimits); tests lower them to exercise the limits.
type Limits struct {
	MaxFiles            int   // .go and go.mod files read from the candidate tree
	MaxBytes            int64 // total source bytes read
	MaxFileBytes        int64 // per file; a larger file is not indexed
	MaxPackages         int   // packages type-checked
	MaxEdges            int   // resolved reference sites recorded
	MaxNameSites        int   // unresolved method calls recorded (tool output only)
	MaxChangedFunctions int   // changed functions listed in the report
	// MaxSearchVisits bounds each of the two impact searches (callers, then
	// reaching tests) over all changed functions: every reference visited
	// and every interface-implementation check costs units. Changed
	// functions not fully searched within it are reported, and the section
	// is limited.
	MaxSearchVisits int64
	// Timeout is the analysis time limit. It is checked between packages and
	// on each type error (remaining packages are not indexed), and during the
	// impact searches, before every interface-implementation check (remaining
	// callers and tests are not searched). go/types cannot be interrupted
	// otherwise: the type check of one package that produces no type error
	// runs to its end, so the limit does not bound the whole analysis.
	Timeout time.Duration
}

// DefaultLimits returns the limits used when Options.Limits leaves a field zero.
func DefaultLimits() Limits {
	return Limits{
		MaxFiles:            20000,
		MaxBytes:            64 << 20,
		MaxFileBytes:        gitrepo.MaxFileBytes,
		MaxPackages:         5000,
		MaxEdges:            2000000,
		MaxNameSites:        200000,
		MaxChangedFunctions: 200,
		MaxSearchVisits:     10000000,
		Timeout:             120 * time.Second,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxPackages <= 0 {
		l.MaxPackages = d.MaxPackages
	}
	if l.MaxEdges <= 0 {
		l.MaxEdges = d.MaxEdges
	}
	if l.MaxNameSites <= 0 {
		l.MaxNameSites = d.MaxNameSites
	}
	if l.MaxChangedFunctions <= 0 {
		l.MaxChangedFunctions = d.MaxChangedFunctions
	}
	if l.MaxSearchVisits <= 0 {
		l.MaxSearchVisits = d.MaxSearchVisits
	}
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	return l
}

// Options configures Analyze.
type Options struct {
	// Sensitive reports repository paths that must be neither read nor
	// indexed. cli passes harness.IsSensitivePath, the snapshot exclusion, so
	// that the tools cannot reveal what the sandbox also hides.
	Sensitive func(path string) bool
	// Limits overrides the defaults field by field.
	Limits Limits
}
