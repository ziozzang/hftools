package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/progress"
)

func TestMultipartResumeAndHashVerification(t *testing.T) {
	data := make([]byte, 9<<20)
	for i := range data {
		data[i] = byte((i*31 + 7) % 251)
	}
	wantHash := sha256.Sum256(data)
	var rangesMu sync.Mutex
	var ranges []string
	var slow atomic.Bool
	slow.Store(true)
	started := make(chan struct{})
	var startedOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		rangesMu.Lock()
		ranges = append(ranges, rangeHeader)
		rangesMu.Unlock()
		var start, end int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		if start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		const chunk = 64 << 10
		for pos := start; pos <= end; {
			next := pos + chunk
			if next > end+1 {
				next = end + 1
			}
			if _, err := w.Write(data[pos:next]); err != nil {
				return
			}
			pos = next
			if pos-start >= 256<<10 {
				startedOnce.Do(func() { close(started) })
			}
			if slow.Load() {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}))
	defer server.Close()

	root := t.TempDir()
	client := hub.New(server.URL, "", 5*time.Second)
	d := Downloader{Client: client, Root: root, StateDir: filepath.Join(root, ".metadata"), Options: Options{
		Parts: 4, MultipartThreshold: 1, BufferSize: 64 << 10, Retries: 0, Resume: true,
	}}
	remote := hub.RepoFile{Path: "weights/model.bin", BlobID: "pointer", Size: int64(len(data)), LFS: &hub.LFSInfo{SHA256: hex.EncodeToString(wantHash[:]), Size: int64(len(data))}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Download(ctx, "owner/model", "commit", remote)
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	if err := <-done; err == nil {
		t.Fatal("expected interrupted download to fail")
	}

	partials, err := filepath.Glob(filepath.Join(root, "tmp", "*", "state.json"))
	if err != nil || len(partials) != 1 {
		t.Fatalf("partial state not retained: %v, %v", partials, err)
	}
	slow.Store(false)
	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "weights", "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("downloaded data differs")
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "tmp", "*")); len(matches) != 0 {
		t.Fatalf("partial files not cleaned: %v", matches)
	}

	rangesMu.Lock()
	defer rangesMu.Unlock()
	resumed := false
	for _, value := range ranges[4:] {
		if !strings.HasSuffix(value, "-2359295") && value != "" {
			// At least one second-pass segment must start after its original boundary.
			var start int64
			_, _ = fmt.Sscanf(value, "bytes=%d-", &start)
			if start != 0 && start != 2359296 && start != 4718592 && start != 7077888 {
				resumed = true
			}
		}
	}
	if !resumed {
		t.Fatalf("no resumed Range request found: %v", ranges)
	}
}

func TestSafeTargetRejectsTraversalAndMetadata(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../secret", "/absolute", "a/../b", "tmp/model.bin", ".metadata/manifest.json", ".hfdown/manifest.json", "hfdown-metadata/manifest.json", ".sha256", `a\b`} {
		if _, err := SafeTarget(root, path); err == nil {
			t.Errorf("SafeTarget accepted %q", path)
		}
	}
	got, err := SafeTarget(root, "sub/model.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join("sub", "model.bin")) {
		t.Fatalf("unexpected target %q", got)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeTarget(root, "linked/model.bin"); err == nil {
		t.Fatal("SafeTarget accepted a symlinked parent")
	}
}

func TestExistingShortFileIsAdoptedAndResumed(t *testing.T) {
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sum := sha256.Sum256(data)
	var mu sync.Mutex
	var starts []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	root := t.TempDir()
	target := filepath.Join(root, "weights", "model.bin")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	const prefixSize = 300_000
	if err := os.WriteFile(target, data[:prefixSize], 0o644); err != nil {
		t.Fatal(err)
	}
	d := Downloader{
		Client: hub.New(server.URL, "", 5*time.Second), Root: root,
		StateDir: filepath.Join(root, ".metadata"),
		Options:  Options{Parts: 4, MultipartThreshold: 1, BufferSize: 64 << 10, Retries: 0, Resume: true},
	}
	remote := hub.RepoFile{Path: "weights/model.bin", Size: int64(len(data)), LFS: &hub.LFSInfo{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}}
	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(data) {
		t.Fatalf("resumed file differs: size=%d, err=%v", len(got), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("range starts = %v, want 3 requests", starts)
	}
	foundPrefix := false
	for _, start := range starts {
		if start == 0 {
			t.Fatalf("download restarted from zero: %v", starts)
		}
		if start == prefixSize {
			foundPrefix = true
		}
	}
	if !foundPrefix {
		t.Fatalf("no range resumed at existing size %d: %v", prefixSize, starts)
	}
}

// TestStallTimeoutAbortsAndResumes verifies that a connection which delivers
// some bytes and then goes silent is torn down after StallTimeout, and that the
// retry resumes from the last received offset rather than restarting at zero.
func TestStallTimeoutAbortsAndResumes(t *testing.T) {
	const size = 512 << 10
	const initial = 64 << 10
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*17 + 3) % 251)
	}
	sum := sha256.Sum256(data)

	var mu sync.Mutex
	var starts []int64
	var stalledOnce atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		if start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if stalledOnce.CompareAndSwap(false, true) {
			// First connection: deliver a prefix, flush it, then go silent so
			// the client's stall watchdog must fire and abort the read.
			deliver := start + initial
			if deliver > end+1 {
				deliver = end + 1
			}
			_, _ = w.Write(data[start:deliver])
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			select {
			case <-r.Context().Done(): // client cancelled after the stall timeout
			case <-time.After(3 * time.Second): // fallback so the goroutine never leaks
			}
			return
		}
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	root := t.TempDir()
	d := Downloader{
		Client: hub.New(server.URL, "", 5*time.Second), Root: root,
		StateDir: filepath.Join(root, ".metadata"),
		Options: Options{
			Parts: 1, MultipartThreshold: 1, BufferSize: 32 << 10,
			Retries: 3, StallTimeout: 100 * time.Millisecond, Resume: true,
		},
	}
	remote := hub.RepoFile{Path: "weights/model.bin", Size: int64(len(data)), LFS: &hub.LFSInfo{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}}

	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err != nil {
		t.Fatalf("stalled download did not recover: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "weights", "model.bin"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("recovered file differs: size=%d, err=%v", len(got), err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 2 {
		t.Fatalf("expected a reconnect after the stall, got range starts %v", starts)
	}
	if starts[0] != 0 {
		t.Fatalf("first range should start at 0, got %v", starts)
	}
	resumed := false
	for _, start := range starts[1:] {
		if start > 0 {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("no resumed (non-zero) range after the stall: %v", starts)
	}
}

// TestServerErrorRetriesAndCompletes verifies that transient 5xx responses are
// retried (not treated as terminal) until the range finally downloads.
func TestServerErrorRetriesAndCompletes(t *testing.T) {
	const size = 256 << 10
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*7 + 1) % 251)
	}
	sum := sha256.Sum256(data)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			http.Error(w, "backend down", http.StatusServiceUnavailable)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	root := t.TempDir()
	d := Downloader{
		Client: hub.New(server.URL, "", 5*time.Second), Root: root,
		StateDir: filepath.Join(root, ".metadata"),
		Options: Options{
			Parts: 1, MultipartThreshold: 1, BufferSize: 64 << 10, Retries: 5,
			RetryMinWait: time.Millisecond, RetryMaxWait: 10 * time.Millisecond, Resume: true,
		},
	}
	remote := hub.RepoFile{Path: "weights/model.bin", Size: int64(len(data)), LFS: &hub.LFSInfo{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}}
	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err != nil {
		t.Fatalf("download did not recover from 503: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "weights", "model.bin"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("recovered file differs: size=%d, err=%v", len(got), err)
	}
	if n := attempts.Load(); n < 3 {
		t.Fatalf("expected retries past the 503 responses, got %d attempts", n)
	}
}

// TestClientErrorDoesNotRetry verifies that a genuine client error (404) fails
// immediately instead of being retried as if transient.
func TestClientErrorDoesNotRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "no such file", http.StatusNotFound)
	}))
	defer server.Close()

	root := t.TempDir()
	d := Downloader{
		Client: hub.New(server.URL, "", 5*time.Second), Root: root,
		StateDir: filepath.Join(root, ".metadata"),
		Options: Options{
			Parts: 1, MultipartThreshold: 1, BufferSize: 64 << 10, Retries: 5,
			RetryMinWait: time.Millisecond, RetryMaxWait: 10 * time.Millisecond, Resume: true,
		},
	}
	remote := hub.RepoFile{Path: "weights/model.bin", Size: 1024, BlobID: "pointer"}
	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err == nil {
		t.Fatal("expected 404 to fail the download")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("404 is terminal; expected 1 attempt, got %d", n)
	}
}

// TestProgressResetsRetryBudget verifies that forward progress clears the
// consecutive-failure budget: every connection is cut mid-stream after a partial
// chunk, yet with only Retries=1 the file still completes because each attempt
// advances the offset.
func TestProgressResetsRetryBudget(t *testing.T) {
	const size = 256 << 10
	const chunk = 64 << 10
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*11 + 9) % 251)
	}
	sum := sha256.Sum256(data)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		// Advertise the full requested range but deliver only a partial chunk,
		// then return so the client sees a truncated body and must reconnect.
		deliver := start + chunk
		if deliver > end+1 {
			deliver = end + 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start:deliver])
	}))
	defer server.Close()

	root := t.TempDir()
	d := Downloader{
		Client: hub.New(server.URL, "", 5*time.Second), Root: root,
		StateDir: filepath.Join(root, ".metadata"),
		Options: Options{
			Parts: 1, MultipartThreshold: 1, BufferSize: 32 << 10, Retries: 1,
			RetryMinWait: time.Millisecond, RetryMaxWait: 10 * time.Millisecond, Resume: true,
		},
	}
	remote := hub.RepoFile{Path: "weights/model.bin", Size: int64(len(data)), LFS: &hub.LFSInfo{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}}
	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err != nil {
		t.Fatalf("download stalled despite making progress each attempt: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "weights", "model.bin"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("recovered file differs: size=%d, err=%v", len(got), err)
	}
	// size/chunk partial deliveries are needed; far more than Retries+1 = 2.
	if n := attempts.Load(); n < size/chunk {
		t.Fatalf("expected at least %d reconnects, got %d", size/chunk, n)
	}
}

// TestMinSpeedAbortsAndResumes verifies that a connection which keeps trickling
// bytes (so the stall timeout never fires) but averages below MinSpeed is torn
// down after MinSpeedWindow, and that the retry resumes rather than restarts.
func TestMinSpeedAbortsAndResumes(t *testing.T) {
	const size = 512 << 10
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*13 + 5) % 251)
	}
	sum := sha256.Sum256(data)

	var mu sync.Mutex
	var starts []int64
	var slowOnce atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		if start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		flusher, _ := w.(http.Flusher)
		if slowOnce.CompareAndSwap(false, true) {
			// First connection: trickle 8 KiB roughly every 60 ms (~133 KiB/s),
			// well under the 1 MiB/s floor but never silent, so only the
			// min-speed check (not the stall timeout) can abort it.
			for pos := start; pos <= end; {
				next := pos + 8<<10
				if next > end+1 {
					next = end + 1
				}
				if _, err := w.Write(data[pos:next]); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				pos = next
				select {
				case <-r.Context().Done():
					return
				case <-time.After(60 * time.Millisecond):
				}
			}
			return
		}
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	root := t.TempDir()
	var logBuf bytes.Buffer
	bar := progress.New(&logBuf, int64(len(data)), "download")
	d := Downloader{
		Client: hub.New(server.URL, "", 5*time.Second), Root: root,
		StateDir: filepath.Join(root, ".metadata"), Progress: bar,
		Options: Options{
			Parts: 1, MultipartThreshold: 1, BufferSize: 32 << 10, Retries: 3,
			MinSpeed: 1 << 20, MinSpeedWindow: 200 * time.Millisecond, Resume: true,
		},
	}
	remote := hub.RepoFile{Path: "weights/model.bin", Size: int64(len(data)), LFS: &hub.LFSInfo{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}}

	if _, err := d.Download(context.Background(), "owner/model", "commit", remote); err != nil {
		t.Fatalf("slow download did not recover: %v", err)
	}
	bar.Finish() // stop the render loop before reading the captured log
	got, err := os.ReadFile(filepath.Join(root, "weights", "model.bin"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("recovered file differs: size=%d, err=%v", len(got), err)
	}
	if log := logBuf.String(); !strings.Contains(log, "too slow") || !strings.Contains(log, "resume at") {
		t.Fatalf("expected a visible slow-retry log, got: %q", log)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 2 {
		t.Fatalf("expected a reconnect after the slow connection, got range starts %v", starts)
	}
	resumed := false
	for _, start := range starts[1:] {
		if start > 0 {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("no resumed (non-zero) range after the slow abort: %v", starts)
	}
}
