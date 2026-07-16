package hub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FileHead summarizes a resolve HEAD request.
type FileHead struct {
	Size         int64
	ETag         string
	AcceptRanges bool
}

// Head issues a HEAD against a resolve URL and reports the object size, ETag,
// and whether the origin advertises byte-range support. It retries transient
// failures using the client's backoff policy.
func (c *Client) Head(ctx context.Context, rawURL string) (FileHead, error) {
	var out FileHead
	err := c.withRetry(ctx, "head", func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
		if err != nil {
			return false, err
		}
		c.setHeaders(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return true, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		if resp.StatusCode != http.StatusOK {
			return RetriableStatus(resp.StatusCode), responseError("head", resp)
		}
		out.ETag = strings.Trim(resp.Header.Get("ETag"), `"`)
		out.AcceptRanges = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
				out.Size = n
			}
		}
		// HF's LFS objects report the true size via a dedicated header.
		if xl := resp.Header.Get("X-Linked-Size"); xl != "" {
			if n, perr := strconv.ParseInt(xl, 10, 64); perr == nil {
				out.Size = n
			}
		}
		return false, nil
	})
	return out, err
}

// GetRange fetches bytes [start, end] (inclusive) from rawURL and returns them.
// It tolerates an origin that ignores the Range header (HTTP 200) by returning
// only the requested prefix window. Transient failures are retried.
func (c *Client) GetRange(ctx context.Context, rawURL string, start, end int64) ([]byte, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid range %d-%d", start, end)
	}
	want := end - start + 1
	var out []byte
	err := c.withRetry(ctx, "range", func() (bool, error) {
		req, err := c.NewDownloadRequest(ctx, rawURL, start, end)
		if err != nil {
			return false, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return true, err
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusPartialContent:
			// Origin honored the range: read exactly the window.
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, want))
			if rerr != nil {
				return true, rerr
			}
			out = b
			return false, nil
		case http.StatusOK:
			// Origin ignored the range: skip to start, then read the window.
			if start > 0 {
				if _, derr := io.CopyN(io.Discard, resp.Body, start); derr != nil {
					return true, derr
				}
			}
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, want))
			if rerr != nil {
				return true, rerr
			}
			out = b
			return false, nil
		default:
			return RetriableStatus(resp.StatusCode), responseError("range", resp)
		}
	})
	return out, err
}

// Fetch streams the whole object at rawURL into w and returns the byte count.
// It restarts the transfer on transient failures; because callers hash the
// result, a truncated retry is caught downstream rather than silently accepted.
func (c *Client) Fetch(ctx context.Context, rawURL string, w io.Writer) (int64, error) {
	var written int64
	err := c.withRetry(ctx, "fetch", func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return false, err
		}
		c.setHeaders(req)
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return true, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return RetriableStatus(resp.StatusCode), responseError("fetch", resp)
		}
		written = 0
		n, cerr := io.Copy(w, resp.Body)
		written = n
		if cerr != nil {
			return true, cerr
		}
		return false, nil
	})
	return written, err
}

// withRetry runs op under the client's randomized backoff policy. op returns
// (retriable, err): a nil err succeeds; a non-nil err with retriable=false is
// terminal; retriable=true retries until the budget or context is exhausted.
func (c *Client) withRetry(ctx context.Context, op string, fn func() (bool, error)) error {
	unlimited := c.Retries < 0
	minWait, maxWait := RetryWaits(c.RetryMinWait, c.RetryMaxWait)
	var lastErr error
	for attempt := 0; unlimited || attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(RetryDelay(attempt-1, minWait, maxWait)):
			}
		}
		retriable, err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retriable {
			return err
		}
		lastErr = fmt.Errorf("%s: %w", op, err)
	}
	return lastErr
}
