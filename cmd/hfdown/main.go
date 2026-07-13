package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ziozzang/hfdownload/internal/download"
	"github.com/ziozzang/hfdownload/internal/hub"
	"github.com/ziozzang/hfdownload/internal/progress"
	"github.com/ziozzang/hfdownload/internal/state"
)

const version = "0.2.3"

type settings struct {
	Endpoint           string   `json:"endpoint"`
	Revision           string   `json:"revision"`
	Output             string   `json:"output"`
	Parts              int      `json:"parts"`
	MultipartThreshold int64    `json:"multipart_threshold"`
	BufferSize         int      `json:"buffer_size"`
	Retries            int      `json:"retries"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
	Resume             bool     `json:"resume"`
	TokenEnv           string   `json:"token_env"`
	Token              string   `json:"-"`
	Tag                string   `json:"-"`
	Filters            []string `json:"filters,omitempty"`
}

type queueFile struct {
	OutputRoot string     `json:"output_root"`
	Jobs       []queueJob `json:"jobs"`
}

type queueJob struct {
	Repo               string   `json:"repo"`
	Output             string   `json:"output,omitempty"`
	Revision           string   `json:"revision,omitempty"`
	Parts              *int     `json:"parts,omitempty"`
	MultipartThreshold *int64   `json:"multipart_threshold,omitempty"`
	BufferSize         *int     `json:"buffer_size,omitempty"`
	Retries            *int     `json:"retries,omitempty"`
	Resume             *bool    `json:"resume,omitempty"`
	RepoType           string   `json:"type,omitempty"`
	Filters            []string `json:"filters,omitempty"`
}

func defaults() settings {
	return settings{Endpoint: "https://huggingface.co", Revision: "main", Output: "", Parts: 4,
		MultipartThreshold: 64 << 20, BufferSize: 1 << 20, Retries: 5, TimeoutSeconds: 30,
		Resume: true, TokenEnv: "HF_TOKEN"}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return flag.ErrHelp
	}
	switch args[0] {
	case "download", "dn", "d":
		return downloadCommand(ctx, args[1:])
	case "dataset", "ds":
		return datasetCommand(ctx, args[1:])
	case "batch":
		return batchCommand(ctx, args[1:])
	case "verify":
		return verifyCommand(args[1:])
	case "verify-batch":
		return verifyBatchCommand(args[1:])
	case "status":
		return statusCommand(args[1:])
	case "version", "--version", "-version", "-v", "-V":
		if len(args) != 1 {
			return fmt.Errorf("usage: hfdown version")
		}
		printVersion(os.Stdout)
		return nil
	case "help":
		return helpCommand(ctx, args[1:])
	case "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "hfdown %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
}

func helpCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: hfdown help [COMMAND]")
	}
	var err error
	switch args[0] {
	case "download", "dn", "d":
		err = repositoryCommand(ctx, []string{"-h"}, hub.RepoTypeModel, "download")
	case "dataset", "ds":
		err = repositoryCommand(ctx, []string{"-h"}, hub.RepoTypeDataset, "dataset")
	case "batch":
		err = batchCommand(ctx, []string{"-h"})
	case "verify":
		err = verifyCommand([]string{"-h"})
	case "verify-batch":
		err = verifyBatchCommand([]string{"-h"})
	case "status":
		err = statusCommand([]string{"-h"})
	case "version":
		printVersion(os.Stdout)
		return nil
	case "help":
		usage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown help topic %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

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
			return fmt.Errorf("usage: hfdown %s [options] REPO", commandName)
		}
	} else if fs.NArg() != 0 {
		return fmt.Errorf("repository supplied both with --repo and as an argument")
	}
	return syncRepository(ctx, cfg, repo, repoType)
}

func batchCommand(ctx context.Context, args []string) error {
	cfg, configPath, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var queuePath, listPath, outputRoot, defaultRepoType string
	var continueOnError bool
	addTransferFlags(fs, &cfg, &configPath)
	fs.StringVar(&queuePath, "queue", "", "JSON queue or line-based repository list")
	fs.StringVar(&listPath, "list", "", "line-based repository list (';' comments and blank lines allowed)")
	fs.StringVar(&outputRoot, "output-root", "", "root for automatically named repository directories")
	fs.StringVar(&defaultRepoType, "type", "model", "repository type for list entries: model or dataset")
	fs.BoolVar(&continueOnError, "continue-on-error", false, "continue with remaining jobs after a failure")
	if err := fs.Parse(args); err != nil {
		return err
	}
	applyTag(&cfg)
	if err := hub.RepoType(defaultRepoType).Validate(); err != nil {
		return err
	}
	if queuePath != "" && listPath != "" {
		return fmt.Errorf("use only one of --queue and --list")
	}
	if listPath != "" {
		queuePath = listPath
	}
	if queuePath == "" {
		if fs.NArg() == 1 {
			queuePath = fs.Arg(0)
		} else {
			return fmt.Errorf("usage: hfdown batch [options] --queue FILE")
		}
	} else if fs.NArg() != 0 {
		return fmt.Errorf("queue supplied both with --queue and as an argument")
	}
	b, err := os.ReadFile(queuePath)
	if err != nil {
		return err
	}
	q, err := parseQueueData(b)
	if err != nil {
		return err
	}
	if outputRoot != "" {
		q.OutputRoot = outputRoot
	}
	if len(q.Jobs) == 0 {
		return fmt.Errorf("queue contains no jobs")
	}
	fmt.Fprintf(os.Stderr, "batch plan: %d repositories\n", len(q.Jobs))
	var failed []string
	for i, job := range q.Jobs {
		if job.Repo == "" {
			return fmt.Errorf("queue job %d has no repo", i+1)
		}
		normalizedRepo, err := hub.NormalizeRepoID(job.Repo)
		if err != nil {
			return fmt.Errorf("queue job %d: %w", i+1, err)
		}
		jobCfg := cfg
		jobRepoType := hub.RepoType(defaultRepoType)
		if job.RepoType != "" {
			jobRepoType = hub.RepoType(job.RepoType)
			if err := jobRepoType.Validate(); err != nil {
				return fmt.Errorf("queue job %d: %w", i+1, err)
			}
		}
		if job.Revision != "" {
			jobCfg.Revision = job.Revision
		}
		if job.Parts != nil {
			jobCfg.Parts = *job.Parts
		}
		if job.MultipartThreshold != nil {
			jobCfg.MultipartThreshold = *job.MultipartThreshold
		}
		if job.BufferSize != nil {
			jobCfg.BufferSize = *job.BufferSize
		}
		if job.Retries != nil {
			jobCfg.Retries = *job.Retries
		}
		if job.Resume != nil {
			jobCfg.Resume = *job.Resume
		}
		if job.Filters != nil {
			jobCfg.Filters = append([]string(nil), job.Filters...)
		}
		if job.Output != "" {
			jobCfg.Output = job.Output
		} else {
			outputRoot := q.OutputRoot
			if outputRoot == "" {
				outputRoot = "."
			}
			jobCfg.Output = filepath.Join(outputRoot, hub.LocalDirectoryName(normalizedRepo))
		}
		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s %s -> %s\n", i+1, len(q.Jobs), jobRepoType, normalizedRepo, jobCfg.Output)
		if err := syncRepository(ctx, jobCfg, normalizedRepo, jobRepoType); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", job.Repo, err))
			if !continueOnError {
				return errors.New(failed[0])
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d queue job(s) failed:\n  %s", len(failed), strings.Join(failed, "\n  "))
	}
	fmt.Fprintf(os.Stderr, "batch complete: %d/%d repositories\n", len(q.Jobs), len(q.Jobs))
	return nil
}

func addTransferFlags(fs *flag.FlagSet, cfg *settings, configPath *string) {
	fs.StringVar(&cfg.Revision, "revision", cfg.Revision, "branch, tag, or commit")
	fs.StringVar(&cfg.Tag, "tag", "", "tag name (overrides --revision)")
	fs.Var(stringSliceValue{&cfg.Filters}, "filter", "include glob; repeat for multiple patterns")
	fs.IntVar(&cfg.Parts, "parts", cfg.Parts, "parallel ranges per large file (1 disables multipart)")
	fs.Var(byteSizeValue{&cfg.MultipartThreshold}, "multipart-threshold", "minimum size to split (for example 64MiB)")
	fs.Var(byteSizeIntValue{&cfg.BufferSize}, "buffer-size", "memory buffer per part (for example 1MiB)")
	fs.IntVar(&cfg.Retries, "retries", cfg.Retries, "retries per range")
	fs.IntVar(&cfg.TimeoutSeconds, "timeout", cfg.TimeoutSeconds, "HTTP response-header timeout in seconds")
	fs.BoolVar(&cfg.Resume, "resume", cfg.Resume, "resume compatible partial downloads")
	fs.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "Hugging Face Hub endpoint")
	fs.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the access token")
	fs.StringVar(&cfg.Token, "token", "", "Hugging Face access token (prefer --token-env for security)")
	fs.StringVar(configPath, "config", *configPath, "JSON config file (default: .hfdown.json if present)")
}

func parseQueueData(data []byte) (queueFile, error) {
	content := strings.TrimPrefix(string(data), "\ufeff")
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		var q queueFile
		if err := json.Unmarshal([]byte(trimmed), &q); err != nil {
			return queueFile{}, fmt.Errorf("JSON queue parse: %w", err)
		}
		return q, nil
	}

	var q queueFile
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		repoID, err := hub.NormalizeRepoID(line)
		if err != nil {
			return queueFile{}, fmt.Errorf("repository list line %d: %w", lineNumber, err)
		}
		q.Jobs = append(q.Jobs, queueJob{Repo: repoID})
	}
	if err := scanner.Err(); err != nil {
		return queueFile{}, fmt.Errorf("repository list read: %w", err)
	}
	return q, nil
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
		Retries: cfg.Retries, Resume: cfg.Resume,
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

func verifyCommand(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	output := fs.String("output", ".", "downloaded repository directory")
	force := fs.Bool("force", false, "rehash every file even when metadata is unchanged")
	buffer := int64(1 << 20)
	fs.Var(byteSizeValue{&buffer}, "buffer-size", "hashing buffer size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return verifyDirectory(*output, *force, int(buffer))
}

func verifyBatchCommand(args []string) error {
	fs := flag.NewFlagSet("verify-batch", flag.ContinueOnError)
	rootFlag := fs.String("root", ".", "root directory containing downloaded repositories")
	force := fs.Bool("force", false, "rehash every file even when metadata is unchanged")
	failFast := fs.Bool("fail-fast", false, "stop after the first repository verification failure")
	buffer := int64(1 << 20)
	fs.Var(byteSizeValue{&buffer}, "buffer-size", "hashing buffer size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	var repositories []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || (entry.Name() != ".metadata" && entry.Name() != "hfdown-metadata" && entry.Name() != ".hfdown") {
			return nil
		}
		manifestPath := filepath.Join(path, "manifest.json")
		if st, statErr := os.Stat(manifestPath); statErr == nil && st.Mode().IsRegular() {
			repositories = append(repositories, filepath.Dir(path))
		}
		return filepath.SkipDir
	})
	if err != nil {
		return err
	}
	if len(repositories) == 0 {
		return fmt.Errorf("no hfdown repository directories found under %s", root)
	}
	sort.Strings(repositories)
	var failures []string
	for i, repositoryDir := range repositories {
		fmt.Fprintf(os.Stderr, "\n[%d/%d] verifying %s\n", i+1, len(repositories), repositoryDir)
		if err := verifyDirectory(repositoryDir, *force, int(buffer)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", repositoryDir, err))
			if *failFast {
				break
			}
		}
	}
	fmt.Printf("batch verify: repositories=%d passed=%d failed=%d\n", len(repositories), len(repositories)-len(failures), len(failures))
	if len(failures) > 0 {
		return fmt.Errorf("%d repository verification(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

func verifyDirectory(output string, force bool, buffer int) error {
	root, err := filepath.Abs(output)
	if err != nil {
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
	manifestPath := filepath.Join(stateDir, "manifest.json")
	m, err := state.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no manifest found at %s", manifestPath)
	}
	history := state.VerifyHistory{StartedAt: time.Now().UTC(), RepoType: m.RepoType, RepoID: m.RepoID, Revision: m.Revision, CommitSHA: m.CommitSHA, Forced: force}
	now := time.Now().UTC()
	for _, rec := range state.SortedFiles(m) {
		target, pathErr := download.SafeTarget(root, rec.Path)
		if pathErr != nil {
			history.Failed++
			history.Failures = append(history.Failures, pathErr.Error())
			continue
		}
		st, statErr := os.Stat(target)
		if statErr != nil {
			history.Failed++
			history.Failures = append(history.Failures, rec.Path+": "+statErr.Error())
			continue
		}
		if !force && rec.LocalSHA1 != "" && st.Mode().IsRegular() && st.Size() == rec.Size && st.ModTime().UnixNano() == rec.ModTimeUnixNano {
			history.Skipped++
			history.Passed++
			continue
		}
		history.Checked++
		bar := progress.New(os.Stderr, rec.Size, "verify "+rec.Path)
		hashes, hashErr := download.HashFileSelective(target, rec.Size, buffer, bar, rec.RemoteLFSSHA256 == "")
		bar.Finish()
		if hashErr != nil || !strings.EqualFold(hashes.SHA256, rec.LocalSHA256) || (rec.LocalSHA1 != "" && !strings.EqualFold(hashes.SHA1, rec.LocalSHA1)) || (rec.RemoteLFSSHA256 != "" && !strings.EqualFold(hashes.SHA256, rec.RemoteLFSSHA256)) || (rec.RemoteLFSSHA256 == "" && rec.RemoteBlobSHA1 != "" && !strings.EqualFold(hashes.GitSHA1, rec.RemoteBlobSHA1)) {
			history.Failed++
			msg := rec.Path + ": hash mismatch"
			if hashErr != nil {
				msg = rec.Path + ": " + hashErr.Error()
			}
			rec.VerificationError = msg
			rec.VerificationFailedAt = &now
			history.Failures = append(history.Failures, msg)
			continue
		}
		history.Passed++
		rec.LocalSHA256, rec.LocalSHA1, rec.LocalGitSHA1 = hashes.SHA256, hashes.SHA1, hashes.GitSHA1
		rec.ModTimeUnixNano, rec.VerifiedAt = st.ModTime().UnixNano(), now
		rec.VerificationError, rec.VerificationFailedAt = "", nil
	}
	history.CompletedAt = time.Now().UTC()
	if history.Failed == 0 {
		m.LastVerifiedAt = &now
	}
	m.UpdatedAt = now
	if err := state.SaveJSONAtomic(manifestPath, m); err != nil {
		return err
	}
	if err := state.AppendHistory(filepath.Join(stateDir, "verification-history.jsonl"), history); err != nil {
		return err
	}
	if history.Failed == 0 {
		if err := state.WriteChecksumFile(filepath.Join(root, ".sha256"), m); err != nil {
			return err
		}
		if err := state.WriteSHA1ChecksumFile(filepath.Join(root, ".sha1sum"), m); err != nil {
			return err
		}
	}
	fmt.Printf("verify: passed=%d checked=%d cached=%d failed=%d\n", history.Passed, history.Checked, history.Skipped, history.Failed)
	if history.Failed > 0 {
		return fmt.Errorf("verification failed:\n  %s", strings.Join(history.Failures, "\n  "))
	}
	return nil
}

func statusCommand(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	output := fs.String("output", ".", "downloaded repository directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(*output)
	if err != nil {
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
	m, err := state.LoadManifest(filepath.Join(stateDir, "manifest.json"))
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no hfdown manifest in %s", root)
	}
	var bytes int64
	for _, f := range m.Files {
		bytes += f.Size
	}
	repoType := m.RepoType
	if repoType == "" {
		repoType = string(hub.RepoTypeModel)
	}
	fmt.Printf("type: %s\nrepo: %s\nrevision: %s\ncommit: %s\nfiles: %d\nsize: %s\nupdated: %s\n", repoType, m.RepoID, m.Revision, m.CommitSHA, len(m.Files), humanBytes(bytes), m.UpdatedAt.Format(time.RFC3339))
	if m.HubLastModified != "" {
		fmt.Printf("hub last modified: %s\n", m.HubLastModified)
	}
	if m.RepositoryCreatedAt != "" {
		fmt.Printf("repository created: %s\n", m.RepositoryCreatedAt)
	}
	if m.MetadataFetchedAt != nil {
		fmt.Printf("metadata fetched: %s\n", m.MetadataFetchedAt.Format(time.RFC3339))
	}
	if m.LastVerifiedAt != nil {
		fmt.Printf("last verified: %s\n", m.LastVerifiedAt.Format(time.RFC3339))
	}
	return nil
}

func stateDirectory(root string) (string, error) {
	current := filepath.Join(root, ".metadata")
	if st, err := os.Lstat(current); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return "", fmt.Errorf("invalid hfdown metadata directory %s", current)
		}
		if err := migrateStateLayout(root, current); err != nil {
			return "", err
		}
		return current, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, name := range []string{"hfdown-metadata", ".hfdown"} {
		legacy := filepath.Join(root, name)
		if st, err := os.Lstat(legacy); err == nil {
			if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				return "", fmt.Errorf("invalid legacy hfdown metadata directory %s", legacy)
			}
			if err := os.Rename(legacy, current); err != nil {
				return "", fmt.Errorf("migrate %s to %s: %w", legacy, current, err)
			}
			fmt.Fprintf(os.Stderr, "migrated metadata: %s -> %s\n", legacy, current)
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if err := migrateStateLayout(root, current); err != nil {
		return "", err
	}
	return current, nil
}

func migrateStateLayout(root, stateDir string) error {
	newTmp := filepath.Join(root, "tmp")
	embeddedTmp := filepath.Join(stateDir, "tmp")
	if st, err := os.Stat(embeddedTmp); err == nil && st.IsDir() {
		if _, err := os.Stat(newTmp); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(embeddedTmp, newTmp); err != nil {
				return err
			}
		} else if err == nil {
			entries, err := os.ReadDir(embeddedTmp)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := os.Rename(filepath.Join(embeddedTmp, entry.Name()), filepath.Join(newTmp, entry.Name())); err != nil {
					return err
				}
			}
			_ = os.Remove(embeddedTmp)
		} else {
			return err
		}
	}
	legacyPartials := filepath.Join(stateDir, "partials")
	entries, err := os.ReadDir(legacyPartials)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(entry.Name(), ".json")
		workDir := filepath.Join(newTmp, base)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			return err
		}
		oldState := filepath.Join(legacyPartials, base+".json")
		oldData := filepath.Join(legacyPartials, base+".data")
		if _, err := os.Stat(oldData); err == nil {
			if err := os.Rename(oldData, filepath.Join(workDir, "download.part")); err != nil {
				return err
			}
		}
		if err := os.Rename(oldState, filepath.Join(workDir, "state.json")); err != nil {
			return err
		}
	}
	if entries, err := os.ReadDir(legacyPartials); err == nil && len(entries) == 0 {
		_ = os.Remove(legacyPartials)
	}
	return nil
}

func recordCurrent(path string, remote hub.RepoFile, rec *state.FileRecord) bool {
	if rec == nil || rec.VerificationError != "" || rec.Size != remote.Size || rec.RemoteBlobSHA1 != remote.BlobID {
		return false
	}
	lfs := ""
	if remote.LFS != nil {
		lfs = remote.LFS.SHA256
	}
	if rec.RemoteLFSSHA256 != lfs || rec.LocalSHA256 == "" || rec.LocalSHA1 == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() == rec.Size && st.ModTime().UnixNano() == rec.ModTimeUnixNano
}

func verifyExisting(path string, remote hub.RepoFile, bufferSize int, sharedBar *progress.Bar) (download.Hashes, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() != remote.Size {
		return download.Hashes{}, false
	}
	bar := sharedBar
	ownedBar := bar == nil
	if ownedBar {
		bar = progress.New(os.Stderr, remote.Size, "check "+remote.Path)
	}
	hashes, err := download.HashFileSelective(path, remote.Size, bufferSize, bar, remote.LFS == nil)
	if ownedBar {
		bar.Finish()
	}
	return hashes, err == nil && download.CheckHashes(remote, hashes) == nil
}

func makeRecord(path string, remote hub.RepoFile, hashes download.Hashes, commit string) (*state.FileRecord, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	rec := &state.FileRecord{Path: remote.Path, Size: remote.Size, RemoteBlobSHA1: remote.BlobID,
		LocalSHA256: hashes.SHA256, LocalSHA1: hashes.SHA1, LocalGitSHA1: hashes.GitSHA1, ModTimeUnixNano: st.ModTime().UnixNano(),
		VerifiedAt: time.Now().UTC(), CommitSHA: commit}
	if remote.LFS != nil {
		rec.RemoteLFSSHA256 = remote.LFS.SHA256
	}
	return rec, nil
}

func loadSettings(args []string) (settings, string, error) {
	cfg := defaults()
	configPath := findFlagValue(args, "config")
	if configPath == "" {
		if _, err := os.Stat(".hfdown.json"); err == nil {
			configPath = ".hfdown.json"
		}
	}
	if configPath != "" {
		b, err := os.ReadFile(configPath)
		if err != nil {
			return cfg, configPath, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, configPath, fmt.Errorf("config parse: %w", err)
		}
	}
	return cfg, configPath, nil
}

func findFlagValue(args []string, name string) string {
	prefix := "--" + name + "="
	for i, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
		if arg == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func validateSettings(cfg settings) error {
	if cfg.Parts < 1 || cfg.Parts > 32 {
		return fmt.Errorf("parts must be between 1 and 32")
	}
	if cfg.MultipartThreshold < 0 {
		return fmt.Errorf("multipart-threshold cannot be negative")
	}
	if cfg.BufferSize < 32<<10 || cfg.BufferSize > 64<<20 {
		return fmt.Errorf("buffer-size must be between 32KiB and 64MiB")
	}
	if cfg.Retries < 0 || cfg.Retries > 100 {
		return fmt.Errorf("retries must be between 0 and 100")
	}
	if cfg.TimeoutSeconds < 1 {
		return fmt.Errorf("timeout must be positive")
	}
	if cfg.Endpoint == "" || cfg.Revision == "" || cfg.TokenEnv == "" {
		return fmt.Errorf("endpoint, revision, and token-env cannot be empty")
	}
	if _, err := normalizeFilters(cfg.Filters); err != nil {
		return err
	}
	return nil
}

func applyTag(cfg *settings) {
	if cfg.Tag != "" {
		cfg.Revision = cfg.Tag
	}
}

func normalizeFilters(expressions []string) ([]string, error) {
	var filters []string
	for _, expression := range expressions {
		for _, pattern := range strings.Split(expression, "|") {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			pattern = strings.ToLower(pattern)
			if _, err := path.Match(pattern, "validation"); err != nil {
				return nil, fmt.Errorf("invalid filter %q: %w", pattern, err)
			}
			filters = append(filters, pattern)
		}
	}
	return filters, nil
}

func filterRepoFiles(files []hub.RepoFile, expressions []string) ([]hub.RepoFile, error) {
	filters, err := normalizeFilters(expressions)
	if err != nil {
		return nil, err
	}
	if len(filters) == 0 {
		return append([]hub.RepoFile(nil), files...), nil
	}
	selected := make([]hub.RepoFile, 0)
	for _, file := range files {
		fullPath := strings.ToLower(file.Path)
		baseName := path.Base(fullPath)
		for _, pattern := range filters {
			target := fullPath
			if !strings.Contains(pattern, "/") {
				target = baseName
			}
			matched, _ := path.Match(pattern, target)
			if matched {
				selected = append(selected, file)
				break
			}
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no repository files matched filter(s): %s", strings.Join(filters, " | "))
	}
	return selected, nil
}

type stringSliceValue struct{ target *[]string }

func (v stringSliceValue) String() string {
	if v.target == nil {
		return ""
	}
	return strings.Join(*v.target, "|")
}

func (v stringSliceValue) Set(value string) error {
	*v.target = append(*v.target, value)
	return nil
}

type byteSizeValue struct{ target *int64 }

func (v byteSizeValue) String() string {
	if v.target == nil {
		return ""
	}
	return strconv.FormatInt(*v.target, 10)
}
func (v byteSizeValue) Set(s string) error {
	n, err := parseBytes(s)
	if err == nil {
		*v.target = n
	}
	return err
}

type byteSizeIntValue struct{ target *int }

func (v byteSizeIntValue) String() string {
	if v.target == nil {
		return ""
	}
	return strconv.Itoa(*v.target)
}
func (v byteSizeIntValue) Set(s string) error {
	n, err := parseBytes(s)
	if err != nil {
		return err
	}
	if int64(int(n)) != n {
		return fmt.Errorf("byte size %q overflows int", s)
	}
	*v.target = int(n)
	return nil
}

func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mul    int64
	}{{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"gb", 1_000_000_000}, {"mb", 1_000_000}, {"kb", 1_000}, {"b", 1}}
	lower := strings.ToLower(s)
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSpace(s[:len(s)-len(u.suffix)]), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid byte size %q", s)
			}
			if u.mul != 0 && n > (1<<63-1)/u.mul {
				return 0, fmt.Errorf("byte size %q overflows int64", s)
			}
			return n * u.mul, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	return n, nil
}

func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n)
	i := -1
	for value >= 1024 && i+1 < len(units) {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `hfdown - resumable, hash-verified Hugging Face repository downloader

Usage:
  hfdown COMMAND [options]

Commands:
  download, dn, d    Download or update a model repository
  dataset, ds        Download or update a dataset repository
  batch              Process a plain-text list or JSON queue
  verify             Verify one downloaded repository
  verify-batch       Recursively verify downloaded repositories
  status             Show stored repository status and revision
  version            Print version and target platform
  help               Show general or command-specific help

Common forms:
  hfdown d [options] OWNER/MODEL
  hfdown ds [options] OWNER/DATASET
  hfdown batch --list repositories.txt [options]
  hfdown batch --queue queue.json [options]
  hfdown verify --output DIR [--force]
  hfdown verify-batch --root DIR [--force]

Help and version:
  hfdown help [COMMAND]
  hfdown COMMAND --help
  hfdown version | --version | -v`)
}
