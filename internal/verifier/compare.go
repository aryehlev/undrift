package verifier

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aryehlev/undrift/internal/config"
	"github.com/aryehlev/undrift/internal/state"
)

// Normalize canonicalizes output for differential comparison: JSON is
// re-encoded with ignored keys removed (and arrays optionally sorted), then
// ignore patterns are masked and lines optionally sorted.
func Normalize(out string, n config.Normalize) string {
	s := strings.TrimSpace(out)
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		ignore := map[string]bool{}
		for _, k := range n.IgnoreKeys {
			ignore[k] = true
		}
		v = canon(v, ignore, n.SortArrays)
		b, _ := json.MarshalIndent(v, "", " ")
		s = string(b)
	}
	for _, p := range n.IgnorePattern {
		if re, err := regexp.Compile(p); err == nil {
			s = re.ReplaceAllString(s, "<ignored>")
		}
	}
	if n.SortLines {
		lines := strings.Split(s, "\n")
		sort.Strings(lines)
		s = strings.Join(lines, "\n")
	}
	return s
}

func canon(v any, ignore map[string]bool, sortArrays bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if ignore[k] {
				delete(t, k)
				continue
			}
			t[k] = canon(t[k], ignore, sortArrays)
		}
		return t
	case []any:
		for i := range t {
			t[i] = canon(t[i], ignore, sortArrays)
		}
		if sortArrays {
			sort.Slice(t, func(i, j int) bool {
				a, _ := json.Marshal(t[i])
				b, _ := json.Marshal(t[j])
				return string(a) < string(b)
			})
		}
		return t
	}
	return v
}

// DiffSummary describes the first differing line of two outputs.
func DiffSummary(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("first difference at line %d:\n  upstream:   %s\n  downstream: %s", i+1, x, y)
		}
	}
	return "outputs differ"
}

// ParseMetrics reads benchmark output: a JSON object of numbers (optionally
// nested under "metrics") or "name value" lines.
func ParseMetrics(out string) (map[string]float64, error) {
	m := map[string]float64{}
	s := strings.TrimSpace(out)
	var raw map[string]any
	if json.Unmarshal([]byte(s), &raw) == nil {
		if inner, ok := raw["metrics"].(map[string]any); ok {
			raw = inner
		}
		for k, v := range raw {
			if f, ok := v.(float64); ok {
				m[k] = f
			}
		}
	} else {
		sc := bufio.NewScanner(strings.NewReader(s))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 2 {
				continue
			}
			if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
				m[strings.Join(f[:len(f)-1], " ")] = v
			} else if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				m[f[0]] = v
			}
		}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("no metrics found")
	}
	return m, nil
}

// Compare computes per-metric change. Metrics are lower-is-better unless
// listed in higherIsBetter.
func Compare(base, cur map[string]float64, maxPct float64, higherIsBetter []string) ([]state.Metric, []string) {
	hib := map[string]bool{}
	for _, h := range higherIsBetter {
		hib[h] = true
	}
	var names []string
	for k := range cur {
		if _, ok := base[k]; ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []state.Metric
	var regressed []string
	for _, k := range names {
		b, c := base[k], cur[k]
		pct := 0.0
		if b != 0 {
			pct = (c - b) / math.Abs(b) * 100
		}
		worse := pct
		if hib[k] {
			worse = -pct
		}
		m := state.Metric{Name: k, Base: b, Current: c, ChangePercent: math.Round(pct*100) / 100, Regression: worse > maxPct}
		if m.Regression {
			regressed = append(regressed, fmt.Sprintf("%s %+.1f%%", k, pct))
		}
		out = append(out, m)
	}
	return out, regressed
}
