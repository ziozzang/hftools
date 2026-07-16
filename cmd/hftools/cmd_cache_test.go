package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestCacheExportImportRoundTrip downloads a small repo (one regular file, one
// LFS file), exports it into the HF cache layout, imports it back into a fresh
// flat directory, and verifies the reconstructed repository passes a forced
// hash verification.
func TestCacheExportImportRoundTrip(t *testing.T) {
	cfg := []byte("{\"hidden_size\": 4096}\n")
	model := make([]byte, 128<<10)
	for i := range model {
		model[i] = byte((i*29 + 3) % 251)
	}
	const commit = "5555555555555555555555555555555555555555"
	files := map[string][]byte{"config.json": cfg, "model.safetensors": model}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/owner/cacherepo/revision/main" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "owner/cacherepo", "sha": commit,
				"siblings": []map[string]any{
					{"rfilename": "config.json", "blobId": gitBlobID(cfg), "size": len(cfg)},
					{"rfilename": "model.safetensors", "size": len(model),
						"lfs": map[string]any{"sha256": sha256Hex(model), "size": len(model)}},
				},
			})
			return
		}
		prefix := "/owner/cacherepo/resolve/" + commit + "/"
		name := ""
		if len(r.URL.Path) > len(prefix) && r.URL.Path[:len(prefix)] == prefix {
			name = r.URL.Path[len(prefix):]
		}
		content, ok := files[name]
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
	defer server.Close()

	base := t.TempDir()
	flat := filepath.Join(base, "owner_cacherepo")
	if err := run(context.Background(), []string{
		"download", "--endpoint", server.URL, "--output", flat,
		"--parts", "2", "--multipart-threshold", "1B", "owner/cacherepo",
	}); err != nil {
		t.Fatalf("download: %v", err)
	}

	cacheRoot := filepath.Join(base, "hf-cache")
	if err := run(context.Background(), []string{"cache-export", "--output", flat, "--cache", cacheRoot}); err != nil {
		t.Fatalf("cache-export: %v", err)
	}

	storage := filepath.Join(cacheRoot, "models--owner--cacherepo")
	if _, err := os.Stat(filepath.Join(storage, "blobs", gitBlobID(cfg))); err != nil {
		t.Fatalf("regular blob missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storage, "blobs", sha256Hex(model))); err != nil {
		t.Fatalf("lfs blob missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(storage, "snapshots", commit, "model.safetensors")); err != nil {
		t.Fatalf("snapshot pointer missing: %v", err)
	}
	if ref, err := os.ReadFile(filepath.Join(storage, "refs", "main")); err != nil || string(ref) != commit {
		t.Fatalf("refs/main = %q, %v", ref, err)
	}

	imported := filepath.Join(base, "imported")
	if err := run(context.Background(), []string{
		"cache-import", "--repo", "owner/cacherepo", "--cache", cacheRoot, "--output", imported,
	}); err != nil {
		t.Fatalf("cache-import: %v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(imported, filepath.FromSlash(name)))
		if err != nil || string(got) != string(want) {
			t.Fatalf("imported %s differs: %v", name, err)
		}
	}
	// The reconstructed manifest must pass a forced rehash verification.
	if err := run(context.Background(), []string{"verify", "--output", imported, "--force"}); err != nil {
		t.Fatalf("imported repository failed verification: %v", err)
	}

	// cache-list and cache-verify operate on the cache with no manifest.
	if err := run(context.Background(), []string{"cache-list", "--cache", cacheRoot}); err != nil {
		t.Fatalf("cache-list: %v", err)
	}
	if err := run(context.Background(), []string{"cache-verify", "--cache", cacheRoot}); err != nil {
		t.Fatalf("cache-verify: %v", err)
	}

	// cache-export --archive writes a tar bundle plus its checksum.
	archive := filepath.Join(base, "bundle.tar")
	if err := run(context.Background(), []string{"cache-export", "--output", flat, "--cache", cacheRoot, "--archive", archive}); err != nil {
		t.Fatalf("cache-export --archive: %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if _, err := os.Stat(archive + ".sha256"); err != nil {
		t.Fatalf("archive checksum missing: %v", err)
	}

	// cache-import-batch restores every repo in the cache at once.
	batch := filepath.Join(base, "batch")
	if err := run(context.Background(), []string{"cache-import-batch", "--cache", cacheRoot, "--output-root", batch}); err != nil {
		t.Fatalf("cache-import-batch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(batch, "owner_cacherepo", "config.json")); err != nil || string(got) != string(cfg) {
		t.Fatalf("batch-imported config.json differs: %v", err)
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
