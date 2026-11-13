package github

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Repo struct {
	FullName    string `json:"full_name"`
	Private     bool   `json:"private"`
	Archived    bool   `json:"archived"`
	Permissions *struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

// CanAdminister reports whether the token may register runners on the repo. Installation
// repos carry no permissions block: the app's own permissions apply.
func (r Repo) CanAdminister() bool { return r.Permissions == nil || r.Permissions.Admin }

// InstallationRepos lists the repos a GitHub App installation reaches.
func (c *Client) InstallationRepos(ctx context.Context) ([]Repo, error) {
	var all []Repo
	for page := 1; ; page++ {
		var resp struct {
			Repositories []Repo `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := c.get(ctx, path, false, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Repositories...)
		if len(resp.Repositories) < 100 {
			return all, nil
		}
	}
}

// UserRepos lists the repos the token's user owns.
func (c *Client) UserRepos(ctx context.Context) ([]Repo, error) {
	var all []Repo
	for page := 1; ; page++ {
		var repos []Repo
		path := fmt.Sprintf("/user/repos?affiliation=owner&per_page=100&page=%d", page)
		if err := c.get(ctx, path, false, &repos); err != nil {
			return nil, err
		}
		all = append(all, repos...)
		if len(repos) < 100 {
			return all, nil
		}
	}
}

type Run struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// ActiveRuns returns the repo's recent workflow runs that have not completed. One request
// covers both queued runs and in-progress runs whose later jobs still wait for a runner.
func (c *Client) ActiveRuns(ctx context.Context, repo string) ([]Run, error) {
	var resp struct {
		WorkflowRuns []Run `json:"workflow_runs"`
	}
	if err := c.get(ctx, "/repos/"+repo+"/actions/runs?per_page=50", true, &resp); err != nil {
		return nil, err
	}
	var active []Run
	for _, run := range resp.WorkflowRuns {
		if run.Status != "completed" {
			active = append(active, run)
		}
	}
	return active, nil
}

type Job struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Labels    []string  `json:"labels"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *Client) RunJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	var resp struct {
		Jobs []Job `json:"jobs"`
	}
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, runID)
	if err := c.get(ctx, path, true, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

func (c *Client) Runners(ctx context.Context, repo string) ([]Runner, error) {
	var resp struct {
		Runners []Runner `json:"runners"`
	}
	if err := c.get(ctx, "/repos/"+repo+"/actions/runners?per_page=100", true, &resp); err != nil {
		return nil, err
	}
	return resp.Runners, nil
}

// GenerateJITConfig registers a just-in-time runner on the repo: it takes one job, and GitHub
// removes it afterwards. The encoded config is the runner's only credential.
func (c *Client) GenerateJITConfig(ctx context.Context, repo, name string, labels []string) (int64, string, error) {
	body := map[string]any{
		"name":            name,
		"runner_group_id": 1, // the only group a repository has
		"labels":          labels,
		"work_folder":     "_work",
	}
	var resp struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	path := "/repos/" + repo + "/actions/runners/generate-jitconfig"
	if err := c.do(ctx, http.MethodPost, path, body, false, &resp); err != nil {
		return 0, "", err
	}
	return resp.Runner.ID, resp.EncodedJITConfig, nil
}

// DeleteRunner deregisters a runner; one that is already gone counts as deleted.
func (c *Client) DeleteRunner(ctx context.Context, repo string, id int64) error {
	path := fmt.Sprintf("/repos/%s/actions/runners/%d", repo, id)
	if err := c.do(ctx, http.MethodDelete, path, nil, false, nil); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
