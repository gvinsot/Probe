package model

// Knowledge entry kinds: what an entry of the codebase knowledge base is about.
const (
	KnowledgeComponent    = "component"    // how a part of the system works
	KnowledgeRelationship = "relationship" // how components depend on each other
	KnowledgeRisk         = "risk"         // a known risk or fragile area
	KnowledgeArchitecture = "architecture" // architectural context and decisions
	KnowledgeConvention   = "convention"   // a project-specific convention
	KnowledgeReview       = "review"       // accumulated review knowledge
	KnowledgeNote         = "note"         // anything else
)

// KnowledgeKinds lists the entry kinds in documentation order.
var KnowledgeKinds = []string{KnowledgeComponent, KnowledgeRelationship, KnowledgeRisk, KnowledgeArchitecture, KnowledgeConvention, KnowledgeReview, KnowledgeNote}

// KnowledgeEntry is one entry of the codebase knowledge base (PROBE_KNOWLEDGE.md).
// Title identifies it, case-insensitively. Paths are repository globs of the
// code it concerns; an entry without paths concerns the whole repository.
type KnowledgeEntry struct {
	Title   string   `json:"title"`
	Kind    string   `json:"kind"`
	Paths   []string `json:"paths"`
	Updated string   `json:"updated,omitempty"`
	Text    string   `json:"text"`
}

// KnowledgeUpdate is an entry the reviewer model proposes to add or to
// replace (same title), or, with Obsolete, to remove. It is model output,
// never evidence: Probe writes it for a person to apply and commit.
type KnowledgeUpdate struct {
	Title    string   `json:"title"`
	Kind     string   `json:"kind"`
	Paths    []string `json:"paths"`
	Text     string   `json:"text"`
	Reason   string   `json:"reason,omitempty"`
	Obsolete bool     `json:"obsolete,omitempty"`
}

// Knowledge records how a review used the knowledge base: the file read at
// the tip of the base ref, the entries given to the reviewer and the updates
// it proposed.
type Knowledge struct {
	Path         string            `json:"path"`
	Commit       string            `json:"commit,omitempty"`
	SHA256       string            `json:"sha256,omitempty"` // of the file read; empty when there was none
	EntriesTotal int               `json:"entries_total"`
	Entries      []KnowledgeEntry  `json:"entries"`
	Updates      []KnowledgeUpdate `json:"updates"`
}

// KnowledgeUpdatesFormat identifies knowledge-updates.json.
const KnowledgeUpdatesFormat = "probe-knowledge-updates"

// KnowledgeProposal is knowledge-updates.json: updates proposed by a review
// or by probe knowledge build, to apply with probe knowledge apply.
type KnowledgeProposal struct {
	Format      string            `json:"format"`
	Version     int               `json:"version"`
	ToolVersion string            `json:"tool_version"`
	Source      string            `json:"source"` // "review" or "build"
	Path        string            `json:"path"`
	BaseCommit  string            `json:"base_commit,omitempty"`
	HeadCommit  string            `json:"head_commit,omitempty"`
	Updates     []KnowledgeUpdate `json:"updates"`
}
