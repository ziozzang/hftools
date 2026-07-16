package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
	// Retries bounds retries for retriable API failures (5xx/429/network).
	// A negative value retries until success or a terminal error.
	Retries int
	// RetryMinWait and RetryMaxWait bound the randomized retry backoff; zero
	// values fall back to 1s and 5m.
	RetryMinWait time.Duration
	RetryMaxWait time.Duration
}

type RepoType string

const (
	RepoTypeModel   RepoType = "model"
	RepoTypeDataset RepoType = "dataset"
)

func (t RepoType) normalized() RepoType {
	if t == "" {
		return RepoTypeModel
	}
	return t
}

func (t RepoType) Validate() error {
	switch t.normalized() {
	case RepoTypeModel, RepoTypeDataset:
		return nil
	default:
		return fmt.Errorf("unsupported repository type %q", t)
	}
}

type RepoInfo struct {
	ID           string          `json:"id"`
	SHA          string          `json:"sha"`
	LastModified string          `json:"lastModified"`
	CreatedAt    string          `json:"createdAt"`
	Author       string          `json:"author"`
	LibraryName  string          `json:"library_name"`
	PipelineTag  string          `json:"pipeline_tag"`
	Private      bool            `json:"private"`
	Gated        json.RawMessage `json:"gated"`
	Tags         []string        `json:"tags"`
	Siblings     []RepoFile      `json:"siblings"`
	RawMetadata  json.RawMessage `json:"-"`
}

type RepoFile struct {
	Path   string   `json:"rfilename"`
	BlobID string   `json:"blobId"`
	Size   int64    `json:"size"`
	LFS    *LFSInfo `json:"lfs,omitempty"`
}

type LFSInfo struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func New(endpoint, token string, timeout time.Duration) *Client {
	endpoint = strings.TrimRight(endpoint, "/")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	transport.ResponseHeaderTimeout = timeout
	return &Client{
		Endpoint: endpoint,
		Token:    token,
		HTTP:     &http.Client{Transport: transport},
	}
}

func (c *Client) RepoInfo(ctx context.Context, repoType RepoType, repoID, revision string) (*RepoInfo, error) {
	if err := repoType.Validate(); err != nil {
		return nil, err
	}
	collection := "models"
	if repoType.normalized() == RepoTypeDataset {
		collection = "datasets"
	}
	u := c.Endpoint + "/api/" + collection + "/" + escapeRepo(repoID) + "/revision/" + url.PathEscape(revision) + "?blobs=true"

	unlimited := c.Retries < 0
	minWait, maxWait := RetryWaits(c.RetryMinWait, c.RetryMaxWait)
	var lastErr error
	for attempt := 0; unlimited || attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(RetryDelay(attempt-1, minWait, maxWait)):
			}
		}
		info, retriable, err := c.fetchRepoInfo(ctx, u)
		if err == nil {
			return info, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retriable {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// fetchRepoInfo performs a single metadata request. The bool reports whether a
// failure is retriable (transient server/network error) versus terminal.
func (c *Client) fetchRepoInfo(ctx context.Context, u string) (*RepoInfo, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	c.setHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("repository metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, RetriableStatus(resp.StatusCode), responseError("repository metadata", resp)
	}
	const maxMetadataSize = 64 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize+1))
	if err != nil {
		return nil, true, fmt.Errorf("repository metadata read: %w", err)
	}
	if len(b) > maxMetadataSize {
		return nil, false, fmt.Errorf("repository metadata exceeds %d bytes", maxMetadataSize)
	}
	var info RepoInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, false, fmt.Errorf("repository metadata decode: %w", err)
	}
	info.RawMetadata = append(json.RawMessage(nil), b...)
	if info.SHA == "" {
		return nil, false, fmt.Errorf("repository metadata did not contain a commit SHA")
	}
	for i := range info.Siblings {
		f := &info.Siblings[i]
		if f.Path == "" || f.Size < 0 {
			return nil, false, fmt.Errorf("invalid file metadata at index %d", i)
		}
		if f.LFS != nil && f.LFS.Size > 0 && f.Size != f.LFS.Size {
			return nil, false, fmt.Errorf("inconsistent size metadata for %q", f.Path)
		}
	}
	return &info, false, nil
}

func (c *Client) DownloadURL(repoType RepoType, repoID, commitSHA, filePath string) string {
	segments := strings.Split(filePath, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	prefix := ""
	if repoType.normalized() == RepoTypeDataset {
		prefix = "/datasets"
	}
	return c.Endpoint + prefix + "/" + escapeRepo(repoID) + "/resolve/" + url.PathEscape(commitSHA) + "/" + strings.Join(segments, "/") + "?download=true"
}

func (c *Client) NewDownloadRequest(ctx context.Context, rawURL string, start, end int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	req.Header.Set("Accept-Encoding", "identity")
	if start >= 0 && end >= start {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}
	return req, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "hftools/1")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func escapeRepo(repoID string) string {
	parts := strings.Split(repoID, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func responseError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(b))
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if msg == "" {
		return fmt.Errorf("%s: HTTP %s", op, resp.Status)
	}
	return fmt.Errorf("%s: HTTP %s: %s", op, resp.Status, msg)
}

func ValidateRepoID(repoID string) error {
	if repoID == "" || strings.Contains(repoID, "\\") || strings.HasPrefix(repoID, "/") || strings.HasSuffix(repoID, "/") {
		return fmt.Errorf("invalid repository ID %q", repoID)
	}
	parts := strings.Split(repoID, "/")
	if len(parts) > 2 {
		return fmt.Errorf("invalid repository ID %q: expected NAME or OWNER/NAME", repoID)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || path.Clean(part) != part {
			return fmt.Errorf("invalid repository ID %q", repoID)
		}
	}
	return nil
}

func NormalizeRepoID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("repository cannot be empty")
	}
	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return "", fmt.Errorf("invalid Hugging Face repository URL %q", input)
		}
		input = strings.Trim(u.Path, "/")
		if strings.HasPrefix(input, "datasets/") {
			input = strings.TrimPrefix(input, "datasets/")
		}
	}
	if err := ValidateRepoID(input); err != nil {
		return "", err
	}
	return input, nil
}

func LocalDirectoryName(repoID string) string {
	return strings.ReplaceAll(repoID, "/", "_")
}
