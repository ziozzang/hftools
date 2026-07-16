package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/download"
	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/modelfile"
	"github.com/ziozzang/hftools/internal/progress"
)

type barWriter struct{ bar *progress.Bar }

func (b barWriter) Write(p []byte) (int, error) { b.bar.Add(int64(len(p))); return len(p), nil }

func findSibling(info *hub.RepoInfo, file string) (hub.RepoFile, bool) {
	for _, f := range info.Siblings {
		if f.Path == file {
			return f, true
		}
	}
	return hub.RepoFile{}, false
}

func getCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	out := fs.String("o", "", "output path ('-' for stdout; default: the file's base name)")
	noVerify := fs.Bool("no-verify", false, "skip hash verification")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: hftools get [options] OWNER/REPO FILE")
	}
	file := fs.Arg(1)
	info, repoID, rt, err := fetchRemote(ctx, cfg, fs.Arg(0), *typeFlag)
	if err != nil {
		return err
	}
	remote, ok := findSibling(info, file)
	if !ok {
		return fmt.Errorf("file %q not found in %s@%s", file, repoID, cfg.Revision)
	}
	client := newHubClient(cfg)
	url := client.DownloadURL(rt, repoID, info.SHA, file)

	toStdout := *out == "-"
	var finalPath, tmpDir string
	if toStdout {
		tmpDir = os.TempDir()
	} else {
		dest := *out
		if dest == "" {
			dest = path.Base(file)
		}
		if st, err := os.Stat(dest); err == nil && st.IsDir() {
			dest = filepath.Join(dest, path.Base(file))
		}
		finalPath, err = filepath.Abs(dest)
		if err != nil {
			return err
		}
		tmpDir = filepath.Dir(finalPath)
	}

	tmp, err := os.CreateTemp(tmpDir, ".hftools-get-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	bar := progress.New(os.Stderr, remote.Size, "get "+file)
	n, err := client.Fetch(ctx, url, io.MultiWriter(tmp, barWriter{bar}))
	bar.Finish()
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !*noVerify {
		hashes, herr := download.HashFileSelective(tmpName, remote.Size, cfg.BufferSize, nil, remote.LFS == nil)
		if herr != nil {
			return fmt.Errorf("verify %q: %w", file, herr)
		}
		if cherr := download.CheckHashes(remote, hashes); cherr != nil {
			return cherr
		}
	}
	if toStdout {
		f, err := os.Open(tmpName)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s to stdout\n", humanBytes(n))
		return nil
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return err
	}
	cleanup = false
	verified := "verified"
	if *noVerify {
		verified = "unverified"
	}
	fmt.Fprintf(os.Stderr, "saved %s (%s, %s)\n", finalPath, humanBytes(n), verified)
	return nil
}

const maxSafetensorsHeader = 512 << 20

func peekCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("peek", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	window := int64(16 << 20)
	fs.Var(byteSizeValue{&window}, "bytes", "header window to fetch (default 16MiB; raise for large GGUF metadata)")
	long := fs.Bool("long", false, "list every tensor / all metadata")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: hftools peek [options] OWNER/REPO FILE")
	}
	file := fs.Arg(1)
	info, repoID, rt, err := fetchRemote(ctx, cfg, fs.Arg(0), *typeFlag)
	if err != nil {
		return err
	}
	remote, ok := findSibling(info, file)
	if !ok {
		return fmt.Errorf("file %q not found in %s@%s", file, repoID, cfg.Revision)
	}
	if remote.Size == 0 {
		return fmt.Errorf("file %q is empty", file)
	}
	client := newHubClient(cfg)
	url := client.DownloadURL(rt, repoID, info.SHA, file)

	probe := window
	if remote.Size < probe {
		probe = remote.Size
	}
	head, err := client.GetRange(ctx, url, 0, probe-1)
	if err != nil {
		return err
	}
	format := modelfile.Detect(head)
	if format == modelfile.FormatUnknown {
		switch {
		case strings.HasSuffix(strings.ToLower(file), ".gguf"):
			format = modelfile.FormatGGUF
		case strings.HasSuffix(strings.ToLower(file), ".safetensors"):
			format = modelfile.FormatSafetensors
		default:
			return fmt.Errorf("%q is not a recognized safetensors or GGUF file", file)
		}
	}
	var parsed *modelfile.Info
	switch format {
	case modelfile.FormatGGUF:
		parsed, err = modelfile.ParseGGUF(head)
	case modelfile.FormatSafetensors:
		n, lerr := modelfile.SafetensorsHeaderLen(head)
		if lerr != nil {
			return lerr
		}
		if n > maxSafetensorsHeader {
			return fmt.Errorf("safetensors header of %s is too large to peek", humanBytes(n))
		}
		if 8+n <= int64(len(head)) {
			parsed, err = modelfile.ParseSafetensors(head[8:8+n], n)
		} else {
			body, gerr := client.GetRange(ctx, url, 8, 8+n-1)
			if gerr != nil {
				return gerr
			}
			parsed, err = modelfile.ParseSafetensors(body, n)
		}
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, parsed)
	}
	printPeek(file, remote.Size, parsed, *long)
	return nil
}

func printPeek(file string, fileSize int64, info *modelfile.Info, long bool) {
	fmt.Printf("file: %s (%s)\nformat: %s\n", file, humanBytes(fileSize), info.Format)
	if info.Format == modelfile.FormatGGUF {
		fmt.Printf("gguf version: %d\n", info.Version)
		if info.Arch != "" {
			fmt.Printf("architecture: %s\n", info.Arch)
		}
	}
	fmt.Printf("tensors: %d\n", info.Tensors)
	if info.ParamsKnown {
		fmt.Printf("parameters: %s (%d)\n", humanCount(info.Params), info.Params)
	} else {
		fmt.Printf("parameters: unknown (header window too small; raise --bytes)\n")
	}
	for _, d := range info.DTypes {
		fmt.Printf("  %-8s %4d tensors  %s params\n", d.DType, d.Count, humanCount(d.Params))
	}
	if long {
		keys := make([]string, 0, len(info.Metadata))
		for k := range info.Metadata {
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			fmt.Println("metadata:")
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("  %s = %s\n", k, info.Metadata[k])
			}
		}
		if len(info.TensorList) > 0 {
			fmt.Println("tensor shapes:")
			for _, t := range info.TensorList {
				fmt.Printf("  %-8s %v  %s\n", t.DType, t.Shape, t.Name)
			}
		}
	}
	if info.Partial {
		fmt.Println("note: header parsing was truncated by the fetch window; raise --bytes for full detail")
	}
}

// humanCount renders large integers with K/M/B/T suffixes.
func humanCount(n int64) string {
	f := float64(n)
	switch {
	case n >= 1_000_000_000_000:
		return fmt.Sprintf("%.2fT", f/1e12)
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", f/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", f/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.2fK", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
