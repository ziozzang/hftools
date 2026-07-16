package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ziozzang/hftools/internal/download"
	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/progress"
	"github.com/ziozzang/hftools/internal/state"
)

func stateDirectory(root string) (string, error) {
	current := filepath.Join(root, ".metadata")
	if st, err := os.Lstat(current); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return "", fmt.Errorf("invalid hftools metadata directory %s", current)
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
				return "", fmt.Errorf("invalid legacy hftools metadata directory %s", legacy)
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
