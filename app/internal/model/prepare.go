package model

// Dependency-preparation statuses.
const (
	PrepareBuilt        = "built"
	PrepareReused       = "reused"
	PrepareFailed       = "failed"
	PrepareNotPermitted = "not_permitted"
	PrepareNotRun       = "not_run"
)

// PrepareNote is the fixed note of the prepare section (F8).
const PrepareNote = "Dependency preparation is part of the trusted environment, like the configured sandbox image: it is not a check, not evidence about the change, and it supports no hypothesis. When the prepare command ran, it ran before any candidate code, on the declared inputs exported from the base commit only; candidate dependency changes are never installed. A network permission for the preparation container never extends to checks, which keep the sandbox network setting. An equal key means equal inputs, not equal image content, and image labels are unsigned local metadata. This section makes no claim that the prepared dependencies are safe, unmodified, license-clean or free of vulnerabilities."

// PreparedInput is one file exported from the source commit into the prepare
// container: its repository path and the SHA-256 and size of the exact blob.
type PreparedInput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Prepare is the dependency-preparation section. It is present exactly when
// the policy has prepare in review mode. It describes the environment every
// sandbox run of the review used; it is not a check and not evidence.
// Network is the network setting of the container that built the recorded
// image (for reused, the build recorded in the key), or the effective setting
// this run would have used when no image was produced.
type Prepare struct {
	Status       string          `json:"status"`
	Reason       string          `json:"reason,omitempty"`
	SourceCommit string          `json:"source_commit"` // change.BaseCommit
	Command      []string        `json:"command"`
	User         string          `json:"user"`    // sandbox | root
	Network      bool            `json:"network"` // effective
	Key          string          `json:"key,omitempty"`
	BaseImage    string          `json:"base_image"`
	BaseImageID  string          `json:"base_image_id,omitempty"`
	ImageID      string          `json:"image_id,omitempty"`
	AddedBytes   int64           `json:"added_bytes,omitempty"`
	Inputs       []PreparedInput `json:"inputs"`
	LogSHA256    string          `json:"log_sha256,omitempty"`
	DurationMS   int64           `json:"duration_ms"`
	Note         string          `json:"note"`
}
