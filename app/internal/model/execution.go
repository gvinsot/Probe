package model

import "time"

// Execution-cache statuses and scope.
const (
	CacheHit           = "hit"
	CacheStored        = "stored"
	CacheEnabled       = "enabled"
	CacheDisabled      = "disabled"
	CacheScopeBaseline = "baseline_only"
)

// ExecutionCacheNote is the fixed note of the execution cache summary. It
// states what a replay is and is not, whatever the cache status.
const ExecutionCacheNote = "The cache is opt-in and covers baseline-side runs only; candidate-side runs always execute. " +
	"A replayed check (cache hit) is the recorded result of an earlier live run of byte-identical inputs, not a fresh execution. " +
	"A replay never supports a reproduced issue, a divergence or a FAILS_ON_CANDIDATE result; a negative conclusion rests on a replay " +
	"only after two agreeing live runs, and replay_backed lists it. The integrity checks of an entry detect corruption, not forgery."

// CheckCache is present only on a base-side check that was stored in or
// replayed from the opt-in execution cache. A hit is not a fresh execution.
type CheckCache struct {
	Status             string    `json:"status"`
	Key                string    `json:"key"`
	RecordedAt         time.Time `json:"recorded_at"`
	RecordedRun        string    `json:"recorded_run"`
	RecordedCheck      string    `json:"recorded_check"`
	RecordedDurationMS int64     `json:"recorded_duration_ms"`
	LiveRuns           int       `json:"live_runs"`
}
type ExecutionCache struct {
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Scope        string `json:"scope"`
	ImageID      string `json:"image_id,omitempty"`
	PolicySHA256 string `json:"policy_sha256,omitempty"`
	// Runtime is the Docker server version, OS type and architecture that
	// every key of this run records (F7a).
	Runtime       string `json:"runtime,omitempty"`
	Hits          int    `json:"hits"`
	Stored        int    `json:"stored"`
	Misses        int    `json:"misses"`
	Uncacheable   int    `json:"uncacheable"`
	Rejected      int    `json:"rejected"`
	WriteFailures int    `json:"write_failures"`
	Evicted       int    `json:"evicted"`
	Contradicted  int    `json:"contradicted"`
	Note          string `json:"note"`
}
type ExecutionParallelism struct {
	Requested int    `json:"requested"`
	Effective int    `json:"effective"`
	Note      string `json:"note"`
}
type ExecutionBudget struct {
	MaxRuntimeMS      int64 `json:"max_runtime_ms"`
	SpentMS           int64 `json:"spent_ms"`
	ReviewerReserveMS int64 `json:"reviewer_reserve_ms"`
	DeadlineReached   bool  `json:"deadline_reached"`
}

// Execution summarizes the cache, parallelism and budget of a run in which a
// harness was created. (Check.Replayed is in status.go.)
type Execution struct {
	Cache        ExecutionCache       `json:"cache"`
	Parallelism  ExecutionParallelism `json:"parallelism"`
	Budget       ExecutionBudget      `json:"budget"`
	ReplayBacked []string             `json:"replay_backed"` // evidence IDs; set by finalizeExecution
}
