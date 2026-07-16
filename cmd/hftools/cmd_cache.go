package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/hfcache"
	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/state"
)

// cacheExportCommand converts an hftools download directory into the
// huggingface_hub cache layout so it can be used offline by HF libraries.
func cacheExportCommand(args []string) error {
	fs := flag.NewFlagSet("cache-export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", ".", "hftools download directory to export")
	cache := fs.String("cache", "", "HF cache root (default: $HF_HUB_CACHE, $HF_HOME/hub, or ~/.cache/huggingface/hub)")
	copyBlobs := fs.Bool("copy", false, "copy blobs instead of hardlinking them from the source")
	archive := fs.String("archive", "", "also write a .tar bundle (and .sha256) of the exported repository to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveExisting(*output)
	if err != nil {
		return err
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		return err
	}
	m, err := state.LoadManifest(filepath.Join(stateDir, "manifest.json"))
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no hftools manifest in %s", root)
	}
	res, err := hfcache.Export(hfcache.ExportOptions{
		Manifest: m, SourceDir: root, CacheRoot: *cache, Copy: *copyBlobs, BufferSize: 1 << 20,
	})
	if err != nil {
		return err
	}
	for _, s := range res.SkippedMsg {
		fmt.Fprintf(os.Stderr, "skip: %s\n", s)
	}
	fmt.Printf("cache-export: repo=%s commit=%s files=%d new-blobs=%d size=%s\n", m.RepoID, m.CommitSHA, res.Files, res.NewBlobs, humanBytes(res.Bytes))
	fmt.Printf("cache root: %s\nrepository: %s\n", res.CacheRoot, res.Storage)
	if *archive != "" {
		if err := writeArchive(res.Storage, *archive); err != nil {
			return err
		}
		fmt.Printf("archive: %s (+ .sha256)\n", *archive)
	}
	return nil
}

// cacheImportCommand converts a huggingface_hub cache snapshot into hftools'
// flat layout, hashing and verifying every file and writing a fresh manifest.
func cacheImportCommand(args []string) error {
	fs := flag.NewFlagSet("cache-import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", "", "destination directory (default: <owner>_<repo>)")
	cache := fs.String("cache", "", "HF cache root (default: $HF_HUB_CACHE, $HF_HOME/hub, or ~/.cache/huggingface/hub)")
	repo := fs.String("repo", "", "repository ID or URL (owner/name)")
	repoType := fs.String("type", "model", "repository type: model or dataset")
	revision := fs.String("revision", "main", "branch, tag, or commit to import")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("cache-import requires --repo OWNER/NAME")
	}
	repoID, err := hub.NormalizeRepoID(*repo)
	if err != nil {
		return err
	}
	out := *output
	if out == "" {
		out = hub.LocalDirectoryName(repoID)
	}
	res, err := importRepo(*cache, repoID, *repoType, *revision, out)
	if err != nil {
		return err
	}
	fmt.Printf("cache-import: repo=%s commit=%s files=%d size=%s\n  source: %s\n  saved to %s\n",
		repoID, res.commit, res.files, humanBytes(res.bytes), res.storage, res.dest)
	return nil
}

// cacheListCommand prints the repositories stored in an HF cache root.
func cacheListCommand(args []string) error {
	fs := flag.NewFlagSet("cache-list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cache := fs.String("cache", "", "HF cache root (default: $HF_HUB_CACHE, $HF_HOME/hub, or ~/.cache/huggingface/hub)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := cacheRootOrDefault(*cache)
	repos, err := hfcache.ListRepos(root)
	if err != nil {
		return err
	}
	fmt.Printf("cache root: %s\n", root)
	if len(repos) == 0 {
		fmt.Println("no cached repositories")
		return nil
	}
	for _, r := range repos {
		fmt.Printf("%-8s %-45s blobs=%-4d size=%-10s commits=%d\n", r.RepoType, r.RepoID, r.Blobs, humanBytes(r.Bytes), len(r.Commits))
	}
	return nil
}

// cacheVerifyCommand rehashes cached blobs and checks them against their
// content-addressed names, verifying an air-gapped cache without a manifest.
func cacheVerifyCommand(args []string) error {
	fs := flag.NewFlagSet("cache-verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cache := fs.String("cache", "", "HF cache root (default: $HF_HUB_CACHE, $HF_HOME/hub, or ~/.cache/huggingface/hub)")
	repo := fs.String("repo", "", "verify only this repository (default: every cached repository)")
	repoType := fs.String("type", "model", "repository type for --repo: model or dataset")
	buffer := int64(1 << 20)
	fs.Var(byteSizeValue{&buffer}, "buffer-size", "hashing buffer size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := cacheRootOrDefault(*cache)
	type target struct{ id, storage string }
	var targets []target
	if *repo != "" {
		repoID, err := hub.NormalizeRepoID(*repo)
		if err != nil {
			return err
		}
		if err := hub.RepoType(*repoType).Validate(); err != nil {
			return err
		}
		targets = append(targets, target{repoID, filepath.Join(root, hfcache.RepoFolderName(*repoType, repoID))})
	} else {
		repos, err := hfcache.ListRepos(root)
		if err != nil {
			return err
		}
		for _, r := range repos {
			targets = append(targets, target{r.RepoID, r.Storage})
		}
		if len(targets) == 0 {
			return fmt.Errorf("no cached repositories under %s", root)
		}
	}
	var blobs, failed int
	for _, tg := range targets {
		rep, err := hfcache.VerifyStorage(tg.storage, int(buffer))
		if err != nil {
			return fmt.Errorf("%s: %w", tg.id, err)
		}
		blobs += rep.Blobs
		failed += rep.Failed
		status := "ok"
		if rep.Failed > 0 {
			status = fmt.Sprintf("FAILED (%d)", rep.Failed)
		}
		fmt.Printf("%-45s blobs=%-4d size=%-10s %s\n", tg.id, rep.Blobs, humanBytes(rep.Bytes), status)
		for _, f := range rep.Failures {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
	}
	fmt.Printf("cache-verify: repositories=%d blobs=%d failed=%d\n", len(targets), blobs, failed)
	if failed > 0 {
		return fmt.Errorf("%d cached blob(s) failed verification", failed)
	}
	return nil
}

// cacheImportBatchCommand imports every repository found in an HF cache root
// into flat directories, for restoring a whole air-gapped cache at once.
func cacheImportBatchCommand(args []string) error {
	fs := flag.NewFlagSet("cache-import-batch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cache := fs.String("cache", "", "HF cache root (default: $HF_HUB_CACHE, $HF_HOME/hub, or ~/.cache/huggingface/hub)")
	outputRoot := fs.String("output-root", ".", "directory to create per-repository flat directories under")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := cacheRootOrDefault(*cache)
	repos, err := hfcache.ListRepos(root)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return fmt.Errorf("no cached repositories under %s", root)
	}
	outRoot, err := filepath.Abs(*outputRoot)
	if err != nil {
		return err
	}
	var failures []string
	for i, r := range repos {
		revision, ok := pickRevision(r)
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: cannot pick a revision (%d commits, no refs)", r.RepoID, len(r.Commits)))
			continue
		}
		dest := filepath.Join(outRoot, hub.LocalDirectoryName(r.RepoID))
		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s %s@%s -> %s\n", i+1, len(repos), r.RepoType, r.RepoID, revision, dest)
		res, err := importRepo(root, r.RepoID, r.RepoType, revision, dest)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.RepoID, err))
			continue
		}
		fmt.Printf("imported %s commit=%s files=%d size=%s\n", r.RepoID, res.commit, res.files, humanBytes(res.bytes))
	}
	fmt.Printf("cache-import-batch: repositories=%d imported=%d failed=%d\n", len(repos), len(repos)-len(failures), len(failures))
	if len(failures) > 0 {
		return fmt.Errorf("%d import(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

type importSummary struct {
	commit, storage, dest string
	files                 int
	bytes                 int64
}

// importRepo imports one cached repository into destDir and writes its manifest
// and checksum files, returning a summary.
func importRepo(cacheRoot, repoID, repoType, revision, destDir string) (importSummary, error) {
	if err := hub.RepoType(repoType).Validate(); err != nil {
		return importSummary{}, err
	}
	root, err := filepath.Abs(destDir)
	if err != nil {
		return importSummary{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return importSummary{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return importSummary{}, err
	}
	m, res, err := hfcache.Import(hfcache.ImportOptions{
		CacheRoot: cacheRoot, RepoID: repoID, RepoType: repoType, Revision: revision, DestDir: root, BufferSize: 1 << 20,
	})
	if err != nil {
		return importSummary{}, err
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		return importSummary{}, err
	}
	if err := saveDownloadCheckpoint(filepath.Join(stateDir, "manifest.json"), root, m); err != nil {
		return importSummary{}, err
	}
	return importSummary{commit: res.Commit, storage: res.Storage, dest: root, files: res.Files, bytes: res.Bytes}, nil
}

// pickRevision chooses a revision to import a cached repo under: "main" when
// present, otherwise any ref, otherwise the sole snapshot commit.
func pickRevision(r hfcache.CachedRepo) (string, bool) {
	if _, ok := r.Refs["main"]; ok {
		return "main", true
	}
	if len(r.Refs) > 0 {
		keys := make([]string, 0, len(r.Refs))
		for k := range r.Refs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys[0], true
	}
	if len(r.Commits) == 1 {
		return r.Commits[0], true
	}
	return "", false
}

func cacheRootOrDefault(cache string) string {
	if cache != "" {
		return cache
	}
	return hfcache.DefaultCacheRoot()
}

func resolveExisting(dir string) (string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

func writeArchive(storage, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	sum := sha256.New()
	if err := hfcache.ArchiveStorage(storage, io.MultiWriter(f, sum)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	line := hex.EncodeToString(sum.Sum(nil)) + "  " + filepath.Base(path) + "\n"
	return os.WriteFile(path+".sha256", []byte(line), 0o644)
}
