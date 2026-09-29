package fuzz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/Probe/app/internal/model"
)

// writeTree writes files (slash paths) under root.
func writeTree(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func modified(paths ...string) model.Change {
	c := model.Change{}
	for _, p := range paths {
		c.Files = append(c.Files, model.ChangedFile{Path: p, Status: "M"})
	}
	return c
}

func defaultLimits() Limits {
	return Limits{MaxFunctions: 8, MaxPackages: 4, MaxInputs: 64, CallTimeout: time.Second, MaxRuntime: 240 * time.Second}
}

// calcBase and calcCandidate are the calc fixture of the F2 design: Percent
// loses its zero guard, Join is rewritten equivalently, Discount changes its
// boundary, Stamp appends the current time and Halt exits the process at 7.
const calcBase = `package calc

// Cents is an amount of money.
type Cents int64

// Percent returns part as a percentage of total.
func Percent(part, total int) int {
	if total == 0 {
		return 0
	}
	return part * 100 / total
}

// Join joins the non-empty parts with commas.
func Join(parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += ","
		}
		out += p
	}
	return out
}

// Discount takes ten percent off from 1000 cents.
func Discount(c Cents) Cents {
	if c >= 1000 {
		return c * 9 / 10
	}
	return c
}

// Stamp returns its label.
func Stamp(label string) string {
	return label
}

// Halt returns n.
func Halt(n int) int {
	return n
}
`

const calcCandidate = `package calc

import (
	"os"
	"strings"
	"time"
)

// Cents is an amount of money.
type Cents int64

// Percent returns part as a percentage of total.
func Percent(part, total int) int {
	return part * 100 / total
}

// Join joins the non-empty parts with commas.
func Join(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(p)
	}
	return b.String()
}

// Discount takes ten percent off above 1000 cents.
func Discount(c Cents) Cents {
	if c > 1000 {
		return c * 9 / 10
	}
	return c
}

// Stamp returns its label and the current time.
func Stamp(label string) string {
	return label + time.Now().Format(time.RFC3339Nano)
}

// Halt returns n, and ends the process at 7.
func Halt(n int) int {
	if n == 7 {
		os.Exit(3)
	}
	return n
}
`

// calcTrees writes the calc fixture as a module in base and candidate
// directories and returns them.
func calcTrees(t *testing.T) (base, candidate string) {
	t.Helper()
	root := t.TempDir()
	base, candidate = filepath.Join(root, "base"), filepath.Join(root, "candidate")
	gomod := "module example.test/fuzzdemo\n\ngo 1.21\n"
	writeTree(t, base, map[string]string{"go.mod": gomod, "calc/calc.go": calcBase})
	writeTree(t, candidate, map[string]string{"go.mod": gomod, "calc/calc.go": calcCandidate})
	return base, candidate
}

func targetNamed(t *testing.T, plan Plan, name string) Target {
	t.Helper()
	for _, p := range plan.Packages {
		for _, target := range p.Targets {
			if target.Name == name {
				return target
			}
		}
	}
	t.Fatalf("no planned target %s in %+v", name, plan)
	return Target{}
}

func skipFor(plan Plan, symbolSuffix string) (model.FuzzSkip, bool) {
	for _, s := range plan.Skipped {
		if strings.HasSuffix(s.Symbol, symbolSuffix) {
			return s, true
		}
	}
	return model.FuzzSkip{}, false
}
