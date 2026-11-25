// Package glob implements path matching with gitignore-like semantics:
//
//   - "**" matches any number of path segments (including zero)
//   - "*" and "?" match within a single segment
//   - a pattern without "/" matches the base name of a path at any depth
//   - a pattern ending in "/" matches everything below that directory
//   - a plain directory pattern ("src/auction") also matches files below it
package glob

import (
	"path"
	"strings"
)

// Match reports whether name matches pattern.
func Match(pattern, name string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	name = strings.TrimPrefix(name, "./")
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !strings.Contains(pattern, "/") {
		if ok, _ := path.Match(pattern, path.Base(name)); ok {
			return true
		}
		// "auction" also matches a top-level directory of that name.
		if !hasMeta(pattern) && strings.HasPrefix(name, pattern+"/") {
			return true
		}
		return false
	}
	ps := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	ns := strings.Split(name, "/")
	if matchSegs(ps, ns) {
		return true
	}
	// Directory prefix: "src/auction" matches "src/auction/engine.rs".
	if !hasMeta(pattern) && strings.HasPrefix(name, strings.TrimSuffix(pattern, "/")+"/") {
		return true
	}
	return false
}

// Any reports whether name matches any of the patterns.
func Any(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}

func hasMeta(p string) bool { return strings.ContainsAny(p, "*?[") }

func matchSegs(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(n); i++ {
				if matchSegs(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], n[0]); !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}
