package pool

import (
	"testing"

	"github.com/bumaruf/gh-runner/internal/github"
)

func repo(name string, private, admin bool) github.Repo {
	r := github.Repo{FullName: name, Private: private}
	r.Permissions = &struct {
		Admin bool `json:"admin"`
	}{Admin: admin}
	return r
}

// An installation's repos carry the installation's own permissions block, without admin;
// what lets the pool register runners there is the app's Administration permission.
func TestWithAppAuthTheRepoPermissionsBlockDoesNotMatter(t *testing.T) {
	p := New(Config{AppAuth: true}, nil, nil, nil)

	if !p.serves(repo("me/web", true, false)) {
		t.Fatal("want a private installation repo served")
	}
}

func TestWithATokenOnlyReposTheUserAdministersAreServed(t *testing.T) {
	p := New(Config{}, nil, nil, nil)

	if p.serves(repo("me/fork", true, false)) || !p.serves(repo("me/web", true, true)) {
		t.Fatal("want only the repo with admin served")
	}
}

func TestPublicReposStayOutUnlessAllowed(t *testing.T) {
	if New(Config{AppAuth: true}, nil, nil, nil).serves(repo("me/cli", false, true)) {
		t.Fatal("want public repos out by default")
	}
	if !New(Config{AppAuth: true, AllowPublic: true}, nil, nil, nil).serves(repo("me/cli", false, true)) {
		t.Fatal("want public repos in with AllowPublic")
	}
}
