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
	ReadOnly       bool
}

type Spec struct {
	Image string
	// Cmd replaces the image's command; with an entrypoint, it is the entrypoint's arguments.
	Cmd    []string
	Env    []string
	Labels map[string]string
	Mounts []Mount // named volumes, created on first use
	// Network is a network name, or container:<id> to share another container's network.
	Network    string
	NanoCPUs   int64
	CpusetCpus string
	Memory     int64
	Privileged bool
	// Init runs a tiny init as PID 1 to reap the zombies that build tools and browsers leave.
	Init bool
}

// Run creates and starts a container. It returns the ID even when only the start failed, so
// the caller can remove what was created.
func (c *Client) Run(ctx context.Context, name string, spec Spec) (string, error) {
	id, err := c.create(ctx, name, spec)
	if err != nil {
		return "", err
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil); err != nil {
		return id, err
	}
	return id, nil
}

// Seed fills empty named volumes with what the image has at their mount points, without
// running anything: Docker copies image content into an empty volume when a container that
// mounts it is created.
func (c *Client) Seed(ctx context.Context, image string, mounts []Mount) error {
	id, err := c.create(ctx, "", Spec{Image: image, Mounts: mounts})
	if err != nil {
		return err
	}
	return c.Remove(ctx, id)
}

func (c *Client) create(ctx context.Context, name string, spec Spec) (string, error) {
	mounts := make([]map[string]any, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		mounts = append(mounts, map[string]any{
			"Type": "volume", "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly,
		})
	}
	host := map[string]any{
		"Mounts":     mounts,
		"NanoCpus":   spec.NanoCPUs,
		"CpusetCpus": spec.CpusetCpus,
		"Memory":     spec.Memory,
		"Privileged": spec.Privileged,
		"Init":       spec.Init,
		"LogConfig": map[string]any{
			"Type":   "json-file",
			"Config": map[string]string{"max-size": "10m", "max-file": "2"},
		},
	}
	if spec.Network != "" {
		host["NetworkMode"] = spec.Network
	}
	body := map[string]any{"Image": spec.Image, "Env": spec.Env, "Labels": spec.Labels, "HostConfig": host}
	if len(spec.Cmd) > 0 {
		body["Cmd"] = spec.Cmd
	}
	query := url.Values{}
	if name != "" {
		query.Set("name", name)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", query, body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

type Volume struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// EnsureVolume creates a named volume with labels; one that exists is left as it is.
func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/volumes/"+name, nil, nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Volumes lists the named volumes that carry the label (key=value).
func (c *Client) Volumes(ctx context.Context, label string) ([]Volume, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Volumes []Volume `json:"Volumes"`
	}
	if err := c.do(ctx, http.MethodGet, "/volumes", url.Values{"filters": {string(filters)}}, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Volumes, nil
}

// RemoveVolume deletes a volume; one already gone counts as removed, and one in use by a
// container is refused by Docker with a conflict error.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func IsConflict(err error) bool { return hasStatus(err, http.StatusConflict) }

// CPUs is the number of CPUs the Docker host has.
func (c *Client) CPUs(ctx context.Context) (int, error) {
	var info struct {
		NCPU int `json:"NCPU"`
	}
	if err := c.do(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return 0, err
	}
	return info.NCPU, nil
}

// ImageID returns the ID of a local image, which changes on every rebuild.
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var image struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &image); err != nil {
		return "", err
	}
	return image.ID, nil
}

// Pull fetches an image from its registry.
func (c *Client) Pull(ctx context.Context, ref string) error {
	name, tag, _ := strings.Cut(ref, ":")
	query := url.Values{"fromImage": {name}}
	if tag != "" {
		query.Set("tag", tag)
	}
	return c.do(ctx, http.MethodPost, "/images/create", query, nil, nil)
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
