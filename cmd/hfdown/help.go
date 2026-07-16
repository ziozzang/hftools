package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ziozzang/hfdownload/internal/hub"
)

func helpCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: hfdown help [COMMAND]")
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
	fmt.Fprintln(w, `hfdown - resumable, hash-verified Hugging Face repository downloader

Usage:
  hfdown COMMAND [options]

Commands:
  download, dn, d    Download or update a model repository
  dataset, ds        Download or update a dataset repository
  batch              Process a plain-text list or JSON queue
  verify             Verify one downloaded repository
  verify-batch       Recursively verify downloaded repositories
  status             Show stored repository status and revision
  cache-export       Convert a download into the Hugging Face cache layout
  cache-import       Convert a Hugging Face cache snapshot into a flat directory
  cache-import-batch Import every repository from a Hugging Face cache
  cache-list         List repositories stored in a Hugging Face cache
  cache-verify       Rehash cached blobs against their content-addressed names
  serve              Serve local downloads over the Hugging Face URL scheme
  version            Print version and target platform
  help               Show general or command-specific help

Common forms:
  hfdown d [options] OWNER/MODEL
  hfdown ds [options] OWNER/DATASET
  hfdown batch --list repositories.txt [options]
  hfdown batch --queue queue.json [options]
  hfdown verify --output DIR [--force]
  hfdown verify-batch --root DIR [--force]
  hfdown cache-export --output DIR [--cache HF_CACHE]
  hfdown cache-import --repo OWNER/MODEL [--cache HF_CACHE] --output DIR
  hfdown serve --root DIR [--addr 0.0.0.0:8080]

Help and version:
  hfdown help [COMMAND]
  hfdown COMMAND --help
  hfdown version | --version | -v`)
}
