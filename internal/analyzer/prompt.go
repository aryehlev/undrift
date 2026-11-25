package analyzer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aryehlev/undrift/internal/state"
)

// AgentAnalysis is the JSON document an agent returns for analysis tasks.
type AgentAnalysis struct {
	Classification       string   `json:"classification"`
	Confidence           float64  `json:"confidence"`
	Reasoning            string   `json:"reasoning"`
	Evidence             []string `json:"evidence"`
	BehaviorChange       string   `json:"behavior_change"`
	Rationale            string   `json:"rationale"`
	AffectedComponents   []string `json:"affected_components"`
	DownstreamComponents []string `json:"downstream_components"`
	AlreadyImplemented   bool     `json:"already_implemented"`
	Applicable           *bool    `json:"applicable"`
	DependsOn            []string `json:"depends_on"`
	SuggestedTests       []string `json:"suggested_tests"`
	Risk                 string   `json:"risk"`
}

// ToAnalysis validates and converts an agent analysis.
func (a *AgentAnalysis) ToAnalysis(agent string) (*state.Analysis, error) {
	cl := state.Classification(strings.ToLower(strings.TrimSpace(a.Classification)))
	if !cl.Valid() {
		return nil, fmt.Errorf("agent returned unknown classification %q", a.Classification)
	}
	if a.Reasoning == "" {
		return nil, fmt.Errorf("agent analysis is missing reasoning")
	}
	conf := a.Confidence
	if conf > 1 {
		conf = conf / 100
	}
	return &state.Analysis{
		Classification:       cl,
		Confidence:           clamp(conf),
		Reasoning:            a.Reasoning,
		Evidence:             a.Evidence,
		BehaviorChange:       a.BehaviorChange,
		Rationale:            a.Rationale,
		AffectedComponents:   a.AffectedComponents,
		DownstreamComponents: a.DownstreamComponents,
		AlreadyAddressed:     a.AlreadyImplemented,
		Applicable:           a.Applicable,
		DependsOn:            a.DependsOn,
		SuggestedTests:       a.SuggestedTests,
		Risk:                 a.Risk,
		Analyzer:             "agent:" + agent,
		AnalyzedAt:           state.Now(),
	}, nil
}

// ParseAgentAnalysis decodes an analysis document from raw agent output.
func ParseAgentAnalysis(raw []byte) (*AgentAnalysis, error) {
	var a AgentAnalysis
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// AnalysisSchema documents the expected analysis output.
const AnalysisSchema = `{
  "classification": "one of: security_fix | bug_fix | feature | performance | breaking_change | refactoring | dependency_update | test | documentation | language_specific | already_implemented | not_applicable | requires_human_review",
  "confidence": 0.0-1.0,
  "reasoning": "why this classification and decision (required)",
  "evidence": ["concrete facts from the diff and downstream code supporting the decision"],
  "behavior_change": "what observable behavior changed upstream",
  "rationale": "why upstream made the change",
  "affected_components": ["upstream components"],
  "downstream_components": ["downstream files/modules that implement the affected functionality"],
  "already_implemented": false,
  "applicable": true,
  "depends_on": ["upstream SHAs this change depends on, if any"],
  "suggested_tests": ["tests to add or update downstream"],
  "risk": "risk of implementing or skipping"
}`
