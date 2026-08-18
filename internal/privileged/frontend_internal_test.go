package privileged

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
)

// The state directory is the one place camp writes that it did not choose:
// XDG_STATE_HOME names it, and it may point anywhere. The guard refuses to
// keep the record inside a repository -- and that has to hold when the
// path only reaches a repository through a symlink, because fsx follows
// the base link when it writes and a lexical compare never sees the alias.
func TestStateHomeAliasedIntoARepositoryIsRefused(t *testing.T) {
	env := t.TempDir()
	repository := filepath.Join(env, "code")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}

	// XDG_STATE_HOME is a link into the repository. state.Dir() spells a
	// path outside it; the writes land inside.
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(repository, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", link)

	if problem := recordsOutsideRepositories(repositoryPlan(t, env)); problem == nil {
		t.Fatal("a state directory aliased into a repository by a symlink was " +
			"accepted, so camp would write a record into the repository at every up")
	}
}

// And a state directory that is genuinely outside every repository is
// accepted -- otherwise the guard would pass by refusing everything.
func TestAnOrdinaryStateHomeIsAccepted(t *testing.T) {
	env := t.TempDir()
	if err := os.MkdirAll(filepath.Join(env, "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if problem := recordsOutsideRepositories(repositoryPlan(t, env)); problem != nil {
		t.Fatalf("a state directory outside the repositories was refused: %v", problem)
	}
}

func repositoryPlan(t *testing.T, env string) plan.Plan {
	t.Helper()
	rel, err := pathx.ParseRel("repositories[0].path", "code")
	if err != nil {
		t.Fatal(err)
	}
	// Opened, not only named: a configuration carries the environment as a
	// capability, and a plan built without one is not a plan camp could act
	// on.
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return plan.Plan{Config: config.Config{
		Env:          root.Name(),
		Root:         root,
		Repositories: []config.Repository{{Name: "code", Path: rel}},
	}}
}
