package upstream

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Host kinds.
const (
	HostGitHub = "github"
	HostGitLab = "gitlab"
)

// Remote is a parsed git remote URL.
type Remote struct {
	Host string // e.g. github.com, gitlab.example.com
	Path string // e.g. org/repo, group/subgroup/project (no .git)
}

// ParseRemote parses https, ssh and scp-style git URLs. It returns false
// for local paths and unrecognized forms.
func ParseRemote(raw string) (Remote, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Remote{}, false
	}
	var host, path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.Scheme == "file" {
			return Remote{}, false
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(s, "@") && strings.Contains(s, ":") && !strings.HasPrefix(s, "/"):
		// scp-like: git@host:group/project.git
		at := strings.Index(s, "@")
		rest := s[at+1:]
		h, p, ok := strings.Cut(rest, ":")
		if !ok {
			return Remote{}, false
		}
		host, path = h, p
	default:
		return Remote{}, false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" {
		return Remote{}, false
	}
	return Remote{Host: strings.ToLower(host), Path: path}, true
}

func envHost(name string) string {
	v := os.Getenv(name)
	if v == "" {
		return ""
	}
	if u, err := url.Parse(v); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(v)
}

// Kind classifies the hosting service: github.com and the GitHub Enterprise
// server of the current Actions run are GitHub; hosts named like GitLab and
// the GitLab instance of the current CI job are GitLab.
func (r Remote) Kind() string {
	switch {
	case r.Host == "github.com" || (r.Host != "" && r.Host == envHost("GITHUB_SERVER_URL")):
		return HostGitHub
	case strings.Contains(r.Host, "gitlab") || (r.Host != "" && r.Host == envHost("CI_SERVER_HOST")):
		return HostGitLab
	}
	return ""
}

// Slug returns "owner/repo" for GitHub and the full project path for GitLab.
func (r Remote) Slug() string {
	if r.Kind() == HostGitHub {
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	return r.Path
}

// GitLabAPI returns the REST API base URL for a GitLab host.
func (r Remote) GitLabAPI() string {
	if v := os.Getenv("GITLAB_API_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	if v := os.Getenv("CI_API_V4_URL"); v != "" && envHost("CI_SERVER_HOST") == r.Host {
		return strings.TrimSuffix(v, "/")
	}
	return "https://" + r.Host + "/api/v4"
}

// GitHubSlug extracts "owner/repo" from a GitHub URL, or "".
func GitHubSlug(raw string) string {
	r, ok := ParseRemote(raw)
	if !ok || r.Kind() != HostGitHub {
		return ""
	}
	return r.Slug()
}

// PRURL returns a browsable pull/merge request URL, or "".
func PRURL(repo string, n int) string {
	r, ok := ParseRemote(repo)
	if !ok {
		return ""
	}
	switch r.Kind() {
	case HostGitHub:
		return fmt.Sprintf("https://%s/%s/pull/%d", r.Host, r.Slug(), n)
	case HostGitLab:
		return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", r.Host, r.Path, n)
	}
	return ""
}

// CommitURL returns a browsable commit URL, or "".
func CommitURL(repo, sha string) string {
	r, ok := ParseRemote(repo)
	if !ok {
		return ""
	}
	switch r.Kind() {
	case HostGitHub:
		return fmt.Sprintf("https://%s/%s/commit/%s", r.Host, r.Slug(), sha)
	case HostGitLab:
		return fmt.Sprintf("https://%s/%s/-/commit/%s", r.Host, r.Path, sha)
	}
	return ""
}
