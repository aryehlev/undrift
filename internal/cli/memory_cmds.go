package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/aryehlev/undrift/internal/mapping"
	"github.com/aryehlev/undrift/internal/memory"
	"github.com/aryehlev/undrift/internal/state"
)

func cmdMappings(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("mappings", flag.ContinueOnError)
	var up, down multi
	fs.Var(&up, "upstream", "upstream path/glob or path#Symbol (repeatable)")
	fs.Var(&down, "downstream", "downstream path/glob or path#symbol (repeatable)")
	kind := fs.String("kind", memory.KindModule, "module | file | function | api | behavior")
	rel := fs.String("relationship", "", "what both sides do")
	status := fs.String("status", memory.MappingMapped, "mapped | missing | deviation | unsupported")
	save := fs.Bool("save", false, "with suggest: store the suggestions")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	sub := "list"
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "list":
		mem, err := e.Memory()
		if err != nil {
			return nil, err
		}
		text := tw(func(w *tabwriter.Writer) {
			fmt.Fprintln(w, "ID\tKIND\tSTATUS\tUPSTREAM\tDOWNSTREAM\tSOURCE")
			for _, m := range mem.Mappings {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Kind, m.Status, strings.Join(m.Upstream, ","), strings.Join(m.Downstream, ","), m.Source)
			}
		})
		if len(mem.Mappings) == 0 {
			text = "no mappings yet (add with `undrift mappings add` or let agents propose them)\n"
		}
		return &output{data: mem.Mappings, text: text}, nil
	case "add":
		if len(up) == 0 {
			return nil, usagef("mappings add --upstream PATH [--downstream PATH] [--relationship TEXT] [--kind K] [--status S]")
		}
		m := memory.Mapping{Kind: *kind, Upstream: memory.StringList(up), Downstream: memory.StringList(down), Relationship: *rel, Status: *status, Source: "human", Confidence: 1}
		if err := e.UpdateMemory(func(mem *memory.Memory) error { mem.UpsertMapping(m); return nil }); err != nil {
			return nil, err
		}
		return &output{data: m, text: "mapping saved"}, nil
	case "remove":
		if len(pos) != 2 {
			return nil, usagef("mappings remove ID")
		}
		var ok bool
		if err := e.UpdateMemory(func(mem *memory.Memory) error { ok = mem.RemoveMapping(pos[1]); return nil }); err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("mapping %s not found", pos[1])
		}
		return &output{text: "removed " + pos[1]}, nil
	case "suggest":
		mem, err := e.Memory()
		if err != nil {
			return nil, err
		}
		cs, err := e.Store.Changes()
		if err != nil {
			return nil, err
		}
		var pending []*state.Change
		for _, c := range cs {
			if !c.Status.Settled() {
				pending = append(pending, c)
			}
		}
		res := (&mapping.Engine{Mem: mem, Root: e.Cfg.Root, Exclude: []string{".undrift/**"}}).Resolve(mapping.InputFor(pending...))
		if *save && len(res.Suggestions) > 0 {
			if err := e.UpdateMemory(func(m *memory.Memory) error {
				for _, s := range res.Suggestions {
					m.UpsertMapping(s)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
		text := tw(func(w *tabwriter.Writer) {
			for _, s := range res.Suggestions {
				fmt.Fprintf(w, "%s\t→ %s\t(%.1f)\n", strings.Join(s.Upstream, ","), strings.Join(s.Downstream, ","), s.Confidence)
			}
			for _, u := range res.Unmapped {
				found := false
				for _, s := range res.Suggestions {
					if len(s.Upstream) > 0 && strings.SplitN(s.Upstream[0], "#", 2)[0] == u {
						found = true
					}
				}
				if !found {
					fmt.Fprintf(w, "%s\t→ ?\t\n", u)
				}
			}
		})
		if text == "" {
			text = "all pending upstream files are mapped\n"
		}
		return &output{data: res, text: text}, nil
	}
	return nil, usagef("unknown mappings subcommand %q", sub)
}

func cmdDeviations(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("deviations", flag.ContinueOnError)
	var up, down, preserve multi
	fs.Var(&up, "upstream", "upstream path/glob (repeatable)")
	fs.Var(&down, "downstream", "downstream path/glob (repeatable)")
	fs.Var(&preserve, "preserve", "property to preserve (repeatable)")
	reason := fs.String("reason", "", "why the implementation differs")
	syncBehavior := fs.Bool("sync-behavior", true, "keep synchronizing behavior")
	syncImpl := fs.Bool("sync-implementation", false, "synchronize implementation details too")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 && pos[0] == "add" {
		if len(up) == 0 || *reason == "" {
			return nil, usagef("deviations add --upstream PATH --downstream PATH --reason TEXT [--preserve X]... [--sync-behavior=false] [--sync-implementation]")
		}
		d := memory.Deviation{Upstream: memory.StringList(up), Downstream: memory.StringList(down), Reason: *reason, Preserve: preserve, SyncBehavior: *syncBehavior, SyncImplementation: *syncImpl}
		if err := e.UpdateMemory(func(m *memory.Memory) error { m.Deviations = append(m.Deviations, d); return nil }); err != nil {
			return nil, err
		}
		return &output{data: d, text: "deviation saved"}, nil
	}
	mem, err := e.Memory()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, d := range mem.Deviations {
		fmt.Fprintf(&b, "%s → %s\n  reason: %s\n  preserve: %s\n  sync_behavior=%v sync_implementation=%v\n", strings.Join(d.Upstream, ","), strings.Join(d.Downstream, ","), strings.TrimSpace(d.Reason), strings.Join(d.Preserve, "; "), d.SyncBehavior, d.SyncImplementation)
	}
	if b.Len() == 0 {
		b.WriteString("no deviations recorded\n")
	}
	return &output{data: mem.Deviations, text: b.String()}, nil
}

func cmdNotes(ctx context.Context, a *App, args []string) (*output, error) {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	var paths multi
	kind := fs.String("kind", memory.NoteConvention, "architecture | convention | unsupported | performance | verification | feedback")
	text := fs.String("text", "", "note text")
	fs.Var(&paths, "path", "restrict to paths/globs (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	e, err := a.Engine()
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 && pos[0] == "add" {
		if *text == "" {
			return nil, usagef("notes add --kind KIND --text TEXT [--path GLOB]...")
		}
		n := memory.Note{Kind: *kind, Text: *text, Paths: memory.StringList(paths), Source: "human"}
		if err := e.UpdateMemory(func(m *memory.Memory) error { m.AddNote(n); return nil }); err != nil {
			return nil, err
		}
		return &output{data: n, text: "note saved"}, nil
	}
	mem, err := e.Memory()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, n := range mem.Notes {
		fmt.Fprintf(&b, "[%s] %s", n.Kind, n.Text)
		if len(n.Paths) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(n.Paths, ", "))
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		b.WriteString("no notes recorded\n")
	}
	return &output{data: mem.Notes, text: b.String()}, nil
}
