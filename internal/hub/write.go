package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// doJSON performs a request with an optional JSON body and optional JSON
// response decode. When retry is true transient failures are retried; callers
// pass false for non-idempotent operations (a commit) so a network error after
// the server already applied the change is not repeated.
func (c *Client) doJSON(ctx context.Context, method, rawURL, op string, reqBytes []byte, out any, retry bool) error {
	attempt := func() (bool, error) {
		var body io.Reader
		if reqBytes != nil {
			body = bytes.NewReader(reqBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			return false, err
		}
		c.setHeaders(req)
		if reqBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return true, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return RetriableStatus(resp.StatusCode), responseError(op, resp)
		}
		if out != nil {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			if err != nil {
				return true, err
			}
			if len(b) > 0 {
				if err := json.Unmarshal(b, out); err != nil {
					return false, fmt.Errorf("%s decode: %w", op, err)
				}
			}
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		}
		return false, nil
	}
	if retry {
		return c.withRetry(ctx, op, attempt)
	}
	_, err := attempt()
	return err
}

// splitRepoID separates an "owner/name" id into organization and name.
func splitRepoID(repoID string) (org, name string) {
	if i := strings.IndexByte(repoID, '/'); i >= 0 {
		return repoID[:i], repoID[i+1:]
	}
	return "", repoID
}

// CreateRepo creates a repository. It is not an error if it already exists when
// ex, exist_ok semantics are handled by the caller inspecting the returned error.
func (c *Client) CreateRepo(ctx context.Context, repoType RepoType, repoID string, private bool) error {
	if err := repoType.Validate(); err != nil {
		return err
	}
	org, name := splitRepoID(repoID)
	payload := map[string]any{"name": name, "type": string(repoType.normalized()), "private": private}
	if org != "" {
		payload["organization"] = org
	}
	b, _ := json.Marshal(payload)
	return c.doJSON(ctx, http.MethodPost, c.Endpoint+"/api/repos/create", "create repo", b, nil, true)
}

// DeleteRepo permanently deletes a repository.
func (c *Client) DeleteRepo(ctx context.Context, repoType RepoType, repoID string) error {
	if err := repoType.Validate(); err != nil {
		return err
	}
	org, name := splitRepoID(repoID)
	payload := map[string]any{"name": name, "type": string(repoType.normalized())}
	if org != "" {
		payload["organization"] = org
	}
	b, _ := json.Marshal(payload)
	return c.doJSON(ctx, http.MethodDelete, c.Endpoint+"/api/repos/delete", "delete repo", b, nil, true)
}

// CreateTag creates a git tag pointing at revision.
func (c *Client) CreateTag(ctx context.Context, repoType RepoType, repoID, tag, revision, message string) error {
	if err := repoType.Validate(); err != nil {
		return err
	}
	payload := map[string]any{"tag": tag}
	if message != "" {
		payload["message"] = message
	}
	b, _ := json.Marshal(payload)
	rev := revision
	if rev == "" {
		rev = "main"
	}
	u := c.Endpoint + "/api/" + repoType.Collection() + "/" + escapeRepo(repoID) + "/tag/" + rev
	return c.doJSON(ctx, http.MethodPost, u, "create tag", b, nil, true)
}

// DeleteTag removes a git tag.
func (c *Client) DeleteTag(ctx context.Context, repoType RepoType, repoID, tag string) error {
	if err := repoType.Validate(); err != nil {
		return err
	}
	u := c.Endpoint + "/api/" + repoType.Collection() + "/" + escapeRepo(repoID) + "/tag/" + tag
	return c.doJSON(ctx, http.MethodDelete, u, "delete tag", nil, nil, true)
}
