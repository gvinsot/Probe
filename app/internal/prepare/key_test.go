package prepare

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
)

type keyCase struct {
	version, base string
	spec          config.Prepare
	inputs        []gitrepo.ExportedFile
	memory, cpus  int
}

func baseKeyCase() keyCase {
	return keyCase{
		version: "v0.4.0", base: goldenImage,
		spec: config.Prepare{Command: []string{"go", "mod", "download"}, Inputs: []string{"go.mod", "go.sum"}, Env: map[string]string{"A": "1", "B": "2"}},
		inputs: []gitrepo.ExportedFile{
			{Path: "go.mod", SHA256: strings.Repeat("1", 64), Size: 10},
			{Path: "go.sum", SHA256: strings.Repeat("2", 64), Size: 20},
		},
		memory: 1024, cpus: 2,
	}
}

func (c keyCase) key() string { return Key(c.version, c.base, c.spec, c.inputs, c.memory, c.cpus) }

func TestKeyDeterministic(t *testing.T) {
	c := baseKeyCase()
	k := c.key()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(k) {
		t.Fatalf("key %q", k)
	}
	for i := 0; i < 20; i++ { // map iteration order varies
		d := baseKeyCase()
		d.inputs[0], d.inputs[1] = d.inputs[1], d.inputs[0]
		d.spec.Env = map[string]string{"B": "2", "A": "1"}
		if d.key() != k {
			t.Fatal("input order or env order changed the key")
		}
	}
}

// Every part of the preimage changes the key.
func TestKeyChangesWithEachPart(t *testing.T) {
	k := baseKeyCase().key()
	for name, mutate := range map[string]func(*keyCase){
		"tool version":  func(c *keyCase) { c.version = "v0.4.1" },
		"base image ID": func(c *keyCase) { c.base = "sha256:" + strings.Repeat("2", 64) },
		"one argv byte": func(c *keyCase) { c.spec.Command = []string{"go", "mod", "downloaD"} },
		"argv split":    func(c *keyCase) { c.spec.Command = []string{"go mod", "download"} },
		"user":          func(c *keyCase) { c.spec.User = "root" },
		"network":       func(c *keyCase) { c.spec.Network = true },
		"env value":     func(c *keyCase) { c.spec.Env = map[string]string{"A": "1", "B": "3"} },
		"env name":      func(c *keyCase) { c.spec.Env = map[string]string{"A": "1", "C": "2"} },
		"no env":        func(c *keyCase) { c.spec.Env = nil },
		"input path":    func(c *keyCase) { c.inputs[1].Path = "sub/go.sum" },
		"input content": func(c *keyCase) { c.inputs[1].SHA256 = strings.Repeat("3", 64) },
		"extra input": func(c *keyCase) {
			c.inputs = append(c.inputs, gitrepo.ExportedFile{Path: "go.work", SHA256: strings.Repeat("4", 64)})
		},
		"memory (docker args)": func(c *keyCase) { c.memory = 2048 },
		"cpus (docker args)":   func(c *keyCase) { c.cpus = 4 },
		"max_added_mb":         func(c *keyCase) { c.spec.MaxAddedMB = 100 },
	} {
		c := baseKeyCase()
		mutate(&c)
		if c.key() == k {
			t.Errorf("%s did not change the key", name)
		}
	}
}

// Patterns, the timeout, sizes and the executable bit do not change what the
// container sees beyond the listed parts (the source commit label covers the
// mode); they are not part of the key.
func TestKeyIgnoresPatternsTimeoutAndSizes(t *testing.T) {
	k := baseKeyCase().key()
	for name, mutate := range map[string]func(*keyCase){
		"patterns":         func(c *keyCase) { c.spec.Inputs = []string{"go.*"} },
		"timeout":          func(c *keyCase) { c.spec.TimeoutSeconds = 900 },
		"size":             func(c *keyCase) { c.inputs[0].Size = 999 },
		"executable bit":   func(c *keyCase) { c.inputs[0].Executable = true },
		"default max size": func(c *keyCase) { c.spec.MaxAddedMB = 4096 },
		"default user":     func(c *keyCase) { c.spec.User = "sandbox" },
	} {
		c := baseKeyCase()
		mutate(&c)
		if c.key() != k {
			t.Errorf("%s changed the key", name)
		}
	}
}

func TestTag(t *testing.T) {
	key := strings.Repeat("ab", 32)
	commit := strings.Repeat("c", 40)
	got := Tag(key, commit)
	if got != "probe-prepared:"+strings.Repeat("ab", 16)+"-"+strings.Repeat("c", 32) {
		t.Fatalf("tag %q", got)
	}
	// Docker tags are at most 128 characters of [A-Za-z0-9_.-].
	if tag := strings.TrimPrefix(Tag(key, strings.Repeat("d", 64)), TagRepository+":"); len(tag) > 128 || !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`).MatchString(tag) {
		t.Fatalf("invalid tag %q", tag)
	}
}
