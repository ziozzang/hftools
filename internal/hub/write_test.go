package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateDeleteRepoAndTag(t *testing.T) {
	var got struct {
		createBody   map[string]any
		deleteBody   map[string]any
		tagBody      map[string]any
		tagRev       string
		deleteTagURL string
		methods      []string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got.methods = append(got.methods, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/api/repos/create":
			_ = json.NewDecoder(r.Body).Decode(&got.createBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://hf.co/owner/repo"})
		case r.URL.Path == "/api/repos/delete":
			_ = json.NewDecoder(r.Body).Decode(&got.deleteBody)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/models/owner/repo/tag/main" && r.Method == http.MethodPost:
			got.tagRev = "main"
			_ = json.NewDecoder(r.Body).Decode(&got.tagBody)
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/api/models/owner/repo/tag/") && r.Method == http.MethodDelete:
			got.deleteTagURL = r.URL.Path
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	ctx := context.Background()

	if err := c.CreateRepo(ctx, RepoTypeDataset, "owner/repo", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.createBody["name"] != "repo" || got.createBody["organization"] != "owner" ||
		got.createBody["type"] != "dataset" || got.createBody["private"] != true {
		t.Fatalf("create body = %+v", got.createBody)
	}

	if err := c.CreateTag(ctx, RepoTypeModel, "owner/repo", "v2.0", "main", "release"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if got.tagBody["tag"] != "v2.0" || got.tagBody["message"] != "release" {
		t.Fatalf("tag body = %+v", got.tagBody)
	}

	if err := c.DeleteTag(ctx, RepoTypeModel, "owner/repo", "v2.0"); err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	if got.deleteTagURL != "/api/models/owner/repo/tag/v2.0" {
		t.Fatalf("delete tag URL = %q", got.deleteTagURL)
	}

	if err := c.DeleteRepo(ctx, RepoTypeModel, "owner/repo"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got.deleteBody["name"] != "repo" || got.deleteBody["type"] != "model" {
		t.Fatalf("delete body = %+v", got.deleteBody)
	}
}

func TestUploadFlow(t *testing.T) {
	dir := t.TempDir()
	smallPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(smallPath, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(dir, "weights.bin")
	bigContent := strings.Repeat("A", 4096)
	if err := os.WriteFile(bigPath, []byte(bigContent), 0o644); err != nil {
		t.Fatal(err)
	}

	var lfsUploaded []byte
	var commitLines []map[string]any
	var lfsBatchSeen bool

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/owner/repo/preupload/main":
			var req struct {
				Files []struct {
					Path string `json:"path"`
					Size int64  `json:"size"`
				} `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var out struct {
				Files []map[string]any `json:"files"`
			}
			for _, f := range req.Files {
				mode := "regular"
				if strings.HasSuffix(f.Path, ".bin") {
					mode = "lfs"
				}
				out.Files = append(out.Files, map[string]any{"path": f.Path, "uploadMode": mode})
			}
			_ = json.NewEncoder(w).Encode(out)

		case strings.HasSuffix(r.URL.Path, ".git/info/lfs/objects/batch"):
			lfsBatchSeen = true
			var req struct {
				Objects []struct {
					OID  string `json:"oid"`
					Size int64  `json:"size"`
				} `json:"objects"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var objs []map[string]any
			for _, o := range req.Objects {
				objs = append(objs, map[string]any{
					"oid":  o.OID,
					"size": o.Size,
					"actions": map[string]any{
						"upload": map[string]any{"href": server.URL + "/lfs-put/" + o.OID},
					},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": objs})

		case strings.HasPrefix(r.URL.Path, "/lfs-put/"):
			lfsUploaded, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/api/models/owner/repo/commit/main":
			sc := bufio.NewScanner(r.Body)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Errorf("commit line not JSON: %q", line)
					continue
				}
				commitLines = append(commitLines, m)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"commitUrl": "https://hf.co/owner/repo/commit/deadbeef"})

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := testClient(server)
	files := []UploadFile{
		{LocalPath: smallPath, PathInRepo: "config.json"},
		{LocalPath: bigPath, PathInRepo: "weights.bin"},
	}
	res, err := c.Upload(context.Background(), RepoTypeModel, "owner/repo", "main", files, "msg", "desc")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res == nil || res.CommitURL == "" {
		t.Fatalf("commit result = %+v", res)
	}
	if !lfsBatchSeen {
		t.Fatalf("LFS batch was not requested")
	}
	if string(lfsUploaded) != bigContent {
		t.Fatalf("LFS upload body mismatch: %d bytes", len(lfsUploaded))
	}

	var haveHeader, haveFile, haveLFS bool
	for _, m := range commitLines {
		switch m["key"] {
		case "header":
			haveHeader = true
			v := m["value"].(map[string]any)
			if v["summary"] != "msg" || v["description"] != "desc" {
				t.Errorf("header value = %+v", v)
			}
		case "file":
			haveFile = true
			v := m["value"].(map[string]any)
			if v["path"] != "config.json" || v["encoding"] != "base64" {
				t.Errorf("file value = %+v", v)
			}
		case "lfsFile":
			haveLFS = true
			v := m["value"].(map[string]any)
			if v["path"] != "weights.bin" || v["algo"] != "sha256" {
				t.Errorf("lfsFile value = %+v", v)
			}
		}
	}
	if !haveHeader || !haveFile || !haveLFS {
		t.Fatalf("commit lines missing: header=%v file=%v lfs=%v", haveHeader, haveFile, haveLFS)
	}
}
