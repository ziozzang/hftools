package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ziozzang/hftools/internal/selfupdate"
)

// updateRepo is the GitHub repository self-update pulls release builds from.
const updateRepo = "ziozzang/hftools"

// updateCommand replaces the running binary with the latest (or a specified)
// GitHub release build, verifying it against the release SHA256SUMS first.
func updateCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	check := fs.Bool("check", false, "only report whether a newer version is available")
	force := fs.Bool("force", false, "reinstall even if already on the latest version")
	targetVer := fs.String("version", "", "install a specific release tag (for example v0.8.1) instead of the latest")
	repo := fs.String("repo", updateRepo, "GitHub owner/repo to update from")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	client := &http.Client{}
	token := os.Getenv("GITHUB_TOKEN")

	var rel *selfupdate.Release
	var err error
	if *targetVer != "" {
		rel, err = selfupdate.ReleaseByTag(ctx, client, "", *repo, *targetVer, token)
	} else {
		rel, err = selfupdate.LatestRelease(ctx, client, "", *repo, token)
	}
	if err != nil {
		return fmt.Errorf("look up release: %w", err)
	}
	latest := rel.Version()
	cmp := selfupdate.CompareVersions(latest, version)

	fmt.Printf("current: %s\nlatest:  %s\n", version, latest)
	switch {
	case cmp > 0:
		fmt.Printf("a newer version is available: %s -> %s\n", version, latest)
	case cmp == 0:
		fmt.Println("you are on the latest version")
	default:
		fmt.Printf("your version is newer than the published release (%s)\n", latest)
	}
	if *check {
		return nil
	}
	if cmp <= 0 && *targetVer == "" && !*force {
		return nil
	}

	assetName, err := selfupdate.CurrentAssetName(latest)
	if err != nil {
		return err
	}
	asset, ok := rel.FindAsset(assetName)
	if !ok {
		return fmt.Errorf("release %s has no build for %s/%s (asset %q)", rel.TagName, runtime.GOOS, runtime.GOARCH, assetName)
	}
	sums, err := selfupdate.Checksums(ctx, client, rel)
	if err != nil {
		return err
	}
	want := sums[assetName]
	if want == "" {
		return fmt.Errorf("no checksum recorded for %s in SHA256SUMS", assetName)
	}

	exe, err := selfupdate.ResolveExecutable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	fmt.Fprintf(os.Stderr, "downloading %s (%s)...\n", assetName, humanBytes(asset.Size))
	tmp, err := selfupdate.DownloadVerified(ctx, client, asset, want, dir)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot write to %s: %w\nre-run with elevated privileges, or download manually from https://github.com/%s/releases", dir, err, *repo)
		}
		return err
	}
	if err := selfupdate.ReplaceExecutable(exe, tmp); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot replace %s: %w\nre-run with elevated privileges, or download manually from https://github.com/%s/releases", exe, err, *repo)
		}
		return err
	}
	fmt.Printf("updated %s: %s -> %s\n", exe, version, latest)
	return nil
}
