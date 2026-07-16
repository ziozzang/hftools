package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// getJSON performs a retriable GET and decodes a JSON body into out.
func (c *Client) getJSON(ctx context.Context, rawURL, op string, out any) error {
	return c.withRetry(ctx, op, func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return false, err
		}
		c.setHeaders(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return true, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return RetriableStatus(resp.StatusCode), responseError(op, resp)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return true, err
		}
		if err := json.Unmarshal(b, out); err != nil {
			return false, fmt.Errorf("%s decode: %w", op, err)
		}
		return false, nil
	})
}

// WhoAmI is the authenticated identity reported by the Hub.
type WhoAmI struct {
	Name     string `json:"name"`
	FullName string `json:"fullname"`
	Email    string `json:"email"`
	Type     string `json:"type"`
	Orgs     []struct {
		Name string `json:"name"`
	} `json:"orgs"`
	Auth struct {
		AccessToken struct {
			DisplayName string `json:"displayName"`
			Role        string `json:"role"`
		} `json:"accessToken"`
	} `json:"auth"`
}

// WhoAmI resolves the current token to a user identity. A missing or invalid
// token yields an error.
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	if c.Token == "" {
		return nil, fmt.Errorf("no token provided")
	}
	var w WhoAmI
	if err := c.getJSON(ctx, c.Endpoint+"/api/whoami-v2", "whoami", &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// Ref is a single git ref (branch or tag).
type Ref struct {
	Name         string `json:"name"`
	Ref          string `json:"ref"`
	TargetCommit string `json:"targetCommit"`
}

// Refs lists a repository's branches, tags, and converts.
type Refs struct {
	Branches []Ref `json:"branches"`
	Tags     []Ref `json:"tags"`
	Converts []Ref `json:"converts"`
}

// Refs fetches the branches and tags of a repository.
func (c *Client) Refs(ctx context.Context, repoType RepoType, repoID string) (*Refs, error) {
	if err := repoType.Validate(); err != nil {
		return nil, err
	}
	var r Refs
	u := c.Endpoint + "/api/" + repoType.Collection() + "/" + escapeRepo(repoID) + "/refs"
	if err := c.getJSON(ctx, u, "refs", &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SearchResult is one repository returned by a Hub search.
type SearchResult struct {
	ID           string          `json:"id"`
	Author       string          `json:"author"`
	Downloads    int64           `json:"downloads"`
	Likes        int64           `json:"likes"`
	LastModified string          `json:"lastModified"`
	PipelineTag  string          `json:"pipeline_tag"`
	LibraryName  string          `json:"library_name"`
	Private      bool            `json:"private"`
	Gated        json.RawMessage `json:"gated"`
	Tags         []string        `json:"tags"`
}

// SearchOptions parameterizes a Hub search.
type SearchOptions struct {
	Query     string
	Author    string
	Filter    []string
	Limit     int
	Sort      string // downloads | likes | lastModified | trendingScore
	Direction int    // -1 for descending
	Full      bool
}

// Search queries the Hub for repositories of the given type.
func (c *Client) Search(ctx context.Context, repoType RepoType, opts SearchOptions) ([]SearchResult, error) {
	if err := repoType.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.Query != "" {
		q.Set("search", opts.Query)
	}
	if opts.Author != "" {
		q.Set("author", opts.Author)
	}
	for _, f := range opts.Filter {
		if f != "" {
			q.Add("filter", f)
		}
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Direction != 0 {
		q.Set("direction", strconv.Itoa(opts.Direction))
	}
	if opts.Full {
		q.Set("full", "true")
	}
	u := c.Endpoint + "/api/" + repoType.Collection()
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	var results []SearchResult
	if err := c.getJSON(ctx, u, "search", &results); err != nil {
		return nil, err
	}
	return results, nil
}

// NormalizeSort maps friendly sort names to Hub field names.
func NormalizeSort(s string) string {
	switch strings.ToLower(s) {
	case "", "downloads":
		return "downloads"
	case "likes":
		return "likes"
	case "modified", "lastmodified", "updated":
		return "lastModified"
	case "trending":
		return "trendingScore"
	default:
		return s
	}
}
