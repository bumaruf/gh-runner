package pool

import (
	"cmp"
	"slices"
	"time"
)

// QueuedJob is a job waiting for a runner with labels the pool has.
type QueuedJob struct {
	Repo      string
	ID        int64
	CreatedAt time.Time
}

// Runner is one runner container of the pool, as Docker and GitHub see it now.
type Runner struct {
	Repo        string
	RunnerID    int64
	Slot        int
	ContainerID string
	StartedAt   time.Time
	Running     bool
	ExitedClean bool
	// Registered and Busy come from GitHub. When GitHub could not be asked, both are true:
	// a runner is never removed on missing information.
	Registered bool
	Busy       bool
}

type Removal struct {
	Runner Runner
	Reason string
}

type Spawn struct {
	Repo string
	Slot int
}

type Plan struct {
	Remove []Removal
	Spawn  []Spawn
}

const (
	reasonExited       = "container exited"
	reasonUnregistered = "runner no longer registered on GitHub"
	reasonIdle         = "idle with no queued job in its repo"
)

type planInput struct {
	Slots       int
	Runners     []Runner
	Queued      []QueuedJob
	Now         time.Time
	IdleTimeout time.Duration
	// OrphanGrace is how long a running container may outlive its registration: GitHub drops
	// a just-in-time runner as its job ends, a moment before the container exits.
	OrphanGrace time.Duration
	// Blocked repos get no new runner, after their runners kept failing to start.
	Blocked map[string]bool
}

// plan decides what to remove and which slot gets a runner for which repo. Each runner that is
// up and not busy will take one queued job of its repo, so only the jobs left over get a new
// runner, oldest first, in the lowest free slot.
func plan(in planInput) Plan {
	demand := map[string]int{}
	for _, job := range in.Queued {
		demand[job.Repo]++
	}

	var p Plan
	usedSlots := map[int]bool{}
	waiting := map[string]int{}
	for _, r := range in.Runners {
		age := in.Now.Sub(r.StartedAt)
		switch {
		case !r.Running:
			p.Remove = append(p.Remove, Removal{r, reasonExited})
		case !r.Registered && age > in.OrphanGrace:
			p.Remove = append(p.Remove, Removal{r, reasonUnregistered})
		case !r.Busy && demand[r.Repo] == 0 && age > in.IdleTimeout:
			p.Remove = append(p.Remove, Removal{r, reasonIdle})
		default:
			usedSlots[r.Slot] = true
			if !r.Busy {
				waiting[r.Repo]++
			}
		}
	}

	queued := slices.Clone(in.Queued)
	slices.SortFunc(queued, func(a, b QueuedJob) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	for _, job := range queued {
		if waiting[job.Repo] > 0 {
			waiting[job.Repo]--
			continue
		}
		if in.Blocked[job.Repo] {
			continue
		}
		slot := lowestFreeSlot(usedSlots, in.Slots)
		if slot == 0 {
			break
		}
		usedSlots[slot] = true
		p.Spawn = append(p.Spawn, Spawn{Repo: job.Repo, Slot: slot})
	}
	return p
}

func lowestFreeSlot(used map[int]bool, slots int) int {
	for slot := 1; slot <= slots; slot++ {
		if !used[slot] {
			return slot
		}
	}
	return 0
}
