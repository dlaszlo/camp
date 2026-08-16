package health_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/health"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

func look(t *testing.T, env *testenv.Env) []health.Note {
	t.Helper()
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		t.Fatal(err)
	}
	return health.Look(cfg, built, table)
}

// Renaming the composed tree's directory changes the name camp's storage
// is derived from, which orphans the old one. Nothing is lost, and
// nothing points at it either -- so doctor lists it, and camp never
// removes it: storage holds worktrees and machine-local files.
func TestOrphanedStorageIsListedAndNotRemoved(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}

	// A storage directory whose composed tree is gone: exactly what a
	// rename leaves behind.
	orphan := filepath.Join(cfg.CampDir(), "storage", "deadbeef0000")
	testenv.MkDir(t, orphan)
	testenv.Write(t, filepath.Join(orphan, compose.MarkerName),
		"live\t"+filepath.Join(env.Path, "gone")+"\nconfig\t"+cfg.Source+"\n")
	testenv.Write(t, filepath.Join(orphan, "unfinished.txt"), "work in progress\n")

	notes := look(t, env)
	rendered := health.Render(notes)
	if !strings.Contains(rendered, orphan) {
		t.Fatalf("the orphaned storage was not listed:\n%s", rendered)
	}
	if !strings.Contains(rendered, "will not remove it") {
		t.Error("the note should say that camp leaves it alone, and why")
	}
	if _, err := os.Stat(filepath.Join(orphan, "unfinished.txt")); err != nil {
		t.Error("looking at the environment removed something; doctor only reads")
	}
}

// A storage directory camp cannot attribute is reported and left alone:
// camp removes only what it can prove is its own.
func TestUnattributableStorageIsReportedRatherThanRemoved(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	strange := filepath.Join(cfg.CampDir(), "storage", "no-marker")
	testenv.MkDir(t, strange)

	rendered := health.Render(look(t, env))
	if !strings.Contains(rendered, strange) {
		t.Fatalf("it was not reported:\n%s", rendered)
	}
	if _, err := os.Stat(strange); err != nil {
		t.Error("it was removed")
	}
}

// The filesystems the composition sits on, and the flags a read-only
// remount will have to replicate. Information, never a refusal.
func TestTheFilesystemsAndTheirLockedFlagsAreReported(t *testing.T) {
	env := testenv.NewEnv(t)
	rendered := health.Render(look(t, env))
	if !strings.Contains(rendered, "locked flags") {
		t.Errorf("the locked flags were not reported:\n%s", rendered)
	}
	if !strings.Contains(rendered, "locale") {
		t.Error("the locale was not reported")
	}
}
