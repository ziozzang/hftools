package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

// UploadFile is a local file to publish at PathInRepo.
type UploadFile struct {
	LocalPath  string
	PathInRepo string

	// Filled in during Upload.
	size   int64
	sha256 string
	sample string // base64 of the leading bytes, for preupload classification
	lfs    bool
}

// CommitResult reports where a commit landed.
type CommitResult struct {
	CommitURL string `json:"commitUrl"`
	CommitOID string `json:"commitOid"`
}

const lfsSampleSize = 512

// Upload publishes files to repoID@revision in a single commit, sending large
// files through Git LFS and small files inline. revision must already exist.
func (c *Client) Upload(ctx context.Context, repoType RepoType, repoID, revision string, files []UploadFile, message, description string) (*CommitResult, error) {
	if err := repoType.Validate(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to upload")
	}
	if revision == "" {
		revision = "main"
	}
	// Stat + sample every file.
	for i := range files {
		st, err := os.Stat(files[i].LocalPath)
		if err != nil {
			return nil, err
		}
		files[i].size = st.Size()
		sample, err := readSample(files[i].LocalPath, lfsSampleSize)
		if err != nil {
			return nil, err
		}
		files[i].sample = base64.StdEncoding.EncodeToString(sample)
	}
	// Classify regular vs LFS via preupload.
	if err := c.preupload(ctx, repoType, repoID, revision, files); err != nil {
		return nil, err
	}
	// Hash and upload LFS objects.
	for i := range files {
		if !files[i].lfs {
			continue
		}
		sum, err := fileSHA256(files[i].LocalPath)
		if err != nil {
			return nil, err
		}
		files[i].sha256 = sum
	}
	if err := c.uploadLFS(ctx, repoType, repoID, revision, files); err != nil {
		return nil, err
	}
	return c.commit(ctx, repoType, repoID, revision, files, message, description)
}

func (c *Client) preupload(ctx context.Context, repoType RepoType, repoID, revision string, files []UploadFile) error {
	type reqFile struct {
		Path   string `json:"path"`
		Sample string `json:"sample"`
		Size   int64  `json:"size"`
	}
	req := struct {
		Files []reqFile `json:"files"`
	}{}
	for _, f := range files {
		req.Files = append(req.Files, reqFile{Path: f.PathInRepo, Sample: f.sample, Size: f.size})
	}
	b, _ := json.Marshal(req)
	var resp struct {
		Files []struct {
			Path         string `json:"path"`
			UploadMode   string `json:"uploadMode"`
			ShouldIgnore bool   `json:"shouldIgnore"`
		} `json:"files"`
	}
	u := c.Endpoint + "/api/" + repoType.Collection() + "/" + escapeRepo(repoID) + "/preupload/" + revision
	if err := c.doJSON(ctx, http.MethodPost, u, "preupload", b, &resp, true); err != nil {
		return err
	}
	mode := map[string]string{}
	for _, f := range resp.Files {
		mode[f.Path] = f.UploadMode
	}
	for i := range files {
		if mode[files[i].PathInRepo] == "lfs" {
			files[i].lfs = true
		}
	}
	return nil
}

func (c *Client) uploadLFS(ctx context.Context, repoType RepoType, repoID, revision string, files []UploadFile) error {
	var lfsFiles []*UploadFile
	for i := range files {
		if files[i].lfs {
			lfsFiles = append(lfsFiles, &files[i])
		}
	}
	if len(lfsFiles) == 0 {
		return nil
	}
	type objID struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	}
	batchReq := map[string]any{
		"operation": "upload",
		"transfers": []string{"basic", "multipart"},
		"ref":       map[string]string{"name": "refs/heads/" + revision},
		"hash_algo": "sha256",
	}
	var objs []objID
	for _, f := range lfsFiles {
		objs = append(objs, objID{OID: f.sha256, Size: f.size})
	}
	batchReq["objects"] = objs
	b, _ := json.Marshal(batchReq)

	batchURL := c.Endpoint + repoType.pathPrefix() + "/" + escapeRepo(repoID) + ".git/info/lfs/objects/batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, batchURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("lfs batch: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("lfs batch: HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var batch struct {
		Objects []struct {
			OID     string `json:"oid"`
			Size    int64  `json:"size"`
			Actions struct {
				Upload *struct {
					Href   string            `json:"href"`
					Header map[string]string `json:"header"`
				} `json:"upload"`
				Verify *struct {
					Href   string            `json:"href"`
					Header map[string]string `json:"header"`
				} `json:"verify"`
			} `json:"actions"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		return fmt.Errorf("lfs batch decode: %w", err)
	}
	byOID := map[string]*UploadFile{}
	for _, f := range lfsFiles {
		byOID[f.sha256] = f
	}
	for _, o := range batch.Objects {
		if o.Error != nil {
			return fmt.Errorf("lfs object %s: %s", o.OID, o.Error.Message)
		}
		if o.Actions.Upload == nil {
			continue // already present on the server
		}
		f := byOID[o.OID]
		if f == nil {
			continue
		}
		if err := c.lfsPut(ctx, f, o.Actions.Upload.Href, o.Actions.Upload.Header); err != nil {
			return err
		}
		if o.Actions.Verify != nil {
			if err := c.lfsVerify(ctx, o.Actions.Verify.Href, f.sha256, f.size); err != nil {
				return err
			}
		}
	}
	return nil
}

// lfsPut uploads a single object, either as one PUT (basic) or as several
// presigned part PUTs followed by a completion POST (multipart).
func (c *Client) lfsPut(ctx context.Context, f *UploadFile, href string, header map[string]string) error {
	if cs, ok := header["chunk_size"]; ok {
		return c.lfsMultipart(ctx, f, href, header, cs)
	}
	file, err := os.Open(f.LocalPath)
	if err != nil {
		return err
	}
	defer file.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, href, file)
	if err != nil {
		return err
	}
	req.ContentLength = f.size
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("lfs upload %s: %w", f.PathInRepo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("lfs upload %s: HTTP %s: %s", f.PathInRepo, resp.Status, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) lfsMultipart(ctx context.Context, f *UploadFile, completionHref string, header map[string]string, chunkSizeStr string) error {
	chunkSize, err := strconv.ParseInt(chunkSizeStr, 10, 64)
	if err != nil || chunkSize <= 0 {
		return fmt.Errorf("invalid multipart chunk_size %q", chunkSizeStr)
	}
	// Part URLs are the numeric header keys, ordered.
	var partKeys []int
	for k := range header {
		if n, err := strconv.Atoi(k); err == nil {
			partKeys = append(partKeys, n)
		}
	}
	sort.Ints(partKeys)
	file, err := os.Open(f.LocalPath)
	if err != nil {
		return err
	}
	defer file.Close()
	type completedPart struct {
		PartNumber int    `json:"partNumber"`
		ETag       string `json:"etag"`
	}
	var parts []completedPart
	buf := make([]byte, chunkSize)
	for _, n := range partKeys {
		read, err := io.ReadFull(file, buf)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			err = nil
		}
		if err != nil {
			return err
		}
		if read == 0 {
			break
		}
		partURL := header[strconv.Itoa(n)]
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, bytes.NewReader(buf[:read]))
		if err != nil {
			return err
		}
		req.ContentLength = int64(read)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("lfs part %d of %s: %w", n, f.PathInRepo, err)
		}
		etag := resp.Header.Get("ETag")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("lfs part %d of %s: HTTP %s", n, f.PathInRepo, resp.Status)
		}
		parts = append(parts, completedPart{PartNumber: n, ETag: etag})
	}
	// Complete the multipart upload.
	body, _ := json.Marshal(map[string]any{"oid": f.sha256, "parts": parts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, completionHref, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("lfs complete %s: %w", f.PathInRepo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("lfs complete %s: HTTP %s: %s", f.PathInRepo, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (c *Client) lfsVerify(ctx context.Context, href, oid string, size int64) error {
	body, _ := json.Marshal(map[string]any{"oid": oid, "size": size})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, href, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("lfs verify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("lfs verify: HTTP %s", resp.Status)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// commit builds the NDJSON commit payload and posts it. It is not retried: a
// network error after the server committed must not create a second commit.
func (c *Client) commit(ctx context.Context, repoType RepoType, repoID, revision string, files []UploadFile, message, description string) (*CommitResult, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if message == "" {
		message = fmt.Sprintf("Upload %d file(s) with hftools", len(files))
	}
	_ = enc.Encode(map[string]any{"key": "header", "value": map[string]any{"summary": message, "description": description}})
	for _, f := range files {
		if f.lfs {
			_ = enc.Encode(map[string]any{"key": "lfsFile", "value": map[string]any{
				"path": f.PathInRepo, "algo": "sha256", "oid": f.sha256, "size": f.size,
			}})
			continue
		}
		content, err := os.ReadFile(f.LocalPath)
		if err != nil {
			return nil, err
		}
		_ = enc.Encode(map[string]any{"key": "file", "value": map[string]any{
			"path": f.PathInRepo, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content),
		}})
	}
	u := c.Endpoint + "/api/" + repoType.Collection() + "/" + escapeRepo(repoID) + "/commit/" + revision
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("commit: HTTP %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var out CommitResult
	_ = json.Unmarshal(respBody, &out)
	return &out, nil
}

func readSample(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:read], nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
