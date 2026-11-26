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

func TestParseRemote(t *testing.T) {
	t.Setenv("GITHUB_SERVER_URL", "https://ghe.corp.example")
	t.Setenv("CI_SERVER_HOST", "")
	cases := []struct {
		url, host, path, kind, slug string
	}{
		{"https://github.com/org/repo.git", "github.com", "org/repo", HostGitHub, "org/repo"},
		{"git@github.com:org/repo.git", "github.com", "org/repo", HostGitHub, "org/repo"},
		{"https://x-access-token:tok@github.com/org/repo", "github.com", "org/repo", HostGitHub, "org/repo"},
		{"https://ghe.corp.example/team/svc", "ghe.corp.example", "team/svc", HostGitHub, "team/svc"},
		{"https://gitlab.com/group/sub/proj.git", "gitlab.com", "group/sub/proj", HostGitLab, "group/sub/proj"},
		{"ssh://git@gitlab.example.com:2222/group/proj.git", "gitlab.example.com", "group/proj", HostGitLab, "group/proj"},
		{"https://git.example.org/a/b", "git.example.org", "a/b", "", "a/b"},
	}
	for _, c := range cases {
		r, ok := ParseRemote(c.url)
		if !ok || r.Host != c.host || r.Path != c.path || r.Kind() != c.kind || r.Slug() != c.slug {
			t.Errorf("%s: %+v kind=%s slug=%s", c.url, r, r.Kind(), r.Slug())
		}
	}
	for _, local := range []string{"/srv/repos/up", "../up", "file:///srv/up"} {
		if _, ok := ParseRemote(local); ok {
			t.Errorf("%s should not parse as a hosted remote", local)
		}
	}
	if u := PRURL("https://gitlab.com/g/p", 9); u != "https://gitlab.com/g/p/-/merge_requests/9" {
		t.Errorf("gitlab PR URL %s", u)
	}
	if u := CommitURL("git@github.com:o/r.git", "abc"); u != "https://github.com/o/r/commit/abc" {
		t.Errorf("github commit URL %s", u)
	}
}
