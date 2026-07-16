package download

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/hfdownload/internal/hub"
	"github.com/ziozzang/hfdownload/internal/progress"
	"github.com/ziozzang/hfdownload/internal/state"
)

type Options struct {
	Parts              int
	MultipartThreshold int64
	BufferSize         int
	// Retries bounds retries per range for retriable failures (5xx/429/network/
	// stall/slow). A negative value retries until success or a terminal error.
	Retries int
	// RetryMinWait and RetryMaxWait bound the randomized retry backoff; zero
	// values fall back to 1s and 5m.
	RetryMinWait time.Duration
	RetryMaxWait time.Duration
	// StallTimeout aborts and retries a range whose stream delivers no bytes
	// for this long. Zero disables stall detection (a hung connection then
	// blocks until the OS or server tears it down).
	StallTimeout time.Duration
	// MinSpeed aborts and retries a range (connection) whose average throughput
	// stays below this many bytes per second over MinSpeedWindow. Because each
	// segment is a separate connection, the floor is per-connection. Zero
	// disables the check.
	MinSpeed int64
	// MinSpeedWindow is the averaging window for MinSpeed. Zero uses 5s.
	MinSpeedWindow time.Duration
	Resume         bool
}

type Hashes struct {
	SHA256  string
	SHA1    string
	GitSHA1 string
}

type Downloader struct {
	Client         *hub.Client
	Root           string
	StateDir       string
	TempDir        string
	RepoType       hub.RepoType
	Options        Options
	Progress       *progress.Bar
	OnNetworkBytes func(int64)
	OnResumedBytes func(int64)
}

type tracker struct {
	mu       sync.Mutex
	state    *state.PartialState
	path     string
	lastSave []int64
}

func (d *Downloader) Download(ctx context.Context, repoID, commitSHA string, remote hub.RepoFile) (Hashes, error) {
	target, err := SafeTarget(d.Root, remote.Path)
	if err != nil {
		return Hashes{}, err
	}
	if err := ensureSafeParent(d.Root, filepath.Dir(target)); err != nil {
		return Hashes{}, err
	}
	if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return Hashes{}, fmt.Errorf("refusing to replace symlink %q", remote.Path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Hashes{}, err
	}

	key := sha256.Sum256([]byte(remote.Path))
	base := hex.EncodeToString(key[:16])
	tmpRoot := d.TempDir
	if tmpRoot == "" {
		tmpRoot = filepath.Join(d.Root, "tmp")
	}
	workDir := filepath.Join(tmpRoot, base)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return Hashes{}, err
	}
	dataPath := filepath.Join(workDir, "download.part")
	statePath := filepath.Join(workDir, "state.json")

	parts := d.Options.Parts
	if parts < 1 {
		parts = 1
	}
	if remote.Size < d.Options.MultipartThreshold || remote.Size < int64(parts) {
		parts = 1
	}
	repoType := d.RepoType
	if repoType == "" {
		repoType = hub.RepoTypeModel
	}
	want := newPartialState(repoType, repoID, commitSHA, remote, parts)
	ps, resumed := loadCompatiblePartial(statePath, dataPath, want, d.Options.Resume)
	if !resumed {
		_ = os.Remove(dataPath)
		_ = os.Remove(statePath)
		ps = want
		if d.Options.Resume {
			if st, statErr := os.Stat(target); statErr == nil && st.Mode().IsRegular() && st.Size() > 0 && st.Size() < remote.Size {
				markPrefixComplete(ps, st.Size())
				if err := state.SaveJSONAtomic(statePath, ps); err != nil {
					return Hashes{}, err
				}
				if err := os.Rename(target, dataPath); err != nil {
					_ = os.Remove(statePath)
					return Hashes{}, fmt.Errorf("adopt existing partial %q: %w", remote.Path, err)
				}
				resumed = true
			}
		}
		if !resumed {
			if err := state.SaveJSONAtomic(statePath, ps); err != nil {
				return Hashes{}, err
			}
		}
	}

	f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Hashes{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	if err := f.Truncate(remote.Size); err != nil {
		return Hashes{}, err
	}

	bar := d.Progress
	ownedBar := bar == nil
	if ownedBar {
		bar = progress.New(os.Stderr, remote.Size, "download "+remote.Path)
	}
	var already int64
	for _, s := range ps.Segments {
		already += s.Next - s.Start
	}
	var contribution atomic.Int64
	contribution.Store(already)
	if ownedBar {
		bar.SetDone(already)
	} else {
		bar.AddCompleted(already)
	}
	if already > 0 && d.OnResumedBytes != nil {
		d.OnResumedBytes(already)
	}

	t := &tracker{state: ps, path: statePath, lastSave: make([]int64, len(ps.Segments))}
	for i := range ps.Segments {
		t.lastSave[i] = ps.Segments[i].Next
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, len(ps.Segments))
	var wg sync.WaitGroup
	for i := range ps.Segments {
		if ps.Segments[i].Next > ps.Segments[i].End {
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if err := d.downloadSegment(workCtx, f, repoID, commitSHA, remote.Path, t, index, bar, &contribution); err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	if ownedBar {
		bar.Finish()
	}
	if err := <-errCh; err != nil {
		_ = t.save()
		return Hashes{}, err
	}
	if err := t.save(); err != nil {
		return Hashes{}, err
	}
	if err := f.Sync(); err != nil {
		return Hashes{}, err
	}
	if err := f.Close(); err != nil {
		return Hashes{}, err
	}
	closed = true

	var verifyBar *progress.Bar
	if ownedBar {
		verifyBar = progress.New(os.Stderr, remote.Size, "hash "+remote.Path)
	} else {
		bar.SetLabel("hash " + remote.Path)
	}
	hashes, err := HashFileSelective(dataPath, remote.Size, d.Options.BufferSize, verifyBar, remote.LFS == nil)
	if verifyBar != nil {
		verifyBar.Finish()
	}
	if err != nil {
		return Hashes{}, err
	}
	if err := CheckHashes(remote, hashes); err != nil {
		_ = os.Remove(dataPath)
		_ = os.Remove(statePath)
		_ = os.Remove(workDir)
		_ = removeIfEmpty(tmpRoot)
		if resumed && ctx.Err() == nil {
			if !ownedBar {
				bar.Add(-contribution.Load())
				bar.Logf("warning: resumed bytes for %s failed final hash; retrying the file from zero\n", remote.Path)
			} else {
				fmt.Fprintf(os.Stderr, "warning: resumed bytes for %s failed final hash; retrying the file from zero\n", remote.Path)
			}
			fresh := *d
			fresh.Options.Resume = false
			return fresh.Download(ctx, repoID, commitSHA, remote)
		}
		return Hashes{}, err
	}
	if err := os.Chmod(dataPath, 0o644); err != nil {
		return Hashes{}, err
	}
	if err := os.Rename(dataPath, target); err != nil {
		return Hashes{}, err
	}
	_ = os.Remove(statePath)
	_ = os.Remove(workDir)
	_ = removeIfEmpty(tmpRoot)
	return hashes, nil
}

func (d *Downloader) downloadSegment(ctx context.Context, f *os.File, repoID, commitSHA, remotePath string, t *tracker, index int, bar *progress.Bar, contribution *atomic.Int64) error {
	bufSize := d.Options.BufferSize
	if bufSize < 32<<10 {
		bufSize = 32 << 10
	}
	buf := make([]byte, bufSize)
	retries := d.Options.Retries
	unlimited := retries < 0
	minWait, maxWait := hub.RetryWaits(d.Options.RetryMinWait, d.Options.RetryMaxWait)
	stallTimeout := d.Options.StallTimeout
	minSpeed := d.Options.MinSpeed
	minSpeedWindow := d.Options.MinSpeedWindow
	if minSpeedWindow <= 0 {
		minSpeedWindow = 5 * time.Second
	}
	rawURL := d.Client.DownloadURL(d.RepoType, repoID, commitSHA, remotePath)
	var lastErr error
	failures := 0 // consecutive attempts without forward progress
	for {
		start, end := t.bounds(index)
		if start > end {
			return nil
		}

		// Each attempt runs under its own cancelable context so a stall
		// watchdog can tear down a hung or crawling connection; the retry loop
		// then resumes from the tracker's last offset. Parent-context
		// cancellation (user interrupt) still propagates and stays terminal.
		attemptCtx, cancel := context.WithCancel(ctx)
		var lastData, attemptBytes atomic.Int64
		var stalled, slow atomic.Bool
		guardStop := make(chan struct{})
		guardExit := make(chan struct{})
		if stallTimeout > 0 || minSpeed > 0 {
			lastData.Store(time.Now().UnixNano())
			go watchAttempt(stallTimeout, minSpeedWindow, minSpeed, &lastData, &attemptBytes, &stalled, &slow, cancel, guardStop, guardExit)
		} else {
			close(guardExit)
		}

		done, err := func() (bool, error) {
			req, err := d.Client.NewDownloadRequest(attemptCtx, rawURL, start, end)
			if err != nil {
				return false, err
			}
			resp, err := d.Client.HTTP.Do(req)
			if err != nil {
				lastErr = err
				return false, nil
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				lastErr = fmt.Errorf("range %d-%d: HTTP %s: %s", start, end, resp.Status, strings.TrimSpace(string(body)))
				if !hub.RetriableStatus(resp.StatusCode) {
					return false, lastErr
				}
				return false, nil
			}
			if err := validateContentRange(resp.Header.Get("Content-Range"), start, end); err != nil {
				return false, err
			}
			pos := start
			for pos <= end {
				want := int64(len(buf))
				if remaining := end - pos + 1; remaining < want {
					want = remaining
				}
				n, readErr := resp.Body.Read(buf[:want])
				if n > 0 {
					if stallTimeout > 0 {
						lastData.Store(time.Now().UnixNano())
					}
					if minSpeed > 0 {
						attemptBytes.Add(int64(n))
					}
					written, writeErr := f.WriteAt(buf[:n], pos)
					if writeErr != nil {
						return false, writeErr
					}
					if written != n {
						return false, io.ErrShortWrite
					}
					pos += int64(n)
					if err := t.update(index, pos, false); err != nil {
						return false, err
					}
					bar.Add(int64(n))
					contribution.Add(int64(n))
					if d.OnNetworkBytes != nil {
						d.OnNetworkBytes(int64(n))
					}
				}
				if readErr != nil {
					if errors.Is(readErr, io.EOF) && pos > end {
						break
					}
					lastErr = readErr
					return false, nil
				}
			}
			if pos > end {
				if err := t.update(index, pos, true); err != nil {
					return false, err
				}
				return true, nil
			}
			return false, nil
		}()

		close(guardStop)
		<-guardExit
		cancel()

		if done {
			return nil
		}
		// A cancelled parent context (interrupt) is terminal; a stall abort is not.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if stalled.Load() {
			lastErr = fmt.Errorf("range %d-%d stalled: no data for %s", start, end, stallTimeout)
		} else if slow.Load() {
			lastErr = fmt.Errorf("range %d-%d too slow: below %s over %s", start, end, humanRate(minSpeed), minSpeedWindow)
		}
		// If the server delivered any bytes this attempt, it has recovered:
		// reset the backoff and retry budget and reconnect immediately, so a long
		// transfer over a flaky link keeps going as long as it makes progress.
		after, _ := t.bounds(index)
		if after > start {
			failures = 0
			bar.Logf("%s: %v; reconnecting to resume at %s\n", remotePath, lastErr, formatSize(after))
			continue
		}
		failures++
		if !unlimited && failures > retries {
			break
		}
		delay := hub.RetryDelay(failures-1, minWait, maxWait)
		bar.Logf("%s: %v; retrying in %s (resume at %s)\n", remotePath, lastErr, delay.Round(time.Millisecond), formatSize(after))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("download %q segment %d failed after %d consecutive attempt(s): %w", remotePath, index+1, failures, lastErr)
}

// watchAttempt aborts an attempt that either makes no progress for stallTimeout
// (stall) or averages below minSpeed bytes/second over minSpeedWindow (too slow).
// It polls the shared progress counters and cancels the attempt context on the
// first tripped condition, recording the reason so the caller can distinguish an
// abort from a genuine error and retry with resume. It exits promptly when the
// caller closes stop (attempt finished), signalling completion by closing exit.
// Either check is skipped when its threshold is zero.
func watchAttempt(stallTimeout, minSpeedWindow time.Duration, minSpeed int64, lastData, received *atomic.Int64, stalled, slow *atomic.Bool, cancel context.CancelFunc, stop <-chan struct{}, exit chan<- struct{}) {
	defer close(exit)
	ticker := time.NewTicker(watchInterval(stallTimeout, minSpeedWindow, minSpeed))
	defer ticker.Stop()
	windowStart := time.Now()
	windowBytes := received.Load()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if stallTimeout > 0 && time.Since(time.Unix(0, lastData.Load())) >= stallTimeout {
				stalled.Store(true)
				cancel()
				return
			}
			if minSpeed > 0 {
				if elapsed := time.Since(windowStart); elapsed >= minSpeedWindow {
					if float64(received.Load()-windowBytes)/elapsed.Seconds() < float64(minSpeed) {
						slow.Store(true)
						cancel()
						return
					}
					windowStart = time.Now()
					windowBytes = received.Load()
				}
			}
		}
	}
}

// watchInterval picks a polling period fine enough for whichever checks are
// enabled, clamped to a sane range.
func watchInterval(stallTimeout, minSpeedWindow time.Duration, minSpeed int64) time.Duration {
	interval := time.Second
	if stallTimeout > 0 && stallTimeout/4 < interval {
		interval = stallTimeout / 4
	}
	if minSpeed > 0 && minSpeedWindow/4 < interval {
		interval = minSpeedWindow / 4
	}
	if interval < 5*time.Millisecond {
		interval = 5 * time.Millisecond
	}
	return interval
}

// formatSize renders a byte count with a binary unit for human-readable logs.
func formatSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// humanRate renders a bytes-per-second threshold for human-readable logs.
func humanRate(b int64) string { return formatSize(b) + "/s" }

func newPartialState(repoType hub.RepoType, repoID, commitSHA string, remote hub.RepoFile, parts int) *state.PartialState {
	ps := &state.PartialState{Version: 1, RepoType: string(repoType), RepoID: repoID, CommitSHA: commitSHA, Path: remote.Path, Size: remote.Size, RemoteBlobSHA1: remote.BlobID}
	if remote.LFS != nil {
		ps.RemoteLFSSHA256 = remote.LFS.SHA256
	}
	if remote.Size == 0 {
		return ps
	}
	segmentSize := (remote.Size + int64(parts) - 1) / int64(parts)
	for start := int64(0); start < remote.Size; start += segmentSize {
		end := start + segmentSize - 1
		if end >= remote.Size {
			end = remote.Size - 1
		}
		ps.Segments = append(ps.Segments, state.Segment{Start: start, End: end, Next: start})
	}
	return ps
}

func markPrefixComplete(ps *state.PartialState, prefixSize int64) {
	for i := range ps.Segments {
		s := &ps.Segments[i]
		switch {
		case prefixSize > s.End:
			s.Next = s.End + 1
		case prefixSize > s.Start:
			s.Next = prefixSize
		default:
			s.Next = s.Start
		}
	}
}

func loadCompatiblePartial(statePath, dataPath string, want *state.PartialState, resume bool) (*state.PartialState, bool) {
	if !resume {
		return nil, false
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		return nil, false
	}
	var got state.PartialState
	if json.Unmarshal(b, &got) != nil {
		return nil, false
	}
	st, err := os.Stat(dataPath)
	if err != nil || st.Size() != want.Size {
		return nil, false
	}
	gotRepoType := got.RepoType
	if gotRepoType == "" {
		gotRepoType = string(hub.RepoTypeModel)
	}
	if got.Version != want.Version || gotRepoType != want.RepoType || got.RepoID != want.RepoID || got.CommitSHA != want.CommitSHA || got.Path != want.Path || got.Size != want.Size || got.RemoteBlobSHA1 != want.RemoteBlobSHA1 || got.RemoteLFSSHA256 != want.RemoteLFSSHA256 || len(got.Segments) != len(want.Segments) {
		return nil, false
	}
	for i, s := range got.Segments {
		if s.Start != want.Segments[i].Start || s.End != want.Segments[i].End || s.Next < s.Start || s.Next > s.End+1 {
			return nil, false
		}
	}
	return &got, true
}

func (t *tracker) bounds(index int) (int64, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state.Segments[index]
	return s.Next, s.End
}

func (t *tracker) update(index int, next int64, force bool) error {
	t.mu.Lock()
	t.state.Segments[index].Next = next
	if !force && next-t.lastSave[index] < 8<<20 {
		t.mu.Unlock()
		return nil
	}
	t.lastSave[index] = next
	err := state.SaveJSONAtomic(t.path, t.state)
	t.mu.Unlock()
	return err
}

func (t *tracker) save() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return state.SaveJSONAtomic(t.path, t.state)
}

func HashFile(path string, expectedSize int64, bufferSize int, bar *progress.Bar) (Hashes, error) {
	return HashFileSelective(path, expectedSize, bufferSize, bar, true)
}

// HashFileSelective always computes raw-content SHA-256 and SHA-1. Git blob
// SHA-1 can be disabled for LFS objects, whose authoritative remote hash is
// already SHA-256.
func HashFileSelective(path string, expectedSize int64, bufferSize int, bar *progress.Bar, computeGitSHA1 bool) (Hashes, error) {
	f, err := os.Open(path)
	if err != nil {
		return Hashes{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Hashes{}, err
	}
	if st.Size() != expectedSize {
		return Hashes{}, fmt.Errorf("size mismatch: got %d, want %d", st.Size(), expectedSize)
	}
	sha256Hash := sha256.New()
	sha1Hash := sha1.New()
	var gitHash hash.Hash
	var writers []io.Writer
	writers = append(writers, sha256Hash, sha1Hash)
	if computeGitSHA1 {
		gitHash = sha1.New()
		_, _ = io.WriteString(gitHash, "blob "+strconv.FormatInt(expectedSize, 10)+"\x00")
		writers = append(writers, gitHash)
	}
	if bar != nil {
		writers = append(writers, progressWriter{bar})
	}
	writer := io.MultiWriter(writers...)
	if bufferSize < 32<<10 {
		bufferSize = 32 << 10
	}
	buf := make([]byte, bufferSize)
	if _, err := io.CopyBuffer(writer, f, buf); err != nil {
		return Hashes{}, err
	}
	hashes := Hashes{
		SHA256: hex.EncodeToString(sha256Hash.Sum(nil)),
		SHA1:   hex.EncodeToString(sha1Hash.Sum(nil)),
	}
	if gitHash != nil {
		hashes.GitSHA1 = hex.EncodeToString(gitHash.Sum(nil))
	}
	return hashes, nil
}

func CheckHashes(remote hub.RepoFile, got Hashes) error {
	if remote.LFS != nil && remote.LFS.SHA256 != "" {
		if !strings.EqualFold(remote.LFS.SHA256, got.SHA256) {
			return fmt.Errorf("SHA-256 mismatch for %q: got %s, want %s", remote.Path, got.SHA256, remote.LFS.SHA256)
		}
		return nil
	}
	if remote.BlobID != "" && !strings.EqualFold(remote.BlobID, got.GitSHA1) {
		return fmt.Errorf("Git blob SHA-1 mismatch for %q: got %s, want %s", remote.Path, got.GitSHA1, remote.BlobID)
	}
	return nil
}

func SafeTarget(root, remotePath string) (string, error) {
	if remotePath == "" || strings.Contains(remotePath, "\\") || filepath.IsAbs(remotePath) {
		return "", fmt.Errorf("unsafe repository path %q", remotePath)
	}
	clean := filepath.Clean(filepath.FromSlash(remotePath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != remotePath {
		return "", fmt.Errorf("unsafe repository path %q", remotePath)
	}
	if clean == "tmp" || strings.HasPrefix(clean, "tmp"+string(filepath.Separator)) || clean == ".metadata" || strings.HasPrefix(clean, ".metadata"+string(filepath.Separator)) || clean == ".hfdown" || strings.HasPrefix(clean, ".hfdown"+string(filepath.Separator)) || clean == "hfdown-metadata" || strings.HasPrefix(clean, "hfdown-metadata"+string(filepath.Separator)) || clean == ".sha256" || clean == ".sha1sum" {
		return "", fmt.Errorf("repository path %q conflicts with hftools metadata", remotePath)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, clean)
	if err := validateExistingPath(rootAbs, target); err != nil {
		return "", err
	}
	return target, nil
}

func removeIfEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(path)
	}
	return nil
}

func validateExistingPath(root, target string) error {
	rootInfo, err := os.Lstat(root)
	if err == nil {
		if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
			return fmt.Errorf("unsafe output root %q", root)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe symlink in output path %q", cur)
		}
		if i < len(parts)-1 && !st.IsDir() {
			return fmt.Errorf("non-directory output parent %q", cur)
		}
	}
	return nil
}

func ensureSafeParent(root, parent string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(rootAbs, 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("target escapes output directory")
	}
	cur := rootAbs
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("unsafe output parent %q", cur)
		}
	}
	return nil
}

type progressWriter struct{ bar *progress.Bar }

func (w progressWriter) Write(p []byte) (int, error) { w.bar.Add(int64(len(p))); return len(p), nil }

func validateContentRange(value string, start, end int64) error {
	if value == "" {
		return fmt.Errorf("server returned 206 without Content-Range")
	}
	var gotStart, gotEnd int64
	if _, err := fmt.Sscanf(value, "bytes %d-%d/", &gotStart, &gotEnd); err != nil {
		// The total after '/' prevents a direct Sscanf match without an extra token.
		parts := strings.SplitN(strings.TrimPrefix(value, "bytes "), "/", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid Content-Range %q", value)
		}
		if _, err := fmt.Sscanf(parts[0], "%d-%d", &gotStart, &gotEnd); err != nil {
			return fmt.Errorf("invalid Content-Range %q", value)
		}
	}
	if gotStart != start || gotEnd != end {
		return fmt.Errorf("unexpected Content-Range %q, requested bytes %d-%d", value, start, end)
	}
	return nil
}
