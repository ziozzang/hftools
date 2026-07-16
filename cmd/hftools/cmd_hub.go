package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/hfcache"
	"github.com/ziozzang/hftools/internal/hub"
)

// whoamiCommand prints the identity the current token authenticates as.
func whoamiCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "Hugging Face Hub endpoint")
	fs.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the access token")
	fs.StringVar(&cfg.Token, "token", "", "Hugging Face access token")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if resolveToken(cfg) == "" {
		return fmt.Errorf("no token found (set $%s, pass --token, or run 'huggingface-cli login')", cfg.TokenEnv)
	}
	w, err := newHubClient(cfg).WhoAmI(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, w)
	}
	fmt.Printf("name: %s\n", w.Name)
	if w.FullName != "" {
		fmt.Printf("fullname: %s\n", w.FullName)
	}
	if w.Type != "" {
		fmt.Printf("type: %s\n", w.Type)
	}
	if w.Email != "" {
		fmt.Printf("email: %s\n", w.Email)
	}
	if w.Auth.AccessToken.DisplayName != "" {
		fmt.Printf("token: %s (%s)\n", w.Auth.AccessToken.DisplayName, w.Auth.AccessToken.Role)
	}
	if len(w.Orgs) > 0 {
		names := make([]string, 0, len(w.Orgs))
		for _, o := range w.Orgs {
			names = append(names, o.Name)
		}
		fmt.Printf("orgs: %s\n", strings.Join(names, ", "))
	}
	return nil
}

// refsCommand lists a repository's branches and tags.
func refsCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("refs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools refs [options] OWNER/REPO")
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
		return printJSON(os.Stdout, refs)
	}
	printRefs("branches", refs.Branches)
	printRefs("tags", refs.Tags)
	printRefs("converts", refs.Converts)
	return nil
}

func printRefs(title string, refs []hub.Ref) {
	if len(refs) == 0 {
		return
	}
	fmt.Printf("%s:\n", title)
	for _, r := range refs {
		commit := r.TargetCommit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Printf("  %-30s %s\n", r.Name, commit)
	}
}

// searchCommand searches the Hub for repositories.
func searchCommand(ctx context.Context, args []string) error {
	cfg, _, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	typeFlag := remoteFlags(fs, &cfg)
	author := fs.String("author", "", "restrict to an owner/organization")
	var filters multiFlag
	fs.Var(&filters, "filter", "tag filter (for example 'text-generation'); repeat for multiple")
	limit := fs.Int("limit", 20, "maximum results")
	sortField := fs.String("sort", "downloads", "sort by: downloads, likes, modified, or trending")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, err := repoTypeFrom(*typeFlag)
	if err != nil {
		return err
	}
	opts := hub.SearchOptions{
		Query:     strings.Join(fs.Args(), " "),
		Author:    *author,
		Filter:    filters,
		Limit:     *limit,
		Sort:      hub.NormalizeSort(*sortField),
		Direction: -1,
	}
	results, err := newHubClient(cfg).Search(ctx, rt, opts)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, results)
	}
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return nil
	}
	for _, r := range results {
		fmt.Printf("%-48s  ↓%-8s ♥%-6s  %s\n", r.ID, humanCount(r.Downloads), humanCount(r.Likes), r.PipelineTag)
	}
	fmt.Fprintf(os.Stderr, "%d result(s)\n", len(results))
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// cacheScanCommand summarizes disk usage of a Hugging Face cache.
func cacheScanCommand(args []string) error {
	fs := flag.NewFlagSet("cache-scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cache := fs.String("cache", "", "Hugging Face cache root (default: the standard location)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repos, err := hfcache.ListRepos(*cache)
	if err != nil {
		return err
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Bytes > repos[j].Bytes })
	if *jsonOut {
		return printJSON(os.Stdout, repos)
	}
	var total int64
	var blobs int
	for _, r := range repos {
		total += r.Bytes
		blobs += r.Blobs
		fmt.Printf("%12s  %-8s %3d rev  %3d blob  %s\n", humanBytes(r.Bytes), r.RepoType, len(r.Commits), r.Blobs, r.RepoID)
	}
	fmt.Printf("total: %s across %d repositories (%d blobs)\n", humanBytes(total), len(repos), blobs)
	return nil
}
