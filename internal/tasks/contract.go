package tasks

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/state"
)

// SchemaVersion identifies the task contract format.
const SchemaVersion = "undrift.task/v1"

// Contract is the standardized, agent-agnostic description of a
// synchronization task. It is written to task.json and rendered as a
// Markdown prompt. Fields marked untrusted contain upstream-controlled text.
type Contract struct {
	Schema       string   `json:"schema"`
	TaskID       string   `json:"task_id"`
	Goal         string   `json:"goal"`
	Instructions []string `json:"instructions"`

	Changes          []ChangeContext   `json:"changes"`
	Downstream       DownstreamContext `json:"downstream"`
	UpstreamCheckout string            `json:"upstream_checkout,omitempty"`

	Mappings           []memory.Mapping   `json:"mappings"`
	SuggestedMappings  []memory.Mapping   `json:"suggested_mappings,omitempty"`
	UnmappedFiles      []string           `json:"unmapped_upstream_files,omitempty"`
	Deviations         []memory.Deviation `json:"deviations"`
	Notes              []memory.Note      `json:"notes"`
	Rules              []string           `json:"rules"`
	PreserveArch       bool               `json:"preserve_architecture"`
	PreviousDecisions  []DecisionContext  `json:"previous_decisions,omitempty"`
	PreviousAttempts   []AttemptContext   `json:"previous_attempts,omitempty"`
	RequiredBehavior   []string           `json:"required_behavior"`
	AcceptanceCriteria []string           `json:"acceptance_criteria"`
	Verification       VerifyContext      `json:"verification"`
	Output             OutputContract     `json:"output"`
	Security           []string           `json:"security"`
}

// ChangeContext describes one upstream change in a task.
type ChangeContext struct {
	ID             string             `json:"id"`
	Upstream       string             `json:"upstream"`
	Repository     string             `json:"repository"`
	SHA            string             `json:"sha"`
	CommitURL      string             `json:"commit_url,omitempty"`
	PRNumber       int                `json:"pr_number,omitempty"`
	PRURL          string             `json:"pr_url,omitempty"`
	Title          string             `json:"title"`             // untrusted
	Message        string             `json:"message"`           // untrusted
	PRBody         string             `json:"pr_body,omitempty"` // untrusted
	Author         string             `json:"author"`
	Date           string             `json:"date"`
	Classification string             `json:"classification,omitempty"`
	Analysis       *state.Analysis    `json:"analysis,omitempty"`
	RevertOf       string             `json:"revert_of,omitempty"`
	Files          []state.FileChange `json:"files"`
	Diff           string             `json:"diff"` // untrusted
	DiffTruncated  bool               `json:"diff_truncated,omitempty"`
	UpstreamFiles  []FileContent      `json:"upstream_files,omitempty"` // untrusted
}

// FileContent is a source file excerpt.
type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// DownstreamContext describes the downstream workspace.
type DownstreamContext struct {
	Repository string        `json:"repository_root"`
	Workdir    string        `json:"workdir"`
	Branch     string        `json:"branch"`
	Base       string        `json:"base"`
	BaseSHA    string        `json:"base_sha,omitempty"`
	StackedOn  string        `json:"stacked_on,omitempty"`
	Files      []FileContent `json:"relevant_files,omitempty"`
}

// DecisionContext is a previous synchronization decision relevant here.
type DecisionContext struct {
	Change string `json:"change"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// AttemptContext summarizes a previous failed attempt.
type AttemptContext struct {
	Number       int      `json:"number"`
	Agent        string   `json:"agent"`
	Outcome      string   `json:"outcome"`
	Error        string   `json:"error,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

// VerifyContext tells the agent how its work will be verified.
type VerifyContext struct {
	Commands     []string `json:"commands"`
	Regression   bool     `json:"regression_tests_expected"`
	Differential bool     `json:"differential_testing"`
	Performance  bool     `json:"performance_thresholds"`
	Note         string   `json:"note"`
}

// OutputContract defines the structured result the agent must produce.
type OutputContract struct {
	ResultFile string   `json:"result_file"`
	Statuses   []string `json:"statuses"`
	Schema     string   `json:"schema"`
}

// Result statuses an agent may report.
const (
	ResultImplemented        = "implemented"
	ResultAlreadyImplemented = "already_implemented"
	ResultNotApplicable      = "not_applicable"
	ResultNeedsHuman         = "needs_human"
	ResultFailed             = "failed"
)

// ResultStatuses lists valid agent result statuses.
var ResultStatuses = []string{ResultImplemented, ResultAlreadyImplemented, ResultNotApplicable, ResultNeedsHuman, ResultFailed}

// ResultSchema documents the agent result document.
const ResultSchema = `{
  "status": "implemented | already_implemented | not_applicable | needs_human | failed",
  "summary": "what you changed downstream and why (required)",
  "files_changed": ["downstream paths you modified"],
  "tests_added": ["downstream test files you added or modified to cover the behavior"],
  "architectural_notes": "how the implementation differs from upstream and why",
  "limitations": ["known gaps or unverified aspects"],
  "confidence": 0.0-1.0,
  "mappings": [{"upstream": ["path#Symbol"], "downstream": ["path#symbol"], "kind": "function|module|api|behavior|file", "relationship": "what they both do", "status": "mapped|missing|deviation|unsupported"}],
  "notes": ["durable project knowledge worth remembering for future tasks"],
  "commit_message": "optional commit message"
}`

// ValidateResult checks an agent result for required fields.
func ValidateResult(r *state.AgentResult) error {
	ok := false
	for _, s := range ResultStatuses {
		if r.Status == s {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("invalid result status %q (want one of %s)", r.Status, strings.Join(ResultStatuses, ", "))
	}
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("result summary is required")
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("confidence must be between 0 and 1")
	}
	return nil
}

// ParseResult decodes an agent result document.
func ParseResult(data []byte) (*state.AgentResult, error) {
	var r state.AgentResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	if err := ValidateResult(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ExtractJSON finds the last JSON object in free-form text that contains
// the given key. It is used to recover structured output from agents that
// print rather than write their result.
func ExtractJSON(text, key string) []byte {
	// Prefer fenced ```json blocks.
	var candidates []string
	rest := text
	for {
		i := strings.Index(rest, "```json")
		if i < 0 {
			break
		}
		rest = rest[i+7:]
		j := strings.Index(rest, "```")
		if j < 0 {
			break
		}
		candidates = append(candidates, rest[:j])
		rest = rest[j+3:]
	}
	// Then balanced top-level objects.
	depth, start, inStr, esc := 0, -1, false, false
	for i, ch := range text {
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			if depth > 0 {
				inStr = true
			}
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					candidates = append(candidates, text[start:i+1])
				}
			}
		}
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(candidates[i])), &m) == nil {
			if _, ok := m[key]; ok {
				return []byte(strings.TrimSpace(candidates[i]))
			}
		}
	}
	return nil
}
