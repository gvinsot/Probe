package fuzz

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gvinsot/Probe/app/internal/harness"
)

// Rendering bounds.
const (
	// MaxHarnessBytes bounds one rendered harness file. A larger file is
	// rendered again with half the inputs, at most maxHalvings times.
	MaxHarnessBytes = 1 << 20
	maxHalvings     = 3

	// MaxDisplayBytes bounds a display value in the stream (Appendix B).
	MaxDisplayBytes = 256
	minDisplayBytes = 32
	// recordOverhead bounds the bytes of one observation record other than its
	// display value: {"f":16,"k":"obs","i":1023,"h":"<64 hex>","l":65536,"o":"",
	// "p":false,"d":false,"t":false} plus a newline is 144 bytes.
	recordOverhead = 160
	// functionOverhead bounds the begin, stop and end records of one function.
	functionOverhead = 128
	// frameReserve covers the frame header and footer the capture script adds
	// around the stream on the payload channel.
	frameReserve = 256

	// Bounds of the in-container canonical encoding of one evaluation.
	MaxEncodingBytes = 64 << 10
	maxEncodingDepth = 16
	maxEncodingElems = 1024

	harnessPrefix = "probeFuzz"
	testPrefix    = "TestProbeFuzz_"
)

// suffixPattern is the shape of a per-run harness suffix.
var suffixPattern = regexp.MustCompile(`^[0-9a-f]{8,32}$`)

// NewSuffix returns a fresh per-run random suffix (16 lowercase hex digits).
// Every harness identifier carries it, so code under review cannot declare a
// colliding identifier in advance.
func NewSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// RenderOptions are the per-run inputs of Render.
type RenderOptions struct {
	Suffix           string        // per-run random suffix (NewSuffix)
	ObservationsPath string        // absolute in-container path of the observation stream
	PayloadLimit     int           // bytes one run may return on the payload channel
	CallTimeout      time.Duration // bound of one evaluation of one input
	Family           string        // TS/JS only: FamilyVitest or FamilyJest
}

// HarnessTest is one rendered fuzz test: one target and its planned inputs.
type HarnessTest struct {
	Name   string
	Target Target // Target.Inputs equals len(Inputs)
	Inputs []Input
}

// Harness is one rendered observation harness: a single internal _test.go file
// that runs identically on both revisions.
type Harness struct {
	Path    string // <dir>/probe_fuzz_<suffix>_test.go, or <dir>/probe-fuzz-<suffix>.test.ts|js
	Content string
	Suffix  string
	Display int // bound of a display value in the stream, in bytes
	Tests   []HarnessTest
	// Runner is the runner of the harness's evidence: go_test_json for a Go
	// harness (also when empty), jest_json for a TS/JS harness.
	Runner string
}

// EvidenceRunner returns the runner recorded on the harness's evidence.
func (h Harness) EvidenceRunner() string {
	if h.Runner == "" {
		return harness.RunnerGo
	}
	return h.Runner
}

// TestNames returns the harness's test names in execution order.
func (h Harness) TestNames() []string {
	names := make([]string, len(h.Tests))
	for i, t := range h.Tests {
		names[i] = t.Name
	}
	return names
}

// Test returns the named test.
func (h Harness) Test(name string) (HarnessTest, bool) {
	for _, t := range h.Tests {
		if t.Name == name {
			return t, true
		}
	}
	return HarnessTest{}, false
}

// Render renders the observation harness of one package. The file asserts
// nothing: for every input it evaluates the target twice with fresh
// arguments, encodes results, recovered panics and slice arguments after the
// call with a fixed reflection-based encoder, and appends one JSON line per
// input to o.ObservationsPath.
//
// The only repository-derived text in the file is the package name, the
// function names and the named parameter types, each re-validated as an
// identifier; literals come from strconv. Every top-level identifier, import
// name and local variable of the file starts with probeFuzz<suffix>, and
// test names are TestProbeFuzz_<suffix>_<n>; a collision with an
// identifier of the package on either revision is an error. When the file or
// its worst-case stream would not fit, the inputs are halved (at most three
// times) before Render gives up.
func Render(p PackagePlan, o RenderOptions) (Harness, error) {
	if !suffixPattern.MatchString(o.Suffix) {
		return Harness{}, errors.New("harness suffix must be 8 to 32 lowercase hex digits")
	}
	if !strings.HasPrefix(o.ObservationsPath, "/") || strings.ContainsAny(o.ObservationsPath, "\x00\n\"\\`") {
		return Harness{}, errors.New("observations path must be an absolute in-container path")
	}
	if o.CallTimeout <= 0 {
		return Harness{}, errors.New("call timeout must be positive")
	}
	if o.PayloadLimit <= 0 {
		return Harness{}, errors.New("payload limit must be positive")
	}
	if len(p.Targets) == 0 {
		return Harness{}, errors.New("no function to render")
	}
	if !validIdentifier(p.Name) {
		return Harness{}, fmt.Errorf("package name %q is not an identifier", p.Name)
	}
	if p.Dir == "" || strings.HasPrefix(p.Dir, "/") || path.Clean(p.Dir) != p.Dir || strings.HasPrefix(p.Dir, "..") {
		return Harness{}, fmt.Errorf("package directory %q is not a clean relative path", p.Dir)
	}
	prefix := harnessPrefix + o.Suffix
	for _, name := range harnessIdentifiers(prefix, o.Suffix, len(p.Targets)) {
		if p.Idents[name] {
			return Harness{}, fmt.Errorf("%w: %s", ErrCollision, name)
		}
	}
	for _, t := range p.Targets {
		if !validIdentifier(t.Name) || p.Idents != nil && !p.Idents[t.Name] {
			return Harness{}, fmt.Errorf("function name %q is not a package identifier", t.Name)
		}
		for _, param := range t.Params {
			if param.Named != "" && (!validIdentifier(param.Named) || p.Idents != nil && !p.Idents[param.Named]) {
				return Harness{}, fmt.Errorf("parameter type %q is not a package identifier", param.Named)
			}
			if !basicTypes[param.Basic] {
				return Harness{}, fmt.Errorf("parameter type %q is not generated", param.Basic)
			}
		}
	}
	file := "probe_fuzz_" + o.Suffix + "_test.go"
	if p.Dir != "." {
		file = p.Dir + "/" + file
	}
	budgets := make([]int, len(p.Targets))
	for i, t := range p.Targets {
		budgets[i] = max(1, t.Inputs)
	}
	var lastErr error
	for attempt := 0; attempt <= maxHalvings; attempt++ {
		h, err := renderOnce(p, o, prefix, file, budgets)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if !errors.Is(err, errTooLarge) {
			break // fewer inputs cannot help
		}
		for i := range budgets {
			budgets[i] = max(1, budgets[i]/2)
		}
	}
	return Harness{}, lastErr
}

var errTooLarge = errors.New("the harness does not fit its bounds")

// ErrNoInput reports a function whose corpus is empty: no call text of it is
// left unchanged by redaction. Select skips such functions, so a plan from
// Select never gives this error.
var ErrNoInput = errors.New("no seeded input has a call text that redaction leaves unchanged")

// ErrCollision reports that a harness identifier is already a package
// identifier on one of the revisions; rendering again with a fresh suffix
// avoids it.
var ErrCollision = errors.New("harness identifier collides with a package identifier")

func renderOnce(p PackagePlan, o RenderOptions, prefix, file string, budgets []int) (Harness, error) {
	h := Harness{Path: file, Suffix: o.Suffix, Runner: harness.RunnerGo}
	records := 0
	for i, t := range p.Targets {
		inputs := Corpus(t, budgets[i])
		if len(inputs) == 0 {
			return Harness{}, fmt.Errorf("%w: %s", ErrNoInput, t.Name)
		}
		t.Inputs = len(inputs)
		h.Tests = append(h.Tests, HarnessTest{Name: testPrefix + o.Suffix + "_" + strconv.Itoa(i+1), Target: t, Inputs: inputs})
		records += len(inputs)
	}
	usable := o.PayloadLimit - frameReserve - functionOverhead*len(h.Tests)
	display := 0
	if usable > 0 {
		display = (usable/records - recordOverhead) / 2
	}
	display = min(MaxDisplayBytes, display)
	if display < minDisplayBytes {
		return Harness{}, errTooLarge
	}
	// A display value can double when it is escaped as JSON; the stream must
	// still fit the payload channel in the worst case.
	if records*(recordOverhead+2*display)+functionOverhead*len(h.Tests)+frameReserve > o.PayloadLimit {
		return Harness{}, errTooLarge
	}
	h.Display = display
	var b strings.Builder
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(harnessHeader, "PKGNAME", p.Name), "ZSPFZ", prefix))
	runtime := strings.NewReplacer(
		"ZSPFZ", prefix,
		"OBSERVATIONS_PATH", strconv.Quote(o.ObservationsPath),
		"DISPLAY_BYTES", strconv.Itoa(display),
		"TIMEOUT_MS", strconv.FormatInt(max(1, o.CallTimeout.Milliseconds()), 10),
		"MAX_BYTES", strconv.Itoa(MaxEncodingBytes),
		"MAX_DEPTH", strconv.Itoa(maxEncodingDepth),
		"MAX_ELEMS", strconv.Itoa(maxEncodingElems),
	).Replace(harnessRuntime)
	b.WriteString(runtime)
	for i, test := range h.Tests {
		renderTest(&b, prefix, i+1, test)
	}
	if b.Len() > MaxHarnessBytes {
		return Harness{}, errTooLarge
	}
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return Harness{}, fmt.Errorf("rendered harness does not parse: %v", err)
	}
	if len(formatted) > MaxHarnessBytes {
		return Harness{}, errTooLarge
	}
	h.Content = string(formatted)
	return h, nil
}

// harnessIdentifiers lists every package-block and import name the rendered
// file declares, and its test names.
func harnessIdentifiers(prefix, suffix string, tests int) []string {
	names := []string{}
	for _, n := range harnessNames {
		names = append(names, prefix+n)
	}
	for i := 1; i <= tests; i++ {
		names = append(names, testPrefix+suffix+"_"+strconv.Itoa(i))
	}
	return names
}

// harnessNames are the suffixes (after the prefix) of the file's import names
// and package-block declarations. render_test.go checks that the rendered
// file declares exactly these.
var harnessNames = []string{
	"_hex", "_math", "_os", "_reflect", "_sha256", "_sort", "_strconv", "_strings", "_testing", "_time", "_utf8",
	"_path", "_display", "_timeout", "_maxBytes", "_maxDepth", "_maxElems",
	"_done", "_timedOut", "_goexit", "_abort",
	"_poisoned", "_errorType", "_encoder", "_newEncoder", "_errorMessage", "_call", "_outcome", "_run", "_quote", "_cut",
}

func validIdentifier(s string) bool {
	return token.IsIdentifier(s) && s != "_"
}

// renderTest writes the test of one target: a slice of case closures, each
// returning fresh argument values, and one evaluation closure.
func renderTest(b *strings.Builder, prefix string, n int, test HarnessTest) {
	t := test.Target
	math := prefix + "_math"
	types := make([]string, len(t.Params))
	for j, p := range t.Params {
		types[j] = p.Type()
	}
	results := strings.Join(types, ", ")
	if len(types) > 1 {
		results = "(" + results + ")"
	}
	caseType := "func()"
	if results != "" {
		caseType += " " + results
	}
	fmt.Fprintf(b, "\n// %s observes %s (%d inputs).\n", test.Name, t.Name, len(test.Inputs))
	fmt.Fprintf(b, "func %s(%s_t *%s_testing.T) {\n", test.Name, prefix, prefix)
	fmt.Fprintf(b, "\t%s_cases := []%s{\n", prefix, caseType)
	for _, in := range test.Inputs {
		args := make([]string, len(in.Args))
		for j, v := range in.Args {
			args[j] = argCode(t.Params[j], v, math)
		}
		if len(args) == 0 {
			fmt.Fprintf(b, "\t\tfunc() {},\n")
			continue
		}
		fmt.Fprintf(b, "\t\tfunc() %s { return %s },\n", results, strings.Join(args, ", "))
	}
	b.WriteString("\t}\n")
	fmt.Fprintf(b, "\t%s_run(%s_t, %d, len(%s_cases), func(%s_k int) (string, bool, bool) {\n", prefix, prefix, n, prefix, prefix)
	argNames := make([]string, len(t.Params))
	callArgs := make([]string, len(t.Params))
	for j, p := range t.Params {
		argNames[j] = fmt.Sprintf("%s_a%d", prefix, j)
		callArgs[j] = argNames[j]
		if p.Kind == ParamVariadic {
			callArgs[j] += "..."
		}
	}
	if len(argNames) > 0 {
		fmt.Fprintf(b, "\t\t%s := %s_cases[%s_k]()\n", strings.Join(argNames, ", "), prefix, prefix)
	} else {
		fmt.Fprintf(b, "\t\t%s_cases[%s_k]()\n", prefix, prefix)
	}
	fmt.Fprintf(b, "\t\t%s_e := %s_newEncoder()\n", prefix, prefix)
	fmt.Fprintf(b, "\t\t%s_returned := false\n", prefix)
	b.WriteString("\t\tfunc() {\n")
	b.WriteString("\t\t\tdefer func() {\n")
	fmt.Fprintf(b, "\t\t\t\t%s_e.recovered(recover(), %s_returned)\n", prefix, prefix)
	b.WriteString("\t\t\t}()\n")
	call := t.Name + "(" + strings.Join(callArgs, ", ") + ")"
	resultNames := make([]string, t.Results)
	for i := range resultNames {
		resultNames[i] = fmt.Sprintf("%s_r%d", prefix, i)
	}
	if t.Results > 0 {
		fmt.Fprintf(b, "\t\t\t%s := %s\n", strings.Join(resultNames, ", "), call)
	} else {
		fmt.Fprintf(b, "\t\t\t%s\n", call)
	}
	fmt.Fprintf(b, "\t\t\t%s_returned = true\n", prefix)
	if t.Results == 0 {
		fmt.Fprintf(b, "\t\t\t%s_e.w(\"()\")\n", prefix)
	}
	for i, r := range resultNames {
		if i > 0 {
			fmt.Fprintf(b, "\t\t\t%s_e.w(\", \")\n", prefix)
		}
		fmt.Fprintf(b, "\t\t\t%s_e.value(%s_reflect.ValueOf(&%s).Elem(), 0, true)\n", prefix, prefix, r)
	}
	b.WriteString("\t\t}()\n")
	for j, p := range t.Params {
		if p.Kind == ParamSlice || p.Kind == ParamVariadic {
			fmt.Fprintf(b, "\t\t%s_e.w(\"; arg %d after call: \")\n", prefix, j+1)
			fmt.Fprintf(b, "\t\t%s_e.value(%s_reflect.ValueOf(&%s).Elem(), 0, true)\n", prefix, prefix, argNames[j])
		}
	}
	fmt.Fprintf(b, "\t\treturn %s_e.finish()\n", prefix)
	b.WriteString("\t})\n}\n")
}

// harnessHeader is the start of every harness file. ZSPFZ becomes the
// per-run prefix and PKGNAME the package clause.
const harnessHeader = `// Code generated by Probe differential fuzzing (probe-fuzz/v1). DO NOT EDIT.
//
// This file records observations of changed functions on seeded inputs. It
// asserts nothing: its tests pass unless the observation file cannot be written.

package PKGNAME

import (
	ZSPFZ_sha256 "crypto/sha256"
	ZSPFZ_hex "encoding/hex"
	ZSPFZ_math "math"
	ZSPFZ_os "os"
	ZSPFZ_reflect "reflect"
	ZSPFZ_sort "sort"
	ZSPFZ_strconv "strconv"
	ZSPFZ_strings "strings"
	ZSPFZ_testing "testing"
	ZSPFZ_time "time"
	ZSPFZ_utf8 "unicode/utf8"
)
`

// harnessRuntime is the fixed part of every harness. It uses only language
// features and standard library APIs available since Go 1.13, so it compiles
// in modules that declare an old go version.
const harnessRuntime = `
var _ = ZSPFZ_math.NaN

const (
	ZSPFZ_path     = OBSERVATIONS_PATH
	ZSPFZ_display  = DISPLAY_BYTES
	ZSPFZ_timeout  = ZSPFZ_time.Duration(TIMEOUT_MS) * ZSPFZ_time.Millisecond
	ZSPFZ_maxBytes = MAX_BYTES
	ZSPFZ_maxDepth = MAX_DEPTH
	ZSPFZ_maxElems = MAX_ELEMS
)

// Evaluation states.
const (
	ZSPFZ_done = iota
	ZSPFZ_timedOut
	ZSPFZ_goexit
	ZSPFZ_abort
)

// ZSPFZ_poisoned is set after an evaluation timed out: its goroutine may still
// run, so later functions of this process are not evaluated.
var ZSPFZ_poisoned bool

var ZSPFZ_errorType = ZSPFZ_reflect.TypeOf((*error)(nil)).Elem()

// ZSPFZ_encoder builds the canonical encoding of one evaluation, bounded in
// size, depth and elements; cut records that a bound was reached.
type ZSPFZ_encoder struct {
	b        ZSPFZ_strings.Builder
	limit    int
	cut      bool
	panicked bool
	stack    []uintptr
}

func ZSPFZ_newEncoder() *ZSPFZ_encoder {
	return &ZSPFZ_encoder{limit: ZSPFZ_maxBytes}
}

func (e *ZSPFZ_encoder) w(s string) {
	if e.cut {
		return
	}
	if e.b.Len()+len(s) > e.limit {
		e.cut = true
		return
	}
	e.b.WriteString(s)
}

func (e *ZSPFZ_encoder) finish() (string, bool, bool) {
	return e.b.String(), e.cut, e.panicked
}

// recovered records how the call ended: a panic of the called function is an
// observation; a panic after it returned means the encoding failed.
func (e *ZSPFZ_encoder) recovered(r interface{}, returned bool) {
	if returned {
		if r != nil {
			e.w("<encoding failed>")
			e.cut = true
		}
		return
	}
	e.panicked = true
	e.w("panic(")
	if err, ok := r.(error); ok {
		e.w("error(" + ZSPFZ_errorMessage(err) + ")")
	} else {
		e.value(ZSPFZ_reflect.ValueOf(r), 0, true)
	}
	e.w(")")
}

func ZSPFZ_errorMessage(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = "<Error method panicked>"
		}
	}()
	m := err.Error()
	if len(m) > ZSPFZ_maxBytes {
		m = m[:ZSPFZ_maxBytes]
	}
	return ZSPFZ_strconv.Quote(m)
}

func (e *ZSPFZ_encoder) scalar(prefix bool, t ZSPFZ_reflect.Type, text string) {
	if prefix {
		e.w(t.String() + "(" + text + ")")
		return
	}
	e.w(text)
}

// value encodes v: a type prefix at the top level and for named types,
// strconv forms for scalars (NaN, infinities and negative zero are distinct),
// nil distinct from empty, maps sorted by encoded key, every struct field in
// order (unexported ones through reflection, never Interface), pointers
// followed with a cycle marker and never printed as addresses, and error
// values as their message.
func (e *ZSPFZ_encoder) value(v ZSPFZ_reflect.Value, depth int, typed bool) {
	if e.cut {
		return
	}
	if depth > ZSPFZ_maxDepth {
		e.w("<depth>")
		e.cut = true
		return
	}
	if !v.IsValid() {
		e.w("nil")
		return
	}
	t := v.Type()
	k := t.Kind()
	if k == ZSPFZ_reflect.Interface {
		if v.IsNil() {
			e.w(t.String() + "(nil)")
			return
		}
		if t == ZSPFZ_errorType && v.CanInterface() {
			if err, ok := v.Interface().(error); ok {
				e.w("error(" + ZSPFZ_errorMessage(err) + ")")
				return
			}
		}
		e.value(v.Elem(), depth+1, true)
		return
	}
	prefix := typed || t.Name() != "" && t.PkgPath() != ""
	switch k {
	case ZSPFZ_reflect.Bool:
		e.scalar(prefix, t, ZSPFZ_strconv.FormatBool(v.Bool()))
	case ZSPFZ_reflect.Int, ZSPFZ_reflect.Int8, ZSPFZ_reflect.Int16, ZSPFZ_reflect.Int32, ZSPFZ_reflect.Int64:
		e.scalar(prefix, t, ZSPFZ_strconv.FormatInt(v.Int(), 10))
	case ZSPFZ_reflect.Uint, ZSPFZ_reflect.Uint8, ZSPFZ_reflect.Uint16, ZSPFZ_reflect.Uint32, ZSPFZ_reflect.Uint64, ZSPFZ_reflect.Uintptr:
		e.scalar(prefix, t, ZSPFZ_strconv.FormatUint(v.Uint(), 10))
	case ZSPFZ_reflect.Float32:
		e.scalar(prefix, t, ZSPFZ_strconv.FormatFloat(v.Float(), 'g', -1, 32))
	case ZSPFZ_reflect.Float64:
		e.scalar(prefix, t, ZSPFZ_strconv.FormatFloat(v.Float(), 'g', -1, 64))
	case ZSPFZ_reflect.Complex64, ZSPFZ_reflect.Complex128:
		bits := 64
		if k == ZSPFZ_reflect.Complex64 {
			bits = 32
		}
		c := v.Complex()
		e.scalar(prefix, t, "("+ZSPFZ_strconv.FormatFloat(real(c), 'g', -1, bits)+"+"+ZSPFZ_strconv.FormatFloat(imag(c), 'g', -1, bits)+"i)")
	case ZSPFZ_reflect.String:
		e.scalar(prefix, t, ZSPFZ_strconv.Quote(v.String()))
	case ZSPFZ_reflect.Slice:
		if v.IsNil() {
			e.w(t.String() + "(nil)")
			return
		}
		e.list(v, t, depth, prefix)
	case ZSPFZ_reflect.Array:
		e.list(v, t, depth, prefix)
	case ZSPFZ_reflect.Map:
		if v.IsNil() {
			e.w(t.String() + "(nil)")
			return
		}
		e.mapping(v, t, depth, prefix)
	case ZSPFZ_reflect.Struct:
		if prefix {
			e.w(t.String())
		}
		e.w("{")
		for i := 0; i < v.NumField(); i++ {
			if i >= ZSPFZ_maxElems {
				e.w(", ...")
				e.cut = true
				return
			}
			if i > 0 {
				e.w(", ")
			}
			e.w(t.Field(i).Name + ": ")
			e.value(v.Field(i), depth+1, false)
		}
		e.w("}")
	case ZSPFZ_reflect.Ptr:
		if v.IsNil() {
			e.w("(" + t.String() + ")(nil)")
			return
		}
		p := v.Pointer()
		for _, q := range e.stack {
			if q == p {
				e.w("<cycle>")
				return
			}
		}
		e.stack = append(e.stack, p)
		e.w("&")
		e.value(v.Elem(), depth+1, typed)
		e.stack = e.stack[:len(e.stack)-1]
	default:
		if v.IsNil() {
			e.w(t.String() + "(nil)")
		} else {
			e.w(t.String() + "(non-nil)")
		}
	}
}

func (e *ZSPFZ_encoder) list(v ZSPFZ_reflect.Value, t ZSPFZ_reflect.Type, depth int, prefix bool) {
	if prefix {
		e.w(t.String())
	}
	e.w("{")
	for i := 0; i < v.Len(); i++ {
		if i >= ZSPFZ_maxElems {
			e.w(", ...")
			e.cut = true
			return
		}
		if i > 0 {
			e.w(", ")
		}
		e.value(v.Index(i), depth+1, false)
		if e.cut {
			return
		}
	}
	e.w("}")
}

func (e *ZSPFZ_encoder) mapping(v ZSPFZ_reflect.Value, t ZSPFZ_reflect.Type, depth int, prefix bool) {
	type entry struct{ key, value string }
	entries := make([]entry, 0, v.Len())
	remaining := e.limit - e.b.Len()
	used := 0
	it := v.MapRange()
	for it.Next() {
		if len(entries) >= ZSPFZ_maxElems || used > remaining {
			e.cut = true
			break
		}
		ke := &ZSPFZ_encoder{limit: remaining - used, stack: e.stack}
		ke.value(it.Key(), depth+1, false)
		ve := &ZSPFZ_encoder{limit: remaining - used, stack: e.stack}
		ve.value(it.Value(), depth+1, false)
		if ke.cut || ve.cut {
			e.cut = true
		}
		k, x := ke.b.String(), ve.b.String()
		used += len(k) + len(x) + 4
		entries = append(entries, entry{k, x})
	}
	ZSPFZ_sort.Slice(entries, func(i, j int) bool {
		if entries[i].key != entries[j].key {
			return entries[i].key < entries[j].key
		}
		return entries[i].value < entries[j].value
	})
	cut := e.cut
	e.cut = false
	if prefix {
		e.w(t.String())
	}
	e.w("{")
	for i, en := range entries {
		if i > 0 {
			e.w(", ")
		}
		e.w(en.key + ": " + en.value)
	}
	e.w("}")
	e.cut = e.cut || cut
}

type ZSPFZ_outcome struct {
	enc           string
	cut, panicked bool
	state         int
}

// ZSPFZ_call evaluates one input in its own goroutine, bounded by the call
// timeout. A goroutine that ends through runtime.Goexit, or panics outside the
// called function, never sends a value of its own: the deferred function
// reports it instead.
func ZSPFZ_call(eval func(int) (string, bool, bool), k int) ZSPFZ_outcome {
	ch := make(chan ZSPFZ_outcome, 1)
	go func() {
		finished := false
		var o ZSPFZ_outcome
		defer func() {
			if !finished {
				o = ZSPFZ_outcome{state: ZSPFZ_goexit}
				if recover() != nil {
					o.state = ZSPFZ_abort
				}
			}
			ch <- o
		}()
		o.enc, o.cut, o.panicked = eval(k)
		finished = true
	}()
	timer := ZSPFZ_time.NewTimer(ZSPFZ_timeout)
	defer timer.Stop()
	select {
	case o := <-ch:
		return o
	case <-timer.C:
		return ZSPFZ_outcome{state: ZSPFZ_timedOut}
	}
}

// ZSPFZ_quote encodes s as a JSON string: quote, backslash and control bytes
// are escaped, everything else is copied (s is valid UTF-8).
func ZSPFZ_quote(s string) string {
	const digits = "0123456789abcdef"
	var b ZSPFZ_strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString("\\\"")
		case c == '\\':
			b.WriteString("\\\\")
		case c < 0x20:
			b.WriteString("\\u00")
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ZSPFZ_cut returns the display value: at most ZSPFZ_display bytes of valid
// UTF-8.
func ZSPFZ_cut(s string) string {
	s = ZSPFZ_strings.ToValidUTF8(s, "�")
	if len(s) <= ZSPFZ_display {
		return s
	}
	s = s[:ZSPFZ_display]
	for len(s) > 0 && !ZSPFZ_utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ZSPFZ_run evaluates every input of function f twice and appends one record
// per input to the observation file, between a begin and an end record. A
// timeout, a Goexit or an abort ends the function with a stop record.
func ZSPFZ_run(t *ZSPFZ_testing.T, f int, n int, eval func(int) (string, bool, bool)) {
	file, err := ZSPFZ_os.OpenFile(ZSPFZ_path, ZSPFZ_os.O_WRONLY|ZSPFZ_os.O_CREATE|ZSPFZ_os.O_APPEND, 0600)
	if err != nil {
		t.Fatal("probe: the observation file could not be opened")
	}
	defer file.Close()
	emit := func(line string) {
		if _, err := file.Write([]byte(line + "\n")); err != nil {
			t.Fatal("probe: the observation file could not be written")
		}
	}
	head := "{\"f\":" + ZSPFZ_strconv.Itoa(f) + ",\"k\":"
	emit(head + "\"begin\",\"n\":" + ZSPFZ_strconv.Itoa(n) + "}")
	if ZSPFZ_poisoned {
		emit(head + "\"stop\",\"i\":0,\"x\":\"poisoned\"}")
		return
	}
	for k := 0; k < n; k++ {
		first := ZSPFZ_call(eval, k)
		second := first
		if first.state == ZSPFZ_done {
			second = ZSPFZ_call(eval, k)
		}
		state := first.state
		if state == ZSPFZ_done {
			state = second.state
		}
		if state != ZSPFZ_done {
			reason := "abort"
			switch state {
			case ZSPFZ_timedOut:
				reason = "timeout"
				ZSPFZ_poisoned = true
			case ZSPFZ_goexit:
				reason = "goexit"
			}
			emit(head + "\"stop\",\"i\":" + ZSPFZ_strconv.Itoa(k) + ",\"x\":\"" + reason + "\"}")
			return
		}
		sum := ZSPFZ_sha256.Sum256([]byte(first.enc))
		unstable := first.enc != second.enc || first.cut != second.cut
		emit(head + "\"obs\",\"i\":" + ZSPFZ_strconv.Itoa(k) +
			",\"h\":\"" + ZSPFZ_hex.EncodeToString(sum[:]) + "\"" +
			",\"l\":" + ZSPFZ_strconv.Itoa(len(first.enc)) +
			",\"o\":" + ZSPFZ_quote(ZSPFZ_cut(first.enc)) +
			",\"p\":" + ZSPFZ_strconv.FormatBool(first.panicked) +
			",\"d\":" + ZSPFZ_strconv.FormatBool(unstable) +
			",\"t\":" + ZSPFZ_strconv.FormatBool(first.cut) + "}")
	}
	emit(head + "\"end\",\"n\":" + ZSPFZ_strconv.Itoa(n) + "}")
}
`
