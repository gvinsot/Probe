package symbols

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Tool response bounds.
const (
	maxQueryResults    = 100
	maxQueryVisits     = 5000
	maxCandidates      = 20
	maxNameMatches     = 20
	maxCallees         = 50
	maxDispatch        = 20
	maxTestsInResponse = 20
)

// Work bounds of one query: every reference visited, interface check and
// redacted snippet file is charged to a budget of maxQueryWork units, and the
// query stops after queryTimeout or when its context ends, whichever comes
// first; both are checked before every interface check. A query that meets a
// bound answers with what it found and "truncated": true.
const maxQueryWork = 2000000

// queryTimeout is a variable only so that tests can lower it.
var queryTimeout = 10 * time.Second

// truncatedNote explains "truncated": true in a tool response.
const truncatedNote = "Not every result is listed: the response keeps at most the listed number of entries, and the search stops at its visit, work and time bounds. It checks at most 200 interface methods of one name, does not check receiver types too large for its bound against interfaces, and does not check interfaces a generic type may implement only once instantiated. More may exist."

// Query answers the reviewer tools find_references, inspect_symbol and
// find_callers from the index, satisfying harness.SymbolIndex. found is false
// when the symbol does not name an indexed Go function or method (the harness
// then falls back to a lexical search); a nil index finds nothing. depth
// applies to find_callers only (0 means 1). Every response carries the method
// and its limitations; it is an observation, never evidence. The work of one
// query is bounded (maxQueryWork, queryTimeout and ctx); it returns ctx's
// error when ctx ends first.
func (x *Index) Query(ctx context.Context, tool, symbol string, depth int) (any, bool, error) {
	if x == nil {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	ids := x.resolve(symbol)
	if len(ids) == 0 {
		return nil, false, nil
	}
	if len(ids) > 1 {
		candidates := []map[string]any{}
		for i, id := range ids {
			if i == maxCandidates {
				break
			}
			d := x.decls[id]
			candidates = append(candidates, map[string]any{"symbol": d.Key, "kind": d.Kind, "path": d.Path, "line": d.Line})
		}
		return map[string]any{
			"method": Method, "limitations": Limitations, "query": symbol, "ambiguous": true,
			"candidates": candidates, "candidates_total": len(ids),
			"note": "Several indexed declarations match; repeat the call with one of the listed symbols.",
		}, true, nil
	}
	id := ids[0]
	bud := newBudget(ctx, time.Now().Add(queryTimeout), maxQueryWork)
	var out map[string]any
	switch tool {
	case "find_callers":
		if depth <= 0 {
			depth = 1
		}
		if depth > MaxDepth {
			depth = MaxDepth
		}
		out = x.callers(id, depth, bud)
	case "inspect_symbol":
		out = x.inspect(id, bud)
	default:
		out = x.referencesOf(id, bud)
	}
	if bud.cancelled {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, context.Canceled
	}
	if out["truncated"] == true {
		out["truncated_note"] = truncatedNote
	}
	return out, true, nil
}

// validSymbol accepts identifiers, import paths and the (*T).M form.
func validSymbol(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_./*()-~", r)) {
			return false
		}
	}
	return true
}

// resolve maps a query to declarations: an exact key; else keys ending in
// "/"+query; else a package-qualified name (pkg.F, pkg.T.M); else a bare name
// (F, T.M, or the method name M). The first class with a match wins.
func (x *Index) resolve(symbol string) []int32 {
	if !validSymbol(symbol) {
		return nil
	}
	s := strings.ReplaceAll(strings.ReplaceAll(symbol, "(*", ""), ")", "")
	s = strings.ReplaceAll(strings.TrimPrefix(s, "*"), "(", "")
	if id, ok := x.byKey[s]; ok {
		return []int32{id}
	}
	var out []int32
	for i, d := range x.decls {
		if strings.HasSuffix(d.Key, "/"+s) {
			out = append(out, int32(i))
		}
	}
	if len(out) > 0 {
		return out
	}
	seen := map[int32]bool{}
	for _, id := range x.byName[s] {
		d := x.decls[id]
		if d.PkgName+"."+d.Name == s && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, id := range x.byName[s] {
		d := x.decls[id]
		if d.Kind != KindPackageInit && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (x *Index) declaration(id int32) map[string]any {
	d := x.decls[id]
	out := map[string]any{"symbol": d.Key, "kind": d.Kind, "path": d.Path, "line": d.Line, "end_line": d.EndLine, "signature": d.Signature, "test": d.Test}
	if c, ok := x.changed[id]; ok {
		out["changed"] = c
	}
	return out
}

func resolutionOf(iface bool) string {
	if iface {
		return "interface"
	}
	return "static"
}

// reference is one site in a tool response.
func (x *Index) reference(e edge, iface bool, bud *budget) map[string]any {
	file := x.files[e.file]
	return map[string]any{
		"path": file, "line": int(e.line), "caller": x.decls[e.caller].Key,
		"resolution": resolutionOf(iface), "call": e.call, "test": strings.HasSuffix(file, "_test.go"),
		"content": x.snippet(e.file, e.line, bud),
	}
}

// referencesOf answers find_references: every recorded site, static then
// through interfaces, plus unresolved method calls of the same name. Totals
// are counted from the index's tables without visiting each site.
func (x *Index) referencesOf(id int32, bud *budget) map[string]any {
	refs := []map[string]any{}
	total := 0
	list := func(sites []edge, iface bool) {
		total += len(sites)
		for _, e := range sites {
			if len(refs) == maxQueryResults {
				return
			}
			refs = append(refs, x.reference(e, iface, bud))
		}
	}
	list(x.references(id), false)
	for _, im := range x.implementers(id, bud) {
		list(x.references(im), true)
	}
	out := map[string]any{
		"method": Method, "limitations": Limitations, "declaration": x.declaration(id),
		"references": refs, "references_total": total, "truncated": total > len(refs) || bud.stopped || bud.implGap(),
	}
	if matches, n := x.nameMatches(id, bud); n > 0 {
		out["name_matches"] = matches
		out["name_matches_total"] = n
		out["name_matches_note"] = "Calls of a method with this name on a value whose type is not resolved (for example from a package outside the repository). They may or may not call this method."
	}
	return out
}

// nameMatches lists unresolved method calls with the method's name, and
// counts them.
func (x *Index) nameMatches(id int32, bud *budget) ([]map[string]any, int) {
	d := x.decls[id]
	if d.Kind != KindMethod && d.Kind != KindInterfaceMethod {
		return nil, 0
	}
	name := lastName(d.Name)
	lo := sort.Search(len(x.nameSites), func(i int) bool { return x.nameSites[i].name >= name })
	hi := sort.Search(len(x.nameSites), func(i int) bool { return x.nameSites[i].name > name })
	var out []map[string]any
	for i := lo; i < hi && len(out) < maxNameMatches; i++ {
		s := x.nameSites[i]
		out = append(out, map[string]any{"path": x.files[s.file], "line": int(s.line), "caller": x.decls[s.caller].Key, "resolution": "name_match", "content": x.snippet(s.file, s.line, bud)})
	}
	return out, hi - lo
}

// callers answers find_callers: the reference sites of id, then of the
// declarations holding them, up to depth, breadth first. Sites are visited
// one at a time against maxQueryVisits and bud, and the references of an
// interface method are followed once per query.
func (x *Index) callers(id int32, depth int, bud *budget) map[string]any {
	type step struct {
		id    int32
		iface bool
		via   []string
	}
	results := []map[string]any{}
	total, visits := 0, 0
	truncated := !bud.ok()
	seen := map[int32]bool{id: true}
	expanded := map[int32]bool{}
	frontier := []step{{id: id, via: []string{x.decls[id].Key}}}
	for level := 1; level <= depth && len(frontier) > 0 && !truncated; level++ {
		var next []step
		// visit records one reference site reached from s; it returns false
		// when the search must stop.
		visit := func(s step, e edge, iface bool) bool {
			visits++
			if visits > maxQueryVisits || !bud.spend(1) {
				truncated = true
				return false
			}
			iface = s.iface || iface
			caller := x.decls[e.caller]
			via := append([]string{caller.Key}, s.via...)
			total++
			if len(results) < maxQueryResults {
				item := x.reference(e, iface, bud)
				item["depth"] = level
				item["via"] = via
				results = append(results, item)
			}
			if !seen[e.caller] && !caller.Test {
				seen[e.caller] = true
				next = append(next, step{id: e.caller, iface: iface, via: via})
			}
			return true
		}
	steps:
		for _, s := range frontier {
			for _, e := range x.references(s.id) {
				if !visit(s, e, false) {
					break steps
				}
			}
			for _, im := range x.implementers(s.id, bud) {
				if expanded[im] {
					continue
				}
				expanded[im] = true
				for _, e := range x.references(im) {
					if !visit(s, e, true) {
						break steps
					}
				}
			}
			if bud.stopped {
				truncated = true
				break
			}
		}
		frontier = next
	}
	return map[string]any{
		"method": Method, "limitations": Limitations, "declaration": x.declaration(id), "depth": depth,
		"callers": results, "callers_total": total, "truncated": truncated || bud.implGap() || total > len(results),
	}
}

// inspect answers inspect_symbol: the declaration, how often it is referenced,
// what it references, interface relations and reaching tests.
func (x *Index) inspect(id int32, bud *budget) map[string]any {
	d := x.decls[id]
	refs := len(x.references(id))
	for _, im := range x.implementers(id, bud) {
		refs += len(x.references(im))
	}
	callees := []map[string]any{}
	seen := map[int32]bool{}
	calleeTotal := 0
	for _, ei := range x.callees(id) {
		if !bud.spend(1) {
			break
		}
		e := x.edges[ei]
		if seen[e.callee] {
			continue
		}
		seen[e.callee] = true
		calleeTotal++
		if len(callees) < maxCallees {
			callees = append(callees, map[string]any{"symbol": x.decls[e.callee].Key, "kind": x.decls[e.callee].Kind, "path": x.files[e.file], "line": int(e.line)})
		}
	}
	// callees are the indexed declarations this one references, not callers.
	out := map[string]any{
		"method": Method, "limitations": Limitations, "declaration": x.declaration(id),
		"reference_sites": refs, "callees": callees, "callees_total": calleeTotal,
	}
	switch d.Kind {
	case KindMethod:
		var dispatch []map[string]any
		for i, im := range x.implementers(id, bud) {
			if i == maxDispatch {
				break
			}
			dispatch = append(dispatch, map[string]any{"interface_method": x.decls[im].Key, "path": x.decls[im].Path, "line": x.decls[im].Line})
		}
		if len(dispatch) > 0 {
			out["interface_dispatch"] = dispatch
			out["interface_dispatch_note"] = "Calls of these interface methods may dispatch to this method; dispatch is possible, not established."
		}
	case KindInterfaceMethod:
		var impls []map[string]any
		name := lastName(d.Name)
		for _, cand := range x.byName[name] {
			if !bud.spend(1) {
				break
			}
			cd := x.decls[cand]
			if cd.Kind != KindMethod || !strings.HasSuffix(cd.Name, "."+name) {
				continue
			}
			for _, im := range x.implementers(cand, bud) {
				if im == id && len(impls) < maxDispatch {
					impls = append(impls, map[string]any{"symbol": cd.Key, "path": cd.Path, "line": cd.Line})
				}
			}
		}
		if len(impls) > 0 {
			out["implementations"] = impls
			out["implementations_note"] = "Indexed methods whose receiver type implements this interface; a call of the interface method may dispatch to any of them or to types outside the index."
		}
	}
	tests, total, _, capped := x.reachingTests(id, modules{}, nil, bud)
	listed := []map[string]any{}
	for i, t := range tests {
		if i == maxTestsInResponse {
			break
		}
		listed = append(listed, map[string]any{"name": t.Name, "path": t.Path, "line": t.Line, "depth": t.Depth, "resolution": t.Resolution, "via": t.Via})
	}
	out["tests_reaching"] = listed
	out["tests_reaching_total"] = total
	out["tests_reaching_note"] = "Existing TestX functions that reach this declaration within 3 references in the static index. Reaching a function is not evidence that a test asserts its behavior."
	out["truncated"] = bud.stopped || bud.implGap() || capped || calleeTotal > len(callees)
	return out
}
