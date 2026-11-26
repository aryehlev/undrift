package pullrequest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHost records requests and serves canned JSON by "METHOD path".
type fakeHost struct {
	mu     sync.Mutex
	routes map[string]string
	got    map[string]map[string]any
	auth   []string
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	f.auth = append(f.auth, r.Header.Get("Authorization")+r.Header.Get("PRIVATE-TOKEN"))
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.got[key] = body
	if q := r.URL.RawQuery; q != "" {
		f.got[key+"?"] = map[string]any{"query": q}
	}
	resp, ok := f.routes[key]
	if !ok {
		http.Error(w, `{"message":"not found"}`, 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(resp))
}

func newFake(routes map[string]string) (*fakeHost, *httptest.Server) {
	f := &fakeHost{routes: routes, got: map[string]map[string]any{}}
	return f, httptest.NewServer(f)
}

func TestGitHubProvider(t *testing.T) {
	f, srv := newFake(map[string]string{
		"GET /repos/org/port/pulls":            `[]`,
		"POST /repos/org/port/pulls":           `{"number": 7, "html_url": "https://github.com/org/port/pull/7", "state": "open", "draft": true}`,
		"POST /repos/org/port/issues/7/labels": `[]`,
		"PATCH /repos/org/port/pulls/7":        `{"number": 7, "state": "open"}`,
		"GET /repos/org/port/pulls/7":          `{"number": 7, "state": "closed", "merged": true}`,
		"PUT /repos/org/port/pulls/7/merge":    `{"merged": true}`,
	})
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "ghs_test")
	t.Setenv("GITHUB_API_URL", srv.URL)
	g, err := NewGitHub("org/port")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if pr, err := g.FindByHead(ctx, "undrift/abc"); err != nil || pr != nil {
		t.Fatalf("find: %v %v", pr, err)
	}
	if !strings.Contains(f.got["GET /repos/org/port/pulls?"]["query"].(string), "head=org%3Aundrift%2Fabc") {
		t.Fatalf("head filter: %v", f.got["GET /repos/org/port/pulls?"])
	}
	pr, err := g.Create(ctx, Request{Title: "t", Body: "b", Head: "undrift/abc", Base: "main", Draft: true, Labels: []string{"undrift"}})
	if err != nil || pr.Number != 7 || !pr.Draft {
		t.Fatalf("create: %+v %v", pr, err)
	}
	if f.got["POST /repos/org/port/pulls"]["draft"] != true || f.got["POST /repos/org/port/pulls"]["base"] != "main" {
		t.Fatalf("create body: %v", f.got["POST /repos/org/port/pulls"])
	}
	if _, err := g.Update(ctx, 7, Request{Title: "t2", Body: "b2", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if pr, err := g.Get(ctx, 7); err != nil || pr.State != "merged" {
		t.Fatalf("get: %+v %v", pr, err)
	}
	if err := g.Merge(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if f.auth[0] != "Bearer ghs_test" {
		t.Fatalf("auth %q", f.auth[0])
	}
	if _, err := g.Get(ctx, 99); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 error: %v", err)
	}
}

func TestGitLabProvider(t *testing.T) {
	proj := "/projects/group%2Fsub%2Fport"
	f, srv := newFake(map[string]string{
		"GET " + proj + "/merge_requests":         `[{"iid": 3, "state": "merged", "web_url": "u3"}, {"iid": 4, "state": "opened", "web_url": "u4"}]`,
		"POST " + proj + "/merge_requests":        `{"iid": 5, "state": "opened", "web_url": "https://gitlab.example.com/group/sub/port/-/merge_requests/5", "draft": true}`,
		"PUT " + proj + "/merge_requests/5":       `{"iid": 5, "state": "opened", "draft": true}`,
		"GET " + proj + "/merge_requests/5":       `{"iid": 5, "state": "merged"}`,
		"GET " + proj + "/merge_requests/6":       `{"iid": 6, "state": "locked"}`,
		"PUT " + proj + "/merge_requests/5/merge": `{"state": "merged"}`,
	})
	defer srv.Close()
	t.Setenv("GITLAB_TOKEN", "glpat-test")
	g, err := NewGitLab("group/sub/port", srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pr, err := g.FindByHead(ctx, "undrift/abc")
	if err != nil || pr.Number != 4 || pr.State != "open" {
		t.Fatalf("find should prefer the open MR: %+v %v", pr, err)
	}
	pr, err = g.Create(ctx, Request{Title: "[Undrift] Port x", Body: "b", Head: "undrift/abc", Base: "main", Draft: true, Labels: []string{"undrift", "sync"}})
	if err != nil || pr.Number != 5 || !pr.Draft || pr.State != "open" {
		t.Fatalf("create: %+v %v", pr, err)
	}
	body := f.got["POST "+proj+"/merge_requests"]
	if body["title"] != "Draft: [Undrift] Port x" || body["source_branch"] != "undrift/abc" || body["labels"] != "undrift,sync" {
		t.Fatalf("create body: %v", body)
	}
	if _, err := g.Update(ctx, 5, Request{Title: "x", Body: "y", Base: "main", Draft: true}); err != nil {
		t.Fatal(err)
	}
	if f.got["PUT "+proj+"/merge_requests/5"]["title"] != "Draft: x" {
		t.Fatalf("update keeps draft: %v", f.got["PUT "+proj+"/merge_requests/5"])
	}
	if pr, _ := g.Get(ctx, 5); pr.State != "merged" {
		t.Fatalf("get: %+v", pr)
	}
	if pr, _ := g.Get(ctx, 6); pr.State != "closed" {
		t.Fatalf("locked maps to closed: %+v", pr)
	}
	if err := g.Merge(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if f.auth[0] != "glpat-test" {
		t.Fatalf("auth %q", f.auth[0])
	}
}
