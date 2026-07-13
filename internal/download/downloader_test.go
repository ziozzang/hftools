package download

import (
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

	"github.com/ziozzang/hfdownload/internal/hub"
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
