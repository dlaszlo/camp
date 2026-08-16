package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
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

// A recorded mount is checked by path and by identity, because the path
// alone cannot tell camp's mount from a stranger's at the same name.
func TestARecordedMountIsCheckedByIdentityAndNotOnlyByPath(t *testing.T) {
	scratch(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	device, inode := deviceAndInode(t, identity)

	mounted := []mountinfo.Entry{{Point: target}}
	mount := state.Mount{Target: target, Device: device, Inode: inode}

	if presence, err := mount.Presence(mounted); presence != state.Same || err != nil {
		t.Errorf("the recorded object is there and read as %q (%v)", presence, err)
	}
	if presence, _ := mount.Presence(nil); presence != state.Gone {
		t.Errorf("nothing is mounted there and it read as %q", presence)
	}

	// The dangerous one: camp's mount went away and something else now
	// stands at the same path. A scan by name would call this present and
	// unmount a stranger's mount.
	stranger := state.Mount{Target: target, Device: device, Inode: inode + 1}
	if presence, _ := stranger.Presence(mounted); presence != state.Different {
		t.Errorf("a different object at the recorded path read as %q", presence)
	}

	// A record written before its mount was made carries no identity, and
	// says so rather than claiming a match it cannot make.
	unmade := state.Mount{Target: target}
	if presence, _ := unmade.Presence(mounted); presence != state.Unverified {
		t.Errorf("a mount with no recorded identity read as %q", presence)
	}
}

func deviceAndInode(t *testing.T, info os.FileInfo) (uint64, uint64) {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("this platform does not report a device and inode")
	}
	return uint64(st.Dev), st.Ino
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

// forget refuses while any mount of the recorded plan is still there.
//
// The record is the only authoritative list of what a teardown has to
// remove -- down's to consume, not forget's to lose -- so the check is
// against the kernel's table and not against the phase, which a crash can
// leave saying anything.
func TestForgetIsRefusedWhileAnythingIsStillMounted(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	record.Phase = state.Partial
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	// The kernel's table, with one of the recorded mounts in it.
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.StillMounted(record, table)) != 0 {
		t.Fatal("the fixture's paths are somehow mounted already")
	}

	pretend := append(table, mountinfo.Entry{Point: record.Mounts[0].Target, FSType: "overlay"})
	still := state.StillMounted(record, pretend)
	if len(still) != 1 || still[0] != record.Mounts[0].Target {
		t.Fatalf("the still-mounted check found %v", still)
	}
}

// list shows the phase of every record, and a corrupt one as corrupt.
func TestListShowsPhases(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	for _, phase := range []state.Phase{state.Mounting, state.Up, state.Partial, state.Down} {
		record.Phase = phase
		record.Hash = string(phase) + "0000000000"
		if err := record.Save(); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[state.Phase]bool{}
	for _, listing := range state.All() {
		if listing.Corrupt == nil {
			seen[listing.Record.Phase] = true
		}
	}
	for _, phase := range []state.Phase{state.Mounting, state.Up, state.Partial, state.Down} {
		if !seen[phase] {
			t.Errorf("no record listed in phase %q", phase)
		}
	}
}

// The record is read when something has already gone wrong, and it is the
// only list of what is mounted. So it is read strictly: a field this
// build does not know is a field somebody expected it to honour, and a
// record naming no mounts would be a teardown that succeeds by doing
// nothing.
func TestARecordThatCannotMeanWhatItSaysIsRefused(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(state.Path(record.Hash))
	if err != nil {
		t.Fatal(err)
	}

	edit := func(t *testing.T, change func(map[string]any)) []byte {
		t.Helper()
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		change(raw)
		rewritten, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return rewritten
	}

	for _, probe := range []struct {
		name   string
		change func(map[string]any)
		says   string
	}{
		{"a field this build does not know",
			func(raw map[string]any) { raw["chown_to"] = 0 }, "unknown field"},
		{"no mounts at all",
			func(raw map[string]any) { raw["mounts"] = []any{} }, "no mounts"},
		{"a phase camp does not have",
			func(raw map[string]any) { raw["phase"] = "halfway" }, "phase"},
		{"a path that is not resolved",
			func(raw map[string]any) { raw["live"] = "/tmp/../tmp/live" }, "resolved"},
		{"the same target twice",
			func(raw map[string]any) {
				mounts := raw["mounts"].([]any)
				raw["mounts"] = append(mounts, mounts[0])
			}, "recorded twice"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, err := state.Decode(edit(t, probe.change))
			if err == nil {
				t.Fatalf("%s was accepted", probe.name)
			}
			if !strings.Contains(err.Error(), probe.says) {
				t.Errorf("the refusal should say what is wrong (%q): %v",
					probe.says, err)
			}
		})
	}

	// And the file's name has to agree with what is inside it: those are
	// the two things a teardown is addressed by.
	t.Run("a record under somebody else's name", func(t *testing.T) {
		other := filepath.Join(state.Dir(), "0000cafe0000.json")
		if err := os.WriteFile(other, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := state.Load("0000cafe0000"); err == nil {
			t.Error("a record filed under another composition's name was read")
		}
	})
}
