package pool

import "testing"

func TestTheJobsContainersJoinTheComposeProjectOfTheService(t *testing.T) {
	cfg := Config{Name: "pc", ComposeProject: "ghpool"}
	spawn := Spawn{Repo: "owner/app", Slot: 2}

	for _, role := range []string{roleRunner, roleDocker} {
		if got := jobLabels(cfg, "pc-2-abc", 7, spawn, role)["com.docker.compose.project"]; got != "ghpool" {
			t.Errorf("%s container: compose project %q, want ghpool", role, got)
		}
	}
}

func TestOutsideComposeTheJobsContainersCarryNoComposeLabel(t *testing.T) {
	labels := jobLabels(Config{Name: "pc"}, "pc-2-abc", 7, Spawn{Repo: "owner/app", Slot: 2}, roleDocker)

	if _, ok := labels["com.docker.compose.project"]; ok {
		t.Errorf("labels %v carry a compose project without one configured", labels)
	}
}
