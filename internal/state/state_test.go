package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
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
	record, built := recorded(t)
	return record, built.Config.Source
}

// recorded is the fixture with the plan it was made from, for the tests
// that hold the record against what would be mounted.
func recorded(t *testing.T) (state.Record, plan.Plan) {
	t.Helper()
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	staging := filepath.Join(built.Work, "staging")
	record := state.FromPlan(built, staging, "test", "cfgdigest", "invdigest",
		os.Getuid(), os.Getgid())
	return record, built
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
	if teardown[0].Path != record.Mounts[len(record.Mounts)-1].Target {
		t.Error("the teardown order is not the reverse of the mount order")
	}
	if record.Phase != state.Mounting {
		t.Errorf("a fresh record is in phase %q; it has to be written before "+
			"anything is mounted, so that there is no moment at which something "+
			"is mounted and nothing knows what", record.Phase)
	}
}

// The record names both places every mount can be, and it names them
// before anything is mounted.
//
// Until the move the whole tree and every mount in it is under the
// staging directory, and the staging self-bind is that tree's parent. A
// record that carried only the final live targets was a valid record
// naming not one mount that existed, for the whole of the helper's work.
func TestARecordNamesWhereEachMountWillStandAndWhereItStandsNow(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)

	if record.Staging == "" {
		t.Fatal("the record does not say where the tree is built, so nothing " +
			"in it can name a mount made before the move")
	}
	// Both self-binds, in the order the helper makes them: the staging
	// point before anything is built in it, the live point before the tree
	// is moved onto it.
	if len(record.Detached) != 2 ||
		record.Detached[0] != record.Staging || record.Detached[1] != record.Live {
		t.Fatalf("the record's detached points are %v; they have to be the "+
			"staging point %s and the live point %s, in that order",
			record.Detached, record.Staging, record.Live)
	}

	staged := 0
	for _, mount := range record.Mounts {
		if !under(mount.Target, record.Live) {
			// The workspace's own self-bind is not in the composed tree: it is
			// made at its final path and never moved, so it has one place.
			if mount.Staging != "" {
				t.Errorf("%s is not in the composed tree and the record gives it "+
					"a staging location %s", mount.Target, mount.Staging)
			}
			continue
		}
		staged++
		if mount.Staging == "" {
			t.Errorf("%s names no staging location, and that is where it stands "+
				"for the whole of the helper's work", mount.Target)
			continue
		}
		// The same relative place in both trees, because the move takes the
		// tree in one step and nothing inside it is rearranged.
		want := record.Staging + strings.TrimPrefix(mount.Target, record.Live)
		if mount.Staging != want {
			t.Errorf("%s stands at %s before the move, and the tree is built at "+
				"%s, so it stands at %s", mount.Target, mount.Staging,
				record.Staging, want)
		}
	}
	if staged == 0 {
		t.Fatal("no recorded mount is in the composed tree, so this test " +
			"measures nothing")
	}
}

func under(path, base string) bool {
	return path == base || strings.HasPrefix(path, strings.TrimRight(base, "/")+"/")
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
	for index, place := range loaded.Teardown()[:len(record.Mounts)] {
		want := record.Mounts[len(record.Mounts)-1-index].Target
		if place.Path != want {
			t.Fatalf("teardown entry %d is %q, wanted %q", index, place.Path, want)
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

// lastMount is the record's last recorded mount, which is one inside the
// composed tree and therefore one with a staging location.
func lastMount(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	mounts := raw["mounts"].([]any)
	mount := mounts[len(mounts)-1].(map[string]any)
	if mount["staging"] == nil {
		t.Fatal("the fixture's last mount carries no staging location, so the " +
			"refusals about one would measure nothing")
	}
	return mount
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

// A record is not discarded while anything it answers for is standing.
//
// The record is the only authoritative list of what a teardown has to
// remove -- down's to consume, not forget's to lose -- so the decision is
// made against the kernel's table and not against the phase, which a
// crash can leave saying anything. And it covers three areas rather than
// only the recorded targets: a mount anywhere in the work, staging or
// live tree is camp's, and one that no plan names is exactly the mount a
// discarded record would leave nothing behind for.
func TestARecordIsKeptWhileAnythingItAnswersForIsMounted(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	record.Phase = state.Partial
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		t.Fatal(err)
	}
	if held := state.Held(record, table); len(held) != 0 {
		t.Fatalf("the fixture's paths are somehow mounted already: %v", held)
	}

	// Each of these is one mount the machine has and the plan does not
	// account for in the same way, and each on its own has to keep the
	// record: at a recorded live target, at a recorded staging location,
	// beneath the live tree, beneath the staging tree, and beneath the work
	// directory. The last three are the ones a check by recorded target
	// alone cannot see.
	staged := ""
	for _, mount := range record.Mounts {
		if mount.Staging != "" {
			staged = mount.Staging
		}
	}
	if staged == "" {
		t.Fatal("no recorded mount carries a staging location, so half of this " +
			"test measures nothing")
	}
	for _, probe := range []struct {
		name  string
		point string
	}{
		{"at a recorded live target", record.Mounts[0].Target},
		{"at a recorded staging location", staged},
		{"beneath the live tree", filepath.Join(record.Live, "deep", "inside")},
		{"beneath the staging tree", filepath.Join(record.Staging, "deep", "inside")},
		{"beneath the work directory", filepath.Join(record.Created[0], "kernel-leftover")},
	} {
		t.Run(probe.name, func(t *testing.T) {
			pretend := append(append([]mountinfo.Entry{}, table...),
				mountinfo.Entry{Point: probe.point, FSType: "overlay"})
			held := state.Held(record, pretend)
			if len(held) != 1 || held[0] != probe.point {
				t.Fatalf("what is standing came back as %v, wanted %s", held, probe.point)
			}
			if err := state.Release(record, pretend); err == nil {
				t.Fatal("the record was discarded with a mount still standing")
			} else if !strings.Contains(err.Error(), probe.point) {
				t.Errorf("the refusal has to name what is still there: %v", err)
			}
			if _, found, _ := state.Load(record.Hash); !found {
				t.Error("the record was removed by a refused release")
			}
		})
	}

	// And a clean table lets it go. Otherwise the refusals above would pass
	// over a record nothing could ever discard.
	if err := state.Release(record, table); err != nil {
		t.Fatalf("a record with nothing standing was kept: %v", err)
	}
	if _, found, _ := state.Load(record.Hash); found {
		t.Error("the record survived a release with nothing mounted")
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
		{"a staging location that is not resolved",
			func(raw map[string]any) {
				lastMount(t, raw)["staging"] = "/tmp/../tmp/staging/point"
			}, "resolved"},
		// A staging location outside the staging tree is a path this record
		// cannot account for: the helper builds inside one directory, and a
		// teardown naming something elsewhere would address a mount nobody
		// made.
		{"a staging location outside the staging tree",
			func(raw map[string]any) {
				lastMount(t, raw)["staging"] = "/tmp/somewhere-else"
			}, "staging tree"},
		{"the same staging location twice",
			func(raw map[string]any) {
				mounts := raw["mounts"].([]any)
				previous := mounts[len(mounts)-2].(map[string]any)
				lastMount(t, raw)["staging"] = previous["staging"]
			}, "recorded twice"},
		{"a stranded mount that is not a resolved path",
			func(raw map[string]any) {
				raw["stranded"] = []any{"relative/path"}
			}, "absolute"},
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

// Spec §12 lists what a record carries. Two of its fields were declared
// and never filled: the overlay's options, and the paths camp created.
//
// And what "the overlay's options" has to mean, which is the repair the
// review asked for: the calls the kernel was given, from the description
// the mount is performed from. A record that rebuilt an option string of
// its own out of the same plan fields was a second account of the mount,
// free to drift from the first with nothing comparing them -- and it did
// not drift only because nobody had changed an operand yet.
func TestARecordCarriesTheOverlaysOptionsAndWhatCampCreated(t *testing.T) {
	scratch(t)
	record, built := recorded(t)

	var overlay state.Mount
	var planned plan.Mount
	for _, mount := range record.Mounts {
		if mount.FSType == "overlay" {
			overlay = mount
		}
	}
	for _, mount := range built.Mounts {
		if mount.Kind == plan.Overlay {
			planned = mount
		}
	}
	if overlay.Options == "" {
		t.Fatal("the composed tree's mount carries no options; the record is " +
			"meant to say what the kernel was given")
	}
	for _, part := range []string{"lowerdir=", "upperdir=", "workdir="} {
		if !strings.Contains(overlay.Options, part) {
			t.Errorf("the recorded options have no %s: %q", part, overlay.Options)
		}
	}

	// Call for call, in order, and not "it contains the right words": the
	// record is what a recovery holds a standing composed tree to.
	described := mountx.DescribeOverlay(planned)
	if overlay.FSType != described.FSType {
		t.Errorf("the record says the composed tree answers as %q and it is "+
			"mounted as %q", overlay.FSType, described.FSType)
	}
	if len(overlay.Operands) != len(described.Steps) {
		t.Fatalf("the record keeps %d of the %d calls the kernel is given:\n"+
			"%+v", len(overlay.Operands), len(described.Steps), overlay.Operands)
	}
	for index, step := range described.Steps {
		kept := overlay.Operands[index]
		if kept.Key != step.Key || kept.Path != step.Path {
			t.Errorf("call %d is recorded as %q=%q and the kernel is given "+
				"%q=%q", index+1, kept.Key, kept.Path, step.Key, step.Path)
		}
	}
	if overlay.Options != described.Options() {
		t.Errorf("the recorded option line is %q and the calls render as %q",
			overlay.Options, described.Options())
	}
	// And the record's own rebuilt description is the one the mount was
	// performed from, which is what makes the comparison in 'camp status'
	// a comparison with the mount and not with a paraphrase of it.
	rebuilt := overlay.Overlay()
	if len(rebuilt.Steps) != len(described.Steps) {
		t.Errorf("the record rebuilds %d calls out of %d", len(rebuilt.Steps),
			len(described.Steps))
	}

	if len(record.Created) == 0 {
		t.Fatal("the record does not say what camp created")
	}
	for _, path := range record.Created {
		if !strings.HasPrefix(path, record.Env) {
			t.Errorf("camp recorded %s as its own, and it is not in the "+
				"environment root", path)
		}
		// Storage holds half-done work and camp never removes it, so a list
		// headed "what camp may clear" must not name it.
		if strings.Contains(path, "/storage/") {
			t.Errorf("%s is in storage, which camp never removes", path)
		}
	}
}

// Two commands making a semantic transition on one record do not
// interleave.
//
// Publishing whole bytes is not the same as a transition being safe.
// down, recovery and forget each read a record, decide from it what has
// to happen, and write back what happened, and none of them holds the
// composition's own locks -- those are on the upper and the live
// directory, taken by the process that mounts, and a teardown runs when
// that process is gone. So the transition is serialized on the record
// directory's inode, which is the one thing here that no rename moves,
// and this is that serialization measured: while somebody else holds it,
// a record transition waits.
func TestARecordTransitionWaitsForTheOneBeforeIt(t *testing.T) {
	scratch(t)
	record, _ := fixture(t)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	directory, err := os.Open(state.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		record.Phase = state.Up
		done <- record.Save()
	}()

	select {
	case err := <-done:
		t.Fatalf("a record transition went through while another one held the "+
			"records: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the transition never finished after the records were released")
	}

	saved, found, err := state.Load(record.Hash)
	if err != nil || !found {
		t.Fatalf("the record is not there afterwards: found=%v err=%v", found, err)
	}
	if saved.Phase != state.Up {
		t.Errorf("the record says %q, wanted the phase the transition wrote", saved.Phase)
	}
}
