package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ziozzang/hfdownload/internal/hub"
	"github.com/ziozzang/hfdownload/internal/state"
)

// resolveToken returns the explicit token or the value of the configured env var.
func resolveToken(cfg settings) string {
	if cfg.Token != "" {
		return cfg.Token
	}
	return os.Getenv(cfg.TokenEnv)
}

// newHubClient builds a Hub client from settings, wiring the retry policy.
func newHubClient(cfg settings) *hub.Client {
	c := hub.New(cfg.Endpoint, resolveToken(cfg), time.Duration(cfg.TimeoutSeconds)*time.Second)
	c.Retries = cfg.Retries
	c.RetryMinWait = time.Duration(cfg.RetryMinWaitSeconds) * time.Second
	c.RetryMaxWait = time.Duration(cfg.RetryMaxWaitSeconds) * time.Second
	return c
}

// remoteFlags registers the flags shared by read-only remote commands and
// returns the --type flag pointer.
func remoteFlags(fs *flag.FlagSet, cfg *settings) *string {
	typeFlag := fs.String("type", "model", "repository type: model or dataset")
	fs.StringVar(&cfg.Revision, "revision", cfg.Revision, "branch, tag, or commit")
	fs.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "Hugging Face Hub endpoint")
	fs.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the access token")
	fs.StringVar(&cfg.Token, "token", "", "Hugging Face access token (prefer --token-env)")
	fs.IntVar(&cfg.Retries, "retries", cfg.Retries, "transient-error retries; -1 retries until success")
	fs.IntVar(&cfg.TimeoutSeconds, "timeout", cfg.TimeoutSeconds, "HTTP response-header timeout in seconds")
	return typeFlag
}

func repoTypeFrom(s string) (hub.RepoType, error) {
	switch s {
	case "", "model":
		return hub.RepoTypeModel, nil
	case "dataset":
		return hub.RepoTypeDataset, nil
	default:
		return "", fmt.Errorf("invalid --type %q (want model or dataset)", s)
	}
}

// fetchRemote normalizes the repo argument and fetches its Hub metadata.
func fetchRemote(ctx context.Context, cfg settings, repoArg, typeStr string) (*hub.RepoInfo, string, hub.RepoType, error) {
	rt, err := repoTypeFrom(typeStr)
	if err != nil {
		return nil, "", "", err
	}
	repoID, err := hub.NormalizeRepoID(repoArg)
	if err != nil {
		return nil, "", "", err
	}
	info, err := newHubClient(cfg).RepoInfo(ctx, rt, repoID, cfg.Revision)
	if err != nil {
		return nil, repoID, rt, err
	}
	return info, repoID, rt, nil
}

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// gatedString renders the Hub's polymorphic "gated" field (false | "auto" | "manual").
func gatedString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", "false":
		return "no"
	case "true":
		return "yes"
	default:
		return strings.Trim(s, `"`)
	}
}

// findRepositories returns the hftools repository roots under root (directories
// holding a metadata manifest), scanning the current and legacy layouts.
func findRepositories(root string) ([]string, error) {
	var repos []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name != ".metadata" && name != "hfdown-metadata" && name != ".hfdown" {
			return nil
		}
		if st, err := os.Stat(filepath.Join(path, "manifest.json")); err == nil && st.Mode().IsRegular() {
			repos = append(repos, filepath.Dir(path))
		}
		return filepath.SkipDir
	})
	return repos, err
}

// loadExistingManifest reads a repository's manifest read-only (no migration),
// checking the current and legacy metadata directory names. It returns (nil,
// nil) when the directory holds no manifest yet.
func loadExistingManifest(root string) (*state.Manifest, error) {
	for _, name := range []string{".metadata", "hfdown-metadata", ".hfdown"} {
		p := filepath.Join(root, name, "manifest.json")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return state.LoadManifest(p)
		}
	}
	return nil, nil
}

// resolveDir turns a user path into an absolute, symlink-evaluated directory.
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
