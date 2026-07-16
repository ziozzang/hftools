package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ziozzang/hfdownload/internal/download"
	"github.com/ziozzang/hfdownload/internal/hub"
	"github.com/ziozzang/hfdownload/internal/state"
)

// serveCommand exposes local downloads over the Hugging Face Hub URL scheme so
// another machine on an offline network can fetch them with
// `hfdown download --endpoint http://this-host:port ...`.
func serveCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", ".", "directory containing hfdown download repositories")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	tokenEnv := fs.String("token-env", "", "require this env var's value as a bearer token (default: no auth)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	rootAbs, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return err
	}
	m, err := newMirror(rootAbs)
	if err != nil {
		return err
	}
	if len(m.repos) == 0 {
		return fmt.Errorf("no hfdown repositories found under %s", rootAbs)
	}
	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}
	for key, e := range m.repos {
		fmt.Fprintf(os.Stderr, "  %s %s@%s (%d files)\n", strings.SplitN(key, "\x00", 2)[0], e.m.RepoID, e.m.CommitSHA, len(e.m.Files))
	}
	fmt.Fprintf(os.Stderr, "serving %d repositories from %s on http://%s\n", len(m.repos), rootAbs, *addr)

	server := &http.Server{Addr: *addr, Handler: m.handler(token)}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type mirror struct {
	repos map[string]*repoEntry // key: repoType + "\x00" + repoID
}

type repoEntry struct {
	dir string
	m   *state.Manifest
}

func mirrorKey(repoType, repoID string) string { return repoType + "\x00" + repoID }

// newMirror scans root for hfdown repositories (directories with a
// .metadata/manifest.json) and indexes them by repository type and id.
func newMirror(root string) (*mirror, error) {
	m := &mirror{repos: make(map[string]*repoEntry)}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		manifestPath := filepath.Join(path, ".metadata", "manifest.json")
		if st, err := os.Stat(manifestPath); err != nil || !st.Mode().IsRegular() {
			return nil
		}
		man, err := state.LoadManifest(manifestPath)
		if err != nil || man == nil {
			return filepath.SkipDir
		}
		repoType := man.RepoType
		if repoType == "" {
			repoType = string(hub.RepoTypeModel)
		}
		m.repos[mirrorKey(repoType, man.RepoID)] = &repoEntry{dir: path, m: man}
		return filepath.SkipDir // a repository root is not nested inside another
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *mirror) handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/models/"):
			s.metadata(w, r, string(hub.RepoTypeModel), strings.TrimPrefix(p, "/api/models/"))
		case strings.HasPrefix(p, "/api/datasets/"):
			s.metadata(w, r, string(hub.RepoTypeDataset), strings.TrimPrefix(p, "/api/datasets/"))
		case strings.HasPrefix(p, "/datasets/") && strings.Contains(p, "/resolve/"):
			s.resolve(w, r, string(hub.RepoTypeDataset), strings.TrimPrefix(p, "/datasets/"))
		case strings.Contains(p, "/resolve/"):
			s.resolve(w, r, string(hub.RepoTypeModel), strings.TrimPrefix(p, "/"))
		default:
			http.NotFound(w, r)
		}
	})
}

type apiLFS struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type apiSibling struct {
	Path   string  `json:"rfilename"`
	BlobID string  `json:"blobId,omitempty"`
	Size   int64   `json:"size"`
	LFS    *apiLFS `json:"lfs,omitempty"`
}

type apiRepoInfo struct {
	ID           string       `json:"id"`
	SHA          string       `json:"sha"`
	LastModified string       `json:"lastModified,omitempty"`
	CreatedAt    string       `json:"createdAt,omitempty"`
	Siblings     []apiSibling `json:"siblings"`
}

func (s *mirror) metadata(w http.ResponseWriter, r *http.Request, repoType, rest string) {
	i := strings.Index(rest, "/revision/")
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	repoID := rest[:i]
	revision := rest[i+len("/revision/"):]
	entry := s.repos[mirrorKey(repoType, repoID)]
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	m := entry.m
	if revision != m.Revision && revision != m.CommitSHA {
		http.Error(w, "revision not available offline", http.StatusNotFound)
		return
	}
	info := apiRepoInfo{ID: m.RepoID, SHA: m.CommitSHA, LastModified: m.HubLastModified, CreatedAt: m.RepositoryCreatedAt}
	for _, rec := range state.SortedFiles(m) {
		sb := apiSibling{Path: rec.Path, BlobID: rec.RemoteBlobSHA1, Size: rec.Size}
		if rec.RemoteLFSSHA256 != "" {
			sb.LFS = &apiLFS{SHA256: rec.RemoteLFSSHA256, Size: rec.Size}
		}
		info.Siblings = append(info.Siblings, sb)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *mirror) resolve(w http.ResponseWriter, r *http.Request, repoType, rest string) {
	i := strings.Index(rest, "/resolve/")
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	repoID := rest[:i]
	tail := rest[i+len("/resolve/"):]
	j := strings.Index(tail, "/")
	if j < 0 {
		http.NotFound(w, r)
		return
	}
	commit := tail[:j]
	relPath := tail[j+1:]
	entry := s.repos[mirrorKey(repoType, repoID)]
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	if commit != entry.m.CommitSHA && commit != entry.m.Revision {
		http.NotFound(w, r)
		return
	}
	target, err := download.SafeTarget(entry.dir, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	f, err := os.Open(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// ServeContent handles Range requests (206 + Content-Range), HEAD, and
	// Content-Length; it never applies compression.
	http.ServeContent(w, r, filepath.Base(target), st.ModTime(), f)
}
