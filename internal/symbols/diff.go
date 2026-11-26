package symbols

import (
	"strconv"
	"strings"
)

// Lines holds changed line numbers on both sides of a file diff.
type Lines struct {
	Old []int
	New []int
}

// ParseUnifiedDiff extracts changed line numbers per file from a git diff
// produced with -U0. Files are keyed by both their old and new paths.
// Pure insertions mark the adjacent old line and pure deletions the
// adjacent new line, so that changes inside a definition are attributed to
// it on both sides.
func ParseUnifiedDiff(diff string) map[string]*Lines {
	out := map[string]*Lines{}
	var cur *Lines
	var oldPath, newPath string
	flush := func() {
		if cur == nil {
			return
		}
		for _, p := range []string{oldPath, newPath} {
			if p != "" {
				out[p] = cur
			}
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur, oldPath, newPath = &Lines{}, "", ""
		case strings.HasPrefix(line, "--- "):
			oldPath = diffPath(line[4:])
		case strings.HasPrefix(line, "+++ "):
			newPath = diffPath(line[4:])
		case strings.HasPrefix(line, "@@ ") && cur != nil:
			os, oc, ns, nc, ok := parseHunk(line)
			if !ok {
				continue
			}
			if oc > 0 {
				for i := 0; i < oc; i++ {
					cur.Old = append(cur.Old, os+i)
				}
			} else if os > 0 {
				cur.Old = append(cur.Old, os)
			}
			if nc > 0 {
				for i := 0; i < nc; i++ {
					cur.New = append(cur.New, ns+i)
				}
			} else if ns > 0 {
				cur.New = append(cur.New, ns)
			}
		}
	}
	flush()
	return out
}

func diffPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

// parseHunk parses "@@ -a,b +c,d @@".
func parseHunk(h string) (os, oc, ns, nc int, ok bool) {
	parts := strings.Fields(h)
	if len(parts) < 3 {
		return
	}
	var err1, err2 error
	os, oc, err1 = rng(strings.TrimPrefix(parts[1], "-"))
	ns, nc, err2 = rng(strings.TrimPrefix(parts[2], "+"))
	return os, oc, ns, nc, err1 == nil && err2 == nil
}

func rng(s string) (int, int, error) {
	start, count, found := strings.Cut(s, ",")
	a, err := strconv.Atoi(start)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return a, 1, nil
	}
	b, err := strconv.Atoi(count)
	return a, b, err
}
