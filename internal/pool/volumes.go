package pool

import (
	"fmt"
	"strings"
	"time"

	"github.com/bumaruf/gh-runner/internal/docker"
)

// Volume kinds, in the ghpool.kind label.
const (
	kindCache     = "cache"
	kindWork      = "work"
	kindExternals = "externals"
)

// jobCaches are the volumes kept per repo and slot. Keying them by repo keeps one repo from
// ever reading what another wrote; keying them by slot means only one job at a time writes
// each of them.
var jobCaches = []struct{ suffix, target string }{
	{"cache", "/home/runner/.cache"},
	{"pnpm", "/home/runner/.local/share/pnpm"},
	{"npm", "/home/runner/.npm"},
	{"toolcache", "/opt/hostedtoolcache"},
}

// dockerCache is the image store of a job's own Docker, kept per repo and slot like the others.
var dockerCache = struct{ suffix, target string }{"docker", "/var/lib/docker"}

// cacheVolume names a cache volume of a repo in a slot. Docker allows [a-zA-Z0-9_.-] after
// the first character; owner/repo maps onto that without collisions.
func cacheVolume(pool, repo string, slot int, suffix string) string {
	return fmt.Sprintf("ghpool-%s-%s-s%d-%s", pool, repoSlug(repo), slot, suffix)
}

func repoSlug(repo string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(repo) {
		switch {
		case r == '/':
			b.WriteString("--")
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

type sweepInput struct {
	Volumes []docker.Volume
	// Served are the repos the pool serves now.
	Served map[string]bool
	// LastRun is when each repo last started a workflow run, for the repos GitHub answered.
	LastRun map[string]time.Time
	// LiveRunners are the runner IDs that still have a container.
	LiveRunners map[string]bool
	// Externals is the volume the current runner image fills.
	Externals string
	Now       time.Time
	CacheTTL  time.Duration
}

// staleVolumes picks the volumes nothing will use again: caches of repos that left the pool or
// have not run for CacheTTL, work folders of runners that are gone, and externals of runner
// images that were replaced. Docker refuses to remove a volume a container still uses.
func staleVolumes(in sweepInput) []string {
	var stale []string
	for _, v := range in.Volumes {
		switch v.Labels[labelKind] {
		case kindCache:
			repo := v.Labels[labelRepo]
			last, known := in.LastRun[repo]
			if !in.Served[repo] || (known && in.Now.Sub(last) > in.CacheTTL) {
				stale = append(stale, v.Name)
			}
		case kindWork:
			if !in.LiveRunners[v.Labels[labelRunnerID]] {
				stale = append(stale, v.Name)
			}
		case kindExternals:
			if v.Name != in.Externals {
				stale = append(stale, v.Name)
			}
		}
	}
	return stale
}

// cpuset pins slot n to its own run of CPUs, so `nproc` in a job reports what the job gets.
// It returns "" when the slots do not fit on the host's CPUs; a CPU quota then applies.
func cpuset(slot, slots, perSlot, hostCPUs int) string {
	if perSlot < 1 || slots*perSlot > hostCPUs {
		return ""
	}
	first := (slot - 1) * perSlot
	return fmt.Sprintf("%d-%d", first, first+perSlot-1)
}
