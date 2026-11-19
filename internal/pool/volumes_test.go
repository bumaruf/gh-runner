package pool

import (
	"slices"
	"testing"
	"time"

	"github.com/bumaruf/gh-runner/internal/docker"
)

func TestEachRepoGetsItsOwnCachesInASlot(t *testing.T) {
	web := cacheVolume("pc", "me/web", 1, "pnpm")
	api := cacheVolume("pc", "me/api", 1, "pnpm")
	otherSlot := cacheVolume("pc", "me/web", 2, "pnpm")

	if web == api || web == otherSlot {
		t.Fatalf("names collide: %s %s %s", web, api, otherSlot)
	}
	if want := "ghpool-pc-me--web-s1-pnpm"; web != want {
		t.Fatalf("name = %s, want %s", web, want)
	}
}

func TestRepoNamesBecomeValidVolumeNamesWithoutCollisions(t *testing.T) {
	if a, b := repoSlug("Me/My.Repo"), repoSlug("me/my.repo"); a != b {
		t.Fatalf("GitHub names are case-insensitive: %s != %s", a, b)
	}
	if a, b := repoSlug("me/a-b"), repoSlug("me-a/b"); a == b {
		t.Fatalf("different repos share the slug %s", a)
	}
}

func volume(name, kind string, labels ...string) docker.Volume {
	l := map[string]string{labelKind: kind}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return docker.Volume{Name: name, Labels: l}
}

func TestStaleVolumesAreThoseNothingWillUseAgain(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	got := staleVolumes(sweepInput{
		Volumes: []docker.Volume{
			volume("active-cache", kindCache, labelRepo, "me/web"),
			volume("quiet-cache", kindCache, labelRepo, "me/old"),
			volume("unknown-cache", kindCache, labelRepo, "me/unknown"),
			volume("gone-repo-cache", kindCache, labelRepo, "me/deleted"),
			volume("live-work", kindWork, labelRunnerID, "7"),
			volume("dead-work", kindWork, labelRunnerID, "8"),
			volume("current-externals", kindExternals),
			volume("old-externals", kindExternals),
		},
		Served:      map[string]bool{"me/web": true, "me/old": true, "me/unknown": true},
		LastRun:     map[string]time.Time{"me/web": now.Add(-time.Hour), "me/old": now.Add(-30 * 24 * time.Hour)},
		LiveRunners: map[string]bool{"7": true},
		Externals:   "current-externals",
		Now:         now,
		CacheTTL:    14 * 24 * time.Hour,
	})

	want := []string{"quiet-cache", "gone-repo-cache", "dead-work", "old-externals"}
	if !slices.Equal(got, want) {
		t.Fatalf("stale = %v, want %v (a repo GitHub did not answer about keeps its caches)", got, want)
	}
}

func TestEachSlotGetsItsOwnCPUsWhenTheyFit(t *testing.T) {
	noSMT := coreOrder(24, func(int) []int { return nil })
	if got := cpuset(1, 6, 4, noSMT); got != "0,1,2,3" {
		t.Fatalf("slot 1 = %q", got)
	}
	if got := cpuset(6, 6, 4, noSMT); got != "20,21,22,23" {
		t.Fatalf("slot 6 = %q", got)
	}
	if got := cpuset(1, 6, 5, noSMT); got != "" {
		t.Fatalf("6 slots of 5 CPUs do not fit on 24, got %q", got)
	}
}

// With SMT, Linux numbers a core's two threads apart (0 and 12 on a 12-core host). A slot that
// took 0 and 1 would leave 12 and 13 to the host, and both would fight over the same cores.
func TestWithSMTASlotOfTwoHoldsOneWholeCore(t *testing.T) {
	smt := coreOrder(24, func(cpu int) []int { return []int{cpu % 12, cpu%12 + 12} })

	if got := cpuset(1, 6, 2, smt); got != "0,12" {
		t.Fatalf("slot 1 = %q, want core 0", got)
	}
	if got := cpuset(6, 6, 2, smt); got != "5,17" {
		t.Fatalf("slot 6 = %q, want core 5", got)
	}
}

func TestTheKernelsCPUListsAreRead(t *testing.T) {
	for list, want := range map[string][]int{"0,12": {0, 12}, "0-1": {0, 1}, "2-3,14-15": {2, 3, 14, 15}, "x": nil} {
		if got := parseCPUList(list); !slices.Equal(got, want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", list, got, want)
		}
	}
}

// Until the pool knows which repos it serves, every cache would look like a repo that left.
func TestNoCacheIsStaleBeforeThePoolKnowsItsRepos(t *testing.T) {
	got := staleVolumes(sweepInput{
		Volumes: []docker.Volume{volume("web-cache", kindCache, labelRepo, "me/web")},
		Now:     time.Now(),
	})

	if len(got) != 0 {
		t.Fatalf("stale = %v, want none", got)
	}
}
