// Package mapping resolves upstream changes to downstream components using
// the semantic mappings stored in project memory, and proposes heuristic
// mappings for unmapped code.
//
// Resolution is symbol-aware when Tree-sitter analysis is available: a
// mapping for "auction/auction.go#RunAuction" applies only to changes that
// touch RunAuction, and unmapped upstream functions are matched to
// downstream definitions with equivalent names across naming conventions
// (RunAuction ↔ run_auction). The Resolver interface is the extension point
// for language servers or symbol indexes.
package mapping

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aryehlev/undrift/internal/glob"
	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/symbols"
)

// Input is what to resolve: upstream files and, when known, the symbols
// touched in each.
type Input struct {
	Files   []string
	Symbols map[string]memory.FileSymbols
}

// InputFor builds an Input from ledger changes.
func InputFor(changes ...*state.Change) Input {
	in := Input{Symbols: map[string]memory.FileSymbols{}}
	seen := map[string]bool{}
	for _, c := range changes {
		for _, f := range c.Files {
			for _, p := range []string{f.Path, f.OldPath} {
				if p == "" || seen[p] {
					continue
				}
				seen[p] = true
				in.Files = append(in.Files, p)
			}
			if f.Parsed {
				cur := in.Symbols[f.Path]
				cur.Parsed = true
				cur.Names = append(cur.Names, f.Symbols...)
				in.Symbols[f.Path] = cur
			}
		}
	}
	return in
}

// Files is shorthand for an Input without symbol information.
func Files(files []string) Input { return Input{Files: files} }

// Resolution describes how a set of upstream files maps downstream.
type Resolution struct {
	Mappings    []memory.Mapping `json:"mappings"`
	Downstream  []string         `json:"downstream"`  // downstream paths/symbols implicated
	Unmapped    []string         `json:"unmapped"`    // upstream files with no mapping
	Unsupported []string         `json:"unsupported"` // upstream files mapped as unsupported
	Missing     []string         `json:"missing"`     // upstream files whose functionality is not yet ported
	Suggestions []memory.Mapping `json:"suggestions,omitempty"`
}

// Resolver maps upstream code to downstream components.
type Resolver interface {
	Resolve(in Input) Resolution
}

// Engine is the default Resolver backed by memory plus heuristics.
type Engine struct {
	Mem *memory.Memory
	// Root is the downstream repository root (for heuristic suggestions).
	Root string
	// Exclude lists downstream paths never suggested.
	Exclude []string

	index    []string            // downstream file list (lazy)
	symIndex map[string][]symRef // normalized name → downstream definitions (lazy)
}

type symRef struct {
	path, qualified string
}

// Resolve implements Resolver.
func (e *Engine) Resolve(in Input) Resolution {
	var r Resolution
	seenDown := map[string]bool{}
	seenMap := map[string]bool{}
	for _, f := range in.Files {
		ms := e.Mem.MappingsForSymbols([]string{f}, in.Symbols)
		if len(ms) == 0 {
			r.Unmapped = append(r.Unmapped, f)
			r.Suggestions = append(r.Suggestions, e.suggestFor(f, in.Symbols[f])...)
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

func (e *Engine) suggestFor(f string, fs memory.FileSymbols) []memory.Mapping {
	if fs.Parsed && len(fs.Names) > 0 {
		if out := e.SuggestSymbols(f, fs.Names); len(out) > 0 {
			return out
		}
	}
	if s, ok := e.Suggest(f); ok {
		return []memory.Mapping{s}
	}
	return nil
}

// Suggest proposes a downstream counterpart for an upstream file using name
// similarity (same stem, related directory). Returns false if nothing
// plausible exists.
func (e *Engine) Suggest(upFile string) (memory.Mapping, bool) {
	if e.Root == "" {
		return memory.Mapping{}, false
	}
	stem := normalizeStem(upFile)
	if stem == "" {
		return memory.Mapping{}, false
	}
	upDirs := dirTokens(upFile)
	best, bestScore := "", 0
	for _, f := range e.files() {
		if normalizeStem(f) != stem {
			continue
		}
		score := 2 + overlap(upDirs, dirTokens(f))
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

// genericNames are too common to suggest mappings from.
var genericNames = map[string]bool{
	"new": true, "string": true, "init": true, "main": true, "test": true, "setup": true, "teardown": true,
	"close": true, "open": true, "read": true, "write": true, "reset": true, "handle": true, "handler": true,
	"error": true, "value": true, "values": true, "parse": true, "update": true, "create": true, "delete": true,
	"apply": true, "build": true, "start": true, "stop": true, "next": true, "len": true, "equals": true,
	"hashcode": true, "tostring": true, "default": true, "clone": true, "drop": true, "from": true, "into": true,
	"config": true, "options": true, "client": true, "server": true, "request": true, "response": true,
}

// SuggestSymbols proposes function-level mappings for touched upstream
// symbols by matching normalized names against downstream definitions.
func (e *Engine) SuggestSymbols(upFile string, names []string) []memory.Mapping {
	if e.Root == "" || !symbols.Available() {
		return nil
	}
	idx := e.symbolIndex()
	upDirs := dirTokens(upFile)
	var out []memory.Mapping
	seen := map[string]bool{}
	for _, n := range names {
		key := symbols.Normalize(n)
		if len(key) < 4 || genericNames[key] || seen[key] {
			continue
		}
		seen[key] = true
		cands := idx[key]
		if len(cands) == 0 || len(cands) > 3 {
			continue // none, or too ambiguous to guess
		}
		best, bestScore := cands[0], -1
		for _, c := range cands {
			if s := overlap(upDirs, dirTokens(c.path)) + boolInt(normalizeStem(c.path) == normalizeStem(upFile)); s > bestScore {
				best, bestScore = c, s
			}
		}
		conf := 0.5
		if bestScore > 0 {
			conf = 0.65
		}
		out = append(out, memory.Mapping{
			Kind:       memory.KindFunction,
			Upstream:   memory.StringList{upFile + "#" + n},
			Downstream: memory.StringList{best.path + "#" + best.qualified},
			Status:     memory.MappingMapped,
			Confidence: conf,
			Source:     "heuristic",
			Notes:      "suggested by symbol-name similarity; verify before relying on it",
		})
	}
	return out
}

const (
	maxIndexFiles     = 4000
	maxIndexFileBytes = 512 << 10
)

func (e *Engine) symbolIndex() map[string][]symRef {
	if e.symIndex != nil {
		return e.symIndex
	}
	e.symIndex = map[string][]symRef{}
	n := 0
	for _, f := range e.files() {
		if !symbols.Supported(f) || n >= maxIndexFiles {
			continue
		}
		p := filepath.Join(e.Root, filepath.FromSlash(f))
		if st, err := os.Stat(p); err != nil || st.Size() > maxIndexFileBytes {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		n++
		syms, err := symbols.Extract(f, src)
		if err != nil {
			continue
		}
		for _, s := range syms {
			k := symbols.Normalize(s.Name)
			e.symIndex[k] = append(e.symIndex[k], symRef{path: f, qualified: s.Qualified()})
		}
	}
	return e.symIndex
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

func overlap(a, b map[string]bool) int {
	n := 0
	for d := range b {
		if a[d] {
			n++
		}
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
