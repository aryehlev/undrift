// Package mapping resolves upstream changes to downstream components using
// the semantic mappings stored in project memory, and proposes heuristic
// mappings for unmapped files.
//
// The MVP relies on stored mappings plus lightweight name similarity. The
// Resolver interface is the extension point for Tree-sitter, language
// servers or symbol indexes later.
package mapping

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aryehlev/undrift/internal/glob"
	"github.com/aryehlev/undrift/internal/memory"
)

// Resolution describes how a set of upstream files maps downstream.
type Resolution struct {
	Mappings    []memory.Mapping `json:"mappings"`
	Downstream  []string         `json:"downstream"`  // downstream paths/symbols implicated
	Unmapped    []string         `json:"unmapped"`    // upstream files with no mapping
	Unsupported []string         `json:"unsupported"` // upstream files mapped as unsupported
	Missing     []string         `json:"missing"`     // upstream files whose functionality is not yet ported
	Suggestions []memory.Mapping `json:"suggestions,omitempty"`
}

// Resolver maps upstream files to downstream components.
type Resolver interface {
	Resolve(files []string) Resolution
}

// Engine is the default Resolver backed by memory plus heuristics.
type Engine struct {
	Mem *memory.Memory
	// Root is the downstream repository root (for heuristic suggestions).
	Root string
	// Exclude lists downstream paths never suggested.
	Exclude []string

	index []string // downstream file list (lazy)
}

// Resolve implements Resolver.
func (e *Engine) Resolve(files []string) Resolution {
	var r Resolution
	seenDown := map[string]bool{}
	seenMap := map[string]bool{}
	for _, f := range files {
		ms := e.Mem.MappingsFor([]string{f})
		if len(ms) == 0 {
			r.Unmapped = append(r.Unmapped, f)
			if s, ok := e.Suggest(f); ok {
				r.Suggestions = append(r.Suggestions, s)
			}
			continue
		}
		allUnsupported := true
		for _, m := range ms {
			if !seenMap[m.ID] {
				seenMap[m.ID] = true
				r.Mappings = append(r.Mappings, m)
			}
			switch m.Status {
			case memory.MappingUnsupported:
				continue
			case memory.MappingMissing:
				r.Missing = append(r.Missing, f)
			}
			allUnsupported = false
			for _, d := range m.Downstream {
				if !seenDown[d] {
					seenDown[d] = true
					r.Downstream = append(r.Downstream, d)
				}
			}
		}
		if allUnsupported {
			r.Unsupported = append(r.Unsupported, f)
		}
	}
	sort.Strings(r.Downstream)
	return r
}

// Suggest proposes a downstream counterpart for an upstream file using name
// similarity (same stem, related directory). Returns false if nothing
// plausible exists.
func (e *Engine) Suggest(upFile string) (memory.Mapping, bool) {
	if e.Root == "" {
		return memory.Mapping{}, false
	}
	idx := e.files()
	stem := normalizeStem(upFile)
	if stem == "" {
		return memory.Mapping{}, false
	}
	upDirs := dirTokens(upFile)
	best, bestScore := "", 0
	for _, f := range idx {
		if normalizeStem(f) != stem {
			continue
		}
		score := 2
		for d := range dirTokens(f) {
			if upDirs[d] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = f, score
		}
	}
	if best == "" {
		return memory.Mapping{}, false
	}
	conf := 0.4
	if bestScore > 2 {
		conf = 0.6
	}
	return memory.Mapping{
		Kind:       memory.KindFile,
		Upstream:   memory.StringList{upFile},
		Downstream: memory.StringList{best},
		Status:     memory.MappingMapped,
		Confidence: conf,
		Source:     "heuristic",
		Notes:      "suggested by file-name similarity; verify before relying on it",
	}, true
}

var testSuffixes = []string{"_test", ".test", "_spec", ".spec", "test_"}

func normalizeStem(p string) string {
	base := path.Base(filepath.ToSlash(p))
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.ToLower(base)
	for _, s := range testSuffixes {
		base = strings.TrimSuffix(strings.TrimPrefix(base, s), s)
	}
	base = strings.NewReplacer("_", "", "-", "").Replace(base)
	switch base {
	case "", "index", "mod", "lib", "main", "init", "__init__", "readme":
		return ""
	}
	return base
}

func dirTokens(p string) map[string]bool {
	out := map[string]bool{}
	for _, d := range strings.Split(path.Dir(filepath.ToSlash(p)), "/") {
		d = strings.ToLower(d)
		switch d {
		case ".", "", "src", "lib", "pkg", "internal", "cmd", "app":
			continue
		}
		out[d] = true
	}
	return out
}

var skipDirs = map[string]bool{
	".git": true, ".undrift": true, "node_modules": true, "target": true, "vendor": true,
	"dist": true, "build": true, ".venv": true, "__pycache__": true,
}

func (e *Engine) files() []string {
	if e.index != nil {
		return e.index
	}
	e.index = []string{}
	_ = filepath.WalkDir(e.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != e.Root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(e.Root, p)
		rel = filepath.ToSlash(rel)
		if glob.Any(e.Exclude, rel) {
			return nil
		}
		e.index = append(e.index, rel)
		if len(e.index) > 200_000 {
			return filepath.SkipAll
		}
		return nil
	})
	return e.index
}
