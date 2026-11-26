package upstream

import (
	"context"

	"github.com/aryehlev/undrift/internal/state"
	"github.com/aryehlev/undrift/internal/symbols"
)

const (
	maxSymbolFiles     = 60
	maxSymbolFileBytes = 1 << 20
)

// AnnotateSymbols records which definitions each changed file touches,
// comparing the file before and after the change. It is a no-op when symbol
// analysis is unavailable; files in unsupported languages stay unparsed.
func (w *Watcher) AnnotateSymbols(ctx context.Context, c *state.Change) error {
	if c.SymbolsAnalyzed || !symbols.Available() {
		return nil
	}
	parent := ""
	if len(c.Parents) > 0 {
		parent = c.Parents[0]
	}
	args := []string{"diff", "-U0", "--no-color", "-M", parent, c.SHA}
	if parent == "" {
		args = []string{"show", "-U0", "--no-color", "--format=", c.SHA}
	}
	diff, err := w.Mirror.Run(ctx, args...)
	if err != nil {
		return err
	}
	lines := symbols.ParseUnifiedDiff(diff)
	n := 0
	for i := range c.Files {
		f := &c.Files[i]
		if !symbols.Supported(f.Path) || n >= maxSymbolFiles {
			continue
		}
		l := lines[f.Path]
		if l == nil {
			continue
		}
		n++
		var oldSrc, newSrc []byte
		if f.Status != "A" && parent != "" {
			op := f.Path
			if f.OldPath != "" {
				op = f.OldPath
			}
			oldSrc, _ = w.FileAt(ctx, parent, op)
		}
		if f.Status != "D" {
			newSrc, _ = w.FileAt(ctx, c.SHA, f.Path)
		}
		if len(oldSrc) > maxSymbolFileBytes || len(newSrc) > maxSymbolFileBytes {
			continue
		}
		fc, err := symbols.Changed(f.Path, oldSrc, newSrc, l.Old, l.New)
		if err != nil {
			continue
		}
		f.Parsed, f.Symbols, f.TopLevel = true, fc.Symbols, fc.TopLevel
	}
	c.SymbolsAnalyzed = true
	return nil
}
