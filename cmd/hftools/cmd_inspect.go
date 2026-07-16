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
	"github.com/ziozzang/hftools/internal/state"
)

// selectRepoFiles applies include globs (never erroring on an empty result).
func selectRepoFiles(files []hub.RepoFile, filters []string) []hub.RepoFile {
	norm, _ := normalizeFilters(filters)
	if len(norm) == 0 {
		return files
	}
	var out []hub.RepoFile
	for _, f := range files {
		full := strings.ToLower(f.Path)
		base := path.Base(full)
		for _, pat := range norm {
			target := full
			if !strings.Contains(pat, "/") {
				target = base
			}
			if ok, _ := path.Match(pat, target); ok {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

type infoFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  bool   `json:"lfs"`
}

type infoReport struct {
	Type         string     `json:"type"`
	RepoID       string     `json:"repo_id"`
	Revision     string     `json:"revision"`
	Commit       string     `json:"commit"`
	Author       string     `json:"author,omitempty"`
	Private      bool       `json:"private"`
	Gated        string     `json:"gated"`
	Library      string     `json:"library,omitempty"`
	Pipeline     string     `json:"pipeline,omitempty"`
	LastModified string     `json:"last_modified,omitempty"`
	CreatedAt    string     `json:"created_at,omitempty"`
	Tags         []string   `json:"tags,omitempty"`
	Files        int        `json:"files"`
	TotalBytes   int64      `json:"total_bytes"`
	LFSFiles     int        `json:"lfs_files"`
	LFSBytes     int64      `json:"lfs_bytes"`
	Siblings     []infoFile `json:"siblings,omitempty"`
}

func infoCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	fs.Var(stringSliceValue{&cfg.Filters}, "filter", "include glob; repeat for multiple patterns")
	jsonOut := fs.Bool("json", false, "emit JSON")
	files := fs.Bool("files", false, "include the per-file list in output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools info [options] OWNER/REPO")
	}
	info, repoID, rt, err := fetchRemote(ctx, cfg, fs.Arg(0), *typeFlag)
	if err != nil {
		return err
	}
	siblings := selectRepoFiles(info.Siblings, cfg.Filters)
	rep := infoReport{
		Type: string(rt), RepoID: repoID, Revision: cfg.Revision, Commit: info.SHA,
		Author: info.Author, Private: info.Private, Gated: gatedString(info.Gated),
		Library: info.LibraryName, Pipeline: info.PipelineTag, LastModified: info.LastModified,
		CreatedAt: info.CreatedAt, Tags: info.Tags,
	}
	for _, f := range siblings {
		rep.Files++
		rep.TotalBytes += f.Size
		if f.LFS != nil {
			rep.LFSFiles++
			rep.LFSBytes += f.Size
		}
		if *files {
			rep.Siblings = append(rep.Siblings, infoFile{Path: f.Path, Size: f.Size, LFS: f.LFS != nil})
		}
	}
	if *jsonOut {
		return printJSON(os.Stdout, rep)
	}
	fmt.Printf("type: %s\nrepo: %s\nrevision: %s\ncommit: %s\n", rep.Type, rep.RepoID, rep.Revision, rep.Commit)
	if rep.Author != "" {
		fmt.Printf("author: %s\n", rep.Author)
	}
	fmt.Printf("private: %t\ngated: %s\n", rep.Private, rep.Gated)
	if rep.Library != "" {
		fmt.Printf("library: %s\n", rep.Library)
	}
	if rep.Pipeline != "" {
		fmt.Printf("pipeline: %s\n", rep.Pipeline)
	}
	if rep.LastModified != "" {
		fmt.Printf("last modified: %s\n", rep.LastModified)
	}
	fmt.Printf("files: %d\ntotal: %s\nlfs: %d files (%s)\n", rep.Files, humanBytes(rep.TotalBytes), rep.LFSFiles, humanBytes(rep.LFSBytes))
	if len(rep.Tags) > 0 {
		fmt.Printf("tags: %s\n", strings.Join(rep.Tags, ", "))
	}
	if *files {
		for _, f := range rep.Siblings {
			marker := "  "
			if f.LFS {
				marker = "L "
			}
			fmt.Printf("  %s%12s  %s\n", marker, humanBytes(f.Size), f.Path)
		}
	}
	return nil
}

func lsCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	fs.Var(stringSliceValue{&cfg.Filters}, "filter", "include glob; repeat for multiple patterns")
	long := fs.Bool("long", false, "show size and LFS marker")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools ls [options] OWNER/REPO")
	}
	info, _, _, err := fetchRemote(ctx, cfg, fs.Arg(0), *typeFlag)
	if err != nil {
		return err
	}
	siblings := selectRepoFiles(info.Siblings, cfg.Filters)
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Path < siblings[j].Path })
	if *jsonOut {
		out := make([]infoFile, 0, len(siblings))
		for _, f := range siblings {
			out = append(out, infoFile{Path: f.Path, Size: f.Size, LFS: f.LFS != nil})
		}
		return printJSON(os.Stdout, out)
	}
	for _, f := range siblings {
		if *long {
			marker := "-"
			if f.LFS != nil {
				marker = "L"
			}
			fmt.Printf("%s %12s  %s\n", marker, humanBytes(f.Size), f.Path)
		} else {
			fmt.Println(f.Path)
		}
	}
	return nil
}

type diffEntry struct {
	Path       string `json:"path"`
	LocalSize  int64  `json:"local_size,omitempty"`
	RemoteSize int64  `json:"remote_size,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type diffReport struct {
	RepoID       string      `json:"repo_id"`
	Type         string      `json:"type"`
	Revision     string      `json:"revision"`
	LocalCommit  string      `json:"local_commit"`
	RemoteCommit string      `json:"remote_commit"`
	CommitMoved  bool        `json:"commit_moved"`
	Added        []diffEntry `json:"added,omitempty"`
	Removed      []diffEntry `json:"removed,omitempty"`
	Changed      []diffEntry `json:"changed,omitempty"`
	Unchanged    int         `json:"unchanged"`
}

func diffCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", ".", "local downloaded repository directory")
	revision := fs.String("revision", "", "remote revision to compare against (default: the recorded one)")
	endpoint := fs.String("endpoint", cfg.Endpoint, "Hugging Face Hub endpoint")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Endpoint = *endpoint
	root, err := resolveDir(*output)
	if err != nil {
		return err
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		return err
	}
	m, err := state.LoadManifest(filepath.Join(stateDir, "manifest.json"))
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no hftools manifest in %s", root)
	}
	repoType := m.RepoType
	if repoType == "" {
		repoType = string(hub.RepoTypeModel)
	}
	cfg.Revision = m.Revision
	if *revision != "" {
		cfg.Revision = *revision
	}
	info, _, _, err := fetchRemote(ctx, cfg, m.RepoID, repoType)
	if err != nil {
		return err
	}
	remote := map[string]hub.RepoFile{}
	for _, f := range selectRepoFiles(info.Siblings, m.Filters) {
		remote[f.Path] = f
	}
	rep := diffReport{RepoID: m.RepoID, Type: repoType, Revision: cfg.Revision, LocalCommit: m.CommitSHA, RemoteCommit: info.SHA, CommitMoved: m.CommitSHA != info.SHA}
	for p, rf := range remote {
		rec := m.Files[p]
		if rec == nil {
			rep.Added = append(rep.Added, diffEntry{Path: p, RemoteSize: rf.Size, Reason: "new"})
			continue
		}
		lfs := ""
		if rf.LFS != nil {
			lfs = rf.LFS.SHA256
		}
		switch {
		case rec.Size != rf.Size:
			rep.Changed = append(rep.Changed, diffEntry{Path: p, LocalSize: rec.Size, RemoteSize: rf.Size, Reason: "size"})
		case lfs != "" && !strings.EqualFold(rec.RemoteLFSSHA256, lfs):
			rep.Changed = append(rep.Changed, diffEntry{Path: p, LocalSize: rec.Size, RemoteSize: rf.Size, Reason: "content (lfs sha256)"})
		case lfs == "" && rf.BlobID != "" && !strings.EqualFold(rec.RemoteBlobSHA1, rf.BlobID):
			rep.Changed = append(rep.Changed, diffEntry{Path: p, LocalSize: rec.Size, RemoteSize: rf.Size, Reason: "content (git blob)"})
		default:
			rep.Unchanged++
		}
	}
	for p, rec := range m.Files {
		if _, ok := remote[p]; !ok {
			rep.Removed = append(rep.Removed, diffEntry{Path: p, LocalSize: rec.Size, Reason: "gone"})
		}
	}
	sortDiff(rep.Added)
	sortDiff(rep.Removed)
	sortDiff(rep.Changed)
	if *jsonOut {
		return printJSON(os.Stdout, rep)
	}
	fmt.Printf("repo: %s (%s)\nrevision: %s\nlocal commit:  %s\nremote commit: %s%s\n", rep.RepoID, rep.Type, rep.Revision, rep.LocalCommit, rep.RemoteCommit, moved(rep.CommitMoved))
	printDiffGroup("+ added", rep.Added, true)
	printDiffGroup("- removed", rep.Removed, false)
	printDiffGroup("~ changed", rep.Changed, true)
	fmt.Printf("summary: %d added, %d removed, %d changed, %d unchanged\n", len(rep.Added), len(rep.Removed), len(rep.Changed), rep.Unchanged)
	return nil
}

func moved(b bool) string {
	if b {
		return "  (moved)"
	}
	return "  (same)"
}

func sortDiff(d []diffEntry) { sort.Slice(d, func(i, j int) bool { return d[i].Path < d[j].Path }) }

func printDiffGroup(title string, entries []diffEntry, useRemote bool) {
	for _, e := range entries {
		size := e.RemoteSize
		if !useRemote {
			size = e.LocalSize
		}
		fmt.Printf("  %s %12s  %s  (%s)\n", title, humanBytes(size), e.Path, e.Reason)
	}
}

type duRepo struct {
	Dir    string `json:"dir"`
	RepoID string `json:"repo_id"`
	Type   string `json:"type"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

type duReport struct {
	Root  string           `json:"root"`
	Repos []duRepo         `json:"repos"`
	Total int64            `json:"total_bytes"`
	Types map[string]int64 `json:"by_type,omitempty"`
}

func duCommand(args []string) error {
	fs := flag.NewFlagSet("du", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootFlag := fs.String("root", ".", "root directory containing downloaded repositories")
	byType := fs.Bool("by-type", false, "break totals down by file extension")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveDir(*rootFlag)
	if err != nil {
		return err
	}
	repos, err := findRepositories(root)
	if err != nil {
		return err
	}
	sort.Strings(repos)
	rep := duReport{Root: root}
	if *byType {
		rep.Types = map[string]int64{}
	}
	for _, dir := range repos {
		stateDir, err := stateDirectory(dir)
		if err != nil {
			return err
		}
		m, err := state.LoadManifest(filepath.Join(stateDir, "manifest.json"))
		if err != nil || m == nil {
			continue
		}
		rt := m.RepoType
		if rt == "" {
			rt = string(hub.RepoTypeModel)
		}
		var bytes int64
		for _, f := range m.Files {
			bytes += f.Size
			if *byType {
				ext := strings.ToLower(filepath.Ext(f.Path))
				if ext == "" {
					ext = "(none)"
				}
				rep.Types[ext] += f.Size
			}
		}
		rel, _ := filepath.Rel(root, dir)
		if rel == "" {
			rel = dir
		}
		rep.Repos = append(rep.Repos, duRepo{Dir: rel, RepoID: m.RepoID, Type: rt, Files: len(m.Files), Bytes: bytes})
		rep.Total += bytes
	}
	if *jsonOut {
		return printJSON(os.Stdout, rep)
	}
	for _, r := range rep.Repos {
		fmt.Printf("%12s  %-8s %4d files  %s\n", humanBytes(r.Bytes), r.Type, r.Files, r.RepoID)
	}
	if *byType {
		fmt.Println("by type:")
		type kv struct {
			ext string
			n   int64
		}
		var kvs []kv
		for e, n := range rep.Types {
			kvs = append(kvs, kv{e, n})
		}
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].n > kvs[j].n })
		for _, k := range kvs {
			fmt.Printf("  %12s  %s\n", humanBytes(k.n), k.ext)
		}
	}
	fmt.Printf("total: %s across %d repositories\n", humanBytes(rep.Total), len(rep.Repos))
	return nil
}
