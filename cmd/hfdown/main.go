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

const version = "0.7.0"

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
	if err := run(ctx, os.Args[1:]); err != nil {
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
	case "batch":
		return batchCommand(ctx, args[1:])
	case "verify":
		return verifyCommand(args[1:])
	case "verify-batch":
		return verifyBatchCommand(args[1:])
	case "status":
		return statusCommand(args[1:])
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
			return fmt.Errorf("usage: hfdown version")
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
	fmt.Fprintf(w, "hfdown %s (%s/%s)\nCreated by Jioh L. Jung <ziozzang@gmail.com>\nGitHub: https://github.com/ziozzang/hfdownload\n", version, runtime.GOOS, runtime.GOARCH)
}
