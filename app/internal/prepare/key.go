package prepare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/gitrepo"
)

// Schema names the key preimage, the labels and the container profile. Bump it
// whenever any of them changes meaning.
const Schema = "probe-prepare/v1"

// TagRepository is the local repository of derived images.
const TagRepository = "probe-prepared"

// Label keys written on every derived image. They are unsigned local metadata:
// Probe checks them before reuse, but they are not an attestation.
const (
	LabelPrefix       = "org.probe.prepare."
	LabelSchema       = LabelPrefix + "schema"
	LabelKey          = LabelPrefix + "key"
	LabelSourceCommit = LabelPrefix + "source_commit"
	LabelBaseImageID  = LabelPrefix + "base_image_id"
	LabelToolVersion  = LabelPrefix + "tool_version"
	LabelOutputs      = LabelPrefix + "outputs"
	LabelLogSHA256    = LabelPrefix + "log_sha256"
)

// Values of LabelOutputs: where the prepare command's changes landed.
const (
	OutputsPersistent = "persistent"
	OutputsShadowed   = "shadowed"
)

// Placeholders the docker-args template is rendered with: the container name
// and the host inputs directory change on every run, everything else is part
// of the key.
const (
	templateName   = containerPrefix + "key"
	templateInputs = "/probe-key-inputs"
)

// keyMaterial is the canonical preimage of a prepare key (contract §2 F8
// delta 4). encoding/json writes struct fields in declaration order, and every
// list is sorted, so equal inputs give byte-equal preimages.
type keyMaterial struct {
	Schema           string      `json:"schema"`
	ToolVersion      string      `json:"tool_version"`
	BaseImageID      string      `json:"base_image_id"`
	Command          []string    `json:"command"`
	User             string      `json:"user"`
	Network          bool        `json:"network"`
	Env              [][2]string `json:"env"`
	Inputs           [][2]string `json:"inputs"`
	DockerArgsSHA256 string      `json:"docker_args_sha256"`
	MaxAddedMB       int         `json:"max_added_mb"`
}

// Key returns the hex SHA-256 of the canonical JSON of: the schema and tool
// version; the base image ID; the argv, the user, the network bit of the
// container that builds the image (policy prepare.network: a build without
// that permission never runs) and the sorted env; the sorted (path, sha256) of
// the exported inputs; the SHA-256 of the docker-args template; and
// max_added_mb. Input patterns, the source commit and the timeout are not part
// of it: they do not change what the container sees. Reuse additionally
// requires the source-commit label to match.
func Key(toolVersion, baseImageID string, spec config.Prepare, inputs []gitrepo.ExportedFile, memoryMB, cpus int) string {
	files := make([][2]string, 0, len(inputs))
	for _, f := range inputs {
		files = append(files, [2]string{f.Path, f.SHA256})
	}
	sort.Slice(files, func(i, j int) bool { return files[i][0] < files[j][0] })
	command := append([]string{}, spec.Command...)
	material := keyMaterial{
		Schema: Schema, ToolVersion: toolVersion, BaseImageID: baseImageID, Command: command,
		User: spec.EffectiveUser(), Network: spec.Network, Env: sortedEnv(spec.Env), Inputs: files,
		DockerArgsSHA256: templateSHA256(baseImageID, spec, memoryMB, cpus), MaxAddedMB: spec.EffectiveMaxAddedMB(),
	}
	b, err := json.Marshal(material)
	if err != nil {
		panic(err) // only strings, bools and ints
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// templateSHA256 hashes the container arguments a build would use, rendered
// with fixed placeholders for the per-run name and inputs directory: it covers
// the argv, the wrapper script, the network, the limits, the user and
// capabilities, the mount, the working directory and the env.
func templateSHA256(baseImageID string, spec config.Prepare, memoryMB, cpus int) string {
	args := createArgs(templateName, templateInputs, baseImageID, spec, spec.Network, memoryMB, cpus)
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Tag returns the local tag of the image derived for key from the inputs of
// commit. Both parts are prefixes; the labels carry the full values, and
// reuse compares those.
func Tag(key, commit string) string {
	return TagRepository + ":" + prefix(key, 32) + "-" + prefix(commit, 32)
}

func prefix(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
