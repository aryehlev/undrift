package upstream

import "testing"

func TestParseTitle(t *testing.T) {
	cases := []struct {
		msg   string
		merge bool
		title string
		pr    int
	}{
		{"Merge pull request #1842 from org/fix-timeout\n\nFix auction timeout", true, "Fix auction timeout", 1842},
		{"Fix auction timeout (#1842)\n\nbody", false, "Fix auction timeout", 1842},
		{"plain commit", false, "plain commit", 0},
		{"Feature X\n\nSee merge request group/proj!77", true, "Feature X", 77},
	}
	for _, c := range cases {
		title, pr := ParseTitle(c.msg, c.merge)
		if title != c.title || pr != c.pr {
			t.Errorf("ParseTitle(%q) = %q, %d", c.msg, title, pr)
		}
	}
	if got := ParseRevert("Revert \"x\"\n\nThis reverts commit 0123456789abcdef0123456789abcdef01234567."); got != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("ParseRevert = %q", got)
	}
	if s := GitHubSlug("git@github.com:org/repo.git"); s != "org/repo" {
		t.Errorf("slug %q", s)
	}
}
