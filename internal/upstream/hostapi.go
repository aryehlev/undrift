package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// PRMeta is pull/merge request metadata from the upstream host. Upstream
// PR text is untrusted and is only ever shown to agents inside clearly
// delimited untrusted-content blocks.
type PRMeta struct {
	Title  string
	Body   string
	URL    string
	Labels []string
}

// SupportsPRMeta reports whether metadata can be fetched for repo.
func SupportsPRMeta(repo string) bool {
	r, ok := ParseRemote(repo)
	return ok && r.Kind() != ""
}

// HasToken reports whether a token is configured for repo's host.
func HasToken(repo string) bool {
	r, _ := ParseRemote(repo)
	return token(r.Kind()) != ""
}

func token(kind string) string {
	names := []string{"UNDRIFT_UPSTREAM_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}
	if kind == HostGitLab {
		names = []string{"UNDRIFT_UPSTREAM_TOKEN", "GITLAB_TOKEN"}
	}
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// FetchPR retrieves PR (GitHub) or MR (GitLab) metadata.
func FetchPR(ctx context.Context, repo string, number int) (*PRMeta, error) {
	r, ok := ParseRemote(repo)
	if !ok {
		return nil, fmt.Errorf("not a hosted repository")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch r.Kind() {
	case HostGitHub:
		api := os.Getenv("GITHUB_API_URL")
		if api == "" {
			api = "https://api.github.com"
		}
		var raw struct {
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			Labels  []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := getJSON(ctx, fmt.Sprintf("%s/repos/%s/pulls/%d", strings.TrimSuffix(api, "/"), r.Slug(), number), "Authorization", bearer(token(HostGitHub)), &raw); err != nil {
			return nil, err
		}
		m := &PRMeta{Title: raw.Title, Body: raw.Body, URL: raw.HTMLURL}
		for _, l := range raw.Labels {
			m.Labels = append(m.Labels, l.Name)
		}
		return m, nil
	case HostGitLab:
		var raw struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			WebURL      string   `json:"web_url"`
			Labels      []string `json:"labels"`
		}
		u := fmt.Sprintf("%s/projects/%s/merge_requests/%d", r.GitLabAPI(), url.PathEscape(r.Path), number)
		if err := getJSON(ctx, u, "PRIVATE-TOKEN", token(HostGitLab), &raw); err != nil {
			return nil, err
		}
		return &PRMeta{Title: raw.Title, Body: raw.Description, URL: raw.WebURL, Labels: raw.Labels}, nil
	}
	return nil, fmt.Errorf("unsupported host %s", r.Host)
}

func bearer(tok string) string {
	if tok == "" {
		return ""
	}
	return "Bearer " + tok
}

func getJSON(ctx context.Context, u, authHeader, authValue string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "undrift")
	if authValue != "" {
		req.Header.Set(authHeader, authValue)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
