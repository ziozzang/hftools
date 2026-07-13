package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBatchQueueAndCachedRerun(t *testing.T) {
	data := []byte("small model fixture\n")
	gitHash := sha1.New()
	_, _ = fmt.Fprintf(gitHash, "blob %d\x00", len(data))
	_, _ = gitHash.Write(data)
	blobID := hex.EncodeToString(gitHash.Sum(nil))
	var downloads atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/owner/model/revision/main":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "owner/model", "sha": "abcdef1234567890",
				"lastModified": "2026-07-01T12:34:56.000Z", "createdAt": "2025-01-02T03:04:05.000Z",
				"tags":     []string{"test", "fixture"},
				"siblings": []map[string]any{{"rfilename": "model.txt", "blobId": blobID, "size": len(data)}},
			})
		case "/owner/model/resolve/abcdef1234567890/model.txt":
			downloads.Add(1)
			var start, end int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(data) {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base := t.TempDir()
	queuePath := filepath.Join(base, "queue.json")
	queue := queueFile{OutputRoot: filepath.Join(base, "models"), Jobs: []queueJob{{Repo: "owner/model"}}}
	b, err := json.Marshal(queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queuePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"batch", "--queue", queuePath, "--endpoint", server.URL, "--parts", "4", "--multipart-threshold", "1B"}
	if err := run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	firstRequests := downloads.Load()
	if firstRequests != 4 {
		t.Fatalf("got %d range requests, want 4", firstRequests)
	}
	if err := run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if got := downloads.Load(); got != firstRequests {
		t.Fatalf("cached rerun made %d additional download requests", got-firstRequests)
	}
	modelPath := filepath.Join(base, "models", "owner_model", "model.txt")
	got, err := os.ReadFile(modelPath)
	if err != nil || string(got) != string(data) {
		t.Fatalf("downloaded file: %q, %v", got, err)
	}
	originalInfo, err := os.Stat(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(modelPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(modelPath, originalInfo.ModTime(), originalInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"verify", "--output", filepath.Dir(modelPath)}); err != nil {
		t.Fatalf("metadata-cached verify should not rehash: %v", err)
	}
	if err := run(context.Background(), []string{"verify", "--output", filepath.Dir(modelPath), "--force"}); err == nil {
		t.Fatal("forced verification did not detect corruption")
	}
	beforeRepair := downloads.Load()
	if err := run(context.Background(), args); err != nil {
		t.Fatalf("repair download: %v", err)
	}
	if downloads.Load() <= beforeRepair {
		t.Fatal("failed verification did not trigger a repair download")
	}
	if err := run(context.Background(), []string{"verify", "--output", filepath.Dir(modelPath), "--force"}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"verify-batch", "--root", filepath.Join(base, "models"), "--force"}); err != nil {
		t.Fatal(err)
	}
	checksums, err := os.ReadFile(filepath.Join(filepath.Dir(modelPath), ".sha256"))
	if err != nil || !strings.Contains(string(checksums), blobSHA256(data)+"  model.txt") {
		t.Fatalf("checksum file missing model hash: %q, %v", checksums, err)
	}
	metadata, err := os.ReadFile(filepath.Join(filepath.Dir(modelPath), ".metadata", "repository.json"))
	if err != nil || !strings.Contains(string(metadata), `"lastModified": "2026-07-01T12:34:56.000Z"`) || !strings.Contains(string(metadata), `"requested_revision": "main"`) {
		t.Fatalf("repository metadata not archived: %s, %v", metadata, err)
	}
	listPath := filepath.Join(base, "models.txt")
	if err := os.WriteFile(listPath, []byte("; comment\n\n owner/model \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listRoot := filepath.Join(base, "from-list")
	listArgs := []string{"batch", "--list", listPath, "--output-root", listRoot, "--endpoint", server.URL, "--parts", "4", "--multipart-threshold", "1B"}
	if err := run(context.Background(), listArgs); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(listRoot, "owner_model", "model.txt")); err != nil || string(got) != string(data) {
		t.Fatalf("plain-list download = %q, %v", got, err)
	}
}

func blobSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestParseBytes(t *testing.T) {
	for input, want := range map[string]int64{"1B": 1, "32KiB": 32 << 10, "2MiB": 2 << 20, "3GB": 3_000_000_000} {
		got, err := parseBytes(input)
		if err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	if _, err := parseBytes("-1MiB"); err == nil {
		t.Fatal("negative byte size accepted")
	}
}

func TestParsePlainModelList(t *testing.T) {
	q, err := parseQueueData([]byte("\ufeff; comment\n\n  FluidInference/silero-vad-coreml  \n ; another comment\nhttps://huggingface.co/openai-community/gpt2\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"FluidInference/silero-vad-coreml", "openai-community/gpt2"}
	if len(q.Jobs) != len(want) {
		t.Fatalf("jobs = %#v", q.Jobs)
	}
	for i := range want {
		if q.Jobs[i].Repo != want[i] {
			t.Errorf("job %d = %q, want %q", i, q.Jobs[i].Repo, want[i])
		}
	}
	if _, err := parseQueueData([]byte("owner/model/extra\n")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("invalid list line error = %v", err)
	}
}

func TestLegacyMetadataAndPartialMigration(t *testing.T) {
	root := t.TempDir()
	legacyPartials := filepath.Join(root, ".hfdown", "partials")
	if err := os.MkdirAll(legacyPartials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyPartials, "abc.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyPartials, "abc.data"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if stateDir != filepath.Join(root, ".metadata") {
		t.Fatalf("state directory = %q", stateDir)
	}
	for _, name := range []string{"state.json", "download.part"} {
		if _, err := os.Stat(filepath.Join(root, "tmp", "abc", name)); err != nil {
			t.Fatalf("migrated %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".hfdown")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy hidden directory still exists: %v", err)
	}
}

func TestRepoUpdateDownloadsOnlyChangedFile(t *testing.T) {
	versions := []map[string][]byte{
		{"a.bin": []byte("AAAA1111"), "b.bin": []byte("BBBB1111")},
		{"a.bin": []byte("AAAA1111"), "b.bin": []byte("BBBB2222")},
	}
	commits := []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"}
	var current atomic.Int64
	var downloads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := int(current.Load())
		if r.URL.Path == "/api/models/owner/updatable/revision/main" {
			files := make([]map[string]any, 0, 2)
			for _, name := range []string{"a.bin", "b.bin"} {
				content := versions[version][name]
				files = append(files, map[string]any{"rfilename": name, "blobId": gitBlobID(content), "size": len(content)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "owner/updatable", "sha": commits[version], "siblings": files})
			return
		}
		prefix := "/owner/updatable/resolve/" + commits[version] + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		content, ok := versions[version][strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer server.Close()

	output := filepath.Join(t.TempDir(), "owner_updatable")
	cfg := defaults()
	cfg.Endpoint, cfg.Output, cfg.Parts, cfg.MultipartThreshold = server.URL, output, 4, 1
	if err := syncRepo(context.Background(), cfg, "owner/updatable"); err != nil {
		t.Fatal(err)
	}
	initialRequests := downloads.Load()
	if initialRequests != 8 {
		t.Fatalf("initial requests = %d, want 8", initialRequests)
	}
	current.Store(1)
	if err := syncRepo(context.Background(), cfg, "owner/updatable"); err != nil {
		t.Fatal(err)
	}
	if added := downloads.Load() - initialRequests; added != 4 {
		t.Fatalf("update downloaded %d ranges, want only 4 for the changed file", added)
	}
	for name, want := range versions[1] {
		got, err := os.ReadFile(filepath.Join(output, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	history, err := os.ReadFile(filepath.Join(output, ".metadata", "repository-history.jsonl"))
	if err != nil || strings.Count(strings.TrimSpace(string(history)), "\n") != 1 {
		t.Fatalf("metadata history should contain two events: %s, %v", history, err)
	}
}

func gitBlobID(data []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
