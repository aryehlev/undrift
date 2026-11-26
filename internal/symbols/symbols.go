// Package symbols finds the definitions (functions, methods, types) touched
// by a change, using Tree-sitter grammars. It needs no build of either
// project and works on raw file contents, so it runs on the bare upstream
// mirror as well as on downstream worktrees.
//
// Tree-sitter requires cgo. Builds with CGO_ENABLED=0 still work: Available
// reports false and Undrift falls back to file-level analysis.
package symbols

import (
	"errors"
	"sort"
	"strings"
	"unicode"
)

// ErrUnsupported is returned for languages without a grammar (or when
// built without cgo).
var ErrUnsupported = errors.New("symbol analysis not supported for this file")

// Symbol is a definition in a source file. Lines are 1-based and inclusive.
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // function | method | type
	Container string `json:"container,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Qualified returns "Container.Name" or "Name".
func (s Symbol) Qualified() string {
	if s.Container != "" {
		return s.Container + "." + s.Name
	}
	return s.Name
}

// FileChange summarizes the symbols touched in one file.
type FileChange struct {
	// Symbols are qualified names of touched definitions (innermost
	// definition containing each changed line).
	Symbols []string
	// TopLevel is true if some changed lines are outside every definition
	// (imports, package-level constants, ...).
	TopLevel bool
}

// Changed computes the touched symbols of a file given its old and new
// contents (either may be nil for added/deleted files) and the changed
// line numbers on each side.
func Changed(path string, oldSrc, newSrc []byte, oldLines, newLines []int) (*FileChange, error) {
	if !Supported(path) {
		return nil, ErrUnsupported
	}
	fc := &FileChange{}
	seen := map[string]bool{}
	side := func(src []byte, lines []int) error {
		if src == nil || len(lines) == 0 {
			return nil
		}
		syms, err := Extract(path, src)
		if err != nil {
			return err
		}
		for _, l := range lines {
			s := innermost(syms, l)
			if s == nil {
				fc.TopLevel = true
				continue
			}
			if q := s.Qualified(); !seen[q] {
				seen[q] = true
				fc.Symbols = append(fc.Symbols, q)
			}
		}
		return nil
	}
	if err := side(oldSrc, oldLines); err != nil {
		return nil, err
	}
	if err := side(newSrc, newLines); err != nil {
		return nil, err
	}
	sort.Strings(fc.Symbols)
	return fc, nil
}

func innermost(syms []Symbol, line int) *Symbol {
	var best *Symbol
	for i := range syms {
		s := &syms[i]
		if line < s.StartLine || line > s.EndLine {
			continue
		}
		if best == nil || s.EndLine-s.StartLine < best.EndLine-best.StartLine {
			best = s
		}
	}
	return best
}

// Find returns symbols whose name or qualified name equals name.
func Find(syms []Symbol, name string) []Symbol {
	var out []Symbol
	for _, s := range syms {
		if s.Name == name || s.Qualified() == name {
			out = append(out, s)
		}
	}
	return out
}

// Excerpt returns the source of the given symbols with a little context,
// joined with elision markers.
func Excerpt(src []byte, syms []Symbol, context int) string {
	lines := strings.Split(string(src), "\n")
	sort.Slice(syms, func(i, j int) bool { return syms[i].StartLine < syms[j].StartLine })
	var b strings.Builder
	last := 0
	for _, s := range syms {
		from, to := s.StartLine-context, s.EndLine+context
		if from < 1 {
			from = 1
		}
		if to > len(lines) {
			to = len(lines)
		}
		if from <= last {
			from = last + 1
		}
		if from > to {
			continue
		}
		if from > last+1 {
			b.WriteString("// … (lines " + itoa(last+1) + "-" + itoa(from-1) + " omitted)\n")
		}
		b.WriteString(strings.Join(lines[from-1:to], "\n"))
		b.WriteString("\n")
		last = to
	}
	if last < len(lines) && last > 0 {
		b.WriteString("// … (remaining lines omitted)\n")
	}
	return b.String()
}

// Normalize maps an identifier to a language-neutral key so that
// RunAuction, run_auction, runAuction and run-auction compare equal.
func Normalize(name string) string {
	if i := strings.LastIndexAny(name, ".:"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
