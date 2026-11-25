// Package state defines the persistent change ledger and task records and a
// file-based store for them. Records are stored one per file as indented
// JSON so they are human-readable, diffable, and merge-friendly.
package state

import (
	"time"
)

// Status is the lifecycle state of an upstream change.
type Status string

// Change lifecycle states.
const (
	StatusDiscovered         Status = "discovered"
	StatusPending            Status = "pending"
	StatusAnalyzed           Status = "analyzed"
	StatusPlanned            Status = "planned"
	StatusInProgress         Status = "in_progress"
	StatusImplemented        Status = "implemented"
	StatusVerified           Status = "verified"
	StatusPROpened           Status = "pr_opened"
	StatusMerged             Status = "merged"
	StatusBlocked            Status = "blocked"
	StatusFailed             Status = "failed"
	StatusAlreadyImplemented Status = "already_implemented"
	StatusNotApplicable      Status = "not_applicable"
	StatusSkipped            Status = "skipped"
	StatusWaitingDependency  Status = "waiting_dependency"
	StatusNeedsReview        Status = "needs_review"
	StatusReverted           Status = "reverted"
	StatusSuperseded         Status = "superseded"
)

// AllStatuses lists statuses in lifecycle order (for reports).
var AllStatuses = []Status{
	StatusDiscovered, StatusPending, StatusAnalyzed, StatusPlanned, StatusWaitingDependency,
	StatusInProgress, StatusImplemented, StatusVerified, StatusPROpened, StatusMerged,
	StatusAlreadyImplemented, StatusNotApplicable, StatusSkipped, StatusReverted,
	StatusSuperseded, StatusNeedsReview, StatusBlocked, StatusFailed,
}

// Settled reports whether a change requires no further synchronization
// work. Only merged changes and explicit non-implementation outcomes count:
// a change is never settled merely because an agent implemented it or a PR
// was opened.
func (s Status) Settled() bool {
	switch s {
	case StatusMerged, StatusAlreadyImplemented, StatusNotApplicable, StatusSkipped,
		StatusReverted, StatusSuperseded:
		return true
	}
	return false
}

// NeedsAttention reports whether a human should look at the change.
func (s Status) NeedsAttention() bool {
	return s == StatusNeedsReview || s == StatusFailed || s == StatusBlocked
}

// Classification is the semantic category of an upstream change.
type Classification string

// Change classifications.
const (
	ClassSecurityFix        Classification = "security_fix"
	ClassBugFix             Classification = "bug_fix"
	ClassFeature            Classification = "feature"
	ClassPerformance        Classification = "performance"
	ClassBreakingChange     Classification = "breaking_change"
	ClassRefactoring        Classification = "refactoring"
	ClassDependencyUpdate   Classification = "dependency_update"
	ClassTest               Classification = "test"
	ClassDocumentation      Classification = "documentation"
	ClassLanguageSpecific   Classification = "language_specific"
	ClassAlreadyImplemented Classification = "already_implemented"
	ClassNotApplicable      Classification = "not_applicable"
	ClassNeedsReview        Classification = "requires_human_review"
)

// Classifications lists all valid classifications.
var Classifications = []Classification{
	ClassSecurityFix, ClassBugFix, ClassFeature, ClassPerformance, ClassBreakingChange,
	ClassRefactoring, ClassDependencyUpdate, ClassTest, ClassDocumentation,
	ClassLanguageSpecific, ClassAlreadyImplemented, ClassNotApplicable, ClassNeedsReview,
}

// Valid reports whether c is a known classification.
func (c Classification) Valid() bool {
	for _, k := range Classifications {
		if k == c {
			return true
		}
	}
	return false
}

// FileChange is one file touched by an upstream change.
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A, M, D, R
	OldPath string `json:"old_path,omitempty"`
}

// Analysis is the semantic analysis of a change.
type Analysis struct {
	Classification       Classification `json:"classification"`
	Confidence           float64        `json:"confidence"`
	Reasoning            string         `json:"reasoning"`
	Evidence             []string       `json:"evidence,omitempty"`
	BehaviorChange       string         `json:"behavior_change,omitempty"`
	Rationale            string         `json:"rationale,omitempty"`
	AffectedComponents   []string       `json:"affected_components,omitempty"`
	DownstreamComponents []string       `json:"downstream_components,omitempty"`
	AlreadyAddressed     bool           `json:"already_addressed,omitempty"`
	Applicable           *bool          `json:"applicable,omitempty"`
	DependsOn            []string       `json:"depends_on,omitempty"`
	SuggestedTests       []string       `json:"suggested_tests,omitempty"`
	Risk                 string         `json:"risk,omitempty"`
	Analyzer             string         `json:"analyzer"`
	AnalyzedAt           time.Time      `json:"analyzed_at"`
}

// Decision is a recorded human (or policy) decision.
type Decision struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Action string    `json:"action"`
	Reason string    `json:"reason,omitempty"`
}

// Event is an entry in a record's history.
type Event struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Detail string    `json:"detail,omitempty"`
}

// PRRef identifies a downstream pull request.
type PRRef struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"` // open | closed | merged
	Draft  bool   `json:"draft,omitempty"`
}

// Change is a ledger record for one logical upstream change.
type Change struct {
	ID           string       `json:"id"`
	Upstream     string       `json:"upstream"`
	UpstreamRepo string       `json:"upstream_repo"`
	SHA          string       `json:"sha"`
	Parents      []string     `json:"parents,omitempty"`
	Seq          int          `json:"seq"`
	PRNumber     int          `json:"pr_number,omitempty"`
	PRURL        string       `json:"pr_url,omitempty"`
	PRBody       string       `json:"pr_body,omitempty"`
	Labels       []string     `json:"labels,omitempty"`
	Title        string       `json:"title"`
	Message      string       `json:"message"`
	Author       string       `json:"author"`
	AuthorEmail  string       `json:"author_email,omitempty"`
	AuthoredAt   time.Time    `json:"authored_at"`
	CommittedAt  time.Time    `json:"committed_at"`
	Files        []FileChange `json:"files"`
	Tags         []string     `json:"tags,omitempty"`
	PatchID      string       `json:"patch_id,omitempty"`
	IsMerge      bool         `json:"is_merge,omitempty"`
	RevertOf     string       `json:"revert_of,omitempty"`

	Status       Status    `json:"status"`
	StatusReason string    `json:"status_reason,omitempty"`
	Analysis     *Analysis `json:"analysis,omitempty"`
	DependsOn    []string  `json:"depends_on,omitempty"`
	TaskID       string    `json:"task_id,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"`

	DownstreamCommit string     `json:"downstream_commit,omitempty"`
	Branch           string     `json:"branch,omitempty"`
	PR               *PRRef     `json:"pr,omitempty"`
	Agent            string     `json:"agent,omitempty"`
	CostUSD          float64    `json:"cost_usd,omitempty"`
	Decisions        []Decision `json:"decisions,omitempty"`
	Events           []Event    `json:"events,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Revision  int64     `json:"revision"`
}

// Short returns an abbreviated SHA.
func (c *Change) Short() string {
	if len(c.SHA) > 7 {
		return c.SHA[:7]
	}
	return c.SHA
}

// Ref returns a human-readable upstream reference ("#123" or short SHA).
func (c *Change) Ref() string {
	if c.PRNumber > 0 {
		return "#" + itoa(c.PRNumber)
	}
	return c.Short()
}

// Paths returns the paths touched by the change (including old paths of
// renames).
func (c *Change) Paths() []string {
	out := make([]string, 0, len(c.Files))
	for _, f := range c.Files {
		out = append(out, f.Path)
		if f.OldPath != "" {
			out = append(out, f.OldPath)
		}
	}
	return out
}

// SetStatus transitions the record and logs an event.
func (c *Change) SetStatus(s Status, reason string) {
	if c.Status == s && c.StatusReason == reason {
		return
	}
	c.AddEvent("status", string(c.Status)+" → "+string(s)+optional(reason))
	c.Status = s
	c.StatusReason = reason
}

// AddEvent appends a history event.
func (c *Change) AddEvent(typ, detail string) {
	c.Events = append(c.Events, Event{At: Now(), Type: typ, Detail: detail})
}

// TaskStatus is the state of a synchronization task.
type TaskStatus string

// Task states.
const (
	TaskOpen        TaskStatus = "open"        // ready or waiting for dependencies
	TaskClaimed     TaskStatus = "claimed"     // leased by a worker
	TaskImplemented TaskStatus = "implemented" // agent finished, not yet verified
	TaskVerified    TaskStatus = "verified"    // verification passed
	TaskPROpened    TaskStatus = "pr_opened"
	TaskDone        TaskStatus = "done" // merged or settled without code
	TaskFailed      TaskStatus = "failed"
	TaskNeedsReview TaskStatus = "needs_review"
	TaskCancelled   TaskStatus = "cancelled"
)

// Terminal reports whether no further automated work will happen.
func (s TaskStatus) Terminal() bool {
	return s == TaskDone || s == TaskCancelled
}

// Lease grants a worker exclusive ownership of a task until ExpiresAt.
type Lease struct {
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	Acquired  time.Time `json:"acquired"`
	ExpiresAt time.Time `json:"expires_at"`
	Backend   string    `json:"backend"`
	Ref       string    `json:"ref,omitempty"` // remote lease ref (git backend)
	RefSHA    string    `json:"ref_sha,omitempty"`
}

// Expired reports whether the lease has lapsed.
func (l *Lease) Expired(now time.Time) bool { return l == nil || now.After(l.ExpiresAt) }

// AgentResult is the structured output an agent reports for a task.
type AgentResult struct {
	Status             string            `json:"status"` // implemented | already_implemented | not_applicable | needs_human | failed
	Summary            string            `json:"summary"`
	FilesChanged       []string          `json:"files_changed,omitempty"`
	TestsAdded         []string          `json:"tests_added,omitempty"`
	ArchitecturalNotes string            `json:"architectural_notes,omitempty"`
	Limitations        []string          `json:"limitations,omitempty"`
	Confidence         float64           `json:"confidence,omitempty"`
	ProposedMappings   []ProposedMapping `json:"mappings,omitempty"`
	Notes              []string          `json:"notes,omitempty"`
	CostUSD            float64           `json:"cost_usd,omitempty"`
	CommitMessage      string            `json:"commit_message,omitempty"`
}

// ProposedMapping is a mapping suggested by an agent.
type ProposedMapping struct {
	Upstream     []string `json:"upstream"`
	Downstream   []string `json:"downstream"`
	Kind         string   `json:"kind,omitempty"`
	Relationship string   `json:"relationship,omitempty"`
	Status       string   `json:"status,omitempty"`
}

// Attempt is one implementation attempt of a task.
type Attempt struct {
	Number       int                 `json:"number"`
	Agent        string              `json:"agent"`
	Owner        string              `json:"owner"`
	StartedAt    time.Time           `json:"started_at"`
	FinishedAt   time.Time           `json:"finished_at,omitempty"`
	Outcome      string              `json:"outcome"` // implemented | verification_failed | agent_failed | no_changes | timeout | ...
	Error        string              `json:"error,omitempty"`
	Result       *AgentResult        `json:"result,omitempty"`
	Verification *VerificationReport `json:"verification,omitempty"`
	Commit       string              `json:"commit,omitempty"`
	CostUSD      float64             `json:"cost_usd,omitempty"`
	LogFile      string              `json:"log_file,omitempty"`
}

// Task is a unit of synchronization work covering one or more changes.
type Task struct {
	ID           string              `json:"id"`
	Changes      []string            `json:"changes"`
	DependsOn    []string            `json:"depends_on,omitempty"`
	Priority     int                 `json:"priority"`
	Seq          int                 `json:"seq"`
	Status       TaskStatus          `json:"status"`
	StatusReason string              `json:"status_reason,omitempty"`
	Lease        *Lease              `json:"lease,omitempty"`
	Branch       string              `json:"branch"`
	Base         string              `json:"base,omitempty"`
	BaseSHA      string              `json:"base_sha,omitempty"`
	StackedOn    string              `json:"stacked_on,omitempty"`
	Worktree     string              `json:"worktree,omitempty"`
	Commit       string              `json:"commit,omitempty"`
	Attempts     []Attempt           `json:"attempts,omitempty"`
	RetryBase    int                 `json:"retry_base,omitempty"` // attempts before the last manual retry
	Result       *AgentResult        `json:"result,omitempty"`
	Verification *VerificationReport `json:"verification,omitempty"`
	PR           *PRRef              `json:"pr,omitempty"`
	CostUSD      float64             `json:"cost_usd,omitempty"`
	Events       []Event             `json:"events,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	UpdatedAt    time.Time           `json:"updated_at"`
	Revision     int64               `json:"revision"`
}

// AddEvent appends a history event.
func (t *Task) AddEvent(typ, detail string) {
	t.Events = append(t.Events, Event{At: Now(), Type: typ, Detail: detail})
}

// SetStatus transitions the task.
func (t *Task) SetStatus(s TaskStatus, reason string) {
	if t.Status == s && t.StatusReason == reason {
		return
	}
	t.AddEvent("status", string(t.Status)+" → "+string(s)+optional(reason))
	t.Status = s
	t.StatusReason = reason
}

// Outcome of verification.
const (
	OutcomeVerified          = "verified"
	OutcomePartiallyVerified = "partially_verified"
	OutcomeUnverified        = "unverified"
	OutcomeFailed            = "failed"
)

// CheckResult is the result of one verification check.
type CheckResult struct {
	Name     string        `json:"name"`
	Level    string        `json:"level"` // build | lint | test | regression | differential | performance
	Command  string        `json:"command,omitempty"`
	Status   string        `json:"status"` // passed | failed | skipped | error
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration_ns"`
	Output   string        `json:"output,omitempty"`
	Note     string        `json:"note,omitempty"`
}

// Metric is one performance measurement comparison.
type Metric struct {
	Name          string  `json:"name"`
	Base          float64 `json:"base"`
	Current       float64 `json:"current"`
	ChangePercent float64 `json:"change_percent"`
	Regression    bool    `json:"regression"`
}

// VerificationReport summarises all verification layers for a commit.
type VerificationReport struct {
	Commit     string        `json:"commit"`
	Tree       string        `json:"tree,omitempty"` // verified tree; reused if the commit is squashed
	Outcome    string        `json:"outcome"`
	Summary    string        `json:"summary"`
	Checks     []CheckResult `json:"checks"`
	Regression *Regression   `json:"regression,omitempty"`
	Metrics    []Metric      `json:"metrics,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Disclaimer string        `json:"disclaimer"`
}

// Regression records a fails-before/passes-after demonstration.
type Regression struct {
	Tests        []string `json:"tests"`
	FailedBefore bool     `json:"failed_before"`
	PassedAfter  bool     `json:"passed_after"`
	Demonstrated bool     `json:"demonstrated"`
	Note         string   `json:"note,omitempty"`
}

// Passed reports whether the outcome allows the change to proceed.
func (r *VerificationReport) Passed(allowPartial bool) bool {
	if r == nil {
		return false
	}
	switch r.Outcome {
	case OutcomeVerified:
		return true
	case OutcomePartiallyVerified, OutcomeUnverified:
		return allowPartial
	}
	return false
}

func optional(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Now is the clock used for records (overridable in tests).
var Now = func() time.Time { return time.Now().UTC().Truncate(time.Second) }
