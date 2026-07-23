package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

const version = "0.14.1"

type settings struct {
	Endpoint              string   `json:"endpoint"`
	Revision              string   `json:"revision"`
	Output                string   `json:"output"`
	Parts                 int      `json:"parts"`
	MultipartThreshold    int64    `json:"multipart_threshold"`
	BufferSize            int      `json:"buffer_size"`
	Retries               int      `json:"retries"`
	RetryMinWaitSeconds   int      `json:"retry_min_wait_seconds"`
	RetryMaxWaitSeconds   int      `json:"retry_max_wait_seconds"`
	TimeoutSeconds        int      `json:"timeout_seconds"`
	StallTimeoutSeconds   int      `json:"stall_timeout_seconds"`
	MinSpeed              int64    `json:"min_speed"`
	MinSpeedWindowSeconds int      `json:"min_speed_window_seconds"`
	Resume                bool     `json:"resume"`
	TokenEnv              string   `json:"token_env"`
	Token                 string   `json:"-"`
	Tag                   string   `json:"-"`
	DryRun                bool     `json:"-"`
	Sign                  bool     `json:"-"`
	Filters               []string `json:"filters,omitempty"`
}

type queueFile struct {
	OutputRoot string     `json:"output_root"`
	Jobs       []queueJob `json:"jobs"`
}

type queueJob struct {
	Repo               string   `json:"repo"`
	Output             string   `json:"output,omitempty"`
	Revision           string   `json:"revision,omitempty"`
	Parts              *int     `json:"parts,omitempty"`
	MultipartThreshold *int64   `json:"multipart_threshold,omitempty"`
	BufferSize         *int     `json:"buffer_size,omitempty"`
	Retries            *int     `json:"retries,omitempty"`
	Resume             *bool    `json:"resume,omitempty"`
	RepoType           string   `json:"type,omitempty"`
	Filters            []string `json:"filters,omitempty"`
}

func defaults() settings {
	return settings{Endpoint: "https://huggingface.co", Revision: "main", Output: "", Parts: 4,
		MultipartThreshold: 64 << 20, BufferSize: 1 << 20, Retries: 5,
		RetryMinWaitSeconds: 1, RetryMaxWaitSeconds: 300, TimeoutSeconds: 30,
		StallTimeoutSeconds: 60, MinSpeedWindowSeconds: 5, Resume: true, TokenEnv: "HF_TOKEN"}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Refresh the cached latest-version in the background (never blocks the
	// foreground; soft-fails offline/air-gapped), then run the command and print
	// a notice from the cache if a newer release is known.
	startUpdateRefresh(os.Args[1:])
	err := run(ctx, os.Args[1:])
	maybeNotifyUpdate(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return flag.ErrHelp
	}
	switch args[0] {
	case "download", "dn", "d":
		return downloadCommand(ctx, args[1:])
	case "dataset", "ds":
		return datasetCommand(ctx, args[1:])
	case "space", "sp":
		return spaceCommand(ctx, args[1:])
	case "batch":
		return batchCommand(ctx, args[1:])
	case "verify":
		return verifyCommand(ctx, args[1:])
	case "verify-batch":
		return verifyBatchCommand(ctx, args[1:])
	case "status":
		return statusCommand(args[1:])
	case "info", "show":
		return infoCommand(ctx, args[1:])
	case "ls":
		return lsCommand(ctx, args[1:])
	case "diff":
		return diffCommand(ctx, args[1:])
	case "refs":
		return refsCommand(ctx, args[1:])
	case "search":
		return searchCommand(ctx, args[1:])
	case "whoami":
		return whoamiCommand(ctx, args[1:])
	case "cache-scan":
		return cacheScanCommand(args[1:])
	case "du":
		return duCommand(args[1:])
	case "get", "cat":
		return getCommand(ctx, args[1:])
	case "peek":
		return peekCommand(ctx, args[1:])
	case "scan":
		return scanCommand(ctx, args[1:])
	case "sign":
		return signCommand(args[1:])
	case "verify-sig":
		return verifySigCommand(args[1:])
	case "key":
		return keyCommand(args[1:])
	case "gc":
		return gcCommand(args[1:])
	case "cache-gc":
		return cacheGCCommand(args[1:])
	case "dedup":
		return dedupCommand(args[1:])
	case "repair":
		return repairCommand(ctx, args[1:])
	case "doctor":
		return doctorCommand(ctx, args[1:])
	case "watch":
		return watchCommand(ctx, args[1:])
	case "completion":
		return completionCommand(args[1:])
	case "update", "self-update":
		return updateCommand(ctx, args[1:])
	case "upload", "up":
		return uploadCommand(ctx, args[1:])
	case "repo":
		return repoCommand(ctx, args[1:])
	case "tag":
		return tagCommand(ctx, args[1:])
	case "cache-export":
		return cacheExportCommand(args[1:])
	case "cache-import":
		return cacheImportCommand(args[1:])
	case "cache-import-batch":
		return cacheImportBatchCommand(args[1:])
	case "cache-list":
		return cacheListCommand(args[1:])
	case "cache-verify":
		return cacheVerifyCommand(args[1:])
	case "serve":
		return serveCommand(ctx, args[1:])
	case "version", "--version", "-version", "-v", "-V":
		if len(args) != 1 {
			return fmt.Errorf("usage: hftools version")
		}
		printVersion(os.Stdout)
		return nil
	case "help":
		return helpCommand(ctx, args[1:])
	case "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "hftools %s (%s/%s)\nCreated by Jioh L. Jung <ziozzang@gmail.com>\nGitHub: https://github.com/ziozzang/hftools\n", version, runtime.GOOS, runtime.GOARCH)
}
