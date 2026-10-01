package fuzz

// Python observation harness. One pytest module per Python module, run by
// the policy's pytest generated_test template on both revisions. It records
// observations the way the TS/JS harness does and writes the same stream
// records to the observation file, framed by a head record before and a done
// record after each test, from which the named execution is validated. It
// asserts nothing.

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/gvinsot/Probe/app/internal/harness"
)

// pythonHarnessPrefix starts the file name of a Python harness, so pytest's
// default python_files pattern (test_*.py) also matches it.
const pythonHarnessPrefix = "test_probe_fuzz_"

// pythonTestPrefix starts the name of every test of a Python harness: pytest
// collects functions whose name starts with "test".
const pythonTestPrefix = "test_probe_fuzz_"

// pythonTestNamePattern is the name of a test of a Python harness.
var pythonTestNamePattern = regexp.MustCompile(`^test_probe_fuzz_[0-9a-f]{8,32}_[1-9][0-9]?$`)

// PythonModule is the Python module whose changed functions one harness
// runs.
type PythonModule struct {
	Path string `json:"path"` // module file, the same path on both revisions
}

// RenderPython renders the observation harness of one Python module:
// <dir>/test_probe_fuzz_<suffix>.py next to the module, with one top-level
// test function per target, test_probe_fuzz_<suffix>_<n>, at column 0, as
// verifiable generated pytest tests are. The only repository-derived text in
// the file is the module path, rendered as a string literal, and the function
// names, each re-checked as an identifier; every value is a literal rendered
// by pythonLiteral. The harness loads the module by the dotted name a
// sys.path entry gives it, checking that the module it imports is the file,
// or from the file itself; it reads the functions as attributes of the
// module, so no name of the module can collide with a harness identifier.
//
// For every input it evaluates the function twice with fresh arguments. Each
// evaluation runs under a repeating interval timer (SIGALRM) with the call
// timeout, which interrupts Python code, and a returned coroutine is run to
// completion in a new event loop within the same timeout. The canonical
// encoding of the result (or the raised exception), and of each list
// argument after the call, is written by a fixed encoder bounded like the Go
// one (64 KiB, depth 16, 1,024 elements) that never calls code of the module
// under test, in printable ASCII only. When the file or its worst-case stream
// would not fit, the inputs are halved (at most three times) before
// RenderPython gives up.
func RenderPython(p PackagePlan, o RenderOptions) (Harness, error) {
	switch {
	case !suffixPattern.MatchString(o.Suffix):
		return Harness{}, errors.New("harness suffix must be 8 to 32 lowercase hex digits")
	case !strings.HasPrefix(o.ObservationsPath, "/") || strings.ContainsAny(o.ObservationsPath, "\x00\n\"\\'`"):
		return Harness{}, errors.New("observations path must be an absolute in-container path")
	case o.CallTimeout <= 0:
		return Harness{}, errors.New("call timeout must be positive")
	case o.PayloadLimit <= 0:
		return Harness{}, errors.New("payload limit must be positive")
	case len(p.Targets) == 0:
		return Harness{}, errors.New("no function to render")
	case p.Python == nil || p.Dir != p.Python.Path || !pythonModulePath(p.Python.Path):
		return Harness{}, errors.New("not a Python module")
	}
	if stem := strings.TrimSuffix(path.Base(p.Python.Path), ".py"); !pythonIdentifier.MatchString(stem) {
		return Harness{}, fmt.Errorf("module %q cannot be imported by a harness", p.Python.Path)
	}
	for _, t := range p.Targets {
		if t.Language != LanguagePython || t.Path != p.Python.Path || !pythonIdentifier.MatchString(t.Name) {
			return Harness{}, fmt.Errorf("function %q is not a function of the module", t.Name)
		}
		for _, param := range t.Params {
			if !pythonScalarType.MatchString(param.Basic) || param.Kind != ParamScalar && param.Kind != ParamSlice && param.Kind != ParamVariadic ||
				param.Keyword != "" && !pythonIdentifier.MatchString(param.Keyword) {
				return Harness{}, fmt.Errorf("parameter type %q is not generated", param.Basic)
			}
		}
	}
	file := pythonHarnessPrefix + o.Suffix + ".py"
	if dir := path.Dir(p.Python.Path); dir != "." {
		file = dir + "/" + file
	}
	budgets := make([]int, len(p.Targets))
	for i, t := range p.Targets {
		budgets[i] = max(1, t.Inputs)
	}
	var lastErr error
	for attempt := 0; attempt <= maxHalvings; attempt++ {
		h, err := renderPythonOnce(p, o, file, budgets)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if !errors.Is(err, errTooLarge) {
			break
		}
		for i := range budgets {
			budgets[i] = max(1, budgets[i]/2)
		}
	}
	return Harness{}, lastErr
}

func renderPythonOnce(p PackagePlan, o RenderOptions, file string, budgets []int) (Harness, error) {
	prefix := "_pf_" + o.Suffix
	h := Harness{Path: file, Suffix: o.Suffix, Runner: harness.RunnerPytest}
	records := 0
	for i, t := range p.Targets {
		inputs := pythonCorpus(t, budgets[i])
		if len(inputs) == 0 {
			return Harness{}, fmt.Errorf("%w: %s", ErrNoInput, t.Name)
		}
		t.Inputs = len(inputs)
		h.Tests = append(h.Tests, HarnessTest{Name: pythonTestPrefix + o.Suffix + "_" + strconv.Itoa(i+1), Target: t, Inputs: inputs})
		records += len(inputs)
	}
	usable := o.PayloadLimit - frameReserve - scriptFunctionOverhead*len(h.Tests)
	display := 0
	if usable > 0 {
		display = (usable/records - recordOverhead) / 2
	}
	display = min(MaxDisplayBytes, display)
	if display < minDisplayBytes {
		return Harness{}, errTooLarge
	}
	if records*(recordOverhead+2*display)+scriptFunctionOverhead*len(h.Tests)+frameReserve > o.PayloadLimit {
		return Harness{}, errTooLarge
	}
	h.Display = display
	var b strings.Builder
	b.WriteString(pythonHeader)
	b.WriteString(strings.NewReplacer(
		"ZPFZ", prefix,
		"OBSERVATIONS_PATH", pyString(o.ObservationsPath),
		"SUFFIX_TEXT", pyString(o.Suffix),
		"MODULE_PATH", pyString(p.Python.Path),
		"DISPLAY_BYTES", strconv.Itoa(display),
		"TIMEOUT_SECONDS", strconv.FormatFloat(max(0.001, o.CallTimeout.Seconds()), 'f', 3, 64),
		"MAX_BYTES", strconv.Itoa(MaxEncodingBytes),
		"MAX_DEPTH", strconv.Itoa(maxEncodingDepth),
		"MAX_ELEMS", strconv.Itoa(maxEncodingElems),
	).Replace(pythonRuntime))
	for i, test := range h.Tests {
		renderPythonTest(&b, prefix, i+1, test)
	}
	if b.Len() > MaxHarnessBytes {
		return Harness{}, errTooLarge
	}
	h.Content = b.String()
	return h, nil
}

// renderPythonTest writes the test of one function: its cases, each a
// function returning fresh positional and keyword arguments, and which
// arguments are lists whose state after the call is encoded (the elements of
// *args are passed as a fresh tuple, so they are not).
func renderPythonTest(b *strings.Builder, prefix string, n int, test HarnessTest) {
	t := test.Target
	var lists []string
	position := 0
	for _, p := range t.Params {
		switch {
		case p.Keyword != "" && p.Kind == ParamSlice:
			lists = append(lists, "("+pyString(p.Keyword)+", None)")
		case p.Keyword == "" && p.Kind == ParamSlice:
			lists = append(lists, "(None, "+strconv.Itoa(position)+")")
		}
		if p.Keyword == "" && p.Kind != ParamVariadic {
			position++
		}
	}
	fmt.Fprintf(b, "\n\n# %s observes %s (%d inputs).\n", test.Name, t.Name, len(test.Inputs))
	fmt.Fprintf(b, "def %s():\n", test.Name)
	fmt.Fprintf(b, "    %s_run(%d, %s, %s, [\n", prefix, n, pyString(test.Name), pyString(t.Name))
	for _, in := range test.Inputs {
		fmt.Fprintf(b, "        lambda: %s,\n", pythonCase(t, in.Args))
	}
	fmt.Fprintf(b, "    ], [%s])\n", strings.Join(lists, ", "))
}

// pythonCase renders the (args, kwargs) pair one case returns: a tuple of
// the positional values with the elements of *args spread in, and a dict of
// the keyword arguments.
func pythonCase(t Target, args []Value) string {
	var positional, keywords []string
	for j, p := range t.Params {
		var text string
		switch p.Kind {
		case ParamScalar:
			text = pythonLiteral(p.Basic, args[j].Scalar)
		case ParamVariadic:
			text = "*" + pythonList(p, args[j])
		default:
			text = pythonList(p, args[j])
		}
		if p.Keyword != "" {
			keywords = append(keywords, pyString(p.Keyword)+": "+text)
			continue
		}
		positional = append(positional, text+",")
	}
	return "((" + strings.Join(positional, " ") + "), {" + strings.Join(keywords, ", ") + "})"
}

// pythonHeader starts every Python harness.
const pythonHeader = `# Code generated by Probe differential fuzzing (probe-fuzz/v1). DO NOT EDIT.
#
# This module records observations of changed functions on seeded inputs. It
# asserts nothing: its tests pass unless the observation file cannot be written.
`

// pythonRuntime is the fixed part of every Python harness. ZPFZ becomes the
// per-run prefix. It defines no name that starts with "test", so pytest
// collects only the rendered tests, and it reads values of the module under
// test only through the methods of their builtin types: it never calls their
// __repr__, __str__, __eq__, properties or __getattr__.
const pythonRuntime = `
import asyncio as ZPFZ_asyncio
import collections as ZPFZ_collections
import datetime as ZPFZ_datetime
import decimal as ZPFZ_decimal
import enum as ZPFZ_enum
import fractions as ZPFZ_fractions
import hashlib as ZPFZ_hashlib
import importlib as ZPFZ_importlib
import importlib.util as ZPFZ_importlib_util
import inspect as ZPFZ_inspect
import os as ZPFZ_os
import pathlib as ZPFZ_pathlib
import signal as ZPFZ_signal
import sys as ZPFZ_sys
import types as ZPFZ_types
import uuid as ZPFZ_uuid

ZPFZ_PATH = OBSERVATIONS_PATH
ZPFZ_SUFFIX = SUFFIX_TEXT
ZPFZ_MODULE = MODULE_PATH
ZPFZ_DISPLAY = DISPLAY_BYTES
ZPFZ_TIMEOUT = TIMEOUT_SECONDS
ZPFZ_MAX_BYTES = MAX_BYTES
ZPFZ_MAX_DEPTH = MAX_DEPTH
ZPFZ_MAX_ELEMS = MAX_ELEMS

# ZPFZ_poisoned is set after an evaluation timed out: the interrupted code may
# have left state behind, so later functions of this process are not
# evaluated.
ZPFZ_state = {"poisoned": False, "armed": False}


def ZPFZ_emit(line):
    data = (line + "\n").encode("ascii")
    fd = ZPFZ_os.open(ZPFZ_PATH, ZPFZ_os.O_WRONLY | ZPFZ_os.O_APPEND | ZPFZ_os.O_CREAT, 0o644)
    try:
        while data:
            data = data[ZPFZ_os.write(fd, data):]
    finally:
        ZPFZ_os.close(fd)


def ZPFZ_load():
    # The module is imported by the dotted name a sys.path entry gives it,
    # as its own tests import it, and only when the imported module is the
    # file; otherwise it is loaded from the file.
    root = ZPFZ_os.getcwd()
    target = ZPFZ_os.path.realpath(ZPFZ_os.path.join(root, ZPFZ_MODULE))
    tried = set()
    for entry in list(ZPFZ_sys.path) + [root]:
        base = ZPFZ_os.path.realpath(entry or root)
        if not target.startswith(base + ZPFZ_os.sep):
            continue
        parts = target[len(base) + 1:-3].split(ZPFZ_os.sep)
        if parts and parts[-1] == "__init__":
            parts = parts[:-1]
        if not parts or not all(part.isidentifier() for part in parts):
            continue
        name = ".".join(parts)
        if name in tried:
            continue
        tried.add(name)
        if entry not in ZPFZ_sys.path:
            ZPFZ_sys.path.append(entry)
        try:
            module = ZPFZ_importlib.import_module(name)
        except ModuleNotFoundError as e:
            if e.name is not None and (name == e.name or name.startswith(e.name + ".")):
                continue
            raise
        found = getattr(module, "__file__", None)
        if found and ZPFZ_os.path.realpath(found) == target:
            return module
    spec = ZPFZ_importlib_util.spec_from_file_location("ZPFZ_module_under_test", target)
    module = ZPFZ_importlib_util.module_from_spec(spec)
    ZPFZ_sys.modules["ZPFZ_module_under_test"] = module
    spec.loader.exec_module(module)
    return module


ZPFZ_module = ZPFZ_load()


class ZPFZ_Timeout(BaseException):
    pass


def ZPFZ_alarm(signum, frame):
    if ZPFZ_state["armed"]:
        raise ZPFZ_Timeout()


def ZPFZ_bounded(thunk, seconds):
    # Runs thunk under a repeating interval timer: 0 when it returned, 1 on
    # the timeout, 3 when the harness itself failed (thunk catches everything
    # the code under test raises).
    if seconds <= 0:
        return 1
    previous = ZPFZ_signal.signal(ZPFZ_signal.SIGALRM, ZPFZ_alarm)
    ZPFZ_state["armed"] = True
    ZPFZ_signal.setitimer(ZPFZ_signal.ITIMER_REAL, seconds, 0.05)
    try:
        thunk()
        ZPFZ_state["armed"] = False
        return 0
    except ZPFZ_Timeout:
        return 1
    except BaseException:
        return 3
    finally:
        ZPFZ_state["armed"] = False
        ZPFZ_signal.setitimer(ZPFZ_signal.ITIMER_REAL, 0)
        ZPFZ_signal.signal(ZPFZ_signal.SIGALRM, previous)


def ZPFZ_text(s, limit):
    # The printable ASCII form of a string: quote and backslash escaped,
    # every other code point as \uXXXX or \UXXXXXXXX; None beyond limit.
    out = []
    size = 0
    for ch in s:
        c = ord(ch)
        if c == 34:
            piece = "\\\""
        elif c == 92:
            piece = "\\\\"
        elif 32 <= c < 127:
            piece = ch
        elif c <= 0xFFFF:
            piece = "\\u%04x" % c
        else:
            piece = "\\U%08x" % c
        size += len(piece)
        if size > limit:
            return None
        out.append(piece)
    return "".join(out)


ZPFZ_SCALARS = (ZPFZ_decimal.Decimal, ZPFZ_fractions.Fraction, ZPFZ_datetime.date, ZPFZ_datetime.datetime,
                ZPFZ_datetime.time, ZPFZ_datetime.timedelta, ZPFZ_datetime.timezone, ZPFZ_uuid.UUID,
                ZPFZ_pathlib.PurePosixPath, ZPFZ_pathlib.PosixPath, range, slice)


class ZPFZ_Encoder:
    # The canonical encoding of one evaluation, bounded in size, depth and
    # elements; cut records that a bound was reached.

    def __init__(self, limit, stack):
        self.parts = []
        self.size = 0
        self.limit = limit
        self.cut = False
        self.stack = stack

    def text(self):
        return "".join(self.parts)

    def w(self, s):
        if self.cut:
            return
        if self.size + len(s) > self.limit:
            self.cut = True
            return
        self.parts.append(s)
        self.size += len(s)

    def q(self, s):
        if self.cut:
            return
        t = ZPFZ_text(s, self.limit - self.size - 2)
        if t is None:
            self.cut = True
            return
        self.w("\"" + t + "\"")

    def name(self, cls):
        n = type.__dict__["__qualname__"].__get__(cls)
        t = ZPFZ_text(n if isinstance(n, str) else "?", 256)
        self.w("?" if t is None else t)

    def value(self, v, depth):
        if self.cut:
            return
        if depth > ZPFZ_MAX_DEPTH:
            self.w("<depth>")
            self.cut = True
            return
        cls = type(v)
        if v is None:
            self.w("None")
            return
        if cls is bool:
            self.w("True" if v else "False")
            return
        if issubclass(cls, ZPFZ_enum.Enum):
            self.name(cls)
            self.w(".")
            member = object.__getattribute__(v, "__dict__").get("_name_", "?")
            self.q(member if type(member) is str else "?")
            return
        if issubclass(cls, int):
            if cls is not int:
                self.name(cls)
                self.w("(")
            try:
                self.w(int.__repr__(v))
            except ValueError:
                self.w("<int too large>")
                self.cut = True
            if cls is not int:
                self.w(")")
            return
        if issubclass(cls, float):
            self.w(float.__repr__(v) if cls is float else type.__dict__["__qualname__"].__get__(cls) + "(" + float.__repr__(v) + ")")
            return
        if issubclass(cls, complex):
            self.w(complex.__repr__(v))
            return
        if issubclass(cls, str):
            if cls is not str:
                self.name(cls)
            self.q(str.__str__(v))
            return
        if issubclass(cls, (bytes, bytearray)):
            data = bytes(v)
            if len(data) > ZPFZ_MAX_BYTES:
                self.w("<bytes>")
                self.cut = True
                return
            self.w(("bytearray" if issubclass(cls, bytearray) else "b") + "\"")
            self.w("".join(chr(c) if 32 <= c < 127 and c not in (34, 92) else "\\x%02x" % c for c in data))
            self.w("\"")
            return
        if cls in ZPFZ_SCALARS:
            self.name(cls)
            self.w("(")
            self.q(cls.__repr__(v))
            self.w(")")
            return
        if cls in (ZPFZ_types.FunctionType, ZPFZ_types.BuiltinFunctionType, ZPFZ_types.MethodType, ZPFZ_types.LambdaType):
            self.w("function(")
            n = getattr(v, "__qualname__", "")
            self.q(n if type(n) is str else "")
            self.w(")")
            return
        if cls is ZPFZ_types.ModuleType:
            self.w("module(")
            n = v.__dict__.get("__name__", "?")
            self.q(n if type(n) is str else "?")
            self.w(")")
            return
        if isinstance(v, type):
            self.w("class(")
            self.name(v)
            self.w(")")
            return
        if cls in (ZPFZ_types.GeneratorType, ZPFZ_types.CoroutineType, ZPFZ_types.AsyncGeneratorType):
            self.w("<" + cls.__name__ + ">")
            return
        key = id(v)
        if key in self.stack:
            self.w("<cycle>")
            return
        self.stack.append(key)
        try:
            self.container(v, cls, depth)
        finally:
            self.stack.pop()

    def container(self, v, cls, depth):
        if issubclass(cls, BaseException):
            self.w("error(")
            self.name(cls)
            self.w(", ")
            self.value(tuple(BaseException.__dict__["args"].__get__(v)), depth + 1)
            self.w(")")
            return
        if issubclass(cls, dict):
            if cls is not dict:
                self.name(cls)
            self.entries([(k, x) for k, x in dict.items(v)], depth, True)
            return
        if issubclass(cls, (set, frozenset)):
            if cls is not set:
                self.name(cls)
            self.entries([(None, x) for x in (set.__iter__(v) if issubclass(cls, set) else frozenset.__iter__(v))], depth, False)
            return
        if issubclass(cls, tuple):
            if cls is not tuple:
                self.name(cls)
            self.sequence(list(tuple.__iter__(v)), depth, "(", ")")
            return
        if issubclass(cls, list):
            if cls is not list:
                self.name(cls)
            self.sequence(list(list.__iter__(v)), depth, "[", "]")
            return
        if issubclass(cls, ZPFZ_collections.deque):
            self.name(cls)
            self.sequence(list(ZPFZ_collections.deque.__iter__(v)), depth, "[", "]")
            return
        self.instance(v, cls, depth)

    def sequence(self, items, depth, open_, close):
        self.w(open_)
        for i, item in enumerate(items):
            if i >= ZPFZ_MAX_ELEMS:
                self.w(", ...")
                self.cut = True
                return
            if i > 0:
                self.w(", ")
            self.value(item, depth + 1)
            if self.cut:
                return
        self.w(close)

    def entries(self, pairs, depth, mapping):
        items = []
        cut = False
        for k, x in pairs:
            remaining = self.limit - self.size
            if len(items) >= ZPFZ_MAX_ELEMS or remaining <= 0:
                cut = True
                break
            e = ZPFZ_Encoder(remaining, self.stack)
            if mapping:
                e.value(k, depth + 1)
                e.w(": ")
            e.value(x, depth + 1)
            if e.cut:
                cut = True
            items.append(e.text())
        items.sort()
        self.w("{")
        self.w(", ".join(items))
        self.w("}")
        if cut:
            self.cut = True

    def instance(self, v, cls, depth):
        # An object: its class name and its attributes, read from the
        # instance dictionary and the slot descriptors of its classes, sorted.
        self.name(cls)
        fields = []
        try:
            own = object.__getattribute__(v, "__dict__")
        except AttributeError:
            own = None
        if type(own) is dict:
            fields.extend((k, x) for k, x in dict.items(own) if type(k) is str)
        for klass in type.__dict__["__mro__"].__get__(cls):
            for k, d in type.__dict__["__dict__"].__get__(klass).items():
                if type(d) is ZPFZ_types.MemberDescriptorType and type(k) is str and not any(k == f for f, _ in fields):
                    try:
                        fields.append((k, d.__get__(v, cls)))
                    except AttributeError:
                        pass
        fields.sort(key=lambda kv: kv[0])
        self.w("{")
        for i, (k, x) in enumerate(fields):
            if i >= ZPFZ_MAX_ELEMS:
                self.w(", ...")
                self.cut = True
                return
            if i > 0:
                self.w(", ")
            self.q(k)
            self.w(": ")
            self.value(x, depth + 1)
            if self.cut:
                return
        self.w("}")


def ZPFZ_call(name, make, lists):
    # Evaluates one input within the call timeout: (0, encoding, cut,
    # raised) when it returned or raised, (1,) on the timeout, (3,) when the
    # harness failed.
    holder = {}

    def run():
        args, kwargs = make()
        holder["args"], holder["kwargs"] = args, kwargs
        holder["awaited"] = False
        try:
            target = ZPFZ_module.__dict__[name]
            result = target(*args, **kwargs)
            if type(result) is ZPFZ_types.CoroutineType:
                holder["awaited"] = True
                result = ZPFZ_asyncio.run(result)
            holder["raised"], holder["result"] = False, result
        except ZPFZ_Timeout:
            raise
        except BaseException as e:
            holder["raised"], holder["result"] = True, e

    state = ZPFZ_bounded(run, ZPFZ_TIMEOUT)
    if state != 0:
        return (state,)
    e = ZPFZ_Encoder(ZPFZ_MAX_BYTES, [])

    def encode():
        try:
            if holder["awaited"]:
                e.w("await ")
            if holder["raised"]:
                e.w("raise(")
                e.value(holder["result"], 0)
                e.w(")")
            else:
                e.value(holder["result"], 0)
            for keyword, position in lists:
                if keyword is None:
                    e.w("; arg %d after call: " % (position + 1))
                    e.value(holder["args"][position], 0)
                else:
                    e.w("; arg ")
                    e.q(keyword)
                    e.w(" after call: ")
                    e.value(holder["kwargs"][keyword], 0)
        except ZPFZ_Timeout:
            raise
        except BaseException:
            e.w("<encoding failed>")
            e.cut = True

    state = ZPFZ_bounded(encode, ZPFZ_TIMEOUT)
    if state != 0:
        return (state,)
    return (0, e.text(), e.cut, holder["raised"])


def ZPFZ_quote(s):
    # Encodes s as a JSON string exactly as the host does: quote, backslash
    # and control characters escaped, everything else copied.
    out = ["\""]
    for ch in s:
        c = ord(ch)
        if c == 34:
            out.append("\\\"")
        elif c == 92:
            out.append("\\\\")
        elif c < 32:
            out.append("\\u%04x" % c)
        else:
            out.append(ch)
    out.append("\"")
    return "".join(out)


def ZPFZ_bool(b):
    return "true" if b else "false"


def ZPFZ_run(f, test, name, cases, lists):
    # Evaluates every input of function name twice and appends one record per
    # input to the observation file, between the head and begin records and
    # an end (or stop) and done record. A timeout or a harness failure ends
    # the function with a stop record.
    head = "{\"f\":%d,\"k\":" % f
    ZPFZ_emit(head + "\"head\",\"s\":" + ZPFZ_quote(ZPFZ_SUFFIX) + ",\"w\":" + ZPFZ_quote(test) + "}")
    n = len(cases)
    ZPFZ_emit(head + "\"begin\",\"n\":%d}" % n)
    observed = 0
    if ZPFZ_state["poisoned"]:
        ZPFZ_emit(head + "\"stop\",\"i\":0,\"x\":\"poisoned\"}")
    else:
        stopped = False
        for k in range(n):
            first = ZPFZ_call(name, cases[k], lists)
            second = first
            if first[0] == 0:
                second = ZPFZ_call(name, cases[k], lists)
            state = second[0] if first[0] == 0 else first[0]
            if state != 0:
                reason = "abort"
                if state == 1:
                    reason = "timeout"
                    ZPFZ_state["poisoned"] = True
                ZPFZ_emit(head + "\"stop\",\"i\":%d,\"x\":\"%s\"}" % (k, reason))
                stopped = True
                break
            enc, cut, raised = first[1], first[2], first[3]
            digest = ZPFZ_hashlib.sha256(enc.encode("ascii")).hexdigest()
            unstable = enc != second[1] or cut != second[2]
            shown = enc[:ZPFZ_DISPLAY]
            ZPFZ_emit(head + "\"obs\",\"i\":%d,\"h\":\"%s\",\"l\":%d,\"o\":%s,\"p\":%s,\"d\":%s,\"t\":%s}" % (
                k, digest, len(enc), ZPFZ_quote(shown), ZPFZ_bool(raised), ZPFZ_bool(unstable), ZPFZ_bool(cut)))
            observed += 1
        if not stopped:
            ZPFZ_emit(head + "\"end\",\"n\":%d}" % n)
    ZPFZ_emit(head + "\"done\",\"w\":" + ZPFZ_quote(test) + ",\"n\":%d}" % observed)
`
