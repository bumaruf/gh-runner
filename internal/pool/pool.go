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
	"math"
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
	Labels []string
	Image  string
	// CPUs per job; 0 means no limit. A whole number that fits pins each slot to its own CPUs.
	CPUs   float64
	Memory int64

	// Docker gives every job its own Docker daemon, in a privileged container beside the
	// runner, for services:, container: and docker build.
	Docker    bool
	DindImage string

	PollInterval time.Duration
	IdleTimeout  time.Duration
	RepoRefresh  time.Duration
	// CacheTTL is how long a repo may go without runs before its caches are deleted.
	CacheTTL time.Duration

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
	labelRole     = "ghpool.role"
	labelKind     = "ghpool.kind"
	labelWork     = "ghpool.work"

	roleRunner = "runner"
	roleDocker = "docker"

	workDir      = "/home/runner/_work"
	externalsDir = "/home/runner/externals"

	// The job's Docker would take 172.17.0.0/16 for its bridge, the same default as the host's.
	// Sharing its network namespace, the runner would then lose any host service on that
	// range, such as a DNS server Docker hands to containers at 172.17.0.1.
	dindBridge = "--bip=10.249.0.1/24"
	dindPools  = "--default-address-pool=base=10.250.0.0/16,size=24"

	orphanGrace = 2 * time.Minute
	orphanSweep = 5 * time.Minute
	volumeSweep = time.Hour
	queueLag    = 30 * time.Second
	// A tick finishes even after a stop signal: cut short, a runner can end up registered on
	// GitHub with no container behind it.
	tickTimeout     = 2 * time.Minute
	cacheMaxIdle    = 10 * time.Minute
	summaryInterval = 5 * time.Minute
	// GitHub's secondary limit allows 900 REST points a minute; this leaves room for the rest.
	requestsPerMinute = 600
	repoConcurrency   = 8
)

type Pool struct {
	cfg    Config
	gh     *github.Client
	docker *docker.Client
	log    *slog.Logger

	registerLabels []string
	accepts        map[string]bool
	ownName        *regexp.Regexp
	hostCPUs       int

	repos     []string
	reposAt   time.Time
	lastRunMu sync.Mutex
	lastRun   map[string]time.Time

	sweptAt        time.Time
	volumesSweptAt time.Time
	externals      map[string]string // runner image ID → its externals volume
	failures       map[string]failure
	busySince      map[int64]time.Time
	lastSummary    time.Time
	lastState      string
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
		lastRun:        map[string]time.Time{},
		externals:      map[string]string{},
		failures:       map[string]failure{},
		busySince:      map[int64]time.Time{},
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
	if p.cfg.Docker {
		if err := p.ensureImage(ctx, p.cfg.DindImage); err != nil {
			return err
		}
	}
	if p.hostCPUs, err = p.docker.CPUs(ctx); err != nil {
		return fmt.Errorf("read host CPUs: %w", err)
	}
	for slot := 1; slot <= p.cfg.Slots; slot++ {
		if err := p.docker.EnsureNetwork(ctx, p.network(slot), map[string]string{labelPool: p.cfg.Name}); err != nil {
			return fmt.Errorf("create network of slot %d: %w", slot, err)
		}
	}
	p.log.Info("pool started", "pool", p.cfg.Name, "slots", p.cfg.Slots, "labels", p.registerLabels,
		"image", p.cfg.Image, "docker_in_jobs", p.cfg.Docker, "cpus_per_job", p.cfg.CPUs,
		"pinned_cpus", cpuset(1, p.cfg.Slots, p.wholeCPUs(), p.hostCPUs) != "")

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

func (p *Pool) ensureImage(ctx context.Context, ref string) error {
	exists, err := p.docker.ImageExists(ctx, ref)
	if err != nil {
		return fmt.Errorf("check image %s: %w", ref, err)
	}
	if exists {
		return nil
	}
	p.log.Info("pulling image", "image", ref)
	if err := p.docker.Pull(ctx, ref); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	if exists, err = p.docker.ImageExists(ctx, ref); err != nil || !exists {
		return fmt.Errorf("image %s still missing after the pull: %v", ref, err)
	}
	return nil
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
	runners := p.observe(ctx, containers, now)
	p.sweepOrphans(ctx, now, runners)

	decided := plan(planInput{
		Slots:       p.cfg.Slots,
		Runners:     runners,
		Queued:      queued,
		Now:         now,
		IdleTimeout: p.cfg.IdleTimeout,
		OrphanGrace: orphanGrace,
		Blocked:     p.blocked(now),
		QueueLag:    queueLag,
	})
	for _, removal := range decided.Remove {
		p.remove(ctx, removal, now)
	}
	for _, spawn := range decided.Spawn {
		p.spawn(ctx, spawn, now)
	}
	p.sweepVolumes(ctx, now)

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
	runs, err := p.gh.RecentRuns(ctx, repo)
	if err != nil {
		return nil, err
	}
	var latest time.Time
	var queued []QueuedJob
	for _, run := range runs {
		if run.CreatedAt.After(latest) {
			latest = run.CreatedAt
		}
		if !run.Active() {
			continue
		}
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
	p.lastRunMu.Lock()
	p.lastRun[repo] = latest
	p.lastRunMu.Unlock()
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

// observe pairs each runner container with its Docker sidecar and asks GitHub how the runner
// stands. A sidecar left without its runner shows up as an exited runner, so it gets removed.
func (p *Pool) observe(ctx context.Context, containers []docker.Container, now time.Time) []Runner {
	byKey := map[string]*Runner{}
	var order []string
	for _, c := range containers {
		key := c.Labels[labelRepo] + "#" + c.Labels[labelRunnerID]
		r, seen := byKey[key]
		if !seen {
			r = &Runner{Repo: c.Labels[labelRepo], ExitedClean: true}
			r.RunnerID, _ = strconv.ParseInt(c.Labels[labelRunnerID], 10, 64)
			r.Slot, _ = strconv.Atoi(c.Labels[labelSlot])
			byKey[key] = r
			order = append(order, key)
		}
		r.WorkVolume = firstNonEmpty(r.WorkVolume, c.Labels[labelWork])
		if c.Labels[labelRole] == roleDocker {
			r.SidecarID = c.ID
			continue
		}
		r.ContainerID = c.ID
		r.StartedAt = time.Unix(c.Created, 0)
		r.Running = c.Running()
		r.ExitedClean = c.ExitedCleanly()
	}

	registered := map[string]map[int64]github.Runner{}
	unknown := map[string]bool{}
	for _, key := range order {
		repo := byKey[key].Repo
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

	runners := make([]Runner, 0, len(order))
	for _, key := range order {
		r := byKey[key]
		if unknown[r.Repo] {
			r.Registered, r.Busy = true, true
		} else {
			gr, ok := registered[r.Repo][r.RunnerID]
			r.Registered, r.Busy = ok, ok && gr.Busy
			if r.Busy {
				delete(p.failures, r.Repo)
				if _, seen := p.busySince[r.RunnerID]; !seen {
					p.busySince[r.RunnerID] = now
				}
			}
		}
		r.BusySince = p.busySince[r.RunnerID]
		runners = append(runners, *r)
	}
	for id := range p.busySince {
		if !slices.ContainsFunc(runners, func(r Runner) bool { return r.RunnerID == id }) {
			delete(p.busySince, id)
		}
	}
	return runners
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sweepOrphans deregisters runners of this pool that GitHub still lists, offline, with no
// container behind them: what a crash between registering a runner and starting its
// container leaves. GitHub would only drop them after a day.
func (p *Pool) sweepOrphans(ctx context.Context, now time.Time, runners []Runner) {
	if now.Sub(p.sweptAt) < orphanSweep {
		return
	}
	p.sweptAt = now
	backed := map[string]bool{}
	for _, r := range runners {
		backed[r.Repo+"#"+strconv.FormatInt(r.RunnerID, 10)] = true
	}
	for _, repo := range p.repos {
		list, err := p.gh.Runners(ctx, repo)
		if err != nil {
			continue
		}
		for _, r := range list {
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

// sweepVolumes deletes the volumes nothing will use again, once an hour.
func (p *Pool) sweepVolumes(ctx context.Context, now time.Time) {
	if now.Sub(p.volumesSweptAt) < volumeSweep {
		return
	}
	p.volumesSweptAt = now
	volumes, err := p.docker.Volumes(ctx, labelPool+"="+p.cfg.Name)
	if err != nil {
		p.log.Warn("could not list volumes", "err", err)
		return
	}
	containers, err := p.docker.Containers(ctx, labelPool+"="+p.cfg.Name)
	if err != nil {
		p.log.Warn("could not list runner containers", "err", err)
		return
	}
	live := map[string]bool{}
	for _, c := range containers {
		live[c.Labels[labelRunnerID]] = true
	}
	served := map[string]bool{}
	for _, repo := range p.repos {
		served[repo] = true
	}
	p.lastRunMu.Lock()
	lastRun := map[string]time.Time{}
	for repo, at := range p.lastRun {
		lastRun[repo] = at
	}
	p.lastRunMu.Unlock()
	current := ""
	if id, err := p.docker.ImageID(ctx, p.cfg.Image); err == nil {
		current = p.externalsVolume(id)
	}

	for _, name := range staleVolumes(sweepInput{
		Volumes: volumes, Served: served, LastRun: lastRun, LiveRunners: live,
		Externals: current, Now: now, CacheTTL: p.cfg.CacheTTL,
	}) {
		if err := p.docker.RemoveVolume(ctx, name); err != nil {
			if !docker.IsConflict(err) {
				p.log.Warn("could not remove volume", "volume", name, "err", err)
			}
			continue
		}
		p.log.Info("volume removed", "volume", name)
	}
}

func (p *Pool) remove(ctx context.Context, removal Removal, now time.Time) {
	r := removal.Runner
	for _, id := range []string{r.ContainerID, r.SidecarID} {
		if id == "" {
			continue
		}
		if err := p.docker.Remove(ctx, id); err != nil {
			p.log.Error("could not remove runner container", "repo", r.Repo, "slot", r.Slot, "err", err)
			return
		}
	}
	if r.WorkVolume != "" {
		if err := p.docker.RemoveVolume(ctx, r.WorkVolume); err != nil {
			p.log.Warn("could not remove work volume", "volume", r.WorkVolume, "err", err)
		}
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
	created, err := p.startJob(ctx, name, runnerID, jitConfig, s)
	if err != nil {
		p.log.Error("could not start runner", "repo", s.Repo, "slot", s.Slot, "err", err)
		for _, id := range created {
			_ = p.docker.Remove(ctx, id)
		}
		_ = p.docker.RemoveVolume(ctx, name+"-work")
		if err := p.gh.DeleteRunner(ctx, s.Repo, runnerID); err != nil {
			p.log.Warn("could not deregister runner", "repo", s.Repo, "runner_id", runnerID, "err", err)
		}
		p.fail(s.Repo, now)
		return
	}
	p.log.Info("runner started", "repo", s.Repo, "slot", s.Slot, "runner", name, "docker", p.cfg.Docker)
}

// startJob starts a job's containers and returns the IDs it created, for cleanup on failure.
// With Docker on, the job's daemon starts first and the runner joins its network namespace:
// ports that services: publish on the daemon are then on the runner's localhost, as on a
// GitHub-hosted runner. Both share the work folder at the same path, so the bind mounts the
// runner asks the daemon for resolve to the same files.
func (p *Pool) startJob(ctx context.Context, name string, runnerID int64, jitConfig string, s Spawn) ([]string, error) {
	labels := func(role string) map[string]string {
		return map[string]string{
			labelPool:     p.cfg.Name,
			labelRepo:     s.Repo,
			labelRunnerID: strconv.FormatInt(runnerID, 10),
			labelSlot:     strconv.Itoa(s.Slot),
			labelRole:     role,
			labelWork:     name + "-work",
		}
	}
	volumeLabels := func(kind string) map[string]string {
		return map[string]string{
			labelPool: p.cfg.Name, labelRepo: s.Repo, labelKind: kind,
			labelRunnerID: strconv.FormatInt(runnerID, 10),
		}
	}

	mounts := make([]docker.Mount, 0, len(jobCaches)+1)
	for _, cache := range jobCaches {
		volume := cacheVolume(p.cfg.Name, s.Repo, s.Slot, cache.suffix)
		if err := p.docker.EnsureVolume(ctx, volume, volumeLabels(kindCache)); err != nil {
			return nil, fmt.Errorf("create cache volume: %w", err)
		}
		mounts = append(mounts, docker.Mount{Source: volume, Target: cache.target})
	}
	work := docker.Mount{Source: name + "-work", Target: workDir}
	if err := p.docker.EnsureVolume(ctx, work.Source, volumeLabels(kindWork)); err != nil {
		return nil, fmt.Errorf("create work volume: %w", err)
	}
	mounts = append(mounts, work)

	cpus, nanoCPUs := cpuset(s.Slot, p.cfg.Slots, p.wholeCPUs(), p.hostCPUs), int64(0)
	if cpus == "" {
		nanoCPUs = int64(p.cfg.CPUs * 1e9)
	}
	network := p.network(s.Slot)
	env := []string{"RUNNER_JITCONFIG=" + jitConfig}
	var created []string

	if p.cfg.Docker {
		externals, err := p.ensureExternals(ctx)
		if err != nil {
			return nil, err
		}
		store := cacheVolume(p.cfg.Name, s.Repo, s.Slot, dockerCache.suffix)
		if err := p.docker.EnsureVolume(ctx, store, volumeLabels(kindCache)); err != nil {
			return nil, fmt.Errorf("create docker cache volume: %w", err)
		}
		sidecar, err := p.docker.Run(ctx, name+"-docker", docker.Spec{
			Image: p.cfg.DindImage,
			Cmd:   []string{dindBridge, dindPools},
			// No TLS: the daemon is reachable only from the runner, through the shared
			// network namespace, and from nothing else on the slot's own network.
			Env:    []string{"DOCKER_TLS_CERTDIR="},
			Labels: labels(roleDocker),
			Mounts: []docker.Mount{
				work,
				{Source: externals, Target: externalsDir, ReadOnly: true},
				{Source: store, Target: dockerCache.target},
			},
			Network:    network,
			Privileged: true,
			CpusetCpus: cpus,
			NanoCPUs:   nanoCPUs,
			Memory:     p.cfg.Memory,
		})
		if sidecar != "" {
			created = append(created, sidecar)
		}
		if err != nil {
			return created, fmt.Errorf("start the job's docker: %w", err)
		}
		network = "container:" + sidecar
		env = append(env, "DOCKER_HOST=tcp://localhost:2375")
	}

	runner, err := p.docker.Run(ctx, name, docker.Spec{
		Image:      p.cfg.Image,
		Env:        env,
		Labels:     labels(roleRunner),
		Mounts:     mounts,
		Network:    network,
		Init:       true,
		CpusetCpus: cpus,
		NanoCPUs:   nanoCPUs,
		Memory:     p.cfg.Memory,
	})
	if runner != "" {
		created = append(created, runner)
	}
	return created, err
}

// ensureExternals returns the volume holding the runner image's externals (the Node builds
// the runner puts in job containers), filling it once per image build. A job's Docker needs
// them at the same path to run `container:` jobs.
func (p *Pool) ensureExternals(ctx context.Context) (string, error) {
	imageID, err := p.docker.ImageID(ctx, p.cfg.Image)
	if err != nil {
		return "", fmt.Errorf("read runner image ID: %w", err)
	}
	if volume, ok := p.externals[imageID]; ok {
		return volume, nil
	}
	volume := p.externalsVolume(imageID)
	exists, err := p.docker.VolumeExists(ctx, volume)
	if err != nil {
		return "", err
	}
	if !exists {
		labels := map[string]string{labelPool: p.cfg.Name, labelKind: kindExternals}
		if err := p.docker.EnsureVolume(ctx, volume, labels); err != nil {
			return "", fmt.Errorf("create externals volume: %w", err)
		}
		mount := []docker.Mount{{Source: volume, Target: externalsDir}}
		if err := p.docker.Seed(ctx, p.cfg.Image, mount); err != nil {
			_ = p.docker.RemoveVolume(ctx, volume)
			return "", fmt.Errorf("fill externals volume: %w", err)
		}
	}
	p.externals[imageID] = volume
	return volume, nil
}

func (p *Pool) externalsVolume(imageID string) string {
	id := strings.TrimPrefix(imageID, "sha256:")
	return fmt.Sprintf("ghpool-%s-externals-%s", p.cfg.Name, id[:min(12, len(id))])
}

// wholeCPUs is the per-job CPU count when it is a whole number, the only case a slot can be
// pinned to its own CPUs.
func (p *Pool) wholeCPUs() int {
	if p.cfg.CPUs <= 0 || p.cfg.CPUs != math.Trunc(p.cfg.CPUs) {
		return 0
	}
	return int(p.cfg.CPUs)
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
	removed := map[int64]bool{}
	for _, r := range decided.Remove {
		removed[r.Runner.RunnerID] = true
	}
	busy, waiting := 0, len(decided.Spawn)
	for _, r := range runners {
		switch {
		case removed[r.RunnerID]:
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
