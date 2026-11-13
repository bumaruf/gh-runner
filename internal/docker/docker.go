// Package docker is the small slice of the Docker Engine API that ghpool needs, spoken over
// the daemon's unix socket.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	http *http.Client
}

func New(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 2 * time.Minute}}
}

type Error struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *Error) Error() string {
	return fmt.Sprintf("docker %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func hasStatus(err error, status int) bool {
	var dockerErr *Error
	return errors.As(err, &dockerErr) && dockerErr.Status == status
}

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

func (c Container) Running() bool { return c.State == "running" }

// ExitedCleanly is true for a container that stopped with exit code 0.
func (c Container) ExitedCleanly() bool {
	return c.State == "exited" && strings.HasPrefix(c.Status, "Exited (0)")
}

// Containers lists every container, running or not, that carries the label (key=value).
func (c *Client) Containers(ctx context.Context, label string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	var containers []Container
	query := url.Values{"all": {"1"}, "filters": {string(filters)}}
	if err := c.do(ctx, http.MethodGet, "/containers/json", query, nil, &containers); err != nil {
		return nil, err
	}
	return containers, nil
}

type Mount struct {
	Source, Target string
}

type Spec struct {
	Image    string
	Env      []string
	Labels   map[string]string
	Mounts   []Mount // named volumes, created on first use
	Network  string
	NanoCPUs int64
	Memory   int64
}

// Run creates and starts a container. It returns the ID even when only the start failed, so
// the caller can remove what was created.
func (c *Client) Run(ctx context.Context, name string, spec Spec) (string, error) {
	mounts := make([]map[string]any, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		mounts = append(mounts, map[string]any{"Type": "volume", "Source": m.Source, "Target": m.Target})
	}
	body := map[string]any{
		"Image":  spec.Image,
		"Env":    spec.Env,
		"Labels": spec.Labels,
		"HostConfig": map[string]any{
			"Mounts":      mounts,
			"NetworkMode": spec.Network,
			"NanoCpus":    spec.NanoCPUs,
			"Memory":      spec.Memory,
			// Reaps the zombies that build tools and browsers leave behind in a job.
			"Init": true,
			"LogConfig": map[string]any{
				"Type":   "json-file",
				"Config": map[string]string{"max-size": "10m", "max-file": "2"},
			},
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &created); err != nil {
		return "", err
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, nil); err != nil {
		return created.ID, err
	}
	return created.ID, nil
}

// Remove kills and deletes a container with its anonymous volumes; named ones stay.
func (c *Client) Remove(ctx context.Context, id string) error {
	query := url.Values{"force": {"1"}, "v": {"1"}}
	err := c.do(ctx, http.MethodDelete, "/containers/"+id, query, nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	err := c.do(ctx, http.MethodGet, "/networks/"+name, nil, nil, nil)
	if err == nil || !hasStatus(err, http.StatusNotFound) {
		return err
	}
	body := map[string]any{"Name": name, "Driver": "bridge", "Labels": labels}
	err = c.do(ctx, http.MethodPost, "/networks/create", nil, body, nil)
	if hasStatus(err, http.StatusConflict) {
		return nil
	}
	return err
}

func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	target := "http://docker" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		var parsed struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &parsed)
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: parsed.Message}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
