package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ManifestVersion = 1

type Manifest struct {
	Version             int                    `json:"version"`
	RepoType            string                 `json:"repo_type,omitempty"`
	RepoID              string                 `json:"repo_id"`
	Filters             []string               `json:"filters,omitempty"`
	Revision            string                 `json:"revision"`
	CommitSHA           string                 `json:"commit_sha"`
	HubLastModified     string                 `json:"hub_last_modified,omitempty"`
	RepositoryCreatedAt string                 `json:"repository_created_at,omitempty"`
	MetadataFetchedAt   *time.Time             `json:"metadata_fetched_at,omitempty"`
	UpdatedAt           time.Time              `json:"updated_at"`
	LastVerifiedAt      *time.Time             `json:"last_verified_at,omitempty"`
	Files               map[string]*FileRecord `json:"files"`
}

type RepositoryMetadata struct {
	Version           int             `json:"version"`
	RepoType          string          `json:"repo_type"`
	FetchedAt         time.Time       `json:"fetched_at"`
	Endpoint          string          `json:"endpoint"`
	RepoID            string          `json:"repo_id"`
	RequestedRevision string          `json:"requested_revision"`
	ResolvedCommitSHA string          `json:"resolved_commit_sha"`
	LastModified      string          `json:"last_modified,omitempty"`
	CreatedAt         string          `json:"created_at,omitempty"`
	Payload           json.RawMessage `json:"hub_api_response"`
}

type RepositoryMetadataEvent struct {
	FetchedAt         time.Time `json:"fetched_at"`
	RepoType          string    `json:"repo_type"`
	Endpoint          string    `json:"endpoint"`
	RepoID            string    `json:"repo_id"`
	RequestedRevision string    `json:"requested_revision"`
	ResolvedCommitSHA string    `json:"resolved_commit_sha"`
	LastModified      string    `json:"last_modified,omitempty"`
	CreatedAt         string    `json:"created_at,omitempty"`
}

type FileRecord struct {
	Path                 string     `json:"path"`
	Size                 int64      `json:"size"`
	RemoteBlobSHA1       string     `json:"remote_blob_sha1,omitempty"`
	RemoteLFSSHA256      string     `json:"remote_lfs_sha256,omitempty"`
	LocalSHA256          string     `json:"local_sha256"`
	LocalSHA1            string     `json:"local_sha1,omitempty"`
	LocalGitSHA1         string     `json:"local_git_sha1,omitempty"`
	ModTimeUnixNano      int64      `json:"mtime_unix_nano"`
	VerifiedAt           time.Time  `json:"verified_at"`
	CommitSHA            string     `json:"commit_sha"`
	VerificationError    string     `json:"verification_error,omitempty"`
	VerificationFailedAt *time.Time `json:"verification_failed_at,omitempty"`
}

type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Next  int64 `json:"next"`
}

type PartialState struct {
	Version         int       `json:"version"`
	RepoType        string    `json:"repo_type,omitempty"`
	RepoID          string    `json:"repo_id"`
	CommitSHA       string    `json:"commit_sha"`
	Path            string    `json:"path"`
	Size            int64     `json:"size"`
	RemoteBlobSHA1  string    `json:"remote_blob_sha1,omitempty"`
	RemoteLFSSHA256 string    `json:"remote_lfs_sha256,omitempty"`
	Segments        []Segment `json:"segments"`
}

type VerifyHistory struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	RepoType    string    `json:"repo_type,omitempty"`
	RepoID      string    `json:"repo_id"`
	Revision    string    `json:"revision"`
	CommitSHA   string    `json:"commit_sha"`
	Forced      bool      `json:"forced"`
	Checked     int       `json:"checked"`
	Skipped     int       `json:"skipped"`
	Passed      int       `json:"passed"`
	Failed      int       `json:"failed"`
	Failures    []string  `json:"failures,omitempty"`
}

func NewManifest(repoID, revision, commitSHA string) *Manifest {
	return &Manifest{
		Version: ManifestVersion, RepoID: repoID, Revision: revision,
		CommitSHA: commitSHA, Files: make(map[string]*FileRecord),
	}
}

func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if m.Files == nil {
		m.Files = make(map[string]*FileRecord)
	}
	return &m, nil
}

func SaveJSONAtomic(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(path, b, 0o600)
}

func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func WriteChecksumFile(path string, m *Manifest) error {
	return writeChecksumFile(path, m, "SHA-256", func(f *FileRecord) string { return f.LocalSHA256 })
}

func WriteSHA1ChecksumFile(path string, m *Manifest) error {
	return writeChecksumFile(path, m, "SHA-1", func(f *FileRecord) string { return f.LocalSHA1 })
}

func writeChecksumFile(path string, m *Manifest, algorithm string, checksum func(*FileRecord) string) error {
	var b strings.Builder
	repoType := m.RepoType
	if repoType == "" {
		repoType = "model"
	}
	fmt.Fprintf(&b, "# hftools %s\n# type: %s\n# repo: %s\n# revision: %s\n# commit: %s\n", algorithm, repoType, m.RepoID, m.Revision, m.CommitSHA)
	for _, f := range SortedFiles(m) {
		hash := checksum(f)
		if hash == "" || f.VerificationError != "" {
			continue
		}
		name := f.Path
		prefix := ""
		if strings.ContainsAny(name, "\\\n") {
			prefix = "\\"
			name = strings.ReplaceAll(name, "\\", "\\\\")
			name = strings.ReplaceAll(name, "\n", "\\n")
		}
		fmt.Fprintf(&b, "%s%s  %s\n", prefix, hash, name)
	}
	return WriteFileAtomic(path, []byte(b.String()), 0o644)
}

func AppendHistory(path string, h VerifyHistory) error {
	return AppendJSONLine(path, h)
}

func AppendJSONLine(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

func SortedFiles(m *Manifest) []*FileRecord {
	files := make([]*FileRecord, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}
