package fuzz

import (
	"strconv"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Parameter kinds a target may take. Anything else is skipped at selection.
const (
	ParamScalar   = "scalar"   // a basic type or a package-local named basic type
	ParamSlice    = "slice"    // []E
	ParamArray    = "array"    // [N]E with 0 <= N <= MaxArrayLen
	ParamVariadic = "variadic" // ...E, always the last parameter
)

// Param describes one generated parameter. The element is either a
// predeclared basic type (Named empty) or a package-local named type whose
// underlying type is that basic type, identical on both revisions.
type Param struct {
	Kind  string `json:"kind"`
	Basic string `json:"basic"`           // bool, string, int..int64, rune, uint..uint64, byte, float32, float64
	Named string `json:"named,omitempty"` // package-local named type of the element
	Len   int    `json:"len,omitempty"`   // array length (ParamArray only)
	// Keyword is the name of a Python keyword-only parameter, passed as
	// name=value; "" for a positional one and for other languages.
	Keyword string `json:"keyword,omitempty"`
}

// Elem is the Go type of one element, or of the scalar itself.
func (p Param) Elem() string {
	if p.Named != "" {
		return p.Named
	}
	return p.Basic
}

// Type is the Go type a case closure returns for the parameter: the scalar
// type, the slice or array type, or the slice a variadic argument spreads.
func (p Param) Type() string {
	switch p.Kind {
	case ParamSlice, ParamVariadic:
		return "[]" + p.Elem()
	case ParamArray:
		return "[" + strconv.Itoa(p.Len) + "]" + p.Elem()
	}
	return p.Elem()
}

// Target is one changed, eligible Go function. Its coordinates are those of
// the candidate revision.
type Target struct {
	Dir       string  `json:"dir"`  // slash package directory relative to the repository root, "." for the root
	Path      string  `json:"path"` // candidate file
	Line      int     `json:"line"`
	EndLine   int     `json:"end_line"`
	Name      string  `json:"name"`
	Symbol    string  `json:"symbol"`    // <dir>.<Name>, or <package>.<Name> for the root package
	Signature string  `json:"signature"` // types only, e.g. func(int64, string) (Cents, error)
	Params    []Param `json:"params"`
	Results   int     `json:"results"`
	Inputs    int     `json:"inputs"` // planned number of seeded inputs
	Exported  bool    `json:"exported"`
	Priority  int     `json:"priority"` // highest severity rank (0..4) of signals overlapping the function
	// Language is "" for a Go function, LanguageScript for a TS/JS function
	// and LanguagePython for a Python function. A TS/JS or Python target's
	// Dir is its module path, so that each module is planned, budgeted and
	// run like one package.
	Language string `json:"language,omitempty"`
}

// PackagePlan is the set of targets of one package, rendered into one harness
// file and run in one container per revision.
type PackagePlan struct {
	Dir     string   `json:"dir"`
	Name    string   `json:"name"` // package clause, identical on both revisions
	Targets []Target `json:"targets"`
	// Idents holds every package-block identifier and every import name of
	// every file of the package on either revision. Rendering refuses a harness
	// identifier that appears here.
	Idents map[string]bool `json:"-"`
	// Script is set for a TS/JS module (F2c); Name and Idents are then empty.
	Script *ScriptModule `json:"script,omitempty"`
	// Python is set for a Python module; Name and Idents are then empty.
	Python *PythonModule `json:"python,omitempty"`
}

// ScriptModule is the TS/JS module whose changed functions one harness runs.
type ScriptModule struct {
	Path   string `json:"path"`   // module file, the same path on both revisions
	Import string `json:"import"` // import specifier from the harness, "./name" or "./name.mjs"
	Ext    string `json:"ext"`    // extension of the harness file: "ts" or "js"
}

// Plan is the outcome of selection. Packages are in priority order; Skipped
// lists every changed function (or file) that is not planned, with a fixed
// reason, sorted by path and line.
type Plan struct {
	Packages      []PackagePlan    `json:"packages"`
	Skipped       []model.FuzzSkip `json:"skipped"`
	BudgetSkipped int              `json:"budget_skipped"` // entries of Skipped cut by max_functions or max_packages
	// GoTemplateSkipped counts the entries of Skipped that are eligible Go
	// functions a Vitest or Jest template cannot run (ReasonGoTemplate).
	GoTemplateSkipped int `json:"go_template_skipped"`
}

// Targets returns the number of planned functions.
func (p Plan) Targets() int {
	n := 0
	for _, pkg := range p.Packages {
		n += len(pkg.Targets)
	}
	return n
}

// GoTargets returns the number of planned Go functions.
func (p Plan) GoTargets() int {
	n := 0
	for _, pkg := range p.Packages {
		if pkg.Script == nil {
			n += len(pkg.Targets)
		}
	}
	return n
}

// Scalar holds one generated value of a basic type. Only the field of the
// parameter's class is meaningful: I for signed integers and runes, U for
// unsigned integers and bytes, F for floats (float32 values are stored
// widened), S for strings and B for booleans.
type Scalar struct {
	I int64
	U uint64
	F float64
	S string
	B bool
}

// Value is one generated argument: a scalar, or the elements of a slice, array
// or variadic argument. Nil marks a nil slice (never an array).
type Value struct {
	Scalar Scalar
	Elems  []Scalar
	Nil    bool
}

// Input is one seeded input of a target. Call is its display form, a Go call
// expression such as Discount(Cents(1000)); it is also the observation key and
// is unique within a target's corpus.
type Input struct {
	Args []Value
	Call string
}
