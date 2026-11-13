// Package pool keeps a fixed number of slots, each running at most one ephemeral runner, and
// gives them to whichever repo has a job waiting. It holds no state of its own: every tick it
// rebuilds the picture from Docker and GitHub, so a restart adopts the runners already up.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bumaruf/gh-runner/internal/docker"
	"github.com/bumaruf/gh-runner/internal/github"
)

type Config struct {
	// Name tells this pool apart from others on the same Docker host and prefixes runner names.
	Name  string
	Slots int
	// Labels are the custom labels jobs use to target the pool, besides self-hosted, linux and
	// the architecture.
	Labels   []string
	Image    string
	NanoCPUs int64
	Memory   int64

	PollInterval time.Duration
	IdleTimeout  time.Duration
	RepoRefresh  time.Duration

	// Include and Exclude are path.Match patterns on owner/repo; an empty Include takes all.
	Include, Exclude []string
	// AllowPublic lets public repos in. Off by default: a pull request from a fork would run
	// its code on this host.
	AllowPublic bool
	// AppAuth lists repos through the app installation instead of the token's user.
	AppAuth bool
}

const (
	labelPool     = "ghpool.pool"
	labelRepo     = "ghpool.repo"
	labelRunnerID = "ghpool.runner-id"
	labelSlot     = "ghpool.slot"

	orphanGrace = 2 * time.Minute
	orphanSweep = 5 * time.Minute
	// A tick finishes even after a stop signal: cut short, a runner can end up registered on
	// GitHub with no container behind it.
	tickTimeout     = 2 * time.Minute
	cacheMaxIdle    = 10 * time.Minute
	summaryInterval = 5 * time.Minute
	// GitHub's secondary limit allows 900 REST points a minute; this leaves room for the rest.
	requestsPerMinute = 600
	repoConcurrency   = 8
)

// slotCaches are the per-slot volumes. A slot runs one job at a time, so no two jobs write
// the same cache at once; jobs of different repos do share a slot's caches over time.
var slotCaches = []struct{ suffix, target string }{
	{"cache", "/home/runner/.cache"},
	{"pnpm", "/home/runner/.local/share/pnpm"},
	{"npm", "/home/runner/.npm"},
	{"toolcache", "/opt/hostedtoolcache"},
}

type Pool struct {
	cfg    Config
	gh     *github.Client
	docker *docker.Client
	log    *slog.Logger

	registerLabels []string
	accepts        map[string]bool

	repos       []string
	reposAt     time.Time
	ownName     *regexp.Regexp
	sweptAt     time.Time
	failures    map[string]failure
	lastSummary time.Time
	lastState   string
}

type failure struct {
	count int
	until time.Time
}

func New(cfg Config, gh *github.Client, dockerClient *docker.Client, log *slog.Logger) *Pool {
	labels := []string{"self-hosted", "linux", runnerArch()}
	for _, label := range cfg.Labels {
		if !slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(l, label) }) {
			labels = append(labels, label)
		}
	}
	accepts := map[string]bool{}
	for _, label := range labels {
		accepts[strings.ToLower(label)] = true
	}
	return &Pool{
		cfg:            cfg,
		gh:             gh,
		docker:         dockerClient,
		log:            log,
		registerLabels: labels,
		accepts:        accepts,
		ownName:        regexp.MustCompile("^" + regexp.QuoteMeta(cfg.Name) + `-\d+-[0-9a-f]{6}$`),
		failures:       map[string]failure{},
	}
}

func (p *Pool) Run(ctx context.Context) error {
	exists, err := p.docker.ImageExists(ctx, p.cfg.Image)
	if err != nil {
		return fmt.Errorf("check runner image: %w", err)
	}
	if !exists {
		return fmt.Errorf("runner image %s not found; build it with `docker compose build`", p.cfg.Image)
	}
	for slot := 1; slot <= p.cfg.Slots; slot++ {
		if err := p.docker.EnsureNetwork(ctx, p.network(slot), map[string]string{labelPool: p.cfg.Name}); err != nil {
			return fmt.Errorf("create network of slot %d: %w", slot, err)
		}
	}
	p.log.Info("pool started", "pool", p.cfg.Name, "slots", p.cfg.Slots, "labels", p.registerLabels, "image", p.cfg.Image)

	for {
		started := time.Now()
		tickCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tickTimeout)
		if err := p.tick(tickCtx); err != nil {
			p.log.Error("tick failed", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			p.log.Info("pool stopped; runner containers keep running and are adopted on the next start")
			return nil
		case <-time.After(p.nextWait(time.Since(started))):
		}
	}
}

func (p *Pool) tick(ctx context.Context) error {
	now := time.Now()
	if p.repos == nil || now.Sub(p.reposAt) >= p.cfg.RepoRefresh {
		p.refreshRepos(ctx, now)
	}

	queued := p.queuedJobs(ctx)
	containers, err := p.docker.Containers(ctx, labelPool+"="+p.cfg.Name)
	if err != nil {
		return fmt.Errorf("list runner containers: %w", err)
	}
	runners := p.observe(ctx, containers)
	p.sweepOrphans(ctx, now, containers)

	decided := plan(planInput{
		Slots:       p.cfg.Slots,
		Runners:     runners,
		Queued:      queued,
		Now:         now,
		IdleTimeout: p.cfg.IdleTimeout,
		OrphanGrace: orphanGrace,
		Blocked:     p.blocked(now),
	})
	for _, removal := range decided.Remove {
		p.remove(ctx, removal, now)
	}
	for _, spawn := range decided.Spawn {
		p.spawn(ctx, spawn, now)
	}

	p.gh.PruneCache(cacheMaxIdle)
	p.summarize(now, runners, decided, queued)
	return nil
}

func (p *Pool) refreshRepos(ctx context.Context, now time.Time) {
	var repos []github.Repo
	var err error
	if p.cfg.AppAuth {
		repos, err = p.gh.InstallationRepos(ctx)
	} else {
		repos, err = p.gh.UserRepos(ctx)
	}
	if err != nil {
		p.log.Error("could not list repos; keeping the previous list", "err", err)
		return
	}
	var names []string
	for _, repo := range repos {
		if p.serves(repo) {
			names = append(names, repo.FullName)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, p.repos) {
		p.log.Info("serving repos", "count", len(names), "repos", names)
	}
	p.repos, p.reposAt = names, now
}

func (p *Pool) serves(repo github.Repo) bool {
	if repo.Archived || !repo.CanAdminister() || (!repo.Private && !p.cfg.AllowPublic) {
		return false
	}
	if len(p.cfg.Include) > 0 && !matchesAny(p.cfg.Include, repo.FullName) {
		return false
	}
	return !matchesAny(p.cfg.Exclude, repo.FullName)
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(name)); ok {
			return true
		}
	}
	return false
}

func (p *Pool) queuedJobs(ctx context.Context) []QueuedJob {
	var (
		mu  sync.Mutex
		all []QueuedJob
		wg  sync.WaitGroup
	)
	limit := make(chan struct{}, repoConcurrency)
	for _, repo := range p.repos {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			jobs, err := p.repoQueuedJobs(ctx, repo)
			if err != nil {
				if !errors.Is(err, github.ErrPaused) && ctx.Err() == nil {
					p.log.Warn("could not read queued jobs", "repo", repo, "err", err)
				}
				return
			}
			mu.Lock()
			all = append(all, jobs...)
			mu.Unlock()
		})
	}
	wg.Wait()
	return all
}

func (p *Pool) repoQueuedJobs(ctx context.Context, repo string) ([]QueuedJob, error) {
	runs, err := p.gh.ActiveRuns(ctx, repo)
	if err != nil {
		return nil, err
	}
	var queued []QueuedJob
	for _, run := range runs {
		jobs, err := p.gh.RunJobs(ctx, repo, run.ID)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if job.Status == "queued" && p.acceptsLabels(job.Labels) {
				queued = append(queued, QueuedJob{Repo: repo, ID: job.ID, CreatedAt: job.CreatedAt})
			}
		}
	}
	return queued, nil
}

// acceptsLabels reports whether a runner of the pool can take a job with these runs-on labels,
// which GitHub matches as a subset of the runner's labels.
func (p *Pool) acceptsLabels(labels []string) bool {
	if len(labels) == 0 {
		return false
	}
	for _, label := range labels {
		if !p.accepts[strings.ToLower(label)] {
			return false
		}
	}
	return true
}

func (p *Pool) observe(ctx context.Context, containers []docker.Container) []Runner {
	registered := map[string]map[int64]github.Runner{}
	unknown := map[string]bool{}
	for _, c := range containers {
		repo := c.Labels[labelRepo]
		if _, seen := registered[repo]; seen || unknown[repo] {
			continue
		}
		list, err := p.gh.Runners(ctx, repo)
		if err != nil {
			unknown[repo] = true
			if !errors.Is(err, github.ErrPaused) {
				p.log.Warn("could not read the repo's runners", "repo", repo, "err", err)
			}
			continue
		}
		byID := map[int64]github.Runner{}
		for _, r := range list {
			byID[r.ID] = r
		}
		registered[repo] = byID
	}

	runners := make([]Runner, 0, len(containers))
	for _, c := range containers {
		r := Runner{
			Repo:        c.Labels[labelRepo],
			ContainerID: c.ID,
			StartedAt:   time.Unix(c.Created, 0),
			Running:     c.Running(),
			ExitedClean: c.ExitedCleanly(),
		}
		r.RunnerID, _ = strconv.ParseInt(c.Labels[labelRunnerID], 10, 64)
		r.Slot, _ = strconv.Atoi(c.Labels[labelSlot])
		if unknown[r.Repo] {
			r.Registered, r.Busy = true, true
		} else {
			gr, ok := registered[r.Repo][r.RunnerID]
			r.Registered, r.Busy = ok, ok && gr.Busy
			if r.Busy {
				delete(p.failures, r.Repo)
			}
		}
		runners = append(runners, r)
	}
	return runners
}

// sweepOrphans deregisters runners of this pool that GitHub still lists, offline, with no
// container behind them: what a crash between registering a runner and starting its
// container leaves. GitHub would only drop them after a day.
func (p *Pool) sweepOrphans(ctx context.Context, now time.Time, containers []docker.Container) {
	if now.Sub(p.sweptAt) < orphanSweep {
		return
	}
	p.sweptAt = now
	backed := map[string]bool{}
	for _, c := range containers {
		backed[c.Labels[labelRepo]+"#"+c.Labels[labelRunnerID]] = true
	}
	for _, repo := range p.repos {
		runners, err := p.gh.Runners(ctx, repo)
		if err != nil {
			continue
		}
		for _, r := range runners {
			if !p.ownName.MatchString(r.Name) || r.Busy || r.Status != "offline" ||
				backed[repo+"#"+strconv.FormatInt(r.ID, 10)] {
				continue
			}
			if err := p.gh.DeleteRunner(ctx, repo, r.ID); err != nil {
				p.log.Warn("could not deregister orphan runner", "repo", repo, "runner", r.Name, "err", err)
				continue
			}
			p.log.Info("orphan runner deregistered", "repo", repo, "runner", r.Name)
		}
	}
}

func (p *Pool) remove(ctx context.Context, removal Removal, now time.Time) {
	r := removal.Runner
	if err := p.docker.Remove(ctx, r.ContainerID); err != nil {
		p.log.Error("could not remove runner container", "repo", r.Repo, "slot", r.Slot, "err", err)
		return
	}
	if r.Registered && r.RunnerID != 0 {
		if err := p.gh.DeleteRunner(ctx, r.Repo, r.RunnerID); err != nil {
			p.log.Warn("could not deregister runner", "repo", r.Repo, "runner_id", r.RunnerID, "err", err)
		}
	}
	p.log.Info("runner removed", "repo", r.Repo, "slot", r.Slot, "reason", removal.Reason)
	if removal.Reason == reasonExited && !r.ExitedClean {
		p.fail(r.Repo, now)
	}
}

func (p *Pool) spawn(ctx context.Context, s Spawn, now time.Time) {
	name := fmt.Sprintf("%s-%d-%s", p.cfg.Name, s.Slot, randomSuffix())
	runnerID, jitConfig, err := p.gh.GenerateJITConfig(ctx, s.Repo, name, p.registerLabels)
	if err != nil {
		p.log.Error("could not register runner", "repo", s.Repo, "slot", s.Slot, "err", err)
		p.fail(s.Repo, now)
		return
	}
	containerID, err := p.docker.Run(ctx, name, docker.Spec{
		Image: p.cfg.Image,
		Env:   []string{"RUNNER_JITCONFIG=" + jitConfig},
		Labels: map[string]string{
			labelPool:     p.cfg.Name,
			labelRepo:     s.Repo,
			labelRunnerID: strconv.FormatInt(runnerID, 10),
			labelSlot:     strconv.Itoa(s.Slot),
		},
		Mounts:   p.mounts(s.Slot),
		Network:  p.network(s.Slot),
		NanoCPUs: p.cfg.NanoCPUs,
		Memory:   p.cfg.Memory,
	})
	if err != nil {
		p.log.Error("could not start runner container", "repo", s.Repo, "slot", s.Slot, "err", err)
		if containerID != "" {
			_ = p.docker.Remove(ctx, containerID)
		}
		if err := p.gh.DeleteRunner(ctx, s.Repo, runnerID); err != nil {
			p.log.Warn("could not deregister runner", "repo", s.Repo, "runner_id", runnerID, "err", err)
		}
		p.fail(s.Repo, now)
		return
	}
	p.log.Info("runner started", "repo", s.Repo, "slot", s.Slot, "runner", name)
}

// fail backs a repo off exponentially, from 10 seconds up to 5 minutes, so a runner that
// cannot start does not spin a new container every tick.
func (p *Pool) fail(repo string, now time.Time) {
	f := p.failures[repo]
	f.count++
	backoff := min(10*time.Second<<min(f.count-1, 10), 5*time.Minute)
	f.until = now.Add(backoff)
	p.failures[repo] = f
	p.log.Warn("repo backed off", "repo", repo, "failures", f.count, "for", backoff)
}

func (p *Pool) blocked(now time.Time) map[string]bool {
	blocked := map[string]bool{}
	for repo, f := range p.failures {
		if now.Before(f.until) {
			blocked[repo] = true
		}
	}
	return blocked
}

// nextWait keeps the poll interval, stretched so a tick of two requests per repo stays under
// the per-minute budget, and longer while a rate limit pauses us.
func (p *Pool) nextWait(elapsed time.Duration) time.Duration {
	wait := p.cfg.PollInterval
	budget := time.Duration(2*len(p.repos)) * time.Minute / requestsPerMinute
	wait = max(wait, budget)
	if paused := p.gh.PausedFor(); paused > wait {
		p.log.Warn("rate limited; pausing", "for", paused.Round(time.Second))
		wait = paused
	}
	if rate := p.gh.Rate(); rate.Limit > 0 && rate.Remaining < rate.Limit/20 {
		if untilReset := time.Until(rate.Reset); untilReset > wait {
			p.log.Warn("rate limit nearly spent; waiting for the reset", "remaining", rate.Remaining, "for", untilReset.Round(time.Second))
			wait = untilReset
		}
	}
	return max(wait-elapsed, time.Second)
}

// summarize logs the pool's state when it changes, and every few minutes regardless.
func (p *Pool) summarize(now time.Time, runners []Runner, decided Plan, queued []QueuedJob) {
	removed := map[string]bool{}
	for _, r := range decided.Remove {
		removed[r.Runner.ContainerID] = true
	}
	busy, waiting := 0, len(decided.Spawn)
	for _, r := range runners {
		switch {
		case removed[r.ContainerID]:
		case r.Busy:
			busy++
		default:
			waiting++
		}
	}
	state := fmt.Sprintf("%d/%d/%d", busy, waiting, len(queued))
	if state == p.lastState && now.Sub(p.lastSummary) < summaryInterval {
		return
	}
	p.lastState, p.lastSummary = state, now
	rate := p.gh.Rate()
	p.log.Info("pool status",
		"busy", busy, "waiting_for_job", waiting, "slots", p.cfg.Slots,
		"queued_jobs", len(queued), "repos", len(p.repos),
		"rate_remaining", rate.Remaining, "rate_limit", rate.Limit)
}

func (p *Pool) network(slot int) string { return fmt.Sprintf("ghpool-%s-slot%d", p.cfg.Name, slot) }

func (p *Pool) mounts(slot int) []docker.Mount {
	mounts := make([]docker.Mount, 0, len(slotCaches))
	for _, cache := range slotCaches {
		source := fmt.Sprintf("ghpool-%s-slot%d-%s", p.cfg.Name, slot, cache.suffix)
		mounts = append(mounts, docker.Mount{Source: source, Target: cache.target})
	}
	return mounts
}

func runnerArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
