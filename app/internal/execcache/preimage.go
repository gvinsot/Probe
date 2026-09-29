// Package execcache is the opt-in, on-disk store of baseline sandbox
// executions (contract §1.11). It keeps one integrity-checked file per key
// under DIR/v1, validates the directory before anything executes, and never
// stores raw output: an entry holds the recorded, redacted log and a payload
// that the harness already required to be complete and a redaction fixed
// point.
//
// The store is a trusted input. Its integrity checks (content hash, key
// recomputed from the stored preimage, strict decoding, bounds, age) detect
// corruption, torn writes and misplaced files; they do not authenticate an
// entry. Anyone who can write the directory can forge entries, which is why
// the directory must be outside the repository and the output directory and,
// on Unix, owner-only.
//
// The package sits in layer 1: it imports the standard library and model
// only. The harness converts its Entry to and from harness.CacheEntry.
package execcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// PreimageSchema names the key preimage format. Changing any field of
// Preimage, or its meaning, requires a new schema name.
const PreimageSchema = "probe-execcache/v1"

// maxAdded bounds the files a caller may add to the baseline tree of one
// cacheable run (staged tests, harness files). A run with more is uncacheable.
const maxAdded = 256

// Tree identifies the baseline tree a run saw. PristineSHA256 is the digest of
// the manifest of the base snapshot as copied (every entry unchanged); Added
// lists what callers added under it, one [path, type, size, sha256] tuple per
// entry in lexical path order, where type is "f" (regular file), "x"
// (executable file) or "d" (directory; size and sha256 empty).
type Tree struct {
	PristineSHA256 string     `json:"pristine_sha256"`
	Added          [][]string `json:"added"`
}

// Preimage is the canonical input of a cache key. It holds hashes and fixed
// identifiers only, never raw argv or output. The key is the hex SHA-256 of
// its JSON encoding (Encode).
type Preimage struct {
	Schema      string `json:"schema"`
	ToolVersion string `json:"tool_version"` // version and SHA-256 of the executable
	Kind        string `json:"kind"`
	BaseCommit  string `json:"base_commit"`
	Tree        Tree   `json:"tree"`
	// PolicySHA256 digests the execution settings the trusted policy gave the
	// sandbox (commands, image reference, network, limits).
	PolicySHA256 string `json:"policy_sha256"`
	ImageID      string `json:"image_id"`
	// DockerServer is the Docker server version, OS type and architecture.
	DockerServer string `json:"docker_server"`
	// DockerArgsSHA256 digests the complete docker argv built with a fixed
	// container name and mount path: argv, wrapper or capture script, network,
	// limits, user, mounts and environment.
	DockerArgsSHA256 string `json:"docker_args_sha256"`
	ArgvSHA256       string `json:"argv_sha256"`
	Capture          string `json:"capture"`    // in-container payload path; "" when none
	TimeoutMS        int64  `json:"timeout_ms"` // configured per-run timeout after tightening, before budget clamping
	MaxOutputBytes   int    `json:"max_output_bytes"`
}

var (
	hexDigest   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	addedTypes  = map[string]bool{"f": true, "x": true, "d": true}
	errPreimage = errors.New("invalid key preimage")
)

// ValidKey reports whether key is a hex SHA-256 digest.
func ValidKey(key string) bool { return hexDigest.MatchString(key) }

// SHA256Hex is the lowercase hex SHA-256 of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Encode returns the canonical JSON of p and its key. It refuses a preimage
// that validate rejects, so that every key it returns can be recomputed and
// re-validated from what the store keeps.
func (p Preimage) Encode() (json.RawMessage, string, error) {
	if err := p.validate(); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	return raw, SHA256Hex(raw), nil
}

// validate checks the fixed shape of a preimage: the schema name, digests in
// their digest fields, and a bounded, well-formed list of added entries.
func (p Preimage) validate() error {
	if p.Schema != PreimageSchema {
		return fmt.Errorf("%w: schema %q", errPreimage, p.Schema)
	}
	if p.Kind == "" || len(p.Kind) > 64 {
		return fmt.Errorf("%w: kind", errPreimage)
	}
	for name, digest := range map[string]string{"tree.pristine_sha256": p.Tree.PristineSHA256, "policy_sha256": p.PolicySHA256, "docker_args_sha256": p.DockerArgsSHA256, "argv_sha256": p.ArgvSHA256} {
		if !hexDigest.MatchString(digest) {
			return fmt.Errorf("%w: %s", errPreimage, name)
		}
	}
	if p.TimeoutMS <= 0 || p.MaxOutputBytes <= 0 {
		return fmt.Errorf("%w: limits", errPreimage)
	}
	if len(p.Tree.Added) > maxAdded {
		return fmt.Errorf("%w: more than %d added entries", errPreimage, maxAdded)
	}
	for _, a := range p.Tree.Added {
		if len(a) != 4 || a[0] == "" || !addedTypes[a[1]] {
			return fmt.Errorf("%w: added entry", errPreimage)
		}
		if a[1] == "d" && (a[2] != "" || a[3] != "") || a[1] != "d" && !hexDigest.MatchString(a[3]) {
			return fmt.Errorf("%w: added entry", errPreimage)
		}
	}
	return nil
}

// DecodePreimage strictly decodes a stored preimage (no unknown field, one
// JSON value) and validates it.
func DecodePreimage(raw []byte) (Preimage, error) {
	var p Preimage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return Preimage{}, fmt.Errorf("%w: %v", errPreimage, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return Preimage{}, fmt.Errorf("%w: trailing data", errPreimage)
	}
	return p, p.validate()
}
