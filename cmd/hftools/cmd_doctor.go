package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ziozzang/hfdownload/internal/hfcache"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

// doctorCommand runs environment diagnostics: Hub reachability, token validity,
// filesystem capabilities the downloader relies on, disk space, and cache health.
func doctorCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "Hugging Face Hub endpoint")
	fs.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the access token")
	cache := fs.String("cache", "", "Hugging Face cache root to inspect")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var checks []doctorCheck
	add := func(name, status, detail string) { checks = append(checks, doctorCheck{name, status, detail}) }

	add("version", "ok", fmt.Sprintf("hftools %s (%s/%s, %s)", version, runtime.GOOS, runtime.GOARCH, runtime.Version()))

	// Hub reachability + token validity via whoami.
	reach, name, tokenPresent := probeHub(ctx, cfg)
	switch reach {
	case "ok":
		add("hub endpoint", "ok", cfg.Endpoint+" reachable")
	case "unreachable":
		add("hub endpoint", "warn", cfg.Endpoint+" not reachable (offline/air-gapped is fine for local ops)")
	}
	switch {
	case !tokenPresent:
		add("token", "warn", fmt.Sprintf("$%s not set (only public repositories are accessible)", cfg.TokenEnv))
	case name != "":
		add("token", "ok", "authenticated as "+name)
	case reach == "ok":
		add("token", "fail", fmt.Sprintf("$%s set but rejected by the Hub", cfg.TokenEnv))
	default:
		add("token", "warn", fmt.Sprintf("$%s set but could not be validated (Hub unreachable)", cfg.TokenEnv))
	}

	// Filesystem capabilities in the current directory.
	cwd, _ := os.Getwd()
	if err := writableCheck(cwd); err != nil {
		add("writable cwd", "fail", err.Error())
	} else {
		add("writable cwd", "ok", cwd)
	}
	if err := symlinkCheck(cwd); err != nil {
		add("symlinks", "warn", "unsupported here: "+err.Error())
	} else {
		add("symlinks", "ok", "supported (needed for cache-export)")
	}
	if err := hardlinkCheck(cwd); err != nil {
		add("hardlinks", "warn", "unsupported here: "+err.Error())
	} else {
		add("hardlinks", "ok", "supported (needed for dedup / cache import)")
	}
	if free, ok := freeBytes(cwd); ok {
		status := "ok"
		if free < 1<<30 {
			status = "warn"
		}
		add("disk free", status, humanBytes(free)+" available")
	} else {
		add("disk free", "warn", "unavailable on this platform")
	}

	// HF cache.
	cacheRoot := *cache
	if cacheRoot == "" {
		cacheRoot = hfcache.DefaultCacheRoot()
	}
	if repos, err := hfcache.ListRepos(cacheRoot); err != nil {
		add("hf cache", "warn", err.Error())
	} else {
		add("hf cache", "ok", fmt.Sprintf("%s (%d repositories)", cacheRoot, len(repos)))
	}

	if *jsonOut {
		return printJSON(os.Stdout, checks)
	}
	failed := 0
	for _, c := range checks {
		badge := map[string]string{"ok": "OK  ", "warn": "WARN", "fail": "FAIL"}[c.Status]
		if c.Status == "fail" {
			failed++
		}
		fmt.Printf("[%s] %-14s %s\n", badge, c.Name, c.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

// probeHub returns reachability ("ok"|"unreachable"), the authenticated user
// name (empty if none/invalid), and whether a token was supplied.
func probeHub(ctx context.Context, cfg settings) (string, string, bool) {
	token := resolveToken(cfg)
	client := &http.Client{Timeout: 10 * time.Second}
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.Endpoint+"/api/whoami-v2", nil)
	if err != nil {
		return "unreachable", "", token != ""
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "unreachable", "", token != ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return "ok", body.Name, token != ""
	}
	return "ok", "", token != ""
}

func writableCheck(dir string) error {
	f, err := os.CreateTemp(dir, ".hftools-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func symlinkCheck(dir string) error {
	tmp, err := os.MkdirTemp(dir, ".hftools-doctor-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	link := filepath.Join(tmp, "link")
	if err := os.Symlink("target", link); err != nil {
		return err
	}
	_, err = os.Readlink(link)
	return err
}

func hardlinkCheck(dir string) error {
	tmp, err := os.MkdirTemp(dir, ".hftools-doctor-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		return err
	}
	dst := filepath.Join(tmp, "dst")
	if err := os.Link(src, dst); err != nil {
		return err
	}
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	di, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if !os.SameFile(si, di) {
		return fmt.Errorf("hardlink did not share inode")
	}
	return nil
}
