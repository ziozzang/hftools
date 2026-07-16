package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/ziozzang/hftools/internal/hub"
)

func loadSettings(args []string) (settings, string, error) {
	cfg := defaults()
	configPath := findFlagValue(args, "config")
	if configPath == "" {
		// Prefer the current config name; keep reading the legacy name so
		// existing project directories continue to work after the rename.
		for _, candidate := range []string{".hftools.json", ".hfdown.json"} {
			if _, err := os.Stat(candidate); err == nil {
				configPath = candidate
				break
			}
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
	if cfg.Retries < -1 || cfg.Retries > 1000 {
		return fmt.Errorf("retries must be between -1 (unlimited) and 1000")
	}
	if cfg.RetryMinWaitSeconds < 1 {
		return fmt.Errorf("retry-min-wait must be positive")
	}
	if cfg.RetryMaxWaitSeconds < cfg.RetryMinWaitSeconds {
		return fmt.Errorf("retry-max-wait must be at least retry-min-wait")
	}
	if cfg.TimeoutSeconds < 1 {
		return fmt.Errorf("timeout must be positive")
	}
	if cfg.StallTimeoutSeconds < 0 {
		return fmt.Errorf("stall-timeout cannot be negative")
	}
	if cfg.MinSpeed < 0 {
		return fmt.Errorf("min-speed cannot be negative")
	}
	if cfg.MinSpeedWindowSeconds < 1 {
		return fmt.Errorf("min-speed-window must be positive")
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
