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

	"github.com/ziozzang/hftools/internal/hub"
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
	sha1Checksums, err := os.ReadFile(filepath.Join(filepath.Dir(modelPath), ".sha1sum"))
	if err != nil || !strings.Contains(string(sha1Checksums), rawSHA1(data)+"  model.txt") {
		t.Fatalf("SHA-1 checksum file missing model hash: %q, %v", sha1Checksums, err)
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

func rawSHA1(data []byte) string {
	sum := sha1.Sum(data)
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

func TestHelpAndVersionAliases(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{{"help"}, {"help", "download"}, {"help", "ds"}, {"version"}, {"--version"}, {"-v"}, {"-V"}} {
		if err := run(ctx, args); err != nil {
			t.Errorf("run(%q): %v", args, err)
		}
	}
	if err := run(ctx, []string{"help", "unknown"}); err == nil {
		t.Fatal("unknown help topic accepted")
	}
	if err := run(ctx, []string{"version", "extra"}); err == nil {
		t.Fatal("extra version argument accepted")
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

func TestDatasetAliasTagAndCaseInsensitiveFilters(t *testing.T) {
	files := map[string][]byte{
		"nested/ONE.JSON":         []byte("json-data"),
		"tables/TWO.PARQUET":      []byte("parquet-data"),
		"weights/MODEL_Q4_0.GGUF": []byte("gguf-data"),
		"skip.txt":                []byte("not-selected"),
	}
	const commit = "3333333333333333333333333333333333333333"
	var metadataRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/datasets/owner/data/revision/v1.2.0" {
			metadataRequests.Add(1)
			siblings := make([]map[string]any, 0, len(files))
			for name, content := range files {
				siblings = append(siblings, map[string]any{"rfilename": name, "blobId": gitBlobID(content), "size": len(content)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "owner/data", "sha": commit, "siblings": siblings})
			return
		}
		prefix := "/datasets/owner/data/resolve/" + commit + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		content, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
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

	output := filepath.Join(t.TempDir(), "dataset_owner_data")
	err := run(context.Background(), []string{
		"ds", "--endpoint", server.URL, "--output", output, "--tag", "v1.2.0",
		"--parts", "1", "--filter", "*.json|*.parquet|*_q4_?.gguf", "owner/data",
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadataRequests.Load() != 1 {
		t.Fatalf("dataset metadata requests = %d", metadataRequests.Load())
	}
	for name, want := range files {
		got, readErr := os.ReadFile(filepath.Join(output, filepath.FromSlash(name)))
		if name == "skip.txt" {
			if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("unmatched file was downloaded: %v", readErr)
			}
			continue
		}
		if readErr != nil || string(got) != string(want) {
			t.Fatalf("dataset file %s = %q, %v", name, got, readErr)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(output, ".metadata", "manifest.json"))
	if err != nil || !strings.Contains(string(manifest), `"repo_type": "dataset"`) {
		t.Fatalf("dataset manifest: %s, %v", manifest, err)
	}
}

func TestFilterRepoFilesRepeatedAndInvalid(t *testing.T) {
	files := []hub.RepoFile{
		{Path: "config.JSON"},
		{Path: "nested/data.PARQUET"},
		{Path: "weights/model_Q8_0.GGUF"},
		{Path: "notes.txt"},
	}
	got, err := filterRepoFiles(files, []string{"*.json|*.parquet", "weights/*_q8_?.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("selected files = %#v", got)
	}
	if _, err := filterRepoFiles(files, []string{"*.safetensors"}); err == nil {
		t.Fatal("zero-match filter did not fail")
	}
	if _, err := filterRepoFiles(files, []string{"[broken"}); err == nil {
		t.Fatal("invalid glob did not fail")
	}
}

func TestNormalizeDatasetURL(t *testing.T) {
	got, err := hub.NormalizeRepoID("https://huggingface.co/datasets/lhoestq/demo1")
	if err != nil || got != "lhoestq/demo1" {
		t.Fatalf("dataset URL = %q, %v", got, err)
	}
}

func TestChecksumCheckpointSurvivesLaterDownloadFailure(t *testing.T) {
	good := []byte("completed-file")
	bad := []byte("never-completed")
	const commit = "4444444444444444444444444444444444444444"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/owner/checkpoint/revision/main":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "owner/checkpoint", "sha": commit,
				"siblings": []map[string]any{
					{"rfilename": "a-good.bin", "blobId": gitBlobID(good), "size": len(good)},
					{"rfilename": "b-fails.bin", "blobId": gitBlobID(bad), "size": len(bad)},
				},
			})
		case "/owner/checkpoint/resolve/" + commit + "/a-good.bin":
			var start, end int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(good)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(good[start : end+1])
		case "/owner/checkpoint/resolve/" + commit + "/b-fails.bin":
			http.Error(w, "intentional failure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	output := filepath.Join(t.TempDir(), "owner_checkpoint")
	cfg := defaults()
	cfg.Endpoint, cfg.Output = server.URL, output
	cfg.Parts, cfg.MultipartThreshold, cfg.Retries = 1, 1, 0
	if err := syncRepo(context.Background(), cfg, "owner/checkpoint"); err == nil {
		t.Fatal("expected the second file to fail")
	}
	checksums, err := os.ReadFile(filepath.Join(output, ".sha256"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(checksums)
	if !strings.Contains(text, blobSHA256(good)+"  a-good.bin") {
		t.Fatalf("successful file missing from checkpoint:\n%s", text)
	}
	if strings.Contains(text, "b-fails.bin") {
		t.Fatalf("failed file present in checkpoint:\n%s", text)
	}
	sha1Checksums, err := os.ReadFile(filepath.Join(output, ".sha1sum"))
	if err != nil {
		t.Fatal(err)
	}
	sha1Text := string(sha1Checksums)
	if !strings.Contains(sha1Text, rawSHA1(good)+"  a-good.bin") || strings.Contains(sha1Text, "b-fails.bin") {
		t.Fatalf("unexpected SHA-1 checkpoint:\n%s", sha1Text)
	}
	if got, err := os.ReadFile(filepath.Join(output, "a-good.bin")); err != nil || string(got) != string(good) {
		t.Fatalf("completed file = %q, %v", got, err)
	}
}

func gitBlobID(data []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
