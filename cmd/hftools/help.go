package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ziozzang/hftools/internal/hub"
)

func helpCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: hftools help [COMMAND]")
	}
	var err error
	switch args[0] {
	case "download", "dn", "d":
		err = repositoryCommand(ctx, []string{"-h"}, hub.RepoTypeModel, "download")
	case "dataset", "ds":
		err = repositoryCommand(ctx, []string{"-h"}, hub.RepoTypeDataset, "dataset")
	case "batch":
		err = batchCommand(ctx, []string{"-h"})
	case "verify":
		err = verifyCommand([]string{"-h"})
	case "verify-batch":
		err = verifyBatchCommand([]string{"-h"})
	case "status":
		err = statusCommand([]string{"-h"})
	case "info", "show":
		err = infoCommand(ctx, []string{"-h"})
	case "ls":
		err = lsCommand(ctx, []string{"-h"})
	case "diff":
		err = diffCommand(ctx, []string{"-h"})
	case "du":
		err = duCommand([]string{"-h"})
	case "get", "cat":
		err = getCommand(ctx, []string{"-h"})
	case "peek":
		err = peekCommand(ctx, []string{"-h"})
	case "scan":
		err = scanCommand([]string{"-h"})
	case "sign":
		err = signCommand([]string{"-h"})
	case "verify-sig":
		err = verifySigCommand([]string{"-h"})
	case "gc":
		err = gcCommand([]string{"-h"})
	case "cache-gc":
		err = cacheGCCommand([]string{"-h"})
	case "dedup":
		err = dedupCommand([]string{"-h"})
	case "repair":
		err = repairCommand(ctx, []string{"-h"})
	case "doctor":
		err = doctorCommand(ctx, []string{"-h"})
	case "watch":
		err = watchCommand(ctx, []string{"-h"})
	case "completion":
		err = completionCommand([]string{"-h"})
	case "cache-export":
		err = cacheExportCommand([]string{"-h"})
	case "cache-import":
		err = cacheImportCommand([]string{"-h"})
	case "cache-import-batch":
		err = cacheImportBatchCommand([]string{"-h"})
	case "cache-list":
		err = cacheListCommand([]string{"-h"})
	case "cache-verify":
		err = cacheVerifyCommand([]string{"-h"})
	case "serve":
		err = serveCommand(ctx, []string{"-h"})
	case "version":
		printVersion(os.Stdout)
		return nil
	case "help":
		usage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown help topic %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `hftools - resumable, hash-verified Hugging Face repository downloader

Usage:
  hftools COMMAND [options]

Download & sync:
  download, dn, d    Download or update a model repository
  dataset, ds        Download or update a dataset repository
  batch              Process a plain-text list or JSON queue
  get, cat           Download a single file (to a path or stdout)
  watch              Periodically re-sync a repository
  repair             Deep-verify and re-fetch missing or corrupt files

Inspect (no download):
  info, show         Summarize a remote repository
  ls                 List a remote repository's files
  peek               Read a remote safetensors/GGUF header via a Range request
  diff               Compare a local download against the remote revision

Verify & trust:
  verify             Verify one downloaded repository
  verify-batch       Recursively verify downloaded repositories
  status             Show stored repository status and revision
  scan               Scan pickle/torch files for unsafe imports
  sign               Sign a repository's content manifest (ed25519)
  verify-sig         Verify a repository signature

Storage & maintenance:
  du                 Report disk usage of local downloads
  gc                 Reclaim tmp/ and untracked files (dry run by default)
  dedup              Hardlink identical files across repositories
  cache-gc           Remove unreferenced blobs from a Hugging Face cache
  doctor             Diagnose environment, network, and filesystem

Hugging Face cache interop:
  cache-export       Convert a download into the Hugging Face cache layout
  cache-import       Convert a Hugging Face cache snapshot into a flat directory
  cache-import-batch Import every repository from a Hugging Face cache
  cache-list         List repositories stored in a Hugging Face cache
  cache-verify       Rehash cached blobs against their content-addressed names

Serve & shell:
  serve              Serve local downloads over the Hugging Face URL scheme
  completion         Emit a bash/zsh/fish completion script
  version            Print version and target platform
  help               Show general or command-specific help

Common forms:
  hftools d [options] OWNER/MODEL
  hftools info OWNER/MODEL
  hftools ls --long OWNER/MODEL
  hftools get OWNER/MODEL config.json -o config.json
  hftools peek OWNER/MODEL model.safetensors
  hftools scan ./OWNER_MODEL
  hftools verify --output DIR [--force]
  hftools dedup --root DIR --yes
  hftools serve --root DIR [--addr 0.0.0.0:8080]

Help and version:
  hftools help [COMMAND]
  hftools COMMAND --help
  hftools version | --version | -v`)
}
