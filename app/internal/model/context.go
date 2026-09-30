package model

// Context repository statuses.
const (
	ContextAvailable   = "available"
	ContextUnavailable = "unavailable"
)

// Context repository sources: a local checkout, or a shallow fetch of the
// policy's url (--fetch-context).
const (
	ContextLocal   = "local"
	ContextFetched = "fetched"
)

// ContextRepo records one repository the reviewer could read as
// cross-repository context: which commit, from where, and why it was
// unavailable when it was. Reading context is inspection, never evidence.
type ContextRepo struct {
	Name     string   `json:"name"`
	Role     string   `json:"role,omitempty"`
	Clusters []string `json:"clusters"`
	Ref      string   `json:"ref"`
	Commit   string   `json:"commit,omitempty"`
	Source   string   `json:"source,omitempty"`
	Files    int      `json:"files"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
}
