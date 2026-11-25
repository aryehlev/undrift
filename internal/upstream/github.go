package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// PRMeta is pull request metadata fetched from GitHub.
type PRMeta struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	URL    string   `json:"html_url"`
	Labels []string `json:"-"`
}

// FetchPR retrieves pull request metadata from the GitHub API. Upstream PR
// text is untrusted and is only ever shown to agents inside clearly
// delimited untrusted-content blocks.
func FetchPR(ctx context.Context, repo string, number int) (*PRMeta, error) {
	slug := GitHubSlug(repo)
	if slug == "" {
		return nil, fmt.Errorf("not a GitHub repository")
	}
	api := os.Getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/repos/%s/pulls/%d", strings.TrimSuffix(api, "/"), slug, number), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "undrift")
	if tok := firstEnv("UNDRIFT_UPSTREAM_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github: %s", resp.Status)
	}
	var raw struct {
		PRMeta
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	m := raw.PRMeta
	for _, l := range raw.Labels {
		m.Labels = append(m.Labels, l.Name)
	}
	return &m, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}
