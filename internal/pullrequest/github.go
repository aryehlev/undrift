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

// Request describes a PR to create or update.
type Request struct {
	Title  string
	Body   string
	Head   string
	Base   string
	Draft  bool
	Labels []string
}

// Provider is a code-hosting backend.
type Provider interface {
	Name() string
	// FindByHead returns the most recent PR (any state) for a head branch.
	FindByHead(ctx context.Context, head string) (*state.PRRef, error)
	Create(ctx context.Context, r Request) (*state.PRRef, error)
	Update(ctx context.Context, number int, r Request) (*state.PRRef, error)
	Get(ctx context.Context, number int) (*state.PRRef, error)
	Merge(ctx context.Context, number int) error
}

// GitHub implements Provider with the REST API.
type GitHub struct {
	Repo   string // owner/name
	Token  string
	APIURL string
	Client *http.Client
}

// NewGitHub returns a GitHub provider using GITHUB_TOKEN or GH_TOKEN.
func NewGitHub(repo string) (*GitHub, error) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		tok = os.Getenv("GH_TOKEN")
	}
	if tok == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN (or GH_TOKEN) is required to manage pull requests")
	}
	if repo == "" {
		return nil, fmt.Errorf("cannot determine downstream GitHub repository; set downstream.repository")
	}
	api := os.Getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	return &GitHub{Repo: repo, Token: tok, APIURL: strings.TrimSuffix(api, "/"), Client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Name implements Provider.
func (g *GitHub) Name() string { return "github" }

type ghPR struct {
	Number   int    `json:"number"`
	HTMLURL  string `json:"html_url"`
	State    string `json:"state"`
	Draft    bool   `json:"draft"`
	Merged   bool   `json:"merged"`
	MergedAt string `json:"merged_at"`
	Body     string `json:"body"`
}

func (p ghPR) ref() *state.PRRef {
	s := p.State
	if p.Merged || p.MergedAt != "" {
		s = "merged"
	}
	return &state.PRRef{Number: p.Number, URL: p.HTMLURL, State: s, Draft: p.Draft}
}

func (g *GitHub) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.APIURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("User-Agent", "undrift")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// FindByHead implements Provider.
func (g *GitHub) FindByHead(ctx context.Context, head string) (*state.PRRef, error) {
	owner := strings.SplitN(g.Repo, "/", 2)[0]
	var prs []ghPR
	q := url.Values{"head": {owner + ":" + head}, "state": {"all"}, "per_page": {"10"}}
	if err := g.do(ctx, "GET", "/repos/"+g.Repo+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return nil, err
	}
	for _, p := range prs {
		if p.State == "open" {
			return p.ref(), nil
		}
	}
	if len(prs) > 0 {
		return prs[0].ref(), nil
	}
	return nil, nil
}

// Create implements Provider.
func (g *GitHub) Create(ctx context.Context, r Request) (*state.PRRef, error) {
	var p ghPR
	err := g.do(ctx, "POST", "/repos/"+g.Repo+"/pulls", map[string]any{
		"title": r.Title, "body": r.Body, "head": r.Head, "base": r.Base, "draft": r.Draft,
		"maintainer_can_modify": true,
	}, &p)
	if err != nil {
		return nil, err
	}
	if len(r.Labels) > 0 {
		_ = g.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/labels", g.Repo, p.Number), map[string]any{"labels": r.Labels}, nil)
	}
	return p.ref(), nil
}

// Update implements Provider.
func (g *GitHub) Update(ctx context.Context, number int, r Request) (*state.PRRef, error) {
	var p ghPR
	err := g.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/pulls/%d", g.Repo, number), map[string]any{
		"title": r.Title, "body": r.Body, "base": r.Base,
	}, &p)
	if err != nil {
		return nil, err
	}
	return p.ref(), nil
}

// Get implements Provider.
func (g *GitHub) Get(ctx context.Context, number int) (*state.PRRef, error) {
	var p ghPR
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", g.Repo, number), nil, &p); err != nil {
		return nil, err
	}
	return p.ref(), nil
}

// Merge implements Provider (squash merge).
func (g *GitHub) Merge(ctx context.Context, number int) error {
	return g.do(ctx, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", g.Repo, number), map[string]any{"merge_method": "squash"}, nil)
}
