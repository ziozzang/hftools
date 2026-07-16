package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hfdownload/internal/hfcache"
	"github.com/ziozzang/hfdownload/internal/state"
)

var repoMetaFiles = map[string]bool{".sha256": true, ".sha1sum": true, signatureFile: true, ".hftools.json": true, ".hfdown.json": true}
var repoMetaDirs = map[string]bool{".metadata": true, "hfdown-metadata": true, ".hfdown": true, "tmp": true}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// gcCommand reclaims space from local repositories: leftover partial-download
// working directories and, optionally, files no longer tracked by the manifest.
// It is a dry run unless --yes is given.
func gcCommand(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootFlag := fs.String("root", ".", "root directory containing downloaded repositories")
	orphans := fs.Bool("orphans", false, "also remove files not tracked by the manifest")
	tmp := fs.Bool("tmp", false, "also remove leftover partial-download working directories (tmp/) — discards resume state")
	yes := fs.Bool("yes", false, "actually delete (default: dry run)")
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
	var reclaim, removed int64
	var items int
	for _, dir := range repos {
		stateDir, err := stateDirectory(dir)
		if err != nil {
			return err
		}
		m, err := state.LoadManifest(filepath.Join(stateDir, "manifest.json"))
		if err != nil || m == nil {
			continue
		}
		if *tmp {
			tmpDir := filepath.Join(dir, "tmp")
			if st, err := os.Stat(tmpDir); err == nil && st.IsDir() {
				sz := dirSize(tmpDir)
				items++
				reclaim += sz
				fmt.Printf("tmp    %s  %s\n", humanBytes(sz), filepathRel(root, tmpDir))
				if *yes {
					if err := os.RemoveAll(tmpDir); err != nil {
						return err
					}
					removed += sz
				}
			}
		}
		if *orphans {
			known := make(map[string]bool, len(m.Files))
			for p := range m.Files {
				known[p] = true
			}
			err = filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
				if werr != nil {
					return werr
				}
				rel, _ := filepath.Rel(dir, p)
				top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
				if d.IsDir() {
					if repoMetaDirs[top] {
						return filepath.SkipDir
					}
					return nil
				}
				if rel == "." {
					return nil
				}
				if repoMetaDirs[top] {
					return nil
				}
				if repoMetaFiles[filepath.ToSlash(rel)] {
					return nil
				}
				if known[filepath.ToSlash(rel)] {
					return nil
				}
				info, err := d.Info()
				if err != nil || !info.Mode().IsRegular() {
					return nil
				}
				items++
				reclaim += info.Size()
				fmt.Printf("orphan %s  %s\n", humanBytes(info.Size()), filepathRel(root, p))
				if *yes {
					if err := os.Remove(p); err != nil {
						return err
					}
					removed += info.Size()
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	if !*tmp && !*orphans {
		fmt.Fprintln(os.Stderr, "nothing selected; pass --tmp and/or --orphans (add --yes to delete)")
		return nil
	}
	if *yes {
		fmt.Printf("removed %d item(s), reclaimed %s\n", items, humanBytes(removed))
	} else {
		fmt.Printf("would remove %d item(s), reclaiming %s (dry run; pass --yes to delete)\n", items, humanBytes(reclaim))
	}
	return nil
}

func filepathRel(base, p string) string {
	if rel, err := filepath.Rel(base, p); err == nil {
		return rel
	}
	return p
}

// cacheGCCommand removes HF-cache blobs no snapshot references. Dry run unless --yes.
func cacheGCCommand(args []string) error {
	fs := flag.NewFlagSet("cache-gc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cache := fs.String("cache", "", "Hugging Face cache root (default: the standard location)")
	yes := fs.Bool("yes", false, "actually delete orphan blobs (default: dry run)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep, err := hfcache.GCCache(*cache, *yes)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, rep)
	}
	for _, name := range rep.OrphanNames {
		fmt.Printf("orphan %s\n", name)
	}
	if *yes {
		fmt.Printf("repos=%d blobs=%d removed=%d reclaimed=%s\n", rep.Repos, rep.Blobs, rep.Removed, humanBytes(rep.RemovedBytes))
	} else {
		fmt.Printf("repos=%d blobs=%d orphans=%d reclaimable=%s (dry run; pass --yes to delete)\n", rep.Repos, rep.Blobs, rep.Orphans, humanBytes(rep.OrphanBytes))
	}
	return nil
}

type dedupFile struct {
	abs  string
	size int64
}

// dedupCommand hardlinks byte-identical files (matched by recorded SHA-256)
// across repositories so duplicates share one copy on disk. Dry run unless --yes.
func dedupCommand(args []string) error {
	fs := flag.NewFlagSet("dedup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootFlag := fs.String("root", ".", "root directory containing downloaded repositories")
	yes := fs.Bool("yes", false, "actually hardlink duplicates (default: dry run)")
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
	groups := map[string][]dedupFile{} // key: sha256 + ":" + size
	for _, dir := range repos {
		stateDir, err := stateDirectory(dir)
		if err != nil {
			return err
		}
		m, err := state.LoadManifest(filepath.Join(stateDir, "manifest.json"))
		if err != nil || m == nil {
			continue
		}
		for _, rec := range m.Files {
			if rec.LocalSHA256 == "" {
				continue
			}
			abs := filepath.Join(dir, filepath.FromSlash(rec.Path))
			st, err := os.Stat(abs)
			if err != nil || !st.Mode().IsRegular() || st.Size() != rec.Size {
				continue
			}
			key := rec.LocalSHA256 + ":" + fmt.Sprint(rec.Size)
			groups[key] = append(groups[key], dedupFile{abs: abs, size: rec.Size})
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var reclaimable, reclaimed int64
	var linked, skipped int
	for _, k := range keys {
		files := groups[k]
		if len(files) < 2 {
			continue
		}
		sort.Slice(files, func(i, j int) bool { return files[i].abs < files[j].abs })
		canonical := files[0]
		cInfo, err := os.Stat(canonical.abs)
		if err != nil {
			continue
		}
		for _, dup := range files[1:] {
			dInfo, err := os.Stat(dup.abs)
			if err != nil {
				continue
			}
			if os.SameFile(cInfo, dInfo) {
				continue // already hardlinked
			}
			reclaimable += dup.size
			if !*yes {
				fmt.Printf("dup   %s  %s -> %s\n", humanBytes(dup.size), filepathRel(root, dup.abs), filepathRel(root, canonical.abs))
				continue
			}
			if err := hardlinkReplace(canonical.abs, dup.abs); err != nil {
				fmt.Printf("skip  %s (%v)\n", filepathRel(root, dup.abs), err)
				skipped++
				continue
			}
			linked++
			reclaimed += dup.size
			fmt.Printf("link  %s  %s -> %s\n", humanBytes(dup.size), filepathRel(root, dup.abs), filepathRel(root, canonical.abs))
		}
	}
	if *yes {
		fmt.Printf("linked %d file(s), reclaimed %s, skipped %d\n", linked, humanBytes(reclaimed), skipped)
	} else {
		fmt.Printf("would link duplicates reclaiming %s (dry run; pass --yes)\n", humanBytes(reclaimable))
	}
	return nil
}

// hardlinkReplace atomically replaces dup with a hardlink to canonical. Both
// must be on the same filesystem; a cross-device attempt returns an error and
// leaves dup untouched.
func hardlinkReplace(canonical, dup string) error {
	tmp := dup + ".hftools-dedup"
	_ = os.Remove(tmp)
	if err := os.Link(canonical, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dup); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
