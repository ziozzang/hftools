package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ziozzang/hftools/internal/download"
	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/progress"
	"github.com/ziozzang/hftools/internal/state"
)

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
		return fmt.Errorf("no hftools repository directories found under %s", root)
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
		return fmt.Errorf("no hftools manifest in %s", root)
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
