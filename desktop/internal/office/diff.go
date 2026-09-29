package office

import "strings"

// pair is one difference between two sequences: a removed item (j < 0), an
// added item (i < 0) or an item replaced by another (both set).
type pair struct {
	i, j int
}

// maxEditDistance bounds the diff: past it, the differing middle of the two
// sequences is reported as replaced wholesale instead of aligned item by item.
const maxEditDistance = 4000

// diffSeq aligns two sequences with Myers' algorithm and returns their
// differences. Consecutive removals and additions are paired in order as
// modifications, which is how a reader perceives an edited paragraph.
func diffSeq(a, b []string) []pair {
	// Documents change locally: trimming the common ends keeps the search small.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]

	ops := myers(ma, mb)
	var out []pair
	var dels, ins []int
	flush := func() {
		n := len(dels)
		if len(ins) < n {
			n = len(ins)
		}
		for k := 0; k < n; k++ {
			out = append(out, pair{i: pre + dels[k], j: pre + ins[k]})
		}
		for _, d := range dels[n:] {
			out = append(out, pair{i: pre + d, j: -1})
		}
		for _, x := range ins[n:] {
			out = append(out, pair{i: -1, j: pre + x})
		}
		dels, ins = dels[:0], ins[:0]
	}
	for _, o := range ops {
		switch o.kind {
		case opEqual:
			flush()
		case opDelete:
			dels = append(dels, o.i)
		case opInsert:
			ins = append(ins, o.j)
		}
	}
	flush()
	return out
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type op struct {
	kind opKind
	i, j int
}

func myers(a, b []string) []op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	whole := func() []op {
		ops := make([]op, 0, n+m)
		for i := 0; i < n; i++ {
			ops = append(ops, op{kind: opDelete, i: i})
		}
		for j := 0; j < m; j++ {
			ops = append(ops, op{kind: opInsert, j: j})
		}
		return ops
	}
	limit := n + m
	if limit > maxEditDistance {
		limit = maxEditDistance
	}
	// trace[d] holds v[k] for k in [-(d+1), d+1] as it was before round d.
	var trace [][]int
	v := map[int]int{1: 0}
	for d := 0; d <= limit; d++ {
		snap := make([]int, 2*d+3)
		for k := -(d + 1); k <= d+1; k++ {
			snap[k+d+1] = v[k]
		}
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1] < v[k+1]) {
				x = v[k+1]
			} else {
				x = v[k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m)
			}
		}
	}
	return whole()
}

func backtrack(trace [][]int, n, m int) []op {
	var rev []op
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		get := func(k int) int { return trace[d][k+d+1] }
		k := x - y
		var prevK int
		if k == -d || (k != d && get(k-1) < get(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := get(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, op{kind: opEqual, i: x - 1, j: y - 1})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				rev = append(rev, op{kind: opInsert, j: y - 1})
			} else {
				rev = append(rev, op{kind: opDelete, i: x - 1})
			}
		}
		x, y = prevX, prevY
	}
	ops := make([]op, len(rev))
	for i, o := range rev {
		ops[len(rev)-1-i] = o
	}
	return ops
}

// normalize makes text comparison insensitive to whitespace-only edits.
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
