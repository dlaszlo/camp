package inventory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

// A new name at the workspace root changes what the derived read-only
// binds protect and what the exclude covers. Both were derived from the
// accepted snapshot, so the new name is protected by neither -- and that
// has to stop an up rather than pass unnoticed.
func TestANewWorkspaceRootEntryBlocks(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	testenv.Write(t, filepath.Join(env.Workspace, "NEW.md"), "appeared\n")

	_, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Has("inventory-appeared") {
		t.Fatalf("the rules that fired were %v", refused.Rules())
	}
	if !strings.Contains(refused.Error(), "camp accept") {
		t.Error("the refusal should name the command that resolves it")
	}
}

// A type change blocks too: a directory that became a file, or anything
// that became a symlink, changes what camp would have to bind.
func TestATypeChangeBlocks(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	if err := os.Remove(filepath.Join(env.Workspace, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	testenv.MkDir(t, filepath.Join(env.Workspace, "CLAUDE.md"))

	_, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Has("inventory-changed") {
		t.Fatalf("the rules that fired were %v", refused.Rules())
	}
}

// A name that has gone, or a change on the code side, only warns. Neither
// leaves a hole in what camp protects.
func TestADisappearedEntryOnlyWarns(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	if err := os.Remove(filepath.Join(env.Workspace, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("a disappeared entry should not stop a composition:\n%v", refused)
	}
	if len(built.Warnings) == 0 {
		t.Error("it should still be said out loud")
	}
}

func TestANewCodeRootEntryOnlyWarns(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	testenv.Write(t, filepath.Join(env.Code, "CHANGELOG.md"), "new\n")

	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("a new name on the code side should not stop a composition:\n%v", refused)
	}
	if len(built.Warnings) == 0 {
		t.Error("it should still be said out loud")
	}
}

// Without a snapshot camp refuses and names the command. It does not
// write one: an up that generated the file it was supposed to be checked
// against would swallow the signal entirely.
func TestAMissingSnapshotIsRefusedAndNotGenerated(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	path := inventory.Path(cfg.Env)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	_, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Has("inventory-missing") {
		t.Fatalf("the rules that fired were %v", refused.Rules())
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the snapshot was written by a run that was supposed to check " +
			"against it")
	}
	if !strings.Contains(refused.Error(), "camp accept") {
		t.Error("the refusal should name the command")
	}
}

// The file is byte-sorted, one record per line, so that its diff is
// something a person can read.
func TestTheSnapshotIsSortedByBytesAndSurvivesHostileNames(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	data, err := os.ReadFile(inventory.Path(cfg.Env))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	// The header says what kind of file this is; the records start after it.
	if !strings.HasPrefix(lines[0], "#") {
		t.Fatal("the snapshot does not open with a header saying it is a " +
			"record rather than a setting")
	}
	for len(lines) > 0 && strings.HasPrefix(lines[0], "#") {
		lines = lines[1:]
	}
	for index := 1; index < len(lines); index++ {
		if lines[index] < lines[index-1] {
			t.Fatalf("line %d (%q) comes before line %d (%q)",
				index+1, lines[index], index, lines[index-1])
		}
	}

	// A name with a tab in it: legal on Linux, and the old space-and-arrow
	// format could not write it down at all.
	testenv.Write(t, filepath.Join(env.Workspace, "two\tcolumns"), "x\n")
	env.Accept(t, cfg)

	snapshot, found, err := inventory.Load(cfg.Root)
	if err != nil || !found {
		t.Fatalf("the snapshot did not read back: found=%v err=%v", found, err)
	}
	var seen bool
	for _, entry := range snapshot.Entries {
		if entry.Name == "two\tcolumns" {
			seen = true
		}
	}
	if !seen {
		t.Error("a name containing a tab did not survive the round trip")
	}

	// And with it accepted, the composition is not blocked by it.
	_, refused := plan.Prepare(cfg, plan.Namespace)
	for _, rule := range refused.Rules() {
		if strings.HasPrefix(rule, "inventory-") {
			t.Errorf("the accepted name still blocked: %v", refused.Error())
		}
	}
}

// A damaged snapshot is an error, not an absence. Treating it as "nothing
// accepted yet" would silently drop every comparison it was carrying.
func TestADamagedSnapshotIsRefusedRatherThanIgnored(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	if err := os.WriteFile(inventory.Path(cfg.Env), []byte("bad\\qline\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Has("inventory-unreadable") {
		t.Fatalf("the rules that fired were %v", refused.Rules())
	}
}
