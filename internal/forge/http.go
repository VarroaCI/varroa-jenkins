package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rateLimitScan is the number of body bytes that rateLimit reads. Only a
// boolean leaves the scan, so the body never reaches an error.
const rateLimitScan = 4 << 10

// httpClient holds the request code that both forge kinds share. It never
// writes a response body or a credential into an error.
type httpClient struct {
	base  string // API base URL without a trailing slash
	auth  string // header scheme prefix, "Bearer " or "token "
	token Token  // the credential
	h     *http.Client
	hdr   http.Header // headers that every request of the kind carries
}

// do sends one request. p is the escaped path under base. It maps every error
// status to a sentinel, a RateLimitError, or a StatusError. A 403 or 429 that a
// rate limit caused maps to a RateLimitError, never to ErrForbidden. The
// returned body carries the call's deadline, so the caller closes it.
func (c *httpClient) do(ctx context.Context, method, p string, q url.Values, body any, hdr http.Header) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	u := c.base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("forge: encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("forge: build request: %w", err)
	}
	for k, vs := range c.hdr {
		req.Header[k] = vs
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !c.token.Empty() {
		req.Header.Set("Authorization", c.auth+c.token.reveal())
	}
	resp, err := c.h.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("forge: request failed: %w", err)
	}
	resp.Body = &cancelBody{body: resp.Body, cancel: cancel}
	if resp.StatusCode < 400 {
		return resp, nil
	}
	status := resp.StatusCode
	head, _ := io.ReadAll(io.LimitReader(resp.Body, rateLimitScan))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if rl := rateLimit(status, resp.Header, head, time.Now()); rl != nil {
		return nil, rl
	}
	switch status {
	case http.StatusUnauthorized:
		return nil, ErrAuth
	case http.StatusForbidden:
		return nil, ErrForbidden
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return nil, ErrConflict
	}
	return nil, &StatusError{Status: status}
}

// getJSON decodes a metadata response into out.
func (c *httpClient) getJSON(ctx context.Context, p string, q url.Values, out any) error {
	resp, err := c.do(ctx, http.MethodGet, p, q, nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeJSON(resp.Body, out)
}

// sendJSON sends a request with a JSON body and decodes the reply into out when
// out is not nil.
func (c *httpClient) sendJSON(ctx context.Context, method, p string, in, out any) error {
	resp, err := c.do(ctx, method, p, nil, in, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	return decodeJSON(resp.Body, out)
}

// getRaw reads at most n bytes of a response body. It accepts a full
// response (200) and a partial one (206). It reads no further than n bytes,
// even when the server ignores the Range header.
func (c *httpClient) getRaw(ctx context.Context, p string, q url.Values, hdr http.Header, n int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, p, q, nil, hdr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, n))
}

func decodeJSON(r io.Reader, out any) error {
	if err := json.NewDecoder(io.LimitReader(r, maxMetaBody)).Decode(out); err != nil {
		return fmt.Errorf("forge: decode response: %w", err)
	}
	return nil
}

// escapePath escapes each segment of a repository path and keeps the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// repoPath returns the API path of a repository.
func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// contentItem is one entry of a contents listing. Both forge kinds return
// this shape.
type contentItem struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	SHA  string `json:"sha"`
}

// lookup returns the type, size, and blob sha of the path p at ref. It reads
// the parent directory listing and finds the child by name.
//
// The path call is not used. For a file, that call returns the content as
// base64 inside the JSON body, and a large file can exceed the metadata read
// limit. The parent listing carries the same fields without the content.
func (c *httpClient) lookup(ctx context.Context, repo, ref, p string) (typ string, size int64, sha string, err error) {
	parent, name := "", p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		parent, name = p[:i], p[i+1:]
	}
	listPath := repo + "/contents"
	if parent != "" {
		listPath += "/" + escapePath(parent)
	}
	var raw json.RawMessage
	if err := c.getJSON(ctx, listPath, url.Values{"ref": {ref}}, &raw); err != nil {
		return "", 0, "", err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", 0, "", ErrNotFound
	}
	var items []contentItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return "", 0, "", fmt.Errorf("forge: decode listing: %w", err)
	}
	for _, it := range items {
		if it.Name == name {
			return it.Type, it.Size, it.SHA, nil
		}
	}
	// A full page may hide the child. GitHub returns at most listingCap
	// entries per directory, so a child past the cap is found by path.
	if len(items) >= listingCap {
		return c.lookupObject(ctx, repo, ref, p)
	}
	return "", 0, "", ErrNotFound
}

// listingCap is the entry count at which a directory listing may be cut.
const listingCap = 1000

// objectMetaBody caps the object response of lookupObject. It is larger than
// maxMetaBody, because a file response can carry base64 content up to 1 MiB.
const objectMetaBody = 4 << 20

// lookupObject reads the metadata of one path. The object media type makes
// GitHub omit a file's content, so the body stays small. A directory answers
// with a listing array, and it is reported as type "dir".
func (c *httpClient) lookupObject(ctx context.Context, repo, ref, p string) (string, int64, string, error) {
	hdr := http.Header{"Accept": {"application/vnd.github.object"}}
	resp, err := c.do(ctx, http.MethodGet, repo+"/contents/"+escapePath(p), url.Values{"ref": {ref}}, nil, hdr)
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// The limit covers a file body, in case a server ignores the object
	// media type and sends the content with the metadata.
	var raw json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, objectMetaBody)).Decode(&raw); err != nil {
		return "", 0, "", fmt.Errorf("forge: decode object: %w", err)
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return "dir", 0, "", nil
	}
	var out contentItem
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, "", fmt.Errorf("forge: decode object: %w", err)
	}
	if out.Type == "" {
		return "", 0, "", ErrNotFound
	}
	return out.Type, out.Size, out.SHA, nil
}

// existingBlob returns the blob sha of a file that exists on ref, or "" when
// the file is absent. A directory at the path is a conflict.
func (c *httpClient) existingBlob(ctx context.Context, repo, ref, p string) (string, error) {
	typ, _, sha, err := c.lookup(ctx, repo, ref, p)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if typ != "file" {
		return "", ErrConflict
	}
	return sha, nil
}

// listDir returns the file and directory children of p at ref, sorted by name.
func (c *httpClient) listDir(ctx context.Context, repo, p, ref string) ([]Child, error) {
	listPath := repo + "/contents"
	if p != "" {
		listPath += "/" + escapePath(p)
	}
	var items []contentItem
	if err := c.getJSON(ctx, listPath, url.Values{"ref": {ref}}, &items); err != nil {
		return nil, err
	}
	if len(items) >= listingCap {
		return nil, ErrListingTruncated
	}
	children := make([]Child, 0, len(items))
	for _, it := range items {
		if it.Type == "file" || it.Type == "dir" {
			children = append(children, Child{Name: it.Name, Type: it.Type})
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return children, nil
}

// read implements Client.Read for both kinds. rawPrefix and rawHdr select the
// raw file endpoint of the kind.
func (c *httpClient) read(ctx context.Context, repo, sha, p string, limit int, rawPrefix string, rawHdr http.Header) (Entry, error) {
	n := int64(limit)
	if limit <= 0 || limit > MaxRead {
		n = MaxRead
	}
	e := Entry{Revision: sha}
	if p == "" {
		children, err := c.listDir(ctx, repo, "", sha)
		if err != nil {
			return Entry{}, err
		}
		e.Dir = true
		e.Children = children
		return e, nil
	}
	typ, size, _, err := c.lookup(ctx, repo, sha, p)
	if err != nil {
		return Entry{}, err
	}
	switch typ {
	case "dir":
		children, err := c.listDir(ctx, repo, p, sha)
		if err != nil {
			return Entry{}, err
		}
		e.Dir = true
		e.Children = children
	case "file":
		e.Size = size
		length := size
		if n < length {
			length = n
		}
		if length > 0 {
			hdr := make(http.Header, len(rawHdr)+1)
			for k, vs := range rawHdr {
				hdr[k] = vs
			}
			hdr.Set("Range", fmt.Sprintf("bytes=0-%d", length-1))
			body, err := c.getRaw(ctx, repo+rawPrefix+escapePath(p), url.Values{"ref": {sha}}, hdr, length)
			if err != nil {
				return Entry{}, err
			}
			e.Content = body
		}
	default:
		// A symlink or a submodule. The caller decides how to answer it.
		return Entry{Revision: sha, Other: true}, nil
	}
	return e, nil
}

// isSHA reports whether s is a lowercase 40 or 64 digit hexadecimal name.
func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
		if !ok {
			return false
		}
	}
	return true
}

// rateLimit reports whether a 403 or 429 answer came from a rate limit. A 429
// always does. A 403 does when it carries Retry-After, when its
// X-RateLimit-Remaining is zero, or when its body names a rate limit. The
// reset time comes from Retry-After first, then from X-RateLimit-Reset.
func rateLimit(status int, h http.Header, head []byte, now time.Time) *RateLimitError {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return nil
	}
	exhausted := h.Get("X-RateLimit-Remaining") == "0"
	if status == http.StatusForbidden && !exhausted && h.Get("Retry-After") == "" &&
		!bytes.Contains(bytes.ToLower(head), []byte("rate limit")) {
		return nil
	}
	return &RateLimitError{Status: status, Reset: rateLimitReset(h, exhausted, now)}
}

// rateLimitReset returns the time that the headers state for a retry, or the
// zero time when they state none.
func rateLimitReset(h http.Header, exhausted bool, now time.Time) time.Time {
	if ra := h.Get("Retry-After"); ra != "" {
		if n, err := strconv.ParseInt(ra, 10, 64); err == nil && n >= 0 {
			return now.Add(time.Duration(n) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t
		}
	}
	if exhausted {
		if v, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(v, 0)
		}
	}
	return time.Time{}
}
