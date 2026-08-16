package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/testenv"
)

// scratch points the state directory at a temporary place, so that these
// tests cannot touch the records of a real composition.
func scratch(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func fixture(t *testing.T) (state.Record, string) {
	t.Helper()
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	record := state.FromPlan(built, "test", "cfgdigest", "invdigest", os.Getuid(), os.Getgid())
	return record, cfg.Source
}

func TestARecordCarriesTheWholeConcretePlanInOrder(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)

	if len(record.Mounts) < 5 {
		t.Fatalf("the record carries %d mounts; the plan has more than that",
			len(record.Mounts))
	}
	if record.Mounts[0].Role != "freeze-lower" {
		t.Errorf("the first recorded mount is %q; the order has to be the "+
			"order they were made, because its reverse is the teardown order",
			record.Mounts[0].Role)
	}

	teardown := record.Teardown()
	if teardown[0].Target != record.Mounts[len(record.Mounts)-1].Target {
		t.Error("the teardown order is not the reverse of the mount order")
	}
	if record.Phase != state.Mounting {
		t.Errorf("a fresh record is in phase %q; it has to be written before "+
			"anything is mounted, so that there is no moment at which something "+
			"is mounted and nothing knows what", record.Phase)
	}
}

// Recovery never needs the configuration. It may have been edited while
// the composition was up, and then the file that says what to unmount
// would describe a composition nobody built.
func TestDownConvergesFromTheRecordWithTheConfigurationDeleted(t *testing.T) {
	scratch(t)
	record, configPath := fixture(t)
	record.Phase = state.Up
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}

	loaded, found, err := state.Load(record.Hash)
	if err != nil || !found {
		t.Fatalf("the record could not be read back: found=%v err=%v", found, err)
	}
	if len(loaded.Mounts) != len(record.Mounts) {
		t.Fatalf("the record came back with %d mounts and had %d",
			len(loaded.Mounts), len(record.Mounts))
	}
	for index, mount := range loaded.Teardown() {
		want := record.Mounts[len(record.Mounts)-1-index].Target
		if mount.Target != want {
			t.Fatalf("teardown entry %d is %q, wanted %q", index, mount.Target, want)
		}
	}
}

func TestEveryPhaseThatMayHaveMountsIsActive(t *testing.T) {
	for _, phase := range []state.Phase{state.Mounting, state.Up, state.Partial} {
		if !phase.Active() {
			t.Errorf("phase %q should count as active: something may still be "+
				"mounted, and another up must not start over it", phase)
		}
	}
	if state.Down.Active() {
		t.Error("phase down should not count as active")
	}
}

// A record from another build is refused rather than read with fields
// that may have moved.
func TestAnUnknownVersionIsRefused(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	path := state.Path(record.Hash)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = 99
	rewritten, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := state.Load(record.Hash); err == nil {
		t.Error("a record of an unknown version was read instead of refused")
	} else if !strings.Contains(err.Error(), "version") {
		t.Errorf("the refusal should say what is wrong: %v", err)
	}
}

// A corrupt record is listed as corrupt, with its path. Silently skipping
// it would lose the only list of what is mounted.
func TestListShowsACorruptRecordRatherThanSkippingIt(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(state.Dir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	listings := state.All()
	if len(listings) != 2 {
		t.Fatalf("%d records listed, wanted 2", len(listings))
	}
	var sawCorrupt bool
	for _, listing := range listings {
		if listing.Corrupt != nil && listing.Path == broken {
			sawCorrupt = true
		}
	}
	if !sawCorrupt {
		t.Error("the unreadable record was not reported as corrupt")
	}
}

func TestTheRecordAndItsDirectoryArePrivate(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(state.Path(record.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the record is mode %v; it names every path of a composition "+
			"and should be 0600", info.Mode().Perm())
	}
	directory, err := os.Stat(state.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Errorf("the state directory is mode %v, wanted 0700", directory.Mode().Perm())
	}
}

func TestForgetRemovesTheRecordAndNothingElse(t *testing.T) {
	scratch(t)
	record, configPath := fixture(t)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	if err := state.Forget(record.Hash); err != nil {
		t.Fatal(err)
	}

	if _, found, _ := state.Load(record.Hash); found {
		t.Error("the record survived being forgotten")
	}
	for _, path := range []string{configPath, record.Live, record.Upper, record.Workspace} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed too; forget deletes one file and nothing "+
				"else: not a repository, not the storage, not the composed tree", path)
		}
	}
}
