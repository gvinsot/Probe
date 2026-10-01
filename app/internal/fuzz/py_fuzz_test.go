package fuzz

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
)

const pyCartBase = `"""Cart helpers."""


def discount(total: float, percent: int) -> float:
    if percent < 0:
        raise ValueError("negative percent")
    return total - total * percent / 100


def label(name: str) -> str:
    return name.strip().title()


def merge(items: list[int], *extra: int, unique: bool = False) -> list[int]:
    out = list(items) + list(extra)
    return sorted(set(out)) if unique else out


def ratio(a: float, b: float) -> float:
    return a / b


async def fetch(key: str) -> str:
    return key.upper()


def mutate(xs: list[int]) -> int:
    return sum(xs)


def spin(n: int) -> int:
    return n
`

const pyCartCandidate = `"""Cart helpers."""


def discount(total: float, percent: int) -> float:
    if percent <= 0:
        raise ValueError("negative percent")
    return total - total * percent / 100


def label(name: str) -> str:
    return str.title(name.strip())


def merge(items: list[int], *extra: int, unique: bool = False) -> list[int]:
    out = [*items, *extra]
    if unique:
        return sorted(set(out))
    return out


def ratio(a: float, b: float) -> float:
    if b == 0:
        return float("inf")
    return a / b


async def fetch(key: str) -> str:
    return str.upper(key)


def mutate(xs: list[int]) -> int:
    xs.sort()
    return sum(xs)


def spin(n: int) -> int:
    while n == 7:
        pass
    return n
`

// pyProject writes a Python project with the given shop/cart.py.
func pyProject(t *testing.T, cart string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"shop/__init__.py": "",
		"shop/cart.py":     cart,
		"pyproject.toml":   "[tool.pytest.ini_options]\npythonpath = [\".\"]\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func requirePython3Pytest(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import pytest").Run() != nil {
		t.Skip("python3 with pytest is not installed on the host")
	}
	return python
}

// pytestRunner runs a rendered Python harness with the real pytest on two
// project copies, as the sandbox does in /workspace, and returns the
// normalized observation streams.
type pytestRunner struct {
	t               *testing.T
	python          string
	base, candidate string
	observations    string
	requests        []Request
	checks          []model.Check
	evidence        []model.Evidence
	contents        []string
}

func (r *pytestRunner) Observe(ctx context.Context, req Request) (Side, Side, error) {
	r.requests = append(r.requests, req)
	r.contents = append(r.contents, req.Harness.Content)
	baseKind, candidateKind := model.CheckFuzzBase, model.CheckFuzzCandidate
	if req.Confirm {
		baseKind, candidateKind = model.CheckFuzzBaseConfirm, model.CheckFuzzCandidateConfirm
	}
	return r.side(ctx, r.base, baseKind, req), r.side(ctx, r.candidate, candidateKind, req), nil
}

func (r *pytestRunner) side(ctx context.Context, dir, kind string, req Request) Side {
	h := req.Harness
	file := filepath.Join(dir, filepath.FromSlash(h.Path))
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		r.t.Fatalf("the harness path %s exists", h.Path)
	}
	if err := os.WriteFile(file, []byte(h.Content), 0644); err != nil {
		r.t.Fatal(err)
	}
	defer os.Remove(file)
	_ = os.Remove(r.observations)
	command := []string{r.python, "-m", "pytest", "-p", "no:cacheprovider", "-q", h.Path, "--junitxml=" + r.observations + ".xml"}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	err := cmd.Run()
	c := model.Check{ID: "check-" + strconv.Itoa(len(r.checks)+1), Kind: kind, Status: "PASS", Command: append([]string{"python3"}, command[1:]...), Output: log.String()}
	if exitErr, ok := err.(*exec.ExitError); ok {
		c.Status, c.ExitCode = "FAIL", exitErr.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	payload, _ := os.ReadFile(r.observations)
	if len(payload) > 0 {
		results, err := h.Normalize(payload)
		if err != nil {
			r.t.Fatalf("%s stream rejected: %v\n%s\n%s", kind, err, payload, log.String())
		}
		c.Results = results
	}
	r.checks = append(r.checks, c)
	return Side{Check: c}
}

func (r *pytestRunner) AddEvidence(e model.Evidence) (model.Evidence, error) {
	e.ID = "evidence-" + strconv.Itoa(len(r.evidence)+1)
	r.evidence = append(r.evidence, e)
	return e, nil
}

// TestPythonFuzzWithRealPytest selects the changed functions of a Python
// module, renders their harness and runs it with the real pytest on both
// revisions: a boundary change, a new zero guard and an in-place sort of a
// list argument diverge; equivalent rewrites (keyword-only and *args
// parameters, an async function) do not; a loop at 7 stops on the call
// timeout.
func TestPythonFuzzWithRealPytest(t *testing.T) {
	python := requirePython3Pytest(t)
	base, candidate := pyProject(t, pyCartBase), pyProject(t, pyCartCandidate)
	change := model.Change{Files: []model.ChangedFile{{Path: "shop/cart.py", Status: "M"}}}
	limits := Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 40, CallTimeout: 200 * time.Millisecond, MaxRuntime: 240 * time.Second}
	plan, err := SelectAll(context.Background(), base, candidate, change, nil, limits, FamilyPytest)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Packages) != 1 || plan.Packages[0].Python == nil || len(plan.Packages[0].Targets) != 7 || len(plan.Skipped) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	observations := filepath.Join(t.TempDir(), "probe-observations.jsonl")
	r := &pytestRunner{t: t, python: python, base: base, candidate: candidate, observations: observations}
	o := Options{Limits: limits, ObservationsPath: observations, PayloadLimit: harness.PayloadLimit(256 * 1024), NewSuffix: func() (string, error) { return "abcdef0123456789", nil }}
	rep := Run(context.Background(), r, plan, o)
	outcomes := map[string]model.FuzzFunction{}
	for _, fn := range rep.Functions {
		outcomes[fn.Symbol] = fn
	}
	want := map[string]string{
		"discount": model.FuzzDiverged, "ratio": model.FuzzDiverged, "mutate": model.FuzzDiverged,
		"label": model.FuzzNotDiverged, "merge": model.FuzzNotDiverged, "fetch": model.FuzzNotDiverged,
		"spin": model.FuzzInconclusive,
	}
	for name, outcome := range want {
		if outcomes[name].Outcome != outcome {
			t.Errorf("%s: %s (%s), want %s", name, outcomes[name].Outcome, outcomes[name].Reason, outcome)
		}
	}
	if c := outcomes["discount"].Counterexample; c == nil || c.Input != "discount(0.0, 0)" || c.Base != "0.0" || c.Candidate != `raise(error(ValueError, ("negative percent")))` {
		t.Errorf("discount counterexample %+v", c)
	}
	if c := outcomes["ratio"].Counterexample; c == nil || !strings.Contains(c.Base, "ZeroDivisionError") || c.Candidate != "inf" {
		t.Errorf("ratio counterexample %+v", c)
	}
	if c := outcomes["mutate"].Counterexample; c == nil || !strings.Contains(c.Candidate, "arg 1 after call") {
		t.Errorf("mutate counterexample %+v", c)
	}
	if !strings.Contains(outcomes["spin"].Reason, "spin(7)") {
		t.Errorf("spin reason %q", outcomes["spin"].Reason)
	}
	if len(r.requests) != 2 || !r.requests[1].Confirm || r.requests[0].Harness.Runner != harness.RunnerPytest {
		t.Fatalf("requests: %d", len(r.requests))
	}
	for _, e := range r.evidence {
		if e.Runner != harness.RunnerPytest || e.Path != "shop/test_probe_fuzz_abcdef0123456789.py" {
			t.Fatalf("evidence %+v", e)
		}
	}
}

// pyHostile is a module whose results define every hook a careless encoder
// would call; each hook writes a marker file.
const pyHostile = `import os

MARK = os.path.join(os.getcwd(), "touched")


def _touch(what):
    with open(MARK, "a") as f:
        f.write(what + "\n")


class Evil:
    __slots__ = ("kind", "count", "__dict__")

    def __init__(self, kind):
        self.kind = kind
        self.count = 2
        self.note = "plain"

    def __repr__(self):
        _touch("repr")
        return "Evil()"

    def __str__(self):
        _touch("str")
        return "Evil"

    def __eq__(self, other):
        _touch("eq")
        return True

    def __hash__(self):
        # build() hashes the key of its dict itself, so this hook is silent.
        return 1

    def __getattr__(self, name):
        _touch("getattr " + name)
        raise AttributeError(name)

    def __iter__(self):
        _touch("iter")
        return iter([])

    @property
    def secret(self):
        _touch("property")
        return 42


class SneakyDict(dict):
    def items(self):
        _touch("items")
        return []

    def __iter__(self):
        _touch("dict iter")
        return iter([])


class SneakyList(list):
    def __iter__(self):
        _touch("list iter")
        return iter([])


class Boom(Exception):
    def __str__(self):
        _touch("exception str")
        return "boom"


def build(kind: str) -> object:
    if kind == "":
        raise Boom("empty kind")
    return [Evil(kind), SneakyDict(a=Evil("x")), SneakyList([1, 2]), {Evil("k"): 1}]
`

func TestPythonHarnessNeverCallsTheCodeUnderTestToEncode(t *testing.T) {
	python := requirePython3Pytest(t)
	base := pyProject(t, strings.Replace(pyHostile, "return [Evil(kind)", "return [Evil(kind + \"\")", 1))
	candidate := pyProject(t, pyHostile)
	change := model.Change{Files: []model.ChangedFile{{Path: "shop/cart.py", Status: "M"}}}
	limits := Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 4, CallTimeout: time.Second, MaxRuntime: 240 * time.Second}
	plan, err := SelectAll(context.Background(), base, candidate, change, nil, limits, FamilyPytest)
	if err != nil || len(plan.Packages) != 1 || len(plan.Packages[0].Targets) != 1 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	observations := filepath.Join(t.TempDir(), "probe-observations.jsonl")
	r := &pytestRunner{t: t, python: python, base: base, candidate: candidate, observations: observations}
	o := Options{Limits: limits, ObservationsPath: observations, PayloadLimit: harness.PayloadLimit(256 * 1024), NewSuffix: func() (string, error) { return "abcdef0123456789", nil }}
	rep := Run(context.Background(), r, plan, o)
	for _, dir := range []string{base, candidate} {
		if b, err := os.ReadFile(filepath.Join(dir, "touched")); err == nil {
			t.Fatalf("the encoder called code under test:\n%s", b)
		}
	}
	fn := rep.Functions[0]
	if fn.Outcome != model.FuzzNotDiverged {
		t.Fatalf("outcome %s (%s)", fn.Outcome, fn.Reason)
	}
	stream, err := ParseResults(r.checks[1].Results)
	if err != nil {
		t.Fatal(err)
	}
	records := stream.Functions[0].Records
	if !strings.HasPrefix(records[0].Display, `raise(error(Boom, ("empty kind")))`) {
		t.Fatalf("exception encoding %q", records[0].Display)
	}
	// Slots and instance attributes, sorted; the dict and list subclasses are
	// read through dict.items and list.__iter__, not their overrides.
	if want := `[Evil{"count": 2, "kind": "a", "note": "plain"}, SneakyDict{"a": Evil{"count": 2, "kind": "x", "note": "plain"}}, SneakyList[1, 2], {Evil{"count": 2, "kind": "k", "note": "plain"}: 1}]`; records[1].Display != want {
		t.Fatalf("object encoding %q\nwant %q", records[1].Display, want)
	}
}

func TestSelectPythonReasons(t *testing.T) {
	baseSrc := `def untyped(a):
    return a


def optional(a: "int") -> int:
    return a


def union(a: int | None) -> int:
    return a or 0


def kwargs(a: int, **kw: int) -> int:
    return a


@staticmethod
def decorated(a: int) -> int:
    return a


def gen(a: int):
    yield a


def twice(a: int) -> int:
    return a


def twice(a: int) -> int:
    return a + 1


def signature(a: int) -> int:
    return a


def same(a: int) -> int:
    return a


def _private(a: int) -> int:
    return a


def public(a: list[str], *rest: float, flag: bool = True) -> int:
    return len(a)
`
	candSrc := strings.NewReplacer(
		"def untyped(a):\n    return a", "def untyped(a):\n    return a + 0",
		"def optional(a: \"int\") -> int:\n    return a", "def optional(a: \"int\") -> int:\n    return a + 0",
		"return a or 0", "return a if a else 0",
		"def kwargs(a: int, **kw: int) -> int:\n    return a", "def kwargs(a: int, **kw: int) -> int:\n    return a + 0",
		"def decorated(a: int) -> int:\n    return a", "def decorated(a: int) -> int:\n    return a + 0",
		"yield a", "yield a + 0",
		"def twice(a: int) -> int:\n    return a + 1", "def twice(a: int) -> int:\n    return a + 2",
		"def signature(a: int) -> int:\n    return a", "def signature(a: float) -> int:\n    return a",
		"def _private(a: int) -> int:\n    return a", "def _private(a: int) -> int:\n    return -a",
		"return len(a)", "return len(a) + len(rest)",
	).Replace(baseSrc)
	base, candidate := t.TempDir(), t.TempDir()
	for dir, src := range map[string]string{base: baseSrc, candidate: candSrc} {
		for _, p := range []string{"lib/mod.py", "tests/test_mod.py", "lib/conftest.py", "lib/my-mod.py"} {
			if err := os.MkdirAll(filepath.Join(dir, "lib"), 0755); err != nil {
				t.Fatal(err)
			}
			_ = os.MkdirAll(filepath.Join(dir, "tests"), 0755)
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(src), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	files := []model.ChangedFile{{Path: "lib/mod.py", Status: "M"}, {Path: "tests/test_mod.py", Status: "M"}, {Path: "lib/conftest.py", Status: "M"}, {Path: "lib/my-mod.py", Status: "M"}}
	sel := SelectPython(context.Background(), base, candidate, model.Change{Files: files})
	got := map[string]string{}
	for _, s := range sel.Skipped {
		got[s.Path+":"+s.Symbol] = s.Reason
	}
	want := map[string]string{
		"lib/mod.py:untyped":    reasonPythonUntyped("a"),
		"lib/mod.py:optional":   reasonParamType(`"int"`),
		"lib/mod.py:union":      reasonParamType("int|None"),
		"lib/mod.py:kwargs":     ReasonPythonKwargs,
		"lib/mod.py:decorated":  ReasonPythonDecorated,
		"lib/mod.py:gen":        ReasonPythonGenerator,
		"lib/mod.py:twice":      ReasonPythonRedefined,
		"lib/my-mod.py:public":  ReasonPythonModuleName,
		"lib/my-mod.py:untyped": ReasonPythonModuleName,
	}
	for k, reason := range want {
		if got[k] != reason {
			t.Errorf("%s: %q, want %q", k, got[k], reason)
		}
	}
	targets := map[string]Target{}
	for _, tg := range sel.Targets {
		targets[tg.Name] = tg
	}
	if len(targets) != 2 || targets["_private"].Exported || !targets["public"].Exported {
		t.Fatalf("targets %+v", sel.Targets)
	}
	public := targets["public"]
	if public.Signature != "(a: list[str], *rest: float, flag: bool = True) -> int" || len(public.Params) != 3 ||
		public.Params[0] != (Param{Kind: ParamSlice, Basic: "str"}) || public.Params[1] != (Param{Kind: ParamVariadic, Basic: "float"}) || public.Params[2] != (Param{Kind: ParamScalar, Basic: "bool", Keyword: "flag"}) {
		t.Fatalf("public %+v", public)
	}
	if _, ok := got["lib/mod.py:signature"]; ok {
		t.Fatal("a function whose signature changed was listed")
	}
	if _, ok := got["lib/mod.py:same"]; ok {
		t.Fatal("an unchanged function was listed")
	}
}

// TestPythonCorpusLiteralsEvaluate checks every edge literal of each type
// with the real Python: it evaluates to exactly the value Probe means.
func TestPythonCorpusLiteralsEvaluate(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on the host")
	}
	var program strings.Builder
	program.WriteString("import math, struct\nout = []\n")
	var want []string
	for _, s := range pythonEdges("int") {
		program.WriteString("out.append(repr(" + pythonLiteral("int", s) + "))\n")
		want = append(want, s.S)
	}
	for _, s := range pythonEdges("float") {
		program.WriteString("out.append(struct.pack('>d', " + pythonLiteral("float", s) + ").hex())\n")
		bits := make([]byte, 8)
		u := mathFloat64bits(s.F)
		if s.F != s.F {
			u = 0x7ff8000000000000 // the payload of a NaN is not part of the value
		}
		for i := 0; i < 8; i++ {
			bits[i] = byte(u >> (56 - 8*i))
		}
		want = append(want, hexString(bits))
	}
	for _, s := range pythonEdges("str") {
		program.WriteString("out.append(' '.join('%x' % ord(c) for c in " + pythonLiteral("str", s) + "))\n")
		var cps []string
		for i := 0; i < len(s.S); {
			r, size := decodeWTF8(s.S[i:])
			i += size
			cps = append(cps, strconv.FormatInt(int64(r), 16))
		}
		want = append(want, strings.Join(cps, " "))
	}
	program.WriteString("print('\\n'.join(out))\n")
	out, err := exec.Command(python, "-c", program.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, program.String())
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("%d values, want %d:\n%s", len(got), len(want), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("value %d: Python reads %q, want %q", i, got[i], want[i])
		}
	}
	// The corpus of a target is deterministic.
	target := Target{Dir: "shop/cart.py", Path: "shop/cart.py", Name: "f", Signature: "(a: int, b: list[str])", Language: LanguagePython,
		Params: []Param{{Kind: ParamScalar, Basic: "int"}, {Kind: ParamSlice, Basic: "str"}}}
	a, b := pythonCorpus(target, 40), pythonCorpus(target, 40)
	if len(a) != 40 || a[39].Call != b[39].Call || a[0].Call != `f(0, [])` {
		t.Fatalf("corpus %d %q", len(a), a[0].Call)
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func TestPythonStreamNames(t *testing.T) {
	lines := []string{scriptHeadLine(1, "abcdef01", "test_probe_fuzz_abcdef01_1"), beginLine(1, 1), obsLine(1, 0, "1", false), endLine(1, 1), scriptDoneLine(1, "test_probe_fuzz_abcdef01_1", 1)}
	h := Harness{Path: "shop/test_probe_fuzz_abcdef01.py", Suffix: "abcdef01", Display: 64, Runner: harness.RunnerPytest,
		Tests: []HarnessTest{{Name: "test_probe_fuzz_abcdef01_1", Target: Target{Name: "f"}, Inputs: []Input{{Call: "f()"}}}}}
	results, err := h.Normalize(rawStream(lines...))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := ParseResults(results); err != nil || s.Runner != harness.RunnerPytest {
		t.Fatalf("stream %+v %v", s, err)
	}
	// A Python test name in a jest_json stream, or a Jest one in a
	// pytest_junit stream, is refused.
	h.Runner = harness.RunnerJest
	if _, err := h.Normalize(rawStream(lines...)); err == nil {
		t.Fatal("a Python test name was accepted in a jest_json stream")
	}
}

func mathFloat64bits(f float64) uint64 { return math.Float64bits(f) }

func TestGoTemplateUnverifiedNamesThePytestTemplate(t *testing.T) {
	rep := model.FuzzReport{Skipped: []model.FuzzSkip{{Path: "pkg/a.go", Line: 3, Symbol: "pkg.A", Reason: ReasonGoTemplatePytest}}}
	lines := Unverified(rep, 0, 1)
	if len(lines) != 1 || !strings.Contains(lines[0], "is a pytest template") {
		t.Fatalf("lines %q", lines)
	}
	rep.Skipped[0].Reason = ReasonGoTemplate
	if lines := Unverified(rep, 0, 1); !strings.Contains(lines[0], "Vitest or Jest") {
		t.Fatalf("lines %q", lines)
	}
}
