package pullrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aryehlev/undrift/internal/state"
)

// GitLab implements Provider with the GitLab REST API (merge requests).
// It works with gitlab.com and self-managed instances.
type GitLab struct {
	Project string // full path, e.g. group/subgroup/project
	Token   string
	APIURL  string // e.g. https://gitlab.com/api/v4
	Client  *http.Client
}

// NewGitLab returns a GitLab provider using GITLAB_TOKEN (or GL_TOKEN).
// The token needs the `api` scope; CI_JOB_TOKEN cannot create merge
// requests.
func NewGitLab(project, apiURL string) (*GitLab, error) {
	tok := os.Getenv("GITLAB_TOKEN")
	if tok == "" {
		tok = os.Getenv("GL_TOKEN")
	}
	if tok == "" {
		return nil, fmt.Errorf("GITLAB_TOKEN (or GL_TOKEN) with api scope is required to manage merge requests")
	}
	if project == "" || apiURL == "" {
		return nil, fmt.Errorf("cannot determine downstream GitLab project; set downstream.repository")
	}
	return &GitLab{Project: project, Token: tok, APIURL: strings.TrimSuffix(apiURL, "/"), Client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Name implements Provider.
func (g *GitLab) Name() string { return "gitlab" }

type glMR struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
	State  string `json:"state"` // opened | closed | locked | merged
	Draft  bool   `json:"draft"`
	WIP    bool   `json:"work_in_progress"`
}

func (m glMR) ref() *state.PRRef {
	s := m.State
	switch s {
	case "opened":
		s = "open"
	case "locked":
		s = "closed"
	}
	return &state.PRRef{Number: m.IID, URL: m.WebURL, State: s, Draft: m.Draft || m.WIP}
}

func (g *GitLab) path(format string, a ...any) string {
	return "/projects/" + url.PathEscape(g.Project) + fmt.Sprintf(format, a...)
}

func (g *GitLab) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.APIURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "undrift")
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func draftTitle(r Request) string {
	if r.Draft && !strings.HasPrefix(r.Title, "Draft:") {
		return "Draft: " + r.Title
	}
	return r.Title
}

// FindByHead implements Provider.
func (g *GitLab) FindByHead(ctx context.Context, head string) (*state.PRRef, error) {
	var mrs []glMR
	q := url.Values{"source_branch": {head}, "state": {"all"}, "order_by": {"created_at"}, "sort": {"desc"}, "per_page": {"10"}}
	if err := g.do(ctx, "GET", g.path("/merge_requests?%s", q.Encode()), nil, &mrs); err != nil {
		return nil, err
	}
	for _, m := range mrs {
		if m.State == "opened" {
			return m.ref(), nil
		}
	}
	if len(mrs) > 0 {
		return mrs[0].ref(), nil
	}
	return nil, nil
}

// Create implements Provider.
func (g *GitLab) Create(ctx context.Context, r Request) (*state.PRRef, error) {
	var m glMR
	body := map[string]any{
		"source_branch": r.Head, "target_branch": r.Base, "title": draftTitle(r),
		"description": r.Body, "remove_source_branch": true, "squash": true,
	}
	if len(r.Labels) > 0 {
		body["labels"] = strings.Join(r.Labels, ",")
	}
	if err := g.do(ctx, "POST", g.path("/merge_requests"), body, &m); err != nil {
		return nil, err
	}
	return m.ref(), nil
}

// Update implements Provider.
func (g *GitLab) Update(ctx context.Context, number int, r Request) (*state.PRRef, error) {
	var m glMR
	err := g.do(ctx, "PUT", g.path("/merge_requests/%d", number), map[string]any{
		"title": draftTitle(r), "description": r.Body, "target_branch": r.Base,
	}, &m)
	if err != nil {
		return nil, err
	}
	return m.ref(), nil
}

// Get implements Provider.
func (g *GitLab) Get(ctx context.Context, number int) (*state.PRRef, error) {
	var m glMR
	if err := g.do(ctx, "GET", g.path("/merge_requests/%d", number), nil, &m); err != nil {
		return nil, err
	}
	return m.ref(), nil
}

// Merge implements Provider (squash merge).
func (g *GitLab) Merge(ctx context.Context, number int) error {
	return g.do(ctx, "PUT", g.path("/merge_requests/%d/merge", number), map[string]any{
		"squash": true, "should_remove_source_branch": true,
	}, nil)
}
