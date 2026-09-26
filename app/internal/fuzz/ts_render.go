package fuzz

// TS/JS observation harness (F2c). One test file per module, run by the
// policy's Vitest or Jest generated_test template on both revisions. It
// records observations the way the Go harness does and writes the same
// stream records to the observation file, framed by a head record (the
// per-run suffix and the test name) before and a done record (the test name
// and the number of observations) after each test, from which the named
// execution is validated (Appendix D.13). It asserts nothing.

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
)

// Runner families of a TS/JS generated_test template.
const (
	FamilyVitest = "vitest"
	FamilyJest   = "jest"
)

// Bounds of a TS/JS harness.
const (
	// scriptFunctionOverhead bounds the head, begin, stop or end, and done
	// records of one function.
	scriptFunctionOverhead = 384
	// scriptTestTimeoutSlack is added to the framework timeout of one test,
	// beyond two evaluations of every input.
	scriptTestTimeoutSlack = 60000
	// maxScriptTestTimeout is the largest timeout the frameworks accept.
	maxScriptTestTimeout = 1<<31 - 1
	scriptHarnessPrefix  = "swiftproof-fuzz-"
)

// ScriptFamily returns the runner family of a generated_test template that
// can run a TS/JS fuzz harness: a verifiable JavaScript/TypeScript template
// (the {file} target and the {results_out} report, no package-manager script
// or shell) that runs vitest or jest. It returns "" otherwise.
func ScriptFamily(cmd []string) string {
	if !harness.VerifiableJSTemplate(cmd) {
		return ""
	}
	family := ""
	for _, arg := range cmd {
		base := path.Base(strings.ReplaceAll(arg, "\\", "/"))
		for _, ext := range []string{".js", ".mjs", ".cjs"} {
			base = strings.TrimSuffix(base, ext)
		}
		if base != FamilyVitest && base != FamilyJest {
			continue
		}
		if family != "" && family != base {
			return ""
		}
		family = base
	}
	return family
}

// scriptModule describes how a harness next to module p imports it: an
// extensionless specifier for .ts, .tsx, .js and .jsx modules (as tests of
// such modules usually import them), and the file name for .mts and .mjs
// modules. The harness is TypeScript for a TypeScript module.
func scriptModule(p string) (ScriptModule, bool) {
	base := path.Base(p)
	ext := path.Ext(base)
	m := ScriptModule{Path: p, Import: "./" + strings.TrimSuffix(base, ext), Ext: "js"}
	switch ext {
	case ".ts", ".tsx":
		m.Ext = "ts"
	case ".mts":
		m.Import, m.Ext = "./"+base, "ts"
	case ".mjs":
		m.Import = "./" + base
	case ".js", ".jsx":
	default:
		return ScriptModule{}, false
	}
	if !scriptModuleName.MatchString(base) {
		return ScriptModule{}, false
	}
	return m, true
}

// RenderScript renders the observation harness of one TS/JS module:
// <dir>/swiftproof-fuzz-<suffix>.test.ts (or .js for a JavaScript module),
// with one top-level test per function, titled TestSwiftProofFuzz_<suffix>_<n>
// at column 0 with a static title, as verifiable generated tests are. The
// only repository-derived text in the file is the import specifier of the
// module and the exported names, each re-checked; every value is a literal
// rendered by scriptLiteral. The file imports the functions under aliases
// that carry the per-run prefix, so no name of the module can collide with a
// harness identifier.
//
// For every input it evaluates the function twice with fresh arguments. Each
// evaluation calls the function inside node:vm with the call timeout, which
// interrupts a synchronous loop; a returned native promise is awaited for the
// rest of the timeout. The canonical encoding of the result (or the thrown
// or rejected value), and of each array argument after the call, is written
// by a fixed encoder bounded like the Go one (64 KiB, depth 16, 1,024
// elements), without calling getters, and in printable ASCII only. When the
// file or its worst-case stream would not fit, the inputs are halved (at most
// three times) before RenderScript gives up.
func RenderScript(p PackagePlan, o RenderOptions) (Harness, error) {
	switch {
	case !suffixPattern.MatchString(o.Suffix):
		return Harness{}, errors.New("harness suffix must be 8 to 32 lowercase hex digits")
	case !strings.HasPrefix(o.ObservationsPath, "/") || strings.ContainsAny(o.ObservationsPath, "\x00\n\"\\`"):
		return Harness{}, errors.New("observations path must be an absolute in-container path")
	case o.CallTimeout <= 0:
		return Harness{}, errors.New("call timeout must be positive")
	case o.PayloadLimit <= 0:
		return Harness{}, errors.New("payload limit must be positive")
	case o.Family != FamilyVitest && o.Family != FamilyJest:
		return Harness{}, errors.New("the TS/JS runner family must be vitest or jest")
	case len(p.Targets) == 0:
		return Harness{}, errors.New("no function to render")
	case p.Script == nil:
		return Harness{}, errors.New("not a TS/JS module")
	}
	module, ok := scriptModule(p.Script.Path)
	if !ok || module != *p.Script || p.Dir != p.Script.Path {
		return Harness{}, fmt.Errorf("module %q cannot be imported by a harness", p.Script.Path)
	}
	for _, t := range p.Targets {
		if t.Language != LanguageScript || t.Path != p.Script.Path || t.Name != "default" && !asciiIdentifier.MatchString(t.Name) {
			return Harness{}, fmt.Errorf("function %q is not an exported name of the module", t.Name)
		}
		for _, param := range t.Params {
			if !scriptScalarType.MatchString(param.Basic) || param.Kind != ParamScalar && param.Kind != ParamSlice && param.Kind != ParamVariadic {
				return Harness{}, fmt.Errorf("parameter type %q is not generated", param.Basic)
			}
		}
	}
	file := scriptHarnessPrefix + o.Suffix + ".test." + module.Ext
	if dir := path.Dir(p.Script.Path); dir != "." {
		file = dir + "/" + file
	}
	budgets := make([]int, len(p.Targets))
	for i, t := range p.Targets {
		budgets[i] = max(1, t.Inputs)
	}
	var lastErr error
	for attempt := 0; attempt <= maxHalvings; attempt++ {
		h, err := renderScriptOnce(p, o, module, file, budgets)
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

func renderScriptOnce(p PackagePlan, o RenderOptions, module ScriptModule, file string, budgets []int) (Harness, error) {
	prefix := harnessPrefix + o.Suffix
	h := Harness{Path: file, Suffix: o.Suffix, Runner: harness.RunnerJest}
	records := 0
	for i, t := range p.Targets {
		inputs := scriptCorpus(t, budgets[i])
		if len(inputs) == 0 {
			return Harness{}, fmt.Errorf("%w: %s", ErrNoInput, t.Name)
		}
		t.Inputs = len(inputs)
		h.Tests = append(h.Tests, HarnessTest{Name: testPrefix + o.Suffix + "_" + strconv.Itoa(i+1), Target: t, Inputs: inputs})
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
	b.WriteString(scriptHeader)
	if o.Family == FamilyVitest {
		b.WriteString("import { test } from \"vitest\";\n")
	}
	b.WriteString(strings.ReplaceAll(scriptImports, "ZSPFZ", prefix))
	aliases := make([]string, len(h.Tests))
	for i, t := range h.Tests {
		aliases[i] = fmt.Sprintf("%s as %s_f%d", t.Target.Name, prefix, i+1)
	}
	fmt.Fprintf(&b, "import { %s } from %s;\n", strings.Join(aliases, ", "), jsString(module.Import))
	b.WriteString(strings.NewReplacer(
		"ZSPFZ", prefix,
		"OBSERVATIONS_PATH", jsString(o.ObservationsPath),
		"SUFFIX_TEXT", jsString(o.Suffix),
		"DISPLAY_BYTES", strconv.Itoa(display),
		"TIMEOUT_MS", strconv.FormatInt(max(1, o.CallTimeout.Milliseconds()), 10),
		"MAX_BYTES", strconv.Itoa(MaxEncodingBytes),
		"MAX_DEPTH", strconv.Itoa(maxEncodingDepth),
		"MAX_ELEMS", strconv.Itoa(maxEncodingElems),
	).Replace(scriptRuntime))
	for i, test := range h.Tests {
		renderScriptTest(&b, prefix, i+1, test, o)
	}
	if b.Len() > MaxHarnessBytes {
		return Harness{}, errTooLarge
	}
	h.Content = b.String()
	return h, nil
}

// renderScriptTest writes the test of one function: its cases, each a
// function returning fresh arguments, and which arguments are arrays whose
// state after the call is encoded (a rest parameter is a fresh array inside
// the function, so its elements are not).
func renderScriptTest(b *strings.Builder, prefix string, n int, test HarnessTest, o RenderOptions) {
	t := test.Target
	timeout := int64(2*len(test.Inputs))*max(1, o.CallTimeout.Milliseconds()) + scriptTestTimeoutSlack
	timeout = min(timeout, maxScriptTestTimeout)
	lists := make([]string, 0, len(t.Params))
	for _, p := range t.Params {
		switch p.Kind {
		case ParamSlice:
			lists = append(lists, "true")
		case ParamScalar:
			lists = append(lists, "false")
		}
	}
	fmt.Fprintf(b, "\n// %s observes %s (%d inputs).\n", test.Name, t.Name, len(test.Inputs))
	fmt.Fprintf(b, "test(%q, async function () {\n", test.Name)
	fmt.Fprintf(b, "  await %s_run(%d, %q, %s_f%d, [\n", prefix, n, test.Name, prefix, n)
	for _, in := range test.Inputs {
		fmt.Fprintf(b, "    function () { return [%s]; },\n", scriptArgs(t, in.Args))
	}
	fmt.Fprintf(b, "  ], [%s]);\n", strings.Join(lists, ", "))
	fmt.Fprintf(b, "}, %d);\n", timeout)
}

// scriptHeader starts every TS/JS harness. The first line keeps a TypeScript
// type check (ts-jest, for example) from rejecting the file for the types it
// does not declare.
const scriptHeader = `// @ts-nocheck
// Code generated by SwiftProof differential fuzzing (swiftproof-fuzz/v1). DO NOT EDIT.
//
// This file records observations of changed functions on seeded inputs. It
// asserts nothing: its tests pass unless the observation file cannot be written.
`

const scriptImports = `import * as ZSPFZ_fs from "node:fs";
import * as ZSPFZ_crypto from "node:crypto";
import * as ZSPFZ_util from "node:util";
import * as ZSPFZ_vm from "node:vm";
`

// scriptRuntime is the fixed part of every TS/JS harness. ZSPFZ becomes the
// per-run prefix. It is plain JavaScript that is also valid TypeScript, it
// declares no line that starts with test( or it(, and it contains no
// backtick.
const scriptRuntime = `
const ZSPFZ_path = OBSERVATIONS_PATH;
const ZSPFZ_suffix = SUFFIX_TEXT;
const ZSPFZ_display = DISPLAY_BYTES;
const ZSPFZ_timeout = TIMEOUT_MS;
const ZSPFZ_maxBytes = MAX_BYTES;
const ZSPFZ_maxDepth = MAX_DEPTH;
const ZSPFZ_maxElems = MAX_ELEMS;

const ZSPFZ_types = ZSPFZ_util.types;
const ZSPFZ_context = ZSPFZ_vm.createContext({});
const ZSPFZ_script = new ZSPFZ_vm.Script("ZSPFZ_thunk()");
// ZSPFZ_poisoned is set after an evaluation timed out: a promise may still
// settle later, so later functions of this process are not evaluated.
let ZSPFZ_poisoned = false;

function ZSPFZ_emit(line) {
  ZSPFZ_fs.appendFileSync(ZSPFZ_path, line + "\n");
}

// ZSPFZ_bounded runs thunk inside node:vm with a timeout, which interrupts
// synchronous code. It returns 0 when thunk returned, 1 on the timeout and 3
// when the harness itself failed: thunk catches everything the code under
// test throws.
function ZSPFZ_bounded(thunk, ms) {
  ZSPFZ_context.ZSPFZ_thunk = thunk;
  try {
    ZSPFZ_script.runInContext(ZSPFZ_context, { timeout: Math.max(1, Math.floor(ms)) });
    return 0;
  } catch (e) {
    return e !== null && typeof e === "object" && e.code === "ERR_SCRIPT_EXECUTION_TIMEOUT" ? 1 : 3;
  } finally {
    ZSPFZ_context.ZSPFZ_thunk = undefined;
  }
}

// ZSPFZ_text returns the printable ASCII form of a string: quote and
// backslash escaped, every other code unit outside printable ASCII as
// \uXXXX; null when it would exceed limit characters.
function ZSPFZ_text(s, limit) {
  let out = "";
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    let piece;
    if (c === 34) piece = "\\\"";
    else if (c === 92) piece = "\\\\";
    else if (c >= 32 && c < 127) piece = s.charAt(i);
    else piece = "\\u" + (c + 0x10000).toString(16).slice(1);
    if (out.length + piece.length > limit) return null;
    out += piece;
  }
  return out;
}

// ZSPFZ_data reads a data property along the prototype chain without calling
// a getter; undefined for an accessor or a missing property.
function ZSPFZ_data(v, key) {
  for (let o = v, n = 0; o !== null && n < ZSPFZ_maxDepth; o = Object.getPrototypeOf(o), n++) {
    const d = Object.getOwnPropertyDescriptor(o, key);
    if (d !== undefined) return "value" in d ? d.value : undefined;
  }
  return undefined;
}

function ZSPFZ_ctor(v) {
  const proto = Object.getPrototypeOf(v);
  if (proto === null) return "null-prototype";
  const c = ZSPFZ_data(proto, "constructor");
  const name = typeof c === "function" ? ZSPFZ_data(c, "name") : undefined;
  return typeof name === "string" && name !== "" ? name : "?";
}

// ZSPFZ_Encoder builds the canonical encoding of one evaluation, bounded in
// size, depth and elements; cut records that a bound was reached.
function ZSPFZ_Encoder(limit, stack) {
  this.b = "";
  this.limit = limit;
  this.cut = false;
  this.stack = stack;
}

ZSPFZ_Encoder.prototype.w = function (s) {
  if (this.cut) return;
  if (this.b.length + s.length > this.limit) {
    this.cut = true;
    return;
  }
  this.b += s;
};

ZSPFZ_Encoder.prototype.q = function (s) {
  if (this.cut) return;
  const t = ZSPFZ_text(s, this.limit - this.b.length - 2);
  if (t === null) {
    this.cut = true;
    return;
  }
  this.w("\"" + t + "\"");
};

ZSPFZ_Encoder.prototype.name = function (s) {
  const t = ZSPFZ_text(s, 256);
  this.w(t === null ? "?" : t);
};

// value encodes v: numbers as JavaScript writes them with negative zero
// apart, strings quoted, undefined and null apart, objects with their
// constructor name and sorted own enumerable keys (accessors are not
// called), Map and Set entries sorted by encoding, errors as name and
// message, cycles marked, and proxies not inspected.
ZSPFZ_Encoder.prototype.value = function (v, depth) {
  if (this.cut) return;
  if (depth > ZSPFZ_maxDepth) {
    this.w("<depth>");
    this.cut = true;
    return;
  }
  switch (typeof v) {
    case "undefined":
      this.w("undefined");
      return;
    case "boolean":
      this.w(v ? "true" : "false");
      return;
    case "number":
      this.w(Object.is(v, -0) ? "-0" : String(v));
      return;
    case "bigint":
      this.w(String(v) + "n");
      return;
    case "string":
      this.q(v);
      return;
    case "symbol": {
      const d = v.description;
      this.w("Symbol(");
      this.q(typeof d === "string" ? d : "");
      this.w(")");
      return;
    }
    case "function": {
      const n = ZSPFZ_types.isProxy(v) ? "" : ZSPFZ_data(v, "name");
      this.w("function(");
      this.q(typeof n === "string" ? n : "");
      this.w(")");
      return;
    }
  }
  if (v === null) {
    this.w("null");
    return;
  }
  if (this.stack.indexOf(v) >= 0) {
    this.w("<cycle>");
    return;
  }
  this.stack.push(v);
  try {
    this.object(v, depth);
  } finally {
    this.stack.pop();
  }
};

ZSPFZ_Encoder.prototype.object = function (v, depth) {
  const t = ZSPFZ_types;
  if (t.isProxy(v)) {
    this.w("<proxy>");
    return;
  }
  if (Array.isArray(v)) {
    const name = ZSPFZ_ctor(v);
    if (name !== "Array") this.name(name);
    this.list(v, v.length, depth, true);
    return;
  }
  if (t.isTypedArray(v)) {
    this.name(ZSPFZ_ctor(v));
    this.list(v, v.length, depth, false);
    return;
  }
  if (t.isArrayBuffer(v) || t.isSharedArrayBuffer(v)) {
    this.w("ArrayBuffer");
    const bytes = new Uint8Array(v);
    this.list(bytes, bytes.length, depth, false);
    return;
  }
  if (t.isDataView(v)) {
    this.w("DataView(" + v.byteLength + ")");
    return;
  }
  if (t.isDate(v)) {
    const ms = Date.prototype.getTime.call(v);
    this.w("Date(" + (ms !== ms ? "Invalid" : Date.prototype.toISOString.call(v)) + ")");
    return;
  }
  if (t.isRegExp(v)) {
    this.w("RegExp(");
    this.q(RegExp.prototype.toString.call(v));
    this.w(")");
    return;
  }
  if (t.isNativeError(v) || v instanceof Error) {
    const n = ZSPFZ_data(v, "name");
    const m = ZSPFZ_data(v, "message");
    this.w("error(");
    this.q(typeof n === "string" ? n : "");
    this.w(", ");
    this.q(typeof m === "string" ? m : "");
    this.w(")");
    return;
  }
  if (t.isMap(v) || t.isSet(v)) {
    this.entries(v, depth, t.isMap(v));
    return;
  }
  if (t.isPromise(v)) {
    this.w("Promise");
    return;
  }
  if (t.isWeakMap(v) || t.isWeakSet(v)) {
    this.w(t.isWeakMap(v) ? "WeakMap" : "WeakSet");
    return;
  }
  if (t.isBoxedPrimitive(v)) {
    let inner;
    if (t.isNumberObject(v)) inner = Number.prototype.valueOf.call(v);
    else if (t.isStringObject(v)) inner = String.prototype.valueOf.call(v);
    else if (t.isBooleanObject(v)) inner = Boolean.prototype.valueOf.call(v);
    else if (t.isBigIntObject(v)) inner = BigInt.prototype.valueOf.call(v);
    else inner = Symbol.prototype.valueOf.call(v);
    this.name(ZSPFZ_ctor(v));
    this.w("(");
    this.value(inner, depth + 1);
    this.w(")");
    return;
  }
  const name = ZSPFZ_ctor(v);
  if (name !== "Object") this.name(name);
  const keys = Object.keys(v).sort();
  this.w("{");
  for (let i = 0; i < keys.length; i++) {
    if (i >= ZSPFZ_maxElems) {
      this.w(", ...");
      this.cut = true;
      return;
    }
    if (i > 0) this.w(", ");
    this.q(keys[i]);
    this.w(": ");
    const d = Object.getOwnPropertyDescriptor(v, keys[i]);
    if (d === undefined) this.w("<removed>");
    else if ("value" in d) this.value(d.value, depth + 1);
    else this.w("<accessor>");
    if (this.cut) return;
  }
  this.w("}");
};

ZSPFZ_Encoder.prototype.list = function (v, n, depth, holes) {
  this.w("[");
  for (let i = 0; i < n; i++) {
    if (i >= ZSPFZ_maxElems) {
      this.w(", ...");
      this.cut = true;
      return;
    }
    if (i > 0) this.w(", ");
    if (holes) {
      const d = Object.getOwnPropertyDescriptor(v, i);
      if (d === undefined) this.w("<hole>");
      else if ("value" in d) this.value(d.value, depth + 1);
      else this.w("<accessor>");
    } else {
      this.value(v[i], depth + 1);
    }
    if (this.cut) return;
  }
  this.w("]");
};

ZSPFZ_Encoder.prototype.entries = function (v, depth, isMap) {
  const items = [];
  let cut = false;
  const self = this;
  const each = function (value, key) {
    if (cut) return;
    const remaining = self.limit - self.b.length;
    if (items.length >= ZSPFZ_maxElems || remaining <= 0) {
      cut = true;
      return;
    }
    const e = new ZSPFZ_Encoder(remaining, self.stack);
    if (isMap) {
      e.value(key, depth + 1);
      e.w(" => ");
    }
    e.value(value, depth + 1);
    if (e.cut) cut = true;
    items.push(e.b);
  };
  if (isMap) Map.prototype.forEach.call(v, each);
  else Set.prototype.forEach.call(v, each);
  items.sort();
  this.name(ZSPFZ_ctor(v));
  this.w("{");
  for (let i = 0; i < items.length; i++) {
    if (i > 0) this.w(", ");
    this.w(items[i]);
  }
  this.w("}");
  if (cut) this.cut = true;
};

// ZSPFZ_settle waits for a promise for at most ms milliseconds: null on the
// timeout.
function ZSPFZ_settle(p, ms) {
  return new Promise(function (resolve) {
    const timer = setTimeout(function () {
      resolve(null);
    }, Math.max(1, Math.floor(ms)));
    Promise.prototype.then.call(
      p,
      function (value) {
        clearTimeout(timer);
        resolve({ ok: true, value: value });
      },
      function (value) {
        clearTimeout(timer);
        resolve({ ok: false, value: value });
      },
    );
  });
}

// ZSPFZ_call evaluates one input within the call timeout: state 0 with the
// encoding, 1 on the timeout, 3 when the harness failed.
async function ZSPFZ_call(target, make, lists) {
  const started = Date.now();
  let args;
  let result;
  let threw = false;
  let pending = false;
  let state = ZSPFZ_bounded(function () {
    args = make();
    try {
      result = Reflect.apply(target, undefined, args);
      pending = ZSPFZ_types.isPromise(result);
    } catch (e) {
      threw = true;
      result = e;
    }
  }, ZSPFZ_timeout);
  if (state !== 0) return { state: state };
  let settled = "";
  if (pending) {
    const outcome = await ZSPFZ_settle(result, ZSPFZ_timeout - (Date.now() - started));
    if (outcome === null) return { state: 1 };
    settled = outcome.ok ? "resolved" : "rejected";
    threw = !outcome.ok;
    result = outcome.value;
  }
  const e = new ZSPFZ_Encoder(ZSPFZ_maxBytes, []);
  state = ZSPFZ_bounded(function () {
    try {
      if (settled !== "") {
        e.w(settled + "(");
        e.value(result, 0);
        e.w(")");
      } else if (threw) {
        e.w("throw(");
        e.value(result, 0);
        e.w(")");
      } else {
        e.value(result, 0);
      }
      for (let j = 0; j < lists.length; j++) {
        if (!lists[j]) continue;
        e.w("; arg " + (j + 1) + " after call: ");
        e.value(args[j], 0);
      }
    } catch (x) {
      e.w("<encoding failed>");
      e.cut = true;
    }
  }, ZSPFZ_timeout - (Date.now() - started));
  if (state !== 0) return { state: state };
  return { state: 0, enc: e.b, cut: e.cut, threw: threw };
}

// ZSPFZ_quote encodes s as a JSON string exactly as the host does: quote,
// backslash and control characters escaped, everything else copied.
function ZSPFZ_quote(s) {
  let out = "\"";
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c === 34) out += "\\\"";
    else if (c === 92) out += "\\\\";
    else if (c < 32) out += "\\u00" + (c + 0x100).toString(16).slice(1);
    else out += s.charAt(i);
  }
  return out + "\"";
}

// ZSPFZ_run evaluates every input of function f twice and appends one record
// per input to the observation file, between the head and begin records and
// an end (or stop) and done record. A timeout or a harness failure ends the
// function with a stop record.
async function ZSPFZ_run(f, name, target, cases, lists) {
  const head = "{\"f\":" + f + ",\"k\":";
  ZSPFZ_emit(head + "\"head\",\"s\":" + ZSPFZ_quote(ZSPFZ_suffix) + ",\"w\":" + ZSPFZ_quote(name) + "}");
  const n = cases.length;
  ZSPFZ_emit(head + "\"begin\",\"n\":" + n + "}");
  let observed = 0;
  if (ZSPFZ_poisoned) {
    ZSPFZ_emit(head + "\"stop\",\"i\":0,\"x\":\"poisoned\"}");
  } else {
    let stopped = false;
    for (let k = 0; k < n; k++) {
      const first = await ZSPFZ_call(target, cases[k], lists);
      let second = first;
      if (first.state === 0) second = await ZSPFZ_call(target, cases[k], lists);
      const state = first.state === 0 ? second.state : first.state;
      if (state !== 0) {
        let reason = "abort";
        if (state === 1) {
          reason = "timeout";
          ZSPFZ_poisoned = true;
        }
        ZSPFZ_emit(head + "\"stop\",\"i\":" + k + ",\"x\":\"" + reason + "\"}");
        stopped = true;
        break;
      }
      const hash = ZSPFZ_crypto.createHash("sha256").update(first.enc, "latin1").digest("hex");
      const unstable = first.enc !== second.enc || first.cut !== second.cut;
      const shown = first.enc.length > ZSPFZ_display ? first.enc.slice(0, ZSPFZ_display) : first.enc;
      ZSPFZ_emit(head + "\"obs\",\"i\":" + k + ",\"h\":\"" + hash + "\",\"l\":" + first.enc.length + ",\"o\":" + ZSPFZ_quote(shown) +
        ",\"p\":" + first.threw + ",\"d\":" + unstable + ",\"t\":" + first.cut + "}");
      observed++;
    }
    if (!stopped) ZSPFZ_emit(head + "\"end\",\"n\":" + n + "}");
  }
  ZSPFZ_emit(head + "\"done\",\"w\":" + ZSPFZ_quote(name) + ",\"n\":" + observed + "}");
}
`
