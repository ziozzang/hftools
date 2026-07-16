package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(server *httptest.Server) *Client {
	c := New(server.URL, "tok", 5*time.Second)
	return c
}

func TestRepoTypeRouting(t *testing.T) {
	if RepoTypeSpace.Collection() != "spaces" {
		t.Fatalf("space collection = %q", RepoTypeSpace.Collection())
	}
	if RepoTypeDataset.Collection() != "datasets" || RepoTypeModel.Collection() != "models" {
		t.Fatalf("collection mapping wrong")
	}
	c := New("https://hf.co", "", 0)
	url := c.DownloadURL(RepoTypeSpace, "owner/space", "abc", "app.py")
	if !strings.Contains(url, "/spaces/owner/space/resolve/abc/app.py") {
		t.Fatalf("space download URL = %q", url)
	}
	if err := RepoTypeSpace.Validate(); err != nil {
		t.Fatalf("space should validate: %v", err)
	}
}

func TestRefsSearchWhoAmI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/whoami-v2":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "alice", "type": "user"})
		case r.URL.Path == "/api/models/owner/repo/refs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"branches": []map[string]string{{"name": "main", "ref": "refs/heads/main", "targetCommit": "abcdef1234567890"}},
				"tags":     []map[string]string{{"name": "v1.0", "ref": "refs/tags/v1.0", "targetCommit": "0011223344556677"}},
			})
		case r.URL.Path == "/api/models":
			if r.URL.Query().Get("search") != "llama" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "meta/llama", "downloads": 1000, "likes": 50, "pipeline_tag": "text-generation"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	ctx := context.Background()

	w, err := c.WhoAmI(ctx)
	if err != nil || w.Name != "alice" {
		t.Fatalf("whoami = %+v, %v", w, err)
	}

	refs, err := c.Refs(ctx, RepoTypeModel, "owner/repo")
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	if len(refs.Branches) != 1 || refs.Branches[0].Name != "main" || len(refs.Tags) != 1 {
		t.Fatalf("refs = %+v", refs)
	}

	res, err := c.Search(ctx, RepoTypeModel, SearchOptions{Query: "llama", Limit: 5, Sort: "downloads", Direction: -1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].ID != "meta/llama" || res[0].Downloads != 1000 {
		t.Fatalf("search = %+v", res)
	}
}

func TestWhoAmINoToken(t *testing.T) {
	c := New("https://hf.co", "", 0)
	if _, err := c.WhoAmI(context.Background()); err == nil {
		t.Fatalf("expected error without a token")
	}
}
