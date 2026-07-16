package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ziozzang/hfdownload/internal/download"
	"github.com/ziozzang/hfdownload/internal/hub"
	"github.com/ziozzang/hfdownload/internal/progress"
	"github.com/ziozzang/hfdownload/internal/state"
)

func downloadCommand(ctx context.Context, args []string) error {
	return repositoryCommand(ctx, args, hub.RepoTypeModel, "download")
}

func datasetCommand(ctx context.Context, args []string) error {
	return repositoryCommand(ctx, args, hub.RepoTypeDataset, "dataset")
}

func repositoryCommand(ctx context.Context, args []string, repoType hub.RepoType, commandName string) error {
	cfg, configPath, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet(commandName, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var repo string
	fs.StringVar(&repo, "repo", "", "Hugging Face repository ID or URL")
	fs.StringVar(&cfg.Output, "output", cfg.Output, "destination directory (default: <owner>_<repo>)")
	addTransferFlags(fs, &cfg, &configPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	applyTag(&cfg)
	if repo == "" {
		if fs.NArg() == 1 {
			repo = fs.Arg(0)
		} else {
			return fmt.Errorf("usage: hftools %s [options] REPO", commandName)
		}
	} else if fs.NArg() != 0 {
		return fmt.Errorf("repository supplied both with --repo and as an argument")
	}
	if cfg.DryRun {
		return planRepository(ctx, cfg, repo, repoType)
	}
	return syncRepository(ctx, cfg, repo, repoType)
}

// planRepository resolves a repository and prints what a download would do,
// touching no files (it only reads an existing manifest to recognize cached
// files).
func planRepository(ctx context.Context, cfg settings, repoID string, repoType hub.RepoType) error {
	if err := repoType.Validate(); err != nil {
		return err
	}
	if err := validateSettings(cfg); err != nil {
		return err
	}
	var err error
	repoID, err = hub.NormalizeRepoID(repoID)
	if err != nil {
		return err
	}
	if cfg.Output == "" {
		cfg.Output = hub.LocalDirectoryName(repoID)
	}
	root, err := filepath.Abs(cfg.Output)
	if err != nil {
		return err
	}
	m, err := loadExistingManifest(root)
	if err != nil {
		return err
	}
	info, err := newHubClient(cfg).RepoInfo(ctx, repoType, repoID, cfg.Revision)
	if err != nil {
		return err
	}
	files, err := filterRepoFiles(info.Siblings, cfg.Filters)
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var total, cachedBytes int64
	var cachedFiles int
	for _, f := range files {
		total += f.Size
		target, terr := download.SafeTarget(root, f.Path)
		if terr != nil {
			return terr
		}
		var rec *state.FileRecord
		if m != nil {
			rec = m.Files[f.Path]
		}
		if recordCurrent(target, f, rec) {
			cachedFiles++
			cachedBytes += f.Size
		}
	}
	fmt.Printf("plan for %s@%s\ndestination: %s\ncommit: %s\n", repoID, cfg.Revision, root, info.SHA)
	fmt.Printf("files: %d • total: %s • cached: %d (%s) • to download: %d (%s)\n",
		len(files), humanBytes(total), cachedFiles, humanBytes(cachedBytes), len(files)-cachedFiles, humanBytes(total-cachedBytes))
	return nil
}

func syncRepo(ctx context.Context, cfg settings, repoID string) error {
	return syncRepository(ctx, cfg, repoID, hub.RepoTypeModel)
}

func syncRepository(ctx context.Context, cfg settings, repoID string, repoType hub.RepoType) error {
	if err := repoType.Validate(); err != nil {
		return err
	}
	if err := validateSettings(cfg); err != nil {
		return err
	}
	var err error
	repoID, err = hub.NormalizeRepoID(repoID)
	if err != nil {
		return err
	}
	if cfg.Output == "" {
		cfg.Output = hub.LocalDirectoryName(repoID)
	}
	root, err := filepath.Abs(cfg.Output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "destination %s\n", root)
	manifestPath := filepath.Join(stateDir, "manifest.json")
	m, err := state.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	if m != nil {
		existingType := hub.RepoType(m.RepoType)
		if existingType == "" {
			existingType = hub.RepoTypeModel
		}
		if m.RepoID != repoID || existingType != repoType {
			return fmt.Errorf("output already belongs to %s %s; choose another directory", existingType, m.RepoID)
		}
	}
	token := cfg.Token
	if token == "" {
		token = os.Getenv(cfg.TokenEnv)
	}
	client := hub.New(cfg.Endpoint, token, time.Duration(cfg.TimeoutSeconds)*time.Second)
	client.Retries = cfg.Retries
	client.RetryMinWait = time.Duration(cfg.RetryMinWaitSeconds) * time.Second
	client.RetryMaxWait = time.Duration(cfg.RetryMaxWaitSeconds) * time.Second
	fmt.Fprintf(os.Stderr, "resolving %s@%s...\n", repoID, cfg.Revision)
	info, err := client.RepoInfo(ctx, repoType, repoID, cfg.Revision)
	if err != nil {
		return err
	}
	metadataFetchedAt := time.Now().UTC()
	repositoryMetadata := state.RepositoryMetadata{
		Version: 1, RepoType: string(repoType), FetchedAt: metadataFetchedAt, Endpoint: cfg.Endpoint, RepoID: repoID,
		RequestedRevision: cfg.Revision, ResolvedCommitSHA: info.SHA,
		LastModified: info.LastModified, CreatedAt: info.CreatedAt, Payload: info.RawMetadata,
	}
	if err := state.SaveJSONAtomic(filepath.Join(stateDir, "repository.json"), repositoryMetadata); err != nil {
		return err
	}
	metadataEvent := state.RepositoryMetadataEvent{
		FetchedAt: metadataFetchedAt, RepoType: string(repoType), Endpoint: cfg.Endpoint, RepoID: repoID,
		RequestedRevision: cfg.Revision, ResolvedCommitSHA: info.SHA,
		LastModified: info.LastModified, CreatedAt: info.CreatedAt,
	}
	if err := state.AppendJSONLine(filepath.Join(stateDir, "repository-history.jsonl"), metadataEvent); err != nil {
		return err
	}
	if m == nil {
		m = state.NewManifest(repoID, cfg.Revision, info.SHA)
	}
	m.RepoType = string(repoType)
	m.Filters = append([]string(nil), cfg.Filters...)
	m.Revision, m.CommitSHA = cfg.Revision, info.SHA
	m.HubLastModified, m.RepositoryCreatedAt, m.MetadataFetchedAt = info.LastModified, info.CreatedAt, &metadataFetchedAt

	files, err := filterRepoFiles(info.Siblings, cfg.Filters)
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	seen := make(map[string]bool, len(files))
	targets := make(map[string]string, len(files))
	cachedPlan := make(map[string]bool, len(files))
	var total, cachedBytes int64
	var cachedFiles int
	for _, f := range files {
		if seen[f.Path] {
			return fmt.Errorf("duplicate repository path %q", f.Path)
		}
		seen[f.Path] = true
		total += f.Size
		target, err := download.SafeTarget(root, f.Path)
		if err != nil {
			return err
		}
		targets[f.Path] = target
		if recordCurrent(target, f, m.Files[f.Path]) {
			cachedPlan[f.Path] = true
			cachedFiles++
			cachedBytes += f.Size
		}
	}
	remainingFiles := len(files) - cachedFiles
	remainingBytes := total - cachedBytes
	fmt.Fprintf(os.Stderr, "commit %s\n", info.SHA)
	fmt.Fprintf(os.Stderr, "plan: %d files • %s total • %d cached (%s) • %d remaining (%s)\n",
		len(files), humanBytes(total), cachedFiles, humanBytes(cachedBytes), remainingFiles, humanBytes(remainingBytes))
	// Persist the current known-good set before network transfer, then refresh it
	// after every file succeeds. An interrupted run therefore leaves a usable
	// manifest, .sha256, and .sha1sum for everything completed so far.
	m.UpdatedAt = metadataFetchedAt
	if err := saveDownloadCheckpoint(manifestPath, root, m); err != nil {
		return err
	}
	overall := progress.New(os.Stderr, total, fmt.Sprintf("%d/%d ready", cachedFiles, len(files)))
	overall.SetDone(cachedBytes)
	defer overall.Finish()
	var networkBytes, resumedBytes atomic.Int64
	d := download.Downloader{Client: client, Root: root, StateDir: stateDir, TempDir: filepath.Join(root, "tmp"), RepoType: repoType, Options: download.Options{
		Parts: cfg.Parts, MultipartThreshold: cfg.MultipartThreshold, BufferSize: cfg.BufferSize,
		Retries:      cfg.Retries,
		RetryMinWait: time.Duration(cfg.RetryMinWaitSeconds) * time.Second,
		RetryMaxWait: time.Duration(cfg.RetryMaxWaitSeconds) * time.Second,
		StallTimeout: time.Duration(cfg.StallTimeoutSeconds) * time.Second,
		MinSpeed:     cfg.MinSpeed, MinSpeedWindow: time.Duration(cfg.MinSpeedWindowSeconds) * time.Second, Resume: cfg.Resume,
	}, Progress: overall,
		OnNetworkBytes: func(n int64) { networkBytes.Add(n) },
		OnResumedBytes: func(n int64) { resumedBytes.Add(n) },
	}
	completedFiles := cachedFiles
	var skipped int64
	var verifiedExisting, downloadedFiles int
	for i, remote := range files {
		target := targets[remote.Path]
		if cachedPlan[remote.Path] {
			rec := m.Files[remote.Path]
			rec.CommitSHA = info.SHA
			skipped += remote.Size
			overall.Logf("[%d/%d] cached %s\n", i+1, len(files), remote.Path)
			continue
		}
		overall.SetLabel(fmt.Sprintf("scan %d/%d %s", completedFiles+1, len(files), remote.Path))
		beforeScan := overall.Done()
		hashes, existingOK := verifyExisting(target, remote, cfg.BufferSize, overall)
		scannedBytes := overall.Done() - beforeScan
		if existingOK {
			m.Files[remote.Path], err = makeRecord(target, remote, hashes, info.SHA)
			if err != nil {
				return err
			}
			verifiedExisting++
			completedFiles++
			overall.SetLabel(fmt.Sprintf("%d/%d ready", completedFiles, len(files)))
			m.UpdatedAt = time.Now().UTC()
			if err := saveDownloadCheckpoint(manifestPath, root, m); err != nil {
				return err
			}
			overall.Logf("[%d/%d] verified existing %s\n", i+1, len(files), remote.Path)
			continue
		}
		if scannedBytes > 0 {
			overall.Add(-scannedBytes)
		}
		overall.SetLabel(fmt.Sprintf("fetch %d/%d %s", completedFiles+1, len(files), remote.Path))
		overall.Logf("[%d/%d] fetching %s (%s)\n", i+1, len(files), remote.Path, humanBytes(remote.Size))
		hashes, err := d.Download(ctx, repoID, info.SHA, remote)
		if err != nil {
			return err
		}
		m.Files[remote.Path], err = makeRecord(target, remote, hashes, info.SHA)
		if err != nil {
			return err
		}
		downloadedFiles++
		completedFiles++
		overall.SetLabel(fmt.Sprintf("%d/%d ready", completedFiles, len(files)))
		m.UpdatedAt = time.Now().UTC()
		if err := saveDownloadCheckpoint(manifestPath, root, m); err != nil {
			return err
		}
	}
	if len(cfg.Filters) == 0 {
		for path := range m.Files {
			if !seen[path] {
				delete(m.Files, path)
			}
		}
	}
	m.UpdatedAt = time.Now().UTC()
	if err := saveDownloadCheckpoint(manifestPath, root, m); err != nil {
		return err
	}
	overall.SetLabel(fmt.Sprintf("complete %d/%d", len(files), len(files)))
	overall.Finish()
	fmt.Fprintf(os.Stderr, "complete: %d files • cached %d (%s) • verified existing %d • downloaded %d • network %s • resumed %s\n",
		len(files), cachedFiles, humanBytes(skipped), verifiedExisting, downloadedFiles, humanBytes(networkBytes.Load()), humanBytes(resumedBytes.Load()))
	fmt.Fprintf(os.Stderr, "saved to %s\n", root)
	return nil
}

func saveDownloadCheckpoint(manifestPath, root string, m *state.Manifest) error {
	if err := state.SaveJSONAtomic(manifestPath, m); err != nil {
		return err
	}
	if err := state.WriteChecksumFile(filepath.Join(root, ".sha256"), m); err != nil {
		return err
	}
	return state.WriteSHA1ChecksumFile(filepath.Join(root, ".sha1sum"), m)
}
