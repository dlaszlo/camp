package privileged

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/testenv"
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

// A record with the helper's reply merged into it tears down the same
// way, with the identities the helper measured.
//
// The reply is what the record cannot produce on its own -- what each
// mount answers as, which is how a teardown tells camp's mount from a
// stranger's at the same path. Merging it must not cost the record what
// it already had: the staging locations stay named, because the reply
// says the mounts were made and says nothing about where they are now.
func TestAMergedReplyKeepsBothPlacesAndCarriesTheIdentities(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	built, refused := plan.Prepare(env.Config(t, ""), plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	staging := filepath.Join(built.Work, "staging")
	record := state.FromPlan(built, staging, "test", "", "", os.Getuid(), os.Getgid())

	// One result per operation, in the order the helper walks them, at the
	// paths it worked at -- which are the staging ones, because that is
	// where the tree is built. The record names the live targets, and the
	// merge is by position for exactly that reason.
	var results []Result
	for index, mount := range record.Mounts {
		at := mount.Target
		if mount.Staging != "" {
			at = mount.Staging
		}
		results = append(results, Result{
			Target:  at,
			Outcome: "mounted",
			Device:  100,
			Inode:   uint64(1000 + index),
		})
	}
	record.Mounts = merge(record.Mounts, results)

	job := UnmountJob(record)
	found := map[string]JobTarget{}
	for _, target := range job.Targets {
		if _, seen := found[target.Path]; !seen {
			found[target.Path] = target
		}
	}
	for index, mount := range record.Mounts {
		want := uint64(1000 + index)
		if target, ok := found[mount.Target]; !ok || target.Inode != want {
			t.Errorf("%s is named as %+v; the identity the helper measured is "+
				"100:%d", mount.Target, target, want)
		}
		if mount.Staging == "" {
			continue
		}
		// The same identity at the staging location: moving a mount does not
		// change the object it put there, so what the helper measured in
		// staging is what stands at the live path afterwards.
		if target, ok := found[mount.Staging]; !ok || target.Inode != want {
			t.Errorf("%s is named as %+v; a merged reply must not cost the "+
				"record the place the mount was made at", mount.Staging, target)
		}
	}
}
