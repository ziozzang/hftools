package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestServeMirrorRoundTrip downloads a repository from an origin, serves the
// local copy over the HF URL scheme, and downloads it again through the mirror,
// exercising both the metadata API and ranged resolve endpoints offline.
func TestServeMirrorRoundTrip(t *testing.T) {
	cfg := []byte("{\"n_layers\": 32}\n")
	model := make([]byte, 160<<10)
	for i := range model {
		model[i] = byte((i*23 + 7) % 251)
	}
	const commit = "6666666666666666666666666666666666666666"
	files := map[string][]byte{"config.json": cfg, "model.safetensors": model}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/owner/servemodel/revision/main" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "owner/servemodel", "sha": commit,
				"siblings": []map[string]any{
					{"rfilename": "config.json", "blobId": gitBlobID(cfg), "size": len(cfg)},
					{"rfilename": "model.safetensors", "size": len(model),
						"lfs": map[string]any{"sha256": sha256Hex(model), "size": len(model)}},
				},
			})
			return
		}
		prefix := "/owner/servemodel/resolve/" + commit + "/"
		if len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
			http.NotFound(w, r)
			return
		}
		content, ok := files[r.URL.Path[len(prefix):]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(content) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer origin.Close()

	base := t.TempDir()
	root := filepath.Join(base, "repos")
	if err := run(context.Background(), []string{
		"download", "--endpoint", origin.URL, "--output", filepath.Join(root, "owner_servemodel"),
		"--parts", "2", "--multipart-threshold", "1B", "owner/servemodel",
	}); err != nil {
		t.Fatalf("origin download: %v", err)
	}

	// Stand up the mirror over the local copy.
	m, err := newMirror(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.repos) != 1 {
		t.Fatalf("mirror indexed %d repos", len(m.repos))
	}
	mirror := httptest.NewServer(m.handler(""))
	defer mirror.Close()

	// Download again, this time entirely from the mirror.
	dst := filepath.Join(base, "from_mirror")
	if err := run(context.Background(), []string{
		"download", "--endpoint", mirror.URL, "--output", dst,
		"--parts", "3", "--multipart-threshold", "1B", "owner/servemodel",
	}); err != nil {
		t.Fatalf("mirror download: %v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != string(want) {
			t.Fatalf("mirror-served %s differs: %v", name, err)
		}
	}
}
