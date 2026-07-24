package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ziozzang/hftools/internal/download"
	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/identity"
	"github.com/ziozzang/hftools/internal/progress"
	"github.com/ziozzang/hftools/internal/sign"
	"github.com/ziozzang/hftools/internal/state"
)

func verifyCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	output := fs.String("output", ".", "downloaded repository directory")
	force := fs.Bool("force", false, "rehash every file even when metadata is unchanged")
	signFlag := fs.Bool("sign", homeAutoSign(), "sign .sha256 with the ~/.hftools identity after a clean verify (default: config.yaml auto_sign)")
	scanFlag := fs.Bool("scan", false, "also scan pickle/torch files for unsafe imports")
	verifySig := fs.Bool("verify-sig", false, "also verify the repository's stored signature")
	pubkey := fs.String("pubkey", "", "pinned public key for --verify-sig: a trusted name, hex, PEM, or file path")
	requireIdentity := fs.Bool("require-signed-identity", false, "with --verify-sig, fail if the signer label and time are not covered by the signature")
	buffer := int64(1 << 20)
	fs.Var(byteSizeValue{&buffer}, "buffer-size", "hashing buffer size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := watchInterrupt(ctx)
	defer stop()
	if err := verifyDirectory(ctx, *output, *force, int(buffer), *signFlag); err != nil {
		if errors.Is(err, context.Canceled) {
			return errInterrupted
		}
		return err
	}
	root, err := resolveDir(*output)
	if err != nil {
		return err
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	errs := extraRepoChecks(ctx, root, *scanFlag, *verifySig, cfg, *pubkey, *requireIdentity)
	if ctx.Err() != nil {
		return errInterrupted
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func verifyBatchCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-batch", flag.ContinueOnError)
	rootFlag := fs.String("root", ".", "root directory containing downloaded repositories")
	force := fs.Bool("force", false, "rehash every file even when metadata is unchanged")
	failFast := fs.Bool("fail-fast", false, "stop after the first repository verification failure")
	signFlag := fs.Bool("sign", homeAutoSign(), "sign each repository's .sha256 with the ~/.hftools identity after a clean verify (default: config.yaml auto_sign)")
	scanFlag := fs.Bool("scan", false, "also scan each repository's pickle/torch files for unsafe imports")
	verifySig := fs.Bool("verify-sig", false, "also verify each repository's stored signature")
	pubkey := fs.String("pubkey", "", "pinned public key for --verify-sig: a trusted name, hex, PEM, or file path")
	requireIdentity := fs.Bool("require-signed-identity", false, "with --verify-sig, fail if the signer label and time are not covered by the signature")
	buffer := int64(1 << 20)
	fs.Var(byteSizeValue{&buffer}, "buffer-size", "hashing buffer size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := watchInterrupt(ctx)
	defer stop()
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
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	var failures []string
	interrupted := false
	for i, repositoryDir := range repositories {
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		fmt.Fprintf(os.Stderr, "\n[%d/%d] verifying %s\n", i+1, len(repositories), repositoryDir)
		var repoErrs []string
		if verr := verifyDirectory(ctx, repositoryDir, *force, int(buffer), *signFlag); verr != nil {
			if errors.Is(verr, context.Canceled) {
				interrupted = true
				break
			}
			repoErrs = append(repoErrs, verr.Error())
		}
		repoErrs = append(repoErrs, extraRepoChecks(ctx, repositoryDir, *scanFlag, *verifySig, cfg, *pubkey, *requireIdentity)...)
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		if len(repoErrs) > 0 {
			failures = append(failures, fmt.Sprintf("%s: %s", repositoryDir, strings.Join(repoErrs, "; ")))
			if *failFast {
				break
			}
		}
	}
	fmt.Printf("batch verify: repositories=%d passed=%d failed=%d\n", len(repositories), len(repositories)-len(failures), len(failures))
	if interrupted {
		return errInterrupted
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d repository verification(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

// extraRepoChecks runs the optional pickle security scan and signature
// verification for a single repository (behind --scan / --verify-sig), printing a
// short per-repo summary and returning any failure messages (empty on success).
func extraRepoChecks(ctx context.Context, root string, doScan, doVerifySig bool, cfg *identity.Config, pubkeySpec string, requireIdentity bool) []string {
	var errs []string
	if doScan {
		reports, dangerous, warnings, err := scanRepositoryDir(ctx, root)
		if err != nil {
			errs = append(errs, "scan: "+err.Error())
		} else {
			fmt.Fprintf(os.Stderr, "  scan: %d file(s), %d critical, %d warning(s)\n", len(reports), dangerous, warnings)
			for _, rep := range reports {
				if rep.Dangerous() {
					printScanReport(rep, true)
				}
			}
			if dangerous > 0 {
				errs = append(errs, fmt.Sprintf("scan: %d dangerous import(s)", dangerous))
			}
		}
	}
	if doVerifySig {
		if stateDir, err := stateDirectory(root); err != nil {
			errs = append(errs, "sig: "+err.Error())
		} else if res, err := verifyRepoSignature(root, stateDir, cfg, pubkeySpec); err != nil {
			errs = append(errs, "sig: "+err.Error())
		} else if err := checkSignedIdentity(res.Record, cfg, requireIdentity); err != nil {
			errs = append(errs, "sig: "+err.Error())
		} else {
			line := "  sig: OK " + sign.ShortFingerprint(res.PublicKey)
			switch {
			case res.TrustLabel != "":
				line += " (trusted: " + res.TrustLabel + ")"
			case !res.Pinned:
				line += " (unpinned — integrity only)"
			}
			fmt.Fprintln(os.Stderr, line)
			// Name who signed it: a verification that does not say who is
			// answerable for the content cannot support an audit.
			fmt.Fprintln(os.Stderr, "       "+signerLine(res.Record))
		}
	}
	return errs
}

func verifyDirectory(ctx context.Context, output string, force bool, buffer int, autoSign bool) error {
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if autoSign {
			if err := autoSignRepo(root, stateDir); err != nil {
				return fmt.Errorf("auto-sign: %w", err)
			}
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
