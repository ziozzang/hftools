// Package hfcache converts between hftools' flat download layout and the
// huggingface_hub local cache layout, so downloads can be moved into an
// air-gapped machine and used offline by the HF Python libraries (and back).
//
// The HF cache layout (see huggingface_hub file_download.py) is:
//
//	<cache>/<repo_folder>/
//	  refs/<revision>            -> text file containing the commit hash
//	  blobs/<etag>               -> raw file content, content-addressed
//	  snapshots/<commit>/<path>  -> relative symlink into ../blobs/<etag>
//
// where repo_folder is "models--org--name" (or "datasets--..."), and etag is
// the LFS SHA-256 for LFS files or the git blob SHA-1 for regular files — the
// exact hashes hftools already records per file.
package hfcache

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ziozzang/hftools/internal/download"
	"github.com/ziozzang/hftools/internal/state"
)

const repoIDSeparator = "--"

// RepoFolderName mirrors huggingface_hub.repo_folder_name: the repo type is
// pluralized and joined with the owner/name by "--".
func RepoFolderName(repoType, repoID string) string {
	if repoType == "" {
		repoType = "model"
	}
	parts := append([]string{repoType + "s"}, strings.Split(repoID, "/")...)
	return strings.Join(parts, repoIDSeparator)
}

// DefaultCacheRoot resolves the HF hub cache directory the same way the Python
// library does, honoring HF_HUB_CACHE, HUGGINGFACE_HUB_CACHE, HF_HOME, and
// XDG_CACHE_HOME before falling back to ~/.cache/huggingface/hub.
func DefaultCacheRoot() string {
	if v := os.Getenv("HF_HUB_CACHE"); v != "" {
		return v
	}
	if v := os.Getenv("HUGGINGFACE_HUB_CACHE"); v != "" {
		return v
	}
	home := os.Getenv("HF_HOME")
	if home == "" {
		base := os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			if h, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(h, ".cache")
			} else {
				base = ".cache"
			}
		}
		home = filepath.Join(base, "huggingface")
	}
	return filepath.Join(home, "hub")
}

func normalizeType(repoType string) (string, error) {
	switch repoType {
	case "", "model":
		return "model", nil
	case "dataset":
		return "dataset", nil
	default:
		return "", fmt.Errorf("unsupported repository type %q", repoType)
	}
}

// blobEtag returns the HF blob name for a record: the LFS SHA-256 when present,
// otherwise the git blob SHA-1. The bool reports whether the file is LFS.
func blobEtag(rec *state.FileRecord) (string, bool, error) {
	if h := strings.ToLower(rec.RemoteLFSSHA256); isHex(h, 64) {
		return h, true, nil
	}
	if h := strings.ToLower(rec.RemoteBlobSHA1); isHex(h, 40) {
		return h, false, nil
	}
	if h := strings.ToLower(rec.LocalGitSHA1); isHex(h, 40) {
		return h, false, nil
	}
	return "", false, fmt.Errorf("no usable blob hash recorded for %q", rec.Path)
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ExportOptions configures Export.
type ExportOptions struct {
	Manifest   *state.Manifest
	SourceDir  string // hftools flat download directory
	CacheRoot  string // HF cache root (blank -> DefaultCacheRoot)
	Copy       bool   // copy blobs instead of hardlinking
	BufferSize int
}

// ExportResult summarizes an Export run.
type ExportResult struct {
	Storage    string
	CacheRoot  string
	Files      int      // snapshot entries written
	NewBlobs   int      // blobs created (deduplicated by etag)
	Bytes      int64    // logical size of exported files
	SkippedMsg []string // files skipped (e.g. failed verification)
}

// Export writes the manifest's files into the HF cache layout under CacheRoot.
func Export(opts ExportOptions) (ExportResult, error) {
	m := opts.Manifest
	if m == nil {
		return ExportResult{}, errors.New("nil manifest")
	}
	repoType, err := normalizeType(m.RepoType)
	if err != nil {
		return ExportResult{}, err
	}
	if m.RepoID == "" || m.CommitSHA == "" {
		return ExportResult{}, errors.New("manifest missing repo id or commit")
	}
	cacheRoot := opts.CacheRoot
	if cacheRoot == "" {
		cacheRoot = DefaultCacheRoot()
	}
	storage := filepath.Join(cacheRoot, RepoFolderName(repoType, m.RepoID))
	blobsDir := filepath.Join(storage, "blobs")
	snapDir := filepath.Join(storage, "snapshots", m.CommitSHA)
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return ExportResult{}, err
	}
	res := ExportResult{Storage: storage, CacheRoot: cacheRoot}
	for _, rec := range state.SortedFiles(m) {
		if rec.VerificationError != "" {
			res.SkippedMsg = append(res.SkippedMsg, rec.Path+": last verification failed; not exported")
			continue
		}
		src, err := download.SafeTarget(opts.SourceDir, rec.Path)
		if err != nil {
			return res, err
		}
		if st, err := os.Stat(src); err != nil {
			return res, fmt.Errorf("source file %q: %w", rec.Path, err)
		} else if st.Size() != rec.Size {
			return res, fmt.Errorf("source file %q size %d != manifest %d", rec.Path, st.Size(), rec.Size)
		}
		etag, _, err := blobEtag(rec)
		if err != nil {
			return res, err
		}
		blob := filepath.Join(blobsDir, etag)
		if _, err := os.Stat(blob); errors.Is(err, os.ErrNotExist) {
			if err := placeBlob(src, blob, opts.Copy); err != nil {
				return res, fmt.Errorf("blob %q: %w", rec.Path, err)
			}
			res.NewBlobs++
		} else if err != nil {
			return res, err
		}
		pointer := filepath.Join(snapDir, filepath.FromSlash(rec.Path))
		if err := os.MkdirAll(filepath.Dir(pointer), 0o755); err != nil {
			return res, err
		}
		if err := linkPointer(blob, pointer); err != nil {
			return res, fmt.Errorf("snapshot %q: %w", rec.Path, err)
		}
		res.Files++
		res.Bytes += rec.Size
	}
	// refs/<revision> records the branch/tag -> commit mapping (HF only writes
	// it when the revision differs from the commit hash).
	if m.Revision != "" && m.Revision != m.CommitSHA {
		refPath := filepath.Join(storage, "refs", m.Revision)
		if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
			return res, err
		}
		if err := state.WriteFileAtomic(refPath, []byte(m.CommitSHA), 0o644); err != nil {
			return res, err
		}
	}
	return res, nil
}

// placeBlob hardlinks src into the content-addressed blob path, falling back to
// a copy across filesystems or when hardlinks are unavailable.
func placeBlob(src, blob string, forceCopy bool) error {
	if !forceCopy {
		if err := os.Link(src, blob); err == nil {
			return nil
		}
	}
	return copyFile(src, blob, 0o644)
}

// linkPointer creates a relative symlink from the snapshot pointer to the blob,
// falling back to a copy when symlinks are unsupported (e.g. Windows).
func linkPointer(blob, pointer string) error {
	_ = os.Remove(pointer)
	rel, err := filepath.Rel(filepath.Dir(pointer), blob)
	if err != nil {
		rel = blob
	}
	if err := os.Symlink(rel, pointer); err == nil {
		return nil
	}
	return copyFile(blob, pointer, 0o644)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ImportOptions configures Import.
type ImportOptions struct {
	CacheRoot  string
	RepoID     string
	RepoType   string
	Revision   string // branch/tag/commit; blank -> "main"
	DestDir    string // hftools flat output directory
	BufferSize int
}

// ImportResult summarizes an Import run.
type ImportResult struct {
	Storage string
	Commit  string
	Files   int
	Bytes   int64
}

// Import materializes an HF cache snapshot into hftools' flat layout, hashing
// every file, verifying it against its content-addressed blob name, and
// returning a reconstructed manifest ready to be persisted.
func Import(opts ImportOptions) (*state.Manifest, ImportResult, error) {
	repoType, err := normalizeType(opts.RepoType)
	if err != nil {
		return nil, ImportResult{}, err
	}
	revision := opts.Revision
	if revision == "" {
		revision = "main"
	}
	cacheRoot := opts.CacheRoot
	if cacheRoot == "" {
		cacheRoot = DefaultCacheRoot()
	}
	storage := filepath.Join(cacheRoot, RepoFolderName(repoType, opts.RepoID))
	if st, err := os.Stat(storage); err != nil || !st.IsDir() {
		return nil, ImportResult{}, fmt.Errorf("no cached repository at %s", storage)
	}
	commit, err := resolveCommit(storage, revision)
	if err != nil {
		return nil, ImportResult{}, err
	}
	snapDir := filepath.Join(storage, "snapshots", commit)
	if st, err := os.Stat(snapDir); err != nil || !st.IsDir() {
		return nil, ImportResult{}, fmt.Errorf("no snapshot for commit %s", commit)
	}

	m := state.NewManifest(opts.RepoID, revision, commit)
	m.RepoType = repoType
	now := time.Now().UTC()
	res := ImportResult{Storage: storage, Commit: commit}

	err = filepath.WalkDir(snapDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(snapDir, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		dst, err := download.SafeTarget(opts.DestDir, relSlash)
		if err != nil {
			return err
		}
		st, err := os.Stat(path) // follows the snapshot symlink to the blob
		if err != nil {
			return fmt.Errorf("%s: %w", relSlash, err)
		}
		if !st.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		// The blob name is the etag; the transferred bytes must match it.
		etag := blobName(path)
		hashes, err := importFile(path, dst, st.Size(), etag, opts.BufferSize)
		if err != nil {
			return fmt.Errorf("%s: %w", relSlash, err)
		}
		isLFS := isHex(etag, 64)
		rec := &state.FileRecord{
			Path: relSlash, Size: st.Size(),
			RemoteBlobSHA1: hashes.GitSHA1,
			LocalSHA256:    hashes.SHA256, LocalSHA1: hashes.SHA1, LocalGitSHA1: hashes.GitSHA1,
			ModTimeUnixNano: fileModNano(dst), VerifiedAt: now, CommitSHA: commit,
		}
		if isLFS {
			rec.RemoteLFSSHA256 = hashes.SHA256
		}
		m.Files[relSlash] = rec
		res.Files++
		res.Bytes += st.Size()
		return nil
	})
	if err != nil {
		return nil, res, err
	}
	if res.Files == 0 {
		return nil, res, fmt.Errorf("snapshot %s contained no files", commit)
	}
	m.UpdatedAt = now
	m.LastVerifiedAt = &now
	return m, res, nil
}

func resolveCommit(storage, revision string) (string, error) {
	refPath := filepath.Join(storage, "refs", revision)
	if b, err := os.ReadFile(refPath); err == nil {
		if c := strings.TrimSpace(string(b)); c != "" {
			return c, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// No ref: the revision may already be a commit whose snapshot exists.
	if st, err := os.Stat(filepath.Join(storage, "snapshots", revision)); err == nil && st.IsDir() {
		return revision, nil
	}
	return "", fmt.Errorf("cannot resolve revision %q (no refs/%s and no matching snapshot)", revision, revision)
}

// blobName returns the etag a snapshot entry points at: the basename of its
// symlink target. Empty when the entry is a plain copy rather than a symlink.
func blobName(pointer string) string {
	target, err := os.Readlink(pointer)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func fileModNano(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().UnixNano()
	}
	return 0
}

// importFile materializes one snapshot file at dst and returns its hashes. If a
// correct copy is already present (same size and matching etag), it is reused —
// making import resumable — otherwise the source is streamed and verified.
func importFile(src, dst string, size int64, etag string, bufSize int) (download.Hashes, error) {
	if st, err := os.Stat(dst); err == nil && st.Size() == size {
		if h, err := download.HashFileSelective(dst, size, bufSize, nil, true); err == nil && etagMatches(etag, h) {
			return h, nil
		}
	}
	h, err := copyAndHash(src, dst, size, bufSize)
	if err != nil {
		return download.Hashes{}, err
	}
	if !etagMatches(etag, h) {
		return download.Hashes{}, fmt.Errorf("content does not match blob %s", etag)
	}
	return h, nil
}

// etagMatches reports whether hashes are consistent with a content-addressed
// blob name (SHA-256 for 64-hex, git SHA-1 for 40-hex). An unrecognized etag
// (e.g. a plain-copied snapshot with no symlink) is accepted.
func etagMatches(etag string, h download.Hashes) bool {
	switch {
	case isHex(etag, 64):
		return strings.EqualFold(etag, h.SHA256)
	case isHex(etag, 40):
		return strings.EqualFold(etag, h.GitSHA1)
	default:
		return true
	}
}

// copyAndHash streams src to dst once, computing the SHA-256, SHA-1, and git
// blob SHA-1 during the copy.
func copyAndHash(src, dst string, size int64, bufSize int) (download.Hashes, error) {
	in, err := os.Open(src)
	if err != nil {
		return download.Hashes{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return download.Hashes{}, err
	}
	sha256h := sha256.New()
	sha1h := sha1.New()
	gith := sha1.New()
	_, _ = io.WriteString(gith, "blob "+strconv.FormatInt(size, 10)+"\x00")
	writer := io.MultiWriter(out, sha256h, sha1h, gith)
	if bufSize < 32<<10 {
		bufSize = 32 << 10
	}
	if _, err := io.CopyBuffer(writer, in, make([]byte, bufSize)); err != nil {
		_ = out.Close()
		return download.Hashes{}, err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return download.Hashes{}, err
	}
	if err := out.Close(); err != nil {
		return download.Hashes{}, err
	}
	return download.Hashes{
		SHA256:  hex.EncodeToString(sha256h.Sum(nil)),
		SHA1:    hex.EncodeToString(sha1h.Sum(nil)),
		GitSHA1: hex.EncodeToString(gith.Sum(nil)),
	}, nil
}
