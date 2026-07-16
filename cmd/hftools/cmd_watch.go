package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/state"
)

// watchCommand periodically re-syncs a repository, pulling any upstream changes.
// It keeps running across transient failures so an unattended mirror stays fresh.
func watchCommand(ctx context.Context, args []string) error {
	cfg, configPath, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var repo string
	fs.StringVar(&repo, "repo", "", "Hugging Face repository ID or URL")
	fs.StringVar(&cfg.Output, "output", cfg.Output, "destination directory")
	typeFlag := fs.String("type", "model", "repository type: model or dataset")
	interval := fs.Int("interval", 300, "seconds between checks")
	once := fs.Bool("once", false, "run a single sync and exit")
	addTransferFlags(fs, &cfg, &configPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	applyTag(&cfg)
	repoType, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	if repo == "" {
		if fs.NArg() == 1 {
			repo = fs.Arg(0)
		} else {
			return fmt.Errorf("usage: hftools watch [options] REPO")
		}
	} else if fs.NArg() != 0 {
		return fmt.Errorf("repository supplied both with --repo and as an argument")
	}
	if *interval < 1 {
		return fmt.Errorf("interval must be positive")
	}
	for {
		start := time.Now().UTC()
		fmt.Fprintf(os.Stderr, "== sync %s at %s ==\n", repo, start.Format(time.RFC3339))
		err := syncRepository(ctx, cfg, repo, repoType)
		if ctx.Err() != nil {
			return nil
		}
		if *once {
			return err
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync failed: %v (will retry)\n", err)
		}
		fmt.Fprintf(os.Stderr, "next check in %ds\n", *interval)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(*interval) * time.Second):
		}
	}
}

// repairCommand deep-verifies a repository to detect on-disk corruption, then
// re-fetches any missing or corrupt files at the exact commit already recorded,
// and confirms the result.
func repairCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", ".", "downloaded repository directory")
	quick := fs.Bool("quick", false, "skip the full rehash; only re-fetch missing files")
	buffer := int64(1 << 20)
	fs.Var(byteSizeValue{&buffer}, "buffer-size", "hashing buffer size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveDir(*output)
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
	repoType := hub.RepoType(m.RepoType)
	if repoType == "" {
		repoType = hub.RepoTypeModel
	}
	// Deep-verify to flag silent corruption (bytes changed but size/mtime intact).
	// A returned error just means files failed; the sync step below repairs them.
	if verr := verifyDirectory(root, !*quick, int(buffer)); verr != nil {
		fmt.Fprintf(os.Stderr, "verification found problems: %v\n", verr)
	}
	origRevision := m.Revision
	cfg := defaults()
	cfg.Output = root
	cfg.Revision = m.CommitSHA // pin the exact commit we already have
	cfg.Filters = append([]string(nil), m.Filters...)
	fmt.Fprintf(os.Stderr, "repairing %s at %s...\n", m.RepoID, m.CommitSHA)
	if err := syncRepository(ctx, cfg, m.RepoID, repoType); err != nil {
		return err
	}
	// Pinning to the commit SHA rewrote the manifest's revision label; restore
	// the original branch/tag name so serve and status keep resolving it.
	manifestPath := filepath.Join(stateDir, "manifest.json")
	if m2, lerr := state.LoadManifest(manifestPath); lerr == nil && m2 != nil && m2.Revision != origRevision {
		m2.Revision = origRevision
		if serr := saveDownloadCheckpoint(manifestPath, root, m2); serr != nil {
			return serr
		}
	}
	if err := verifyDirectory(root, false, int(buffer)); err != nil {
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}
