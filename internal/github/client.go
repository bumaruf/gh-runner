// Package github is the small slice of the GitHub REST API that ghpool needs.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiVersion = "2022-11-28"

// Client polls cheaply: GETs that change rarely are revalidated by ETag, and GitHub does not
// count a 304 Not Modified against the rate limit.
type Client struct {
	baseURL string
	http    *http.Client
	tokens  TokenSource

	mu          sync.Mutex
	cache       map[string]*cacheEntry
	rate        Rate
	pausedUntil time.Time
}

type cacheEntry struct {
	etag     string
	body     []byte
	lastUsed time.Time
}

// Rate is the primary rate limit as of the last response.
type Rate struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

func NewClient(baseURL string, tokens TokenSource) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
		tokens:  tokens,
		cache:   map[string]*cacheEntry{},
	}
}

type Error struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// ErrPaused means a rate limit asked us to stop until PausedFor runs out.
var ErrPaused = errors.New("github: paused by rate limit")

func (c *Client) Rate() Rate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rate
}

func (c *Client) PausedFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Until(c.pausedUntil)
}

// PruneCache drops revalidation entries not used for maxIdle, such as finished runs.
func (c *Client) PruneCache(maxIdle time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path, entry := range c.cache {
		if time.Since(entry.lastUsed) > maxIdle {
			delete(c.cache, path)
		}
	}
}

func (c *Client) get(ctx context.Context, path string, conditional bool, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, conditional, out)
}

func (c *Client) do(ctx context.Context, method, path string, body any, conditional bool, out any) error {
	if c.PausedFor() > 0 {
		return ErrPaused
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	setCommonHeaders(req, token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	var cached *cacheEntry
	if conditional {
		c.mu.Lock()
		cached = c.cache[path]
		c.mu.Unlock()
		if cached != nil {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.trackRate(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		c.mu.Lock()
		cached.lastUsed = time.Now()
		data = cached.body
		c.mu.Unlock()
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if etag := resp.Header.Get("ETag"); conditional && etag != "" {
			c.mu.Lock()
			c.cache[path] = &cacheEntry{etag: etag, body: data, lastUsed: time.Now()}
			c.mu.Unlock()
		}
	default:
		c.pauseIfLimited(resp)
		return newError(method, path, resp, data)
	}

	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github %s %s: decode: %w", method, path, err)
	}
	return nil
}

func (c *Client) trackRate(resp *http.Response) {
	remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	limit, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	c.mu.Lock()
	c.rate = Rate{Limit: limit, Remaining: remaining, Reset: time.Unix(reset, 0)}
	c.mu.Unlock()
}

// pauseIfLimited honours both the primary limit (remaining 0 until the reset) and the
// secondary one (Retry-After, or a minute when GitHub gives no hint).
func (c *Client) pauseIfLimited(resp *http.Response) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	var until time.Time
	switch {
	case resp.Header.Get("Retry-After") != "":
		seconds, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		until = time.Now().Add(time.Duration(seconds) * time.Second)
	case resp.Header.Get("X-RateLimit-Remaining") == "0":
		reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		until = time.Unix(reset, 0)
	case resp.StatusCode == http.StatusTooManyRequests:
		until = time.Now().Add(time.Minute)
	default:
		return
	}
	c.mu.Lock()
	if until.After(c.pausedUntil) {
		c.pausedUntil = until
	}
	c.mu.Unlock()
}

func setCommonHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "ghpool")
	req.Header.Set("Authorization", "Bearer "+token)
}

func newError(method, path string, resp *http.Response, body []byte) error {
	var parsed struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Message == "" {
		parsed.Message = http.StatusText(resp.StatusCode)
	}
	return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: parsed.Message}
}
