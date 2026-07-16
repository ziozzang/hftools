package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ziozzang/hftools/internal/selfupdate"
)

// updateCheckInterval throttles how often the background version check hits the
// network. Between checks the cached result is reused.
const updateCheckInterval = 24 * time.Hour

// notifyExemptCommands are commands for which an update notice would be noise or
// redundant (or which are long-lived / non-interactive).
var notifyExemptCommands = map[string]bool{
	"": true, "update": true, "self-update": true, "version": true,
	"--version": true, "-version": true, "-v": true, "-V": true,
	"help": true, "-h": true, "--help": true, "completion": true,
}

type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest_version"`
}

// maybeNotifyUpdate prints a single-line notice when a newer release is
// available. It is deliberately unobtrusive: it does nothing when output is not
// a terminal (scripts/pipes), when HFTOOLS_NO_UPDATE_CHECK is set, or for
// exempt commands, and it never blocks for long or surfaces errors.
func maybeNotifyUpdate(ctx context.Context, args []string) {
	if os.Getenv("HFTOOLS_NO_UPDATE_CHECK") != "" {
		return
	}
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if notifyExemptCommands[cmd] {
		return
	}
	if !stderrIsTerminal() {
		return
	}
	latest, ok := latestKnownVersion(ctx)
	if !ok {
		return
	}
	if notice := updateNoticeText(latest, version); notice != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", notice)
	}
}

// updateNoticeText returns the upgrade notice when latest is newer than
// current, or "" otherwise.
func updateNoticeText(latest, current string) string {
	if selfupdate.CompareVersions(latest, current) > 0 {
		return fmt.Sprintf("hftools %s is available (you have %s). Run 'hftools update' to upgrade.", latest, current)
	}
	return ""
}

// latestKnownVersion returns the latest release version, reusing a cached value
// while it is fresh and otherwise refreshing it with a short, failure-tolerant
// network call.
func latestKnownVersion(ctx context.Context) (string, bool) {
	path := updateCachePath()
	cache := readUpdateCache(path)
	if cache.Latest != "" && time.Since(cache.CheckedAt) < updateCheckInterval {
		return cache.Latest, true
	}
	// Stale or missing: refresh with a short timeout so we never hold up the
	// shell for long, and ignore any failure (offline is expected and fine).
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rel, err := selfupdate.LatestRelease(cctx, &http.Client{}, "", updateRepo, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return cache.Latest, cache.Latest != "" // fall back to any previous value
	}
	cache = updateCache{CheckedAt: time.Now(), Latest: rel.Version()}
	writeUpdateCache(path, cache)
	return cache.Latest, cache.Latest != ""
}

func updateCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "hftools", "update-check.json")
}

func readUpdateCache(path string) updateCache {
	var c updateCache
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func writeUpdateCache(path string, c updateCache) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	// Write atomically so concurrent hftools processes never read a half file.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".update-check-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, path)
}

// stderrIsTerminal reports whether stderr is a character device (a terminal),
// which we use to suppress the notice when output is redirected to a file/pipe.
func stderrIsTerminal() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
