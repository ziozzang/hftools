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
	Retries            int
	Resume             bool
}

type Hashes struct {
	SHA256  string
	GitSHA1 string
}

type Downloader struct {
	Client         *hub.Client
	Root           string
	StateDir       string
	TempDir        string
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
	want := newPartialState(repoID, commitSHA, remote, parts)
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
	if retries < 0 {
		retries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		start, end := t.bounds(index)
		if start > end {
			return nil
		}
		if attempt > 0 {
			delay := time.Duration(1<<min(attempt-1, 5)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := d.Client.NewDownloadRequest(ctx, d.Client.DownloadURL(repoID, commitSHA, remotePath), start, end)
		if err != nil {
			return err
		}
		resp, err := d.Client.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("range %d-%d: HTTP %s: %s", start, end, resp.Status, strings.TrimSpace(string(body)))
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return lastErr
			}
			continue
		}
		if err := validateContentRange(resp.Header.Get("Content-Range"), start, end); err != nil {
			_ = resp.Body.Close()
			return err
		}
		pos := start
		for pos <= end {
			want := int64(len(buf))
			if remaining := end - pos + 1; remaining < want {
				want = remaining
			}
			n, readErr := resp.Body.Read(buf[:want])
			if n > 0 {
				written, writeErr := f.WriteAt(buf[:n], pos)
				if writeErr != nil {
					_ = resp.Body.Close()
					return writeErr
				}
				if written != n {
					_ = resp.Body.Close()
					return io.ErrShortWrite
				}
				pos += int64(n)
				if err := t.update(index, pos, false); err != nil {
					_ = resp.Body.Close()
					return err
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
				break
			}
		}
		_ = resp.Body.Close()
		if pos > end {
			if err := t.update(index, pos, true); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("download %q segment %d failed after %d attempt(s): %w", remotePath, index+1, retries+1, lastErr)
}

func newPartialState(repoID, commitSHA string, remote hub.RepoFile, parts int) *state.PartialState {
	ps := &state.PartialState{Version: 1, RepoID: repoID, CommitSHA: commitSHA, Path: remote.Path, Size: remote.Size, RemoteBlobSHA1: remote.BlobID}
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
	if got.Version != want.Version || got.RepoID != want.RepoID || got.CommitSHA != want.CommitSHA || got.Path != want.Path || got.Size != want.Size || got.RemoteBlobSHA1 != want.RemoteBlobSHA1 || got.RemoteLFSSHA256 != want.RemoteLFSSHA256 || len(got.Segments) != len(want.Segments) {
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

// HashFileSelective always computes SHA-256. Git SHA-1 can be disabled for LFS
// objects, whose authoritative remote hash is already SHA-256.
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
	var gitHash hash.Hash
	var writers []io.Writer
	writers = append(writers, sha256Hash)
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
	hashes := Hashes{SHA256: hex.EncodeToString(sha256Hash.Sum(nil))}
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
	if clean == "tmp" || strings.HasPrefix(clean, "tmp"+string(filepath.Separator)) || clean == ".metadata" || strings.HasPrefix(clean, ".metadata"+string(filepath.Separator)) || clean == ".hfdown" || strings.HasPrefix(clean, ".hfdown"+string(filepath.Separator)) || clean == "hfdown-metadata" || strings.HasPrefix(clean, "hfdown-metadata"+string(filepath.Separator)) || clean == ".sha256" {
		return "", fmt.Errorf("repository path %q conflicts with hfdown metadata", remotePath)
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
