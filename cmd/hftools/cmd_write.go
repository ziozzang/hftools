package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/hub"
)

// requireToken returns the resolved token or a helpful error for write commands.
func requireToken(cfg settings) (string, error) {
	tok := resolveToken(cfg)
	if tok == "" {
		return "", fmt.Errorf("no token found (set $%s, pass --token, or run 'huggingface-cli login'); write operations require a token with write access", cfg.TokenEnv)
	}
	return tok, nil
}

// repoCommand manages repositories: create and delete.
func repoCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hftools repo <create|delete> OWNER/REPO [options]")
	}
	switch args[0] {
	case "create":
		return repoCreateCommand(ctx, args[1:])
	case "delete", "rm":
		return repoDeleteCommand(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stdout, "usage: hftools repo <create|delete> OWNER/REPO [--type model|dataset|space] [options]\n\n  create  Create a repository (--private, --exist-ok)\n  delete  Permanently delete a repository (--yes required)")
		return nil
	default:
		return fmt.Errorf("unknown repo subcommand %q (want create or delete)", args[0])
	}
}

func repoCreateCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("repo create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	private := fs.Bool("private", false, "create a private repository")
	existOK := fs.Bool("exist-ok", false, "do not fail if the repository already exists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools repo create OWNER/REPO [--type ...] [--private]")
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	repoID, err := hub.NormalizeRepoID(fs.Arg(0))
	if err != nil {
		return err
	}
	if _, err := requireToken(cfg); err != nil {
		return err
	}
	if err := newHubClient(cfg).CreateRepo(ctx, rt, repoID, *private); err != nil {
		if *existOK && strings.Contains(strings.ToLower(err.Error()), "already") {
			fmt.Fprintf(os.Stderr, "%s already exists\n", repoID)
			return nil
		}
		return err
	}
	kind := "public"
	if *private {
		kind = "private"
	}
	fmt.Printf("created %s %s (%s)\n", string(rt), repoID, kind)
	return nil
}

func repoDeleteCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("repo delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	yes := fs.Bool("yes", false, "confirm deletion (required; deletion is permanent)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools repo delete OWNER/REPO [--type ...] --yes")
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	repoID, err := hub.NormalizeRepoID(fs.Arg(0))
	if err != nil {
		return err
	}
	if !*yes {
		return fmt.Errorf("refusing to delete %s %s without --yes (this permanently deletes the repository and all its files)", string(rt), repoID)
	}
	if _, err := requireToken(cfg); err != nil {
		return err
	}
	if err := newHubClient(cfg).DeleteRepo(ctx, rt, repoID); err != nil {
		return err
	}
	fmt.Printf("deleted %s %s\n", string(rt), repoID)
	return nil
}

// tagCommand manages git tags: create, list, delete.
func tagCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hftools tag <create|list|delete> OWNER/REPO [TAG] [options]")
	}
	switch args[0] {
	case "create", "add":
		return tagCreateCommand(ctx, args[1:])
	case "list", "ls":
		return tagListCommand(ctx, args[1:])
	case "delete", "rm":
		return tagDeleteCommand(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stdout, "usage: hftools tag <create|list|delete> OWNER/REPO [TAG] [--type ...] [options]\n\n  create  Create a tag at --revision (--message)\n  list    List tags\n  delete  Delete a tag (--yes required)")
		return nil
	default:
		return fmt.Errorf("unknown tag subcommand %q (want create, list, or delete)", args[0])
	}
}

func tagCreateCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("tag create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	message := fs.String("message", "", "annotation message for the tag")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: hftools tag create OWNER/REPO TAG [--revision REV] [--message MSG]")
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	repoID, err := hub.NormalizeRepoID(fs.Arg(0))
	if err != nil {
		return err
	}
	tag := fs.Arg(1)
	if _, err := requireToken(cfg); err != nil {
		return err
	}
	if err := newHubClient(cfg).CreateTag(ctx, rt, repoID, tag, cfg.Revision, *message); err != nil {
		return err
	}
	fmt.Printf("created tag %s on %s at %s\n", tag, repoID, cfg.Revision)
	return nil
}

func tagListCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("tag list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools tag list OWNER/REPO [--type ...]")
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	repoID, err := hub.NormalizeRepoID(fs.Arg(0))
	if err != nil {
		return err
	}
	refs, err := newHubClient(cfg).Refs(ctx, rt, repoID)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, refs.Tags)
	}
	if len(refs.Tags) == 0 {
		fmt.Fprintln(os.Stderr, "no tags")
		return nil
	}
	printRefs("tags", refs.Tags)
	return nil
}

func tagDeleteCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("tag delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	yes := fs.Bool("yes", false, "confirm deletion (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: hftools tag delete OWNER/REPO TAG --yes")
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	repoID, err := hub.NormalizeRepoID(fs.Arg(0))
	if err != nil {
		return err
	}
	tag := fs.Arg(1)
	if !*yes {
		return fmt.Errorf("refusing to delete tag %s on %s without --yes", tag, repoID)
	}
	if _, err := requireToken(cfg); err != nil {
		return err
	}
	if err := newHubClient(cfg).DeleteTag(ctx, rt, repoID, tag); err != nil {
		return err
	}
	fmt.Printf("deleted tag %s on %s\n", tag, repoID)
	return nil
}

// uploadCommand publishes local files or a folder to a repository in one commit.
func uploadCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	pathInRepo := fs.String("path-in-repo", "", "destination path or prefix inside the repository")
	message := fs.String("message", "", "commit summary")
	description := fs.String("description", "", "commit description")
	create := fs.Bool("create", false, "create the repository first if it does not exist")
	private := fs.Bool("private", false, "with --create, create the repository private")
	dryRun := fs.Bool("dry-run", false, "list what would be uploaded without contacting the Hub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: hftools upload [--path-in-repo P] [--message MSG] OWNER/REPO PATH [PATH...]\n(flags must precede the repository and file arguments)")
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	repoID, err := hub.NormalizeRepoID(fs.Arg(0))
	if err != nil {
		return err
	}
	files, err := collectUploadFiles(fs.Args()[1:], *pathInRepo)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files to upload")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].PathInRepo < files[j].PathInRepo })

	if *dryRun {
		var total int64
		for _, f := range files {
			st, err := os.Stat(f.LocalPath)
			if err != nil {
				return err
			}
			total += st.Size()
			fmt.Printf("%12s  %s -> %s\n", humanBytes(st.Size()), f.LocalPath, f.PathInRepo)
		}
		fmt.Printf("would upload %d file(s), %s to %s %s@%s (dry run)\n", len(files), humanBytes(total), string(rt), repoID, cfg.Revision)
		return nil
	}

	if _, err := requireToken(cfg); err != nil {
		return err
	}
	client := newHubClient(cfg)
	if *create {
		if err := client.CreateRepo(ctx, rt, repoID, *private); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "already") {
				return err
			}
		}
	}
	res, err := client.Upload(ctx, rt, repoID, cfg.Revision, files, *message, *description)
	if err != nil {
		return err
	}
	fmt.Printf("uploaded %d file(s) to %s %s@%s\n", len(files), string(rt), repoID, cfg.Revision)
	if res != nil && res.CommitURL != "" {
		fmt.Printf("commit: %s\n", res.CommitURL)
	}
	return nil
}

// collectUploadFiles maps local paths to repository destinations. A single
// directory argument uploads its contents (under path-in-repo); a single file
// uploads to path-in-repo (or its basename); multiple file arguments each land
// at path-in-repo/basename.
func collectUploadFiles(paths []string, pathInRepo string) ([]hub.UploadFile, error) {
	prefix := strings.Trim(filepath.ToSlash(pathInRepo), "/")
	var files []hub.UploadFile

	if len(paths) == 1 {
		st, err := os.Stat(paths[0])
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			root := paths[0]
			err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					if skipUploadDir(d.Name()) {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				dest := path.Join(prefix, filepath.ToSlash(rel))
				if err := validateRepoPath(dest); err != nil {
					return err
				}
				files = append(files, hub.UploadFile{LocalPath: p, PathInRepo: dest})
				return nil
			})
			if err != nil {
				return nil, err
			}
			return files, nil
		}
		dest := prefix
		if dest == "" {
			dest = filepath.Base(paths[0])
		}
		if err := validateRepoPath(dest); err != nil {
			return nil, err
		}
		return []hub.UploadFile{{LocalPath: paths[0], PathInRepo: dest}}, nil
	}

	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("%s is a directory; upload a single folder on its own, or list individual files", p)
		}
		dest := path.Join(prefix, filepath.Base(p))
		if err := validateRepoPath(dest); err != nil {
			return nil, err
		}
		files = append(files, hub.UploadFile{LocalPath: p, PathInRepo: dest})
	}
	return files, nil
}

func skipUploadDir(name string) bool {
	switch name {
	case ".git", ".metadata", ".hfdown", "hfdown-metadata":
		return true
	}
	return false
}

func validateRepoPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") {
		return fmt.Errorf("invalid destination path %q", p)
	}
	if p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return fmt.Errorf("destination path escapes the repository: %q", p)
	}
	return nil
}
