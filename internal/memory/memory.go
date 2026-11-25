// Package memory manages Undrift's persistent, version-controlled project
// knowledge: semantic mappings, intentional deviations and free-form notes
// (architectural decisions, conventions, unsupported features, feedback).
//
// Memory lives in human-readable YAML under .undrift/memory/ so that it is
// portable between agents and reviewable like any other code.
package memory

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aryehlev/undrift/internal/glob"
	"github.com/aryehlev/undrift/internal/state"
)

// StringList unmarshals from either a scalar or a sequence.
type StringList []string

// UnmarshalYAML accepts "a" or ["a", "b"].
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value == "" {
			*s = nil
			return nil
		}
		*s = StringList{n.Value}
		return nil
	}
	var l []string
	if err := n.Decode(&l); err != nil {
		return err
	}
	*s = l
	return nil
}

// MarshalYAML emits a scalar for single-element lists.
func (s StringList) MarshalYAML() (any, error) {
	if len(s) == 1 {
		return s[0], nil
	}
	return []string(s), nil
}

// Mapping kinds.
const (
	KindModule   = "module"
	KindFile     = "file"
	KindFunction = "function"
	KindAPI      = "api"
	KindBehavior = "behavior"
)

// Mapping statuses.
const (
	MappingMapped      = "mapped"      // functionality exists downstream
	MappingMissing     = "missing"     // not yet ported
	MappingDeviation   = "deviation"   // intentionally different implementation
	MappingUnsupported = "unsupported" // intentionally not supported downstream
)

// Mapping relates upstream functionality to downstream functionality. Paths
// are globs; symbols are written as "path#Symbol". Mappings never assume
// identical names or structure.
type Mapping struct {
	ID           string     `yaml:"id"`
	Kind         string     `yaml:"kind,omitempty"`
	Upstream     StringList `yaml:"upstream"`
	Downstream   StringList `yaml:"downstream,omitempty"`
	Relationship string     `yaml:"relationship,omitempty"`
	Status       string     `yaml:"status,omitempty"`
	Confidence   float64    `yaml:"confidence,omitempty"`
	Source       string     `yaml:"source,omitempty"` // human | agent | heuristic
	Notes        string     `yaml:"notes,omitempty"`
	UpdatedAt    string     `yaml:"updated_at,omitempty"`
}

// Deviation records an intentional architectural difference that agents
// must preserve.
type Deviation struct {
	ID                 string     `yaml:"id,omitempty"`
	Upstream           StringList `yaml:"upstream"`
	Downstream         StringList `yaml:"downstream,omitempty"`
	Reason             string     `yaml:"reason"`
	Preserve           []string   `yaml:"preserve,omitempty"`
	SyncBehavior       bool       `yaml:"sync_behavior"`
	SyncImplementation bool       `yaml:"sync_implementation"`
}

// Note kinds.
const (
	NoteArchitecture = "architecture"
	NoteConvention   = "convention"
	NoteUnsupported  = "unsupported"
	NotePerformance  = "performance"
	NoteVerification = "verification"
	NoteFeedback     = "feedback"
	NoteFailure      = "failure"
)

// Note is a free-form piece of project knowledge.
type Note struct {
	Kind   string     `yaml:"kind"`
	Text   string     `yaml:"text"`
	Paths  StringList `yaml:"paths,omitempty"`
	Source string     `yaml:"source,omitempty"`
	At     string     `yaml:"at,omitempty"`
}

// Memory is the loaded project memory.
type Memory struct {
	Dir        string
	Mappings   []Mapping
	Deviations []Deviation
	Notes      []Note
}

type mappingsFile struct {
	Mappings []Mapping `yaml:"mappings"`
}
type deviationsFile struct {
	Deviations []Deviation `yaml:"deviations"`
}
type notesFile struct {
	Notes []Note `yaml:"notes"`
}

// Load reads memory from dir; missing files are treated as empty.
func Load(dir string) (*Memory, error) {
	m := &Memory{Dir: dir}
	var mf mappingsFile
	var df deviationsFile
	var nf notesFile
	for name, v := range map[string]any{"mappings.yaml": &mf, "deviations.yaml": &df, "notes.yaml": &nf} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, v); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
	}
	m.Mappings, m.Deviations, m.Notes = mf.Mappings, df.Deviations, nf.Notes
	for i := range m.Mappings {
		if m.Mappings[i].ID == "" {
			m.Mappings[i].ID = MappingID(m.Mappings[i].Upstream, m.Mappings[i].Downstream)
		}
	}
	for i := range m.Deviations {
		if m.Deviations[i].ID == "" {
			m.Deviations[i].ID = "dev-" + MappingID(m.Deviations[i].Upstream, m.Deviations[i].Downstream)[2:]
		}
	}
	return m, nil
}

// Save writes all memory files.
func (m *Memory) Save() error {
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	sort.SliceStable(m.Mappings, func(i, j int) bool {
		return strings.Join(m.Mappings[i].Upstream, ",") < strings.Join(m.Mappings[j].Upstream, ",")
	})
	files := []struct {
		name   string
		header string
		v      any
	}{
		{"mappings.yaml", "# Upstream → downstream semantic mappings. Edit freely; entries with\n# source: human are never overwritten by agents.\n", mappingsFile{m.Mappings}},
		{"deviations.yaml", "# Intentional architectural deviations agents must preserve.\n", deviationsFile{m.Deviations}},
		{"notes.yaml", "# Project knowledge: architecture decisions, conventions, unsupported\n# features, performance constraints, human feedback.\n", notesFile{m.Notes}},
	}
	for _, f := range files {
		data, err := yaml.Marshal(f.v)
		if err != nil {
			return err
		}
		if err := state.WriteFileAtomic(filepath.Join(m.Dir, f.name), append([]byte(f.header), data...)); err != nil {
			return err
		}
	}
	return nil
}

// MappingID derives a stable identifier from mapping endpoints.
func MappingID(up, down []string) string {
	h := sha1.New()
	for _, s := range up {
		h.Write([]byte("u:" + s + "\n"))
	}
	for _, s := range down {
		h.Write([]byte("d:" + s + "\n"))
	}
	return "m-" + hex.EncodeToString(h.Sum(nil))[:10]
}

// pathOf strips a "#Symbol" suffix.
func pathOf(s string) string {
	if i := strings.Index(s, "#"); i >= 0 {
		return s[:i]
	}
	return s
}

func matchesAny(patterns []string, files []string) bool {
	for _, p := range patterns {
		for _, f := range files {
			if glob.Match(pathOf(p), f) {
				return true
			}
		}
	}
	return false
}

// MappingsFor returns mappings whose upstream side matches any file.
func (m *Memory) MappingsFor(files []string) []Mapping {
	var out []Mapping
	for _, mp := range m.Mappings {
		if matchesAny(mp.Upstream, files) {
			out = append(out, mp)
		}
	}
	return out
}

// DeviationsFor returns deviations that touch any of the upstream or
// downstream files.
func (m *Memory) DeviationsFor(upFiles, downFiles []string) []Deviation {
	var out []Deviation
	for _, d := range m.Deviations {
		if matchesAny(d.Upstream, upFiles) || matchesAny(d.Downstream, downFiles) {
			out = append(out, d)
		}
	}
	return out
}

// NotesFor returns global notes (no paths) and notes matching any file.
func (m *Memory) NotesFor(files []string) []Note {
	var out []Note
	for _, n := range m.Notes {
		if len(n.Paths) == 0 || matchesAny(n.Paths, files) {
			out = append(out, n)
		}
	}
	return out
}

// AddNote appends a note.
func (m *Memory) AddNote(n Note) {
	if n.At == "" {
		n.At = time.Now().UTC().Format(time.RFC3339)
	}
	m.Notes = append(m.Notes, n)
}

// UpsertMapping adds or updates a mapping. Human-authored mappings are
// never modified by non-human sources. It returns true if memory changed.
func (m *Memory) UpsertMapping(mp Mapping) bool {
	if mp.ID == "" {
		mp.ID = MappingID(mp.Upstream, mp.Downstream)
	}
	if mp.Status == "" {
		mp.Status = MappingMapped
	}
	mp.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	for i, cur := range m.Mappings {
		same := cur.ID == mp.ID || (equalSets(cur.Upstream, mp.Upstream) && (len(mp.Downstream) == 0 || equalSets(cur.Downstream, mp.Downstream)))
		if !same {
			continue
		}
		if cur.Source == "human" && mp.Source != "human" {
			return false
		}
		mp.ID = cur.ID
		if mp.Relationship == "" {
			mp.Relationship = cur.Relationship
		}
		if mp.Kind == "" {
			mp.Kind = cur.Kind
		}
		m.Mappings[i] = mp
		return true
	}
	m.Mappings = append(m.Mappings, mp)
	return true
}

// RemoveMapping deletes a mapping by ID.
func (m *Memory) RemoveMapping(id string) bool {
	for i, mp := range m.Mappings {
		if mp.ID == id {
			m.Mappings = append(m.Mappings[:i], m.Mappings[i+1:]...)
			return true
		}
	}
	return false
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// Starter files written by `undrift init`.
const (
	StarterMappings = `# Upstream → downstream semantic mappings.
#
# Mappings do not assume identical file names, functions or architecture.
# Paths are globs; symbols can be written as "path#Symbol".
#
# mappings:
#   - kind: function
#     upstream: auction/auction.go#RunAuction
#     downstream: src/auction/engine.rs#execute
#     relationship: Execute an auction and collect bidder responses.
#     status: mapped        # mapped | missing | deviation | unsupported
#     source: human
mappings: []
`
	StarterDeviations = `# Intentional architectural deviations. Agents must not undo these while
# synchronizing unrelated upstream functionality.
#
# deviations:
#   - upstream: auction/auction.go
#     downstream: src/auction/engine.rs
#     reason: >
#       The Rust implementation uses FuturesUnordered instead of spawning a
#       task per bidder.
#     preserve:
#       - Single-task execution
#       - Zero-copy request handling
#     sync_behavior: true
#     sync_implementation: false
deviations: []
`
	StarterNotes = `# Project knowledge shared with every agent: architecture decisions,
# coding conventions, unsupported upstream features, performance
# constraints, verification requirements and human feedback.
#
# notes:
#   - kind: convention
#     text: Errors are returned as thiserror enums, never strings.
#   - kind: unsupported
#     text: The downstream port does not implement the legacy XML API.
#     paths: [api/xml/**]
notes: []
`
)
