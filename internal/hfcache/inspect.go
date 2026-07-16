package hfcache

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/download"
)

// ParseRepoFolder reverses RepoFolderName. It is best-effort: repo owners or
// names containing "--" cannot be disambiguated, which does not occur for Hub
// repositories in practice.
func ParseRepoFolder(name string) (repoType, repoID string, ok bool) {
	parts := strings.Split(name, repoIDSeparator)
	if len(parts) < 2 {
		return "", "", false
	}
	t := strings.TrimSuffix(parts[0], "s")
	if t != "model" && t != "dataset" {
		return "", "", false
	}
	return t, strings.Join(parts[1:], "/"), true
}

// CachedRepo describes one repository found in an HF cache root.
type CachedRepo struct {
	RepoType string
	RepoID   string
	Storage  string
	Commits  []string
	Refs     map[string]string // revision -> commit
	Blobs    int
	Bytes    int64
}

// ListRepos enumerates the repositories stored under an HF cache root.
func ListRepos(cacheRoot string) ([]CachedRepo, error) {
	if cacheRoot == "" {
		cacheRoot = DefaultCacheRoot()
	}
	entries, err := os.ReadDir(cacheRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var repos []CachedRepo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, id, ok := ParseRepoFolder(e.Name())
		if !ok {
			continue
		}
		storage := filepath.Join(cacheRoot, e.Name())
		r := CachedRepo{RepoType: t, RepoID: id, Storage: storage, Refs: map[string]string{}}
		if snaps, err := os.ReadDir(filepath.Join(storage, "snapshots")); err == nil {
			for _, s := range snaps {
				if s.IsDir() {
					r.Commits = append(r.Commits, s.Name())
				}
			}
		}
		refsRoot := filepath.Join(storage, "refs")
		_ = filepath.WalkDir(refsRoot, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(refsRoot, p)
			if err != nil {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil {
				r.Refs[filepath.ToSlash(rel)] = strings.TrimSpace(string(b))
			}
			return nil
		})
		if blobs, err := os.ReadDir(filepath.Join(storage, "blobs")); err == nil {
			for _, b := range blobs {
				if info, err := b.Info(); err == nil && info.Mode().IsRegular() {
					r.Blobs++
					r.Bytes += info.Size()
				}
			}
		}
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].RepoType != repos[j].RepoType {
			return repos[i].RepoType < repos[j].RepoType
		}
		return repos[i].RepoID < repos[j].RepoID
	})
	return repos, nil
}

// VerifyReport summarizes a VerifyStorage run.
type VerifyReport struct {
	Blobs    int
	Bytes    int64
	Failed   int
	Failures []string
}

// VerifyStorage rehashes every blob and checks it against its content-addressed
// name, then confirms that snapshot pointers resolve. It needs no manifest, so
// it can validate a cache received across an air gap on its own.
func VerifyStorage(storage string, bufSize int) (VerifyReport, error) {
	var rep VerifyReport
	blobsDir := filepath.Join(storage, "blobs")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		return rep, err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		name := e.Name()
		h, err := download.HashFileSelective(filepath.Join(blobsDir, name), info.Size(), bufSize, nil, true)
		if err != nil {
			rep.Failed++
			rep.Failures = append(rep.Failures, name+": "+err.Error())
			continue
		}
		rep.Blobs++
		rep.Bytes += info.Size()
		if !etagMatches(name, h) && (isHex(name, 64) || isHex(name, 40)) {
			rep.Failed++
			rep.Failures = append(rep.Failures, name+": content does not match blob name")
		}
	}
	snapRoot := filepath.Join(storage, "snapshots")
	_ = filepath.WalkDir(snapRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, err := os.Stat(p); err != nil { // follows symlink; fails if the blob is missing
			rel, _ := filepath.Rel(storage, p)
			rep.Failed++
			rep.Failures = append(rep.Failures, filepath.ToSlash(rel)+": broken pointer")
		}
		return nil
	})
	return rep, nil
}

// GCReport summarizes an orphan-blob collection over an HF cache.
type GCReport struct {
	Repos        int      `json:"repos"`
	Blobs        int      `json:"blobs"`
	Orphans      int      `json:"orphans"`
	OrphanBytes  int64    `json:"orphan_bytes"`
	Removed      int      `json:"removed"`
	RemovedBytes int64    `json:"removed_bytes"`
	OrphanNames  []string `json:"orphan_names,omitempty"`
}

// GCCache scans every repository in an HF cache root for blobs that no snapshot
// references and, when apply is true, deletes them. Snapshots point at blobs
// through relative symlinks, so a blob whose basename is not the target of any
// snapshot link is unreachable (typically left behind by a superseded revision).
func GCCache(cacheRoot string, apply bool) (GCReport, error) {
	if cacheRoot == "" {
		cacheRoot = DefaultCacheRoot()
	}
	var rep GCReport
	repos, err := ListRepos(cacheRoot)
	if err != nil {
		return rep, err
	}
	for _, r := range repos {
		rep.Repos++
		if err := gcStorage(r.Storage, apply, &rep); err != nil {
			return rep, err
		}
	}
	sort.Strings(rep.OrphanNames)
	return rep, nil
}

// GCStorage runs orphan collection over a single repository storage folder.
func GCStorage(storage string, apply bool) (GCReport, error) {
	var rep GCReport
	rep.Repos = 1
	if err := gcStorage(storage, apply, &rep); err != nil {
		return rep, err
	}
	sort.Strings(rep.OrphanNames)
	return rep, nil
}

func gcStorage(storage string, apply bool, rep *GCReport) error {
	blobsDir := filepath.Join(storage, "blobs")
	entries, err := os.ReadDir(blobsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	referenced := map[string]bool{}
	snapRoot := filepath.Join(storage, "snapshots")
	_ = filepath.WalkDir(snapRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return nil
		}
		referenced[filepath.Base(target)] = true
		return nil
	})
	base := filepath.Base(storage)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		rep.Blobs++
		if referenced[e.Name()] {
			continue
		}
		rep.Orphans++
		rep.OrphanBytes += info.Size()
		rep.OrphanNames = append(rep.OrphanNames, filepath.ToSlash(filepath.Join(base, "blobs", e.Name())))
		if apply {
			if err := os.Remove(filepath.Join(blobsDir, e.Name())); err != nil {
				return err
			}
			rep.Removed++
			rep.RemovedBytes += info.Size()
		}
	}
	return nil
}

// ArchiveStorage writes a tar of the repository storage folder to out,
// preserving the relative symlinks so it can be unpacked and used as-is.
func ArchiveStorage(storage string, out io.Writer) error {
	tw := tar.NewWriter(out)
	base := filepath.Dir(storage)
	err := filepath.WalkDir(storage, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = tw.Close()
		return err
	}
	return tw.Close()
}
