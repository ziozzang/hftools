package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
)

func addTransferFlags(fs *flag.FlagSet, cfg *settings, configPath *string) {
	fs.StringVar(&cfg.Revision, "revision", cfg.Revision, "branch, tag, or commit")
	fs.StringVar(&cfg.Tag, "tag", "", "tag name (overrides --revision)")
	fs.Var(stringSliceValue{&cfg.Filters}, "filter", "include glob; repeat for multiple patterns")
	fs.IntVar(&cfg.Parts, "parts", cfg.Parts, "parallel ranges per large file (1 disables multipart)")
	fs.Var(byteSizeValue{&cfg.MultipartThreshold}, "multipart-threshold", "minimum size to split (for example 64MiB)")
	fs.Var(byteSizeIntValue{&cfg.BufferSize}, "buffer-size", "memory buffer per part (for example 1MiB)")
	fs.IntVar(&cfg.Retries, "retries", cfg.Retries, "retries per range and per API call for transient errors (5xx/429/network); -1 retries until success")
	fs.IntVar(&cfg.RetryMinWaitSeconds, "retry-min-wait", cfg.RetryMinWaitSeconds, "minimum randomized backoff between retries, in seconds")
	fs.IntVar(&cfg.RetryMaxWaitSeconds, "retry-max-wait", cfg.RetryMaxWaitSeconds, "maximum randomized backoff between retries, in seconds")
	fs.IntVar(&cfg.TimeoutSeconds, "timeout", cfg.TimeoutSeconds, "HTTP response-header timeout in seconds")
	fs.IntVar(&cfg.StallTimeoutSeconds, "stall-timeout", cfg.StallTimeoutSeconds, "reconnect and resume a range after this many seconds without progress (0 disables)")
	fs.Var(byteSizeValue{&cfg.MinSpeed}, "min-speed", "reconnect and resume a range (connection) averaging below this rate, for example 1MiB (0 disables)")
	fs.IntVar(&cfg.MinSpeedWindowSeconds, "min-speed-window", cfg.MinSpeedWindowSeconds, "averaging window in seconds for --min-speed")
	fs.BoolVar(&cfg.Resume, "resume", cfg.Resume, "resume compatible partial downloads")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "resolve and print the download plan without transferring")
	fs.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "Hugging Face Hub endpoint")
	fs.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the access token")
	fs.StringVar(&cfg.Token, "token", "", "Hugging Face access token (prefer --token-env for security)")
	fs.StringVar(configPath, "config", *configPath, "JSON config file (default: .hftools.json or .hfdown.json if present)")
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
	}{{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"gb", 1_000_000_000}, {"mb", 1_000_000}, {"kb", 1_000},
		{"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}}
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
