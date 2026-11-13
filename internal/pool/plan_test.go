package pool

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func input(slots int, runners []Runner, queued ...QueuedJob) planInput {
	return planInput{
		Slots:       slots,
		Runners:     runners,
		Queued:      queued,
		Now:         t0,
		IdleTimeout: 5 * time.Minute,
		OrphanGrace: 2 * time.Minute,
	}
}

func job(repo string, id int64, age time.Duration) QueuedJob {
	return QueuedJob{Repo: repo, ID: id, CreatedAt: t0.Add(-age)}
}

func runner(repo string, slot int, age time.Duration, busy bool) Runner {
	return Runner{
		Repo: repo, Slot: slot, ContainerID: repo + "-" + string(rune('0'+slot)),
		StartedAt: t0.Add(-age), Running: true, Registered: true, Busy: busy,
	}
}

func reasons(p Plan) []string {
	var out []string
	for _, r := range p.Remove {
		out = append(out, r.Reason)
	}
	return out
}

func TestQueuedJobsOfAnyRepoShareTheSlotsOldestFirst(t *testing.T) {
	p := plan(input(2, nil,
		job("me/web", 1, time.Minute),
		job("me/api", 2, 3*time.Minute),
		job("me/web", 3, 2*time.Minute),
	))

	want := []Spawn{{Repo: "me/api", Slot: 1}, {Repo: "me/web", Slot: 2}}
	if !slices.Equal(p.Spawn, want) {
		t.Fatalf("spawn = %v, want %v", p.Spawn, want)
	}
}

func TestARunnerWaitingForAJobCoversOneQueuedJobOfItsRepo(t *testing.T) {
	p := plan(input(3, []Runner{runner("me/web", 1, 10*time.Second, false)},
		job("me/web", 1, time.Minute),
		job("me/web", 2, time.Minute),
	))

	if want := []Spawn{{Repo: "me/web", Slot: 2}}; !slices.Equal(p.Spawn, want) {
		t.Fatalf("spawn = %v, want %v", p.Spawn, want)
	}
}

func TestAWaitingRunnerDoesNotCoverJobsOfAnotherRepo(t *testing.T) {
	p := plan(input(2, []Runner{runner("me/web", 1, 10*time.Second, false)},
		job("me/api", 1, time.Minute),
	))

	if want := []Spawn{{Repo: "me/api", Slot: 2}}; !slices.Equal(p.Spawn, want) {
		t.Fatalf("spawn = %v, want %v", p.Spawn, want)
	}
}

func TestBusyRunnersHoldTheirSlots(t *testing.T) {
	p := plan(input(2, []Runner{
		runner("me/web", 1, time.Hour, true),
		runner("me/api", 2, time.Hour, true),
	}, job("me/web", 1, time.Minute)))

	if len(p.Spawn) != 0 || len(p.Remove) != 0 {
		t.Fatalf("plan = %+v, want nothing while every slot is busy", p)
	}
}

func TestAnExitedContainerFreesItsSlotInTheSameTick(t *testing.T) {
	done := runner("me/web", 1, time.Hour, false)
	done.Running = false

	p := plan(input(1, []Runner{done}, job("me/api", 1, time.Minute)))

	if got := reasons(p); !slices.Equal(got, []string{reasonExited}) {
		t.Fatalf("removals = %v", got)
	}
	if want := []Spawn{{Repo: "me/api", Slot: 1}}; !slices.Equal(p.Spawn, want) {
		t.Fatalf("spawn = %v, want %v", p.Spawn, want)
	}
}

func TestAnIdleRunnerGoesOnlyAfterTheTimeoutAndWithoutDemandInItsRepo(t *testing.T) {
	young := runner("me/web", 1, 4*time.Minute, false)
	old := runner("me/api", 2, 6*time.Minute, false)
	oldWithDemand := runner("me/cli", 3, 6*time.Minute, false)

	p := plan(input(3, []Runner{young, old, oldWithDemand}, job("me/cli", 1, time.Minute)))

	if len(p.Remove) != 1 || p.Remove[0].Runner.Repo != "me/api" || p.Remove[0].Reason != reasonIdle {
		t.Fatalf("removals = %+v, want only the old idle runner of me/api", p.Remove)
	}
	if len(p.Spawn) != 0 {
		t.Fatalf("spawn = %v, want none: me/cli already has a runner waiting", p.Spawn)
	}
}

func TestARunnerGitHubNoLongerKnowsGoesAfterTheGrace(t *testing.T) {
	fresh := runner("me/web", 1, time.Minute, false)
	fresh.Registered = false
	stale := runner("me/api", 2, 3*time.Minute, false)
	stale.Registered = false

	p := plan(input(2, []Runner{fresh, stale}))

	if len(p.Remove) != 1 || p.Remove[0].Runner.Repo != "me/api" || p.Remove[0].Reason != reasonUnregistered {
		t.Fatalf("removals = %+v, want only the stale one", p.Remove)
	}
}

func TestABlockedRepoGetsNoRunnerButOthersStillDo(t *testing.T) {
	in := input(2, nil, job("me/broken", 1, 2*time.Minute), job("me/web", 2, time.Minute))
	in.Blocked = map[string]bool{"me/broken": true}

	p := plan(in)

	if want := []Spawn{{Repo: "me/web", Slot: 1}}; !slices.Equal(p.Spawn, want) {
		t.Fatalf("spawn = %v, want %v", p.Spawn, want)
	}
}

// GitHub lists a job as queued for a few seconds after a runner took it.
func TestARunnerThatJustTookAJobStillCoversItWhileTheQueueLags(t *testing.T) {
	justBusy := runner("me/web", 1, 20*time.Second, true)
	justBusy.BusySince = t0.Add(-5 * time.Second)
	busyForLong := runner("me/api", 2, time.Minute, true)
	busyForLong.BusySince = t0.Add(-time.Minute)

	in := input(4, []Runner{justBusy, busyForLong}, job("me/web", 1, time.Minute), job("me/api", 2, time.Minute))
	in.QueueLag = 30 * time.Second
	p := plan(in)

	if want := []Spawn{{Repo: "me/api", Slot: 3}}; !slices.Equal(p.Spawn, want) {
		t.Fatalf("spawn = %v, want only me/api: the me/web job is the one its runner just took", p.Spawn)
	}
}
