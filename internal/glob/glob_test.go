package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/guide/intro.md", true},
		{"*.md", "main.go", false},
		{"docs/", "docs/a/b.txt", true},
		{"docs/**", "docs/a/b.txt", true},
		{"docs/**", "src/docs/x", false},
		{"**/testdata/**", "pkg/x/testdata/f.json", true},
		{"auction/auction.go", "auction/auction.go", true},
		{"auction/*.go", "auction/auction.go", true},
		{"auction/*.go", "auction/sub/auction.go", false},
		{"src/auction", "src/auction/engine.rs", true},
		{"auction", "auction/x.go", true},
		{"**/*_test.go", "a/b/c_test.go", true},
		{"**/*_test.go", "c_test.go", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
