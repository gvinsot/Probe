package symbols

import (
	"context"
	"sort"
	"strings"
	"time"
)

// This file exposes the whole index to the repository graph (package graph),
// which persists it. The queries of query.go answer one symbol at a time.

// Call is one reference from a declaration to another: a call, or a use of
// the callee as a value (Call false). Several references between the same
// declarations are reported once, with the first site and their count.
type Call struct {
	Caller, Callee string // declaration keys
	Path           string // file of the first site
	Line           int
	Call           bool // at least one site is a call
	Count          int
}

// Implementation records that the concrete method Method may be the target of
// a call to the interface method Interface (Go only).
type Implementation struct {
	Method, Interface string // declaration keys
}

// Decls returns a copy of the indexed declarations, sorted by key.
func (x *Index) Decls() []Decl {
	if x == nil {
		return nil
	}
	out := make([]Decl, len(x.decls))
	copy(out, x.decls)
	return out
}

// Calls returns the references between declarations, one per (caller,
// callee) pair, in caller order.
func (x *Index) Calls() []Call {
	if x == nil {
		return nil
	}
	var out []Call
	for caller := range x.decls {
		seen := map[int32]int{}
		for _, ei := range x.callees(int32(caller)) {
			e := x.edges[ei]
			if i, ok := seen[e.callee]; ok {
				out[i].Count++
				out[i].Call = out[i].Call || e.call
				continue
			}
			seen[e.callee] = len(out)
			out = append(out, Call{
				Caller: x.decls[caller].Key, Callee: x.decls[e.callee].Key,
				Path: x.files[e.file], Line: int(e.line), Call: e.call, Count: 1,
			})
		}
	}
	return out
}

// maxImplementationUnits bounds the interface checks of Implementations.
const maxImplementationUnits = 20_000_000

// Implementations returns the interface methods each concrete Go method may
// implement, within a work budget and a deadline. complete is false when the
// budget or the deadline stopped the search, or when some checks were skipped
// (see implementers); the result then lists what was found.
func (x *Index) Implementations(ctx context.Context, timeout time.Duration) (out []Implementation, complete bool) {
	if x == nil {
		return nil, true
	}
	bud := newBudget(ctx, time.Now().Add(timeout), maxImplementationUnits)
	complete = true
	ids := make([]int32, 0, len(x.recv))
	for id := range x.recv {
		ids = append(ids, id)
	}
	// In declaration order, so that a search the budget stops finds the same
	// part every time.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if !bud.ok() {
			complete = false
			break
		}
		for _, iface := range x.implementers(id, bud) {
			out = append(out, Implementation{Method: x.decls[id].Key, Interface: x.decls[iface].Key})
		}
		if bud.implGap() {
			complete = false
			bud.resetImpl()
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Interface < out[j].Interface
	})
	return out, complete && !bud.stopped
}

// LanguageOf returns the language the index gives a file: "go", one of the
// lexical languages, or "" for a file it does not index. Declarations of Go
// files have Language "".
func LanguageOf(path string) string {
	if strings.HasSuffix(path, ".go") {
		return "go"
	}
	return lexLanguage(path)
}
