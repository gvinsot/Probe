package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
	"github.com/gvinsot/SwiftProof/app/internal/mutation"
)

const cartCandidate = "package cart\n\nfunc Total(prices []int) int {\n\tsum := 0\n\tfor _, p := range prices {\n\t\tsum += p\n\t}\n\treturn sum\n}\n\nfunc Count(prices []int) int { return len(prices) }\n\nfunc IsBig(n int) bool { return n > 100 }\n"

// boundaryMutant replaces cart.go with a version whose init panics when the
// sandbox boundary does not hold, so the mutant run passes only inside the
// unchanged boundary: non-root, read-only source and root filesystem, no
// network interface but loopback, no host environment.
const boundaryMutant = `package cart

import (
	"net"
	"os"
	"time"
)

func init() {
	if os.Geteuid() == 0 {
		panic("sandbox must be non-root")
	}
	if os.Getenv("SWIFTPROOF_HOST_SECRET") != "" {
		panic("host environment reached the sandbox")
	}
	if err := os.WriteFile("/source/host-write", []byte("unsafe"), 0644); err == nil {
		panic("source mount is writable")
	}
	if err := os.WriteFile("/etc/swiftproof-write", []byte("unsafe"), 0644); err == nil {
		panic("root filesystem is writable")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		panic(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			panic("unexpected network interface " + iface.Name)
		}
	}
	if c, err := net.DialTimeout("tcp", "192.0.2.1:443", 200*time.Millisecond); err == nil {
		c.Close()
		panic("sandbox connected to an external network")
	}
}

func Total(prices []int) int {
	sum := 0
	for _, p := range prices {
		sum += p
	}
	return sum
}

func Count(prices []int) int { return len(prices) }

func IsBig(n int) bool { return n > 100 }
`

// A mutant runs inside the unchanged sandbox boundary, in the private copy
// only: the harness snapshots and the restored workspace are byte-identical
// before and after, and no container survives.
func TestDockerMutantBoundaryUnchanged(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	t.Setenv("SWIFTPROOF_HOST_SECRET", "must-not-reach-container")
	h, names := dockerRunFixture(t, image, cartCandidate)
	candidateBefore, baseBefore := treeDigest(t, h.candidate), treeDigest(t, h.base)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	workspaceBefore := treeDigest(t, w.dir)
	command := []string{"go", "test", "-json", "-count=1", "./cart"}
	c, _, err := w.RunMutant(context.Background(), "mutant-1", "cart/cart.go", []byte(cartCandidate), []byte(boundaryMutant), command, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "PASS" || c.Kind != model.CheckMutant || !strings.HasPrefix(c.ID, "mutation-check-") {
		t.Fatalf("boundary mutant run %s (exit %d):\n%s", c.Status, c.ExitCode, c.Output)
	}
	if !reflect.DeepEqual(treeDigest(t, h.candidate), candidateBefore) || !reflect.DeepEqual(treeDigest(t, h.base), baseBefore) || !reflect.DeepEqual(treeDigest(t, w.dir), workspaceBefore) {
		t.Fatal("a host tree changed")
	}
	for _, dir := range []string{h.candidate, h.base, w.dir} {
		if _, err := os.Stat(filepath.Join(dir, "host-write")); !os.IsNotExist(err) {
			t.Fatalf("the sandbox wrote into %s", dir)
		}
	}
	assertNoContainers(t, *names)
}

// Real control and mutant runs classify as the mutation package expects:
// a changed sum is KILLED, a boundary change of an untested function
// SURVIVES, and a type error is INVALID.
func TestDockerMutationClassifiesRealRuns(t *testing.T) {
	image := os.Getenv("SWIFTPROOF_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set SWIFTPROOF_TEST_DOCKER_IMAGE to a preloaded Go image")
	}
	h, names := dockerRunFixture(t, image, cartCandidate)
	w, err := h.NewMutationWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	command := []string{"go", "test", "-json", "-count=1", "-failfast", "./cart"}
	control, err := w.RunControl(context.Background(), "./cart", command, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ctl := mutation.NewControl(control); ctl.Reason() != "" || ctl.Package() != "example.test/shop/cart" {
		t.Fatalf("control %q %q:\n%s", ctl.Package(), ctl.Reason(), control.Output)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"killed", "sum += p", "sum -= p", model.MutantKilled},
		{"survived", "n > 100", "n >= 100", model.MutantSurvived},
		{"invalid", "return sum", "return \"sum\"", model.MutantInvalid},
	} {
		mutated := strings.Replace(cartCandidate, tc.from, tc.to, 1)
		c, _, err := w.RunMutant(context.Background(), "mutant-"+tc.name, "cart/cart.go", []byte(cartCandidate), []byte(mutated), command, 0)
		if err != nil {
			t.Fatal(err)
		}
		v := mutation.Classify(control, c)
		if v.Status != tc.want {
			t.Fatalf("%s: %+v\n%s", tc.name, v, c.Output)
		}
		switch tc.want {
		case model.MutantKilled:
			if len(v.FailedTests) != 1 || v.FailedTests[0] != "TestTotal" {
				t.Fatalf("killed by %v", v.FailedTests)
			}
		case model.MutantSurvived:
			if v.TestsRun != 2 {
				t.Fatalf("tests run %d", v.TestsRun)
			}
		}
	}
	if len(h.Checks()) != 0 || len(h.MutationChecks()) != 4 {
		t.Fatalf("ledgers %d / %d", len(h.Checks()), len(h.MutationChecks()))
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "cart", "cart.go")); string(b) != cartCandidate {
		t.Fatal("the workspace was not restored")
	}
	assertNoContainers(t, *names)
}
