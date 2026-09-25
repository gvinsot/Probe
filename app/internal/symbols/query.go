package symbols

import (
	"context"
	"sort"
	"strings"
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

// Query answers the reviewer tools find_references, inspect_symbol and
// find_callers from the index, satisfying harness.SymbolIndex. found is false
// when the symbol does not name an indexed Go function or method (the harness
// then falls back to a lexical search); a nil index finds nothing. depth
// applies to find_callers only (0 means 1). Every response carries the method
// and its limitations; it is an observation, never evidence.
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
	switch tool {
	case "find_callers":
		if depth <= 0 {
			depth = 1
		}
		if depth > MaxDepth {
			depth = MaxDepth
		}
		return x.callers(id, depth), true, nil
	case "inspect_symbol":
		return x.inspect(id), true, nil
	default:
		return x.referencesOf(id), true, nil
	}
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
func (x *Index) reference(e edge, iface bool) map[string]any {
	file := x.files[e.file]
	return map[string]any{
		"path": file, "line": int(e.line), "caller": x.decls[e.caller].Key,
		"resolution": resolutionOf(iface), "call": e.call, "test": strings.HasSuffix(file, "_test.go"),
		"content": x.snippet(e.file, e.line),
	}
}

// referencesOf answers find_references: every recorded site, static then
// through interfaces, plus unresolved method calls of the same name.
func (x *Index) referencesOf(id int32) map[string]any {
	refs := []map[string]any{}
	total := 0
	add := func(e edge, iface bool) {
		total++
		if len(refs) < maxQueryResults {
			refs = append(refs, x.reference(e, iface))
		}
	}
	for _, e := range x.references(id) {
		add(e, false)
	}
	for _, im := range x.implementers(id) {
		for _, e := range x.references(im) {
			add(e, true)
		}
	}
	out := map[string]any{
		"method": Method, "limitations": Limitations, "declaration": x.declaration(id),
		"references": refs, "references_total": total, "truncated": total > len(refs),
	}
	if matches, n := x.nameMatches(id); n > 0 {
		out["name_matches"] = matches
		out["name_matches_total"] = n
		out["name_matches_note"] = "Calls of a method with this name on a value whose type is not resolved (for example from a package outside the repository). They may or may not call this method."
	}
	return out
}

// nameMatches lists unresolved method calls with the method's name.
func (x *Index) nameMatches(id int32) ([]map[string]any, int) {
	d := x.decls[id]
	if d.Kind != KindMethod && d.Kind != KindInterfaceMethod {
		return nil, 0
	}
	name := d.Name[strings.LastIndex(d.Name, ".")+1:]
	lo := sort.Search(len(x.nameSites), func(i int) bool { return x.nameSites[i].name >= name })
	var out []map[string]any
	n := 0
	for i := lo; i < len(x.nameSites) && x.nameSites[i].name == name; i++ {
		s := x.nameSites[i]
		n++
		if len(out) < maxNameMatches {
			out = append(out, map[string]any{"path": x.files[s.file], "line": int(s.line), "caller": x.decls[s.caller].Key, "resolution": "name_match", "content": x.snippet(s.file, s.line)})
		}
	}
	return out, n
}

// callers answers find_callers: the reference sites of id, then of the
// declarations holding them, up to depth, breadth first.
func (x *Index) callers(id int32, depth int) map[string]any {
	type step struct {
		id    int32
		depth int
		iface bool
		via   []string
	}
	results := []map[string]any{}
	total, visits := 0, 0
	truncated := false
	seen := map[int32]bool{id: true}
	frontier := []step{{id: id, via: []string{x.decls[id].Key}}}
	for level := 1; level <= depth && len(frontier) > 0; level++ {
		var next []step
		for _, s := range frontier {
			type ref struct {
				e     edge
				iface bool
			}
			var refs []ref
			for _, e := range x.references(s.id) {
				refs = append(refs, ref{e, false})
			}
			for _, im := range x.implementers(s.id) {
				for _, e := range x.references(im) {
					refs = append(refs, ref{e, true})
				}
			}
			for _, r := range refs {
				visits++
				if visits > maxQueryVisits {
					truncated = true
					break
				}
				iface := s.iface || r.iface
				caller := x.decls[r.e.caller]
				via := append([]string{caller.Key}, s.via...)
				total++
				if len(results) < maxQueryResults {
					item := x.reference(r.e, iface)
					item["depth"] = level
					item["via"] = via
					results = append(results, item)
				}
				if !seen[r.e.caller] && !caller.Test {
					seen[r.e.caller] = true
					next = append(next, step{id: r.e.caller, depth: level, iface: iface, via: via})
				}
			}
		}
		frontier = next
	}
	return map[string]any{
		"method": Method, "limitations": Limitations, "declaration": x.declaration(id), "depth": depth,
		"callers": results, "callers_total": total, "truncated": truncated || total > len(results),
	}
}

// inspect answers inspect_symbol: the declaration, how often it is referenced,
// what it references, interface relations and reaching tests.
func (x *Index) inspect(id int32) map[string]any {
	d := x.decls[id]
	refs := len(x.references(id))
	for _, im := range x.implementers(id) {
		refs += len(x.references(im))
	}
	callees := []map[string]any{}
	seen := map[int32]bool{}
	calleeTotal := 0
	for _, ei := range x.callees(id) {
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
		for i, im := range x.implementers(id) {
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
		name := d.Name[strings.LastIndex(d.Name, ".")+1:]
		for _, cand := range x.byName[name] {
			cd := x.decls[cand]
			if cd.Kind != KindMethod || !strings.HasSuffix(cd.Name, "."+name) {
				continue
			}
			for _, im := range x.implementers(cand) {
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
	tests, total := x.reachingTests(id, modules{}, nil)
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
	return out
}
