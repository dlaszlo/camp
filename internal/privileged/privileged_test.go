package privileged_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/privileged"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/testenv"
)

// sessionYAML is the fixture's configuration with a session: section on
// it. The privileged mode neither applies nor refuses one, so everything
// this package does has to behave exactly as it did before the section
// existed.
//
// The name it reads is deliberately not a CAMP_ one. Those are refused as
// interpolation inputs by definition, so a sentinel behind one could never
// be copied into anything however wrong the code was -- a guard that
// cannot fail is not a guard.
const sessionYAML = `
session:
  identity: uidmap
  environment:
    SESSION_TOKEN: "$TEST_SENTINEL"
    PATH: "$CAMP_LIVE/.workspace/bin:$PATH"
`

func fixture(t *testing.T) state.Record {
	t.Helper()
	return recordFor(t, "")
}

func recordFor(t *testing.T, tail string) state.Record {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	yaml := ""
	if tail != "" {
		yaml = env.YAML() + tail
	}
	cfg := env.Config(t, yaml)
	built, refused := plan.Prepare(cfg, plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	return state.FromPlan(built, filepath.Join(built.Work, "staging"), "test", "",
		"", os.Getuid(), os.Getgid())
}

// The teardown instruction comes from the record and from nothing else.
// That is the whole recovery story: after a crash, with the configuration
// edited or deleted, the record still names every mount in the order they
// have to come down.
func TestTheTeardownJobIsBuiltFromTheRecordAlone(t *testing.T) {
	record := fixture(t)
	if err := os.Remove(record.Config); err != nil {
		t.Fatal(err)
	}

	job := privileged.UnmountJob(record)
	if job.Action != privileged.ActionUnmount {
		t.Errorf("the job's action is %q", job.Action)
	}
	// Every recorded mount at both places it can be, and after them the two
	// points the helper bound onto themselves so nothing could propagate.
	staged := 0
	for _, mount := range record.Mounts {
		if mount.Staging != "" {
			staged++
		}
	}
	if want := len(record.Mounts) + staged + len(record.Detached); len(job.Targets) != want {
		t.Fatalf("the job has %d targets, and the record %d mounts, %d of them "+
			"with a staging location, and %d detached points", len(job.Targets),
			len(record.Mounts), staged, len(record.Detached))
	}
	if last := job.Targets[len(job.Targets)-1]; last.Path != record.Staging {
		t.Errorf("the last thing to come down is %s, and it should be the "+
			"staging point: it is the first mount the helper makes and the "+
			"parent of everything built before the move", last.Path)
	}
	if job.Targets[0].Path != record.Mounts[len(record.Mounts)-1].Target {
		t.Error("the job's first target is not the last mount made; teardown " +
			"is the mount order reversed")
	}
	if len(job.WorkParts) == 0 {
		t.Error("the job does not name camp's work directory; the kernel's " +
			"leftover there is root-owned and only the helper can remove it")
	}
}

// The teardown names every place the machine could have a mount, in the
// order that takes the tree apart from the top -- from the record alone,
// with no helper reply merged into it.
//
// This is the record a kill leaves behind: written before the helper's
// first syscall and never updated, because the helper died before it
// could reply. Until the move the whole tree is under the staging
// directory, so a teardown built from this record has to name the staging
// locations as well as the live ones, and it cannot know which of the two
// the machine is in.
func TestTheTeardownNamesBothPlacesInTheOrderTheyComeApart(t *testing.T) {
	record := fixture(t)
	if record.Phase != state.Mounting {
		t.Fatalf("the fixture is in phase %q, and the record this is about is "+
			"the one written before anything was mounted", record.Phase)
	}

	// The live targets in reverse, then the staging locations in reverse,
	// then the live self-bind, then the staging self-bind: the reverse of
	// the order the helper makes them in. A mount is only ever in one of
	// its two places, so naming the other costs one "absent" answer -- what
	// must not happen is a place that has a mount and no target names it.
	var want []string
	for index := len(record.Mounts) - 1; index >= 0; index-- {
		want = append(want, record.Mounts[index].Target)
	}
	for index := len(record.Mounts) - 1; index >= 0; index-- {
		if staged := record.Mounts[index].Staging; staged != "" {
			want = append(want, staged)
		}
	}
	want = append(want, record.Live, record.Staging)

	got := targetPaths(privileged.UnmountJob(record))
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the teardown order is\n  %s\nand it has to be\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// The two paths that carry two mounts each are named twice, and that is
	// the point: the composed tree stands on the self-bind underneath it,
	// at the live path and -- before the move -- at the staging path, and
	// each of those is one unmount.
	counted := map[string]int{}
	for _, path := range got {
		counted[path]++
	}
	for _, path := range []string{record.Live, record.Staging} {
		if counted[path] != 2 {
			t.Errorf("%s is named %d time(s); the tree and the bind it stands "+
				"on are two mounts at one path", path, counted[path])
		}
	}
	for path, times := range counted {
		if times > 1 && path != record.Live && path != record.Staging {
			t.Errorf("%s is named %d times, and one path is one mount", path, times)
		}
	}
}

// What a failed rollback could not remove survives into the record and is
// named by the next teardown.
//
// The helper reports it, the front end used to print it in a sentence and
// drop it, and then 'camp down' -- which is built from the record and
// nothing else -- could not address it at all.
func TestAStrandedMountBecomesATargetOfTheNextTeardown(t *testing.T) {
	record := fixture(t)

	// One place the plan does not name -- a mount that appeared where
	// nothing planned one -- and one it does.
	unplanned := filepath.Join(record.Staging, "propagated", "copy")
	record.Strand(unplanned, record.Live)
	record.Phase = state.Partial
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, found, err := state.Load(record.Hash)
	if err != nil || !found {
		t.Fatalf("the record could not be read back: found=%v err=%v", found, err)
	}
	paths := targetPaths(privileged.UnmountJob(loaded))
	if len(paths) == 0 {
		t.Fatal("the teardown job names nothing")
	}
	if paths[0] != unplanned {
		t.Fatalf("the teardown's first target is %q, and a stranded mount the "+
			"plan does not name goes first: nothing knows what stands on it",
			paths[0])
	}
	// A stranded path the plan already names keeps the place the order
	// gives it, rather than being named a third time.
	appearances := 0
	for _, path := range paths {
		if path == record.Live {
			appearances++
		}
	}
	if appearances != 2 {
		t.Errorf("%s is named %d times; it was stranded and it is already the "+
			"tree and the bind underneath it", record.Live, appearances)
	}

	// And it does not grow: the same mount stranded twice is one mount.
	loaded.Strand(unplanned)
	if len(loaded.Stranded) != 2 {
		t.Errorf("the record lists %d stranded places after the same one was "+
			"recorded twice: %v", len(loaded.Stranded), loaded.Stranded)
	}
}

func targetPaths(job privileged.Job) []string {
	paths := make([]string, 0, len(job.Targets))
	for _, target := range job.Targets {
		paths = append(paths, target.Path)
	}
	return paths
}

// A configuration that gains a session: section changes nothing about a
// teardown. The record is the authority, the section describes something
// this mode never started, and down must not become sensitive to it: a
// composition that could be brought up and then not taken down would wall
// the user in.
func TestATeardownIsUnaffectedByASessionSection(t *testing.T) {
	plain := fixture(t)
	withSection := recordFor(t, sessionYAML)

	// Both the environment root and the composition's own identifier come
	// out of the comparison: the two fixtures are two scratch trees with
	// two live paths, and the hash is derived from the live path, so the
	// work directory's name differs for a reason that has nothing to do
	// with the section.
	targets := func(record state.Record) []string {
		job := privileged.UnmountJob(record)
		names := make([]string, 0, len(job.Targets))
		for _, target := range job.Targets {
			name := strings.TrimPrefix(target.Path, record.Env)
			names = append(names, strings.ReplaceAll(name, record.Hash, "<hash>"))
		}
		return names
	}
	if strings.Join(targets(plain), "\n") != strings.Join(targets(withSection), "\n") {
		t.Errorf("the teardown order changed when the configuration gained a "+
			"session: section:\n%v\n---\n%v", targets(plain), targets(withSection))
	}

	// And it still comes from the record alone, with the configuration gone.
	if err := os.Remove(withSection.Config); err != nil {
		t.Fatal(err)
	}
	if job := privileged.UnmountJob(withSection); len(job.Targets) !=
		len(withSection.Teardown()) {
		t.Errorf("the teardown job has %d targets and the record names %d "+
			"places", len(job.Targets), len(withSection.Teardown()))
	}
}

// Where a resolved value may appear: the workload's own environment, and
// nowhere else. Not in the record camp writes to disk, and not in the job
// that crosses into the privileged half -- neither of which has any reason
// to carry one, and both of which outlive the session.
func TestNoDeclaredValueReachesTheRecordOrTheHelpersJob(t *testing.T) {
	const sentinel = "s3cret-inherited-value"
	t.Setenv("TEST_SENTINEL", sentinel)

	record := recordFor(t, sessionYAML)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Errorf("the state record carries an inherited value:\n%s", encoded)
	}
	if !bytes.Contains(encoded, []byte(record.Live)) {
		t.Error("the state record does not carry the composed tree's path, which " +
			"it is supposed to")
	}

	for name, job := range map[string]privileged.Job{
		"the unmount job": privileged.UnmountJob(record),
	} {
		encoded, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(sentinel)) {
			t.Errorf("%s carries an inherited value:\n%s", name, encoded)
		}
	}
}

// The same, for the job the helper is handed when mounting: it names
// paths, identities and options, and no environment ever enters it.
func TestTheMountJobCarriesPathsAndNoEnvironment(t *testing.T) {
	const sentinel = "s3cret-inherited-value"
	t.Setenv("TEST_SENTINEL", sentinel)

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	built, refused := plan.Prepare(env.Config(t, env.YAML()+sessionYAML), plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}

	// The operands have to exist: the job records what each one was when
	// the front end looked at it, which is what closes the swap race.
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, built.ExcludeFile(), "")

	job, problems := privileged.MountJob(built, filepath.Join(built.Work, "staging"), nil)
	if !problems.Empty() {
		t.Fatalf("the mount job was refused:\n%v", problems)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Errorf("the helper's job carries an inherited value:\n%s", encoded)
	}
	for _, want := range []string{"SESSION_TOKEN", "session"} {
		if bytes.Contains(encoded, []byte(want)) {
			t.Errorf("the helper's job mentions %q; the session is not the "+
				"helper's business:\n%s", want, encoded)
		}
	}
	if !bytes.Contains(encoded, []byte(built.Config.Env)) {
		t.Error("the helper's job does not carry the environment root, which it " +
			"resolves every operand beneath")
	}
}

// The job carries everything the helper's verification needs, because the
// helper has nothing else.
//
// It reads no configuration by design, so anything the pass reaches for
// through a configuration is the zero value there. That is not a partial
// check but a silent one: an empty lower path matches no mount, so the
// frame's first mount -- the workspace bound read-only onto itself, which
// every plan has -- read as missing, and every honest privileged
// composition was rolled back before it could be moved into place.
func TestTheMountJobCarriesWhatTheVerificationNeeds(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	built, refused := plan.Prepare(env.Config(t, ""), plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, built.ExcludeFile(), "")

	payload := []byte("# the validated exclude\n")
	job, problems := privileged.MountJob(built, filepath.Join(built.Work, "staging"), payload)
	if !problems.Empty() {
		t.Fatalf("the mount job was refused:\n%v", problems)
	}

	if job.LowerPath != built.Config.LowerPath() || job.LowerPath == "" {
		t.Errorf("the job's lower path is %q and the composition's is %q",
			job.LowerPath, built.Config.LowerPath())
	}
	if job.Storage != built.Storage || job.Storage == "" {
		t.Errorf("the job's storage is %q and the composition's is %q",
			job.Storage, built.Storage)
	}
	if string(job.Exclude) != string(payload) {
		t.Errorf("the job carries %q as the exclude payload", job.Exclude)
	}

	// And the mount the whole thing turned on is in there, at its own path
	// rather than remapped into the staging tree: it is not in the tree.
	var found bool
	for _, mount := range job.Mounts {
		if mount.Target == built.Config.LowerPath() {
			found = true
		}
	}
	if !found {
		t.Error("the job has no mount at the workspace's own path, so the " +
			"check this test is about could not fire either way")
	}
}

// The helper reads its whole instruction from stdin. Never from argv:
// /proc exposes a process's arguments to every user on the machine, and
// the job names every path of the composition.
func TestTheHelperRefusesAJobItDoesNotUnderstand(t *testing.T) {
	cases := []struct {
		name string
		job  string
		want string
	}{
		{"not JSON", "{not json", "does not parse"},
		{"another version", `{"version":99,"action":"unmount"}`, "version"},
		{"another action", `{"version":1,"action":"mount"}`, "asks for"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			code := privileged.Helper(privileged.ActionUnmount,
				strings.NewReader(test.job), &out)
			if code == 0 {
				t.Error("the helper accepted a job it should have refused")
			}
			var reply privileged.Reply
			if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
				t.Fatalf("the reply did not parse: %v\n%s", err, out.String())
			}
			if !strings.Contains(reply.Error, test.want) {
				t.Errorf("the refusal is %q and should mention %q", reply.Error, test.want)
			}
		})
	}
}

// An empty teardown is not an error: the helper unmounts what it was
// given, and being given nothing is a job with nothing to do.
func TestTheHelperUnmountsNothingQuietly(t *testing.T) {
	asInvoker(t)
	job := privileged.Job{Version: 1, Action: privileged.ActionUnmount,
		Base: t.TempDir()}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := privileged.Helper(privileged.ActionUnmount, bytes.NewReader(encoded), &out); code != 0 {
		t.Errorf("an empty job exited %d\n%s", code, out.String())
	}
}

// A path that is not a mount point is "absent", which is the job already
// done however it came about -- and never a reason to detach anything.
func TestUnmountingSomethingThatIsNotMountedIsNotAFailure(t *testing.T) {
	asInvoker(t)
	directory := t.TempDir()
	job := privileged.Job{
		Version: 1,
		Action:  privileged.ActionUnmount,
		Base:    directory,
		Targets: []privileged.JobTarget{{Path: filepath.Join(directory, "nothing-here")}},
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := privileged.Helper(privileged.ActionUnmount, bytes.NewReader(encoded), &out)
	if code != 0 {
		t.Errorf("exited %d\n%s", code, out.String())
	}
	var reply privileged.Reply
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Results) != 1 || reply.Results[0].Outcome != "absent" {
		t.Errorf("the result is %+v, wanted one 'absent'", reply.Results)
	}
}

// Running the front end as root is refused. Under root the generation
// step would run as root -- handing a shell to whoever can edit the
// configuration -- and everything camp creates would belong to root,
// including the storage the design guarantees the user can write.
func TestRunningTheFrontEndAsRootIsRefused(t *testing.T) {
	err := privileged.RefuseRoot("up")
	if os.Geteuid() == 0 {
		if err == nil {
			t.Error("running as root was accepted")
		}
		return
	}
	if err != nil {
		t.Errorf("an ordinary user was refused: %v", err)
	}
}

// A mount that is simply gone is not a stranger's mount.
//
// After a teardown that unmounted everything and died before it could say
// so, every recorded path resolves to whatever was underneath it -- for
// the live directory, the empty directory the composition stood on, whose
// identity is of course not the composition's. Comparing those made 'camp
// down' refuse to remove eleven mounts that were already gone, and leave
// the record behind claiming somebody else's mounts were in the way.
// Whether anything is mounted there is the first question, and only the
// mount table can answer it.
func TestAPathWhoseMountIsGoneIsAbsentAndNotAStranger(t *testing.T) {
	asInvoker(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// An identity nothing on this machine has, at a path that is a real
	// directory and not a mount point.
	job := privileged.Job{
		Version: 1,
		Action:  privileged.ActionUnmount,
		Base:    directory,
		Targets: []privileged.JobTarget{{Path: target, Device: 222, Inode: 6295166}},
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	reply := ask(t, privileged.ActionUnmount, encoded)

	if len(reply.Results) != 1 {
		t.Fatalf("%d results came back, wanted 1: %+v", len(reply.Results), reply)
	}
	if reply.Results[0].Outcome == "mismatch" {
		t.Errorf("a path with no mount on it was read as somebody else's "+
			"mount: %q", reply.Results[0].Error)
	}
}

// The composed tree's own operands travel as components and identities,
// not only as paths.
//
// The overlay decides what the whole composition shows and where every
// write lands, and it was the one operation whose operands crossed into
// root as bare strings: three paths the kernel resolved again, at mount
// time, following whatever stood there then.
func TestTheMountJobPinsTheOverlaysOperands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	built, refused := plan.Prepare(env.Config(t, ""), plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, built.ExcludeFile(), "")

	job, problems := privileged.MountJob(built, filepath.Join(built.Work, "staging"), nil)
	if !problems.Empty() {
		t.Fatalf("the mount job was refused:\n%v", problems)
	}

	var overlay *privileged.JobMount
	for index, mount := range job.Mounts {
		if mount.Kind == string(plan.Overlay) {
			overlay = &job.Mounts[index]
		}
	}
	if overlay == nil {
		t.Fatal("the job has no overlay, so this test measures nothing")
	}
	if len(overlay.LowerParts) != len(overlay.Lower) {
		t.Errorf("%d lower layers and %d of them addressed by component",
			len(overlay.Lower), len(overlay.LowerParts))
	}
	for index, identity := range overlay.LowerIdents {
		if identity == "" {
			t.Errorf("lower layer %d crosses with no identity", index)
		}
	}
	if overlay.UpperIdent == "" || len(overlay.UpperParts) == 0 {
		t.Errorf("the upper layer crosses unpinned: %q %v",
			overlay.UpperIdent, overlay.UpperParts)
	}
	if overlay.WorkIdent == "" || len(overlay.WorkParts) == 0 {
		t.Errorf("the work directory crosses unpinned: %q %v",
			overlay.WorkIdent, overlay.WorkParts)
	}
}

// An operand with nothing to compare against is refused before anything
// is mounted. A missing identity used to be accepted outright, which made
// "camp could not look at it" and "it is not there yet" the same thing on
// the wire.
func TestTheHelperRefusesAnOperandItCannotCheck(t *testing.T) {
	base := t.TempDir()
	testenv.MkDir(t, filepath.Join(base, "work", "staging"))
	testenv.MkDir(t, filepath.Join(base, "lower"))
	testenv.MkDir(t, filepath.Join(base, "point"))

	cases := []struct {
		name string
		job  privileged.Job
		says string
	}{
		{
			name: "a mount point outside the staging tree with no identity",
			says: "not inside the staging tree",
			job: privileged.Job{
				Version: 1, Action: privileged.ActionMount, Base: base,
				StagingParts: []string{"work", "staging"},
				LiveParts:    []string{"live"},
				Mounts: []privileged.JobMount{{
					Kind: string(plan.BindRO), Target: filepath.Join(base, "point"),
					TargetParts: []string{"point"},
					Source:      filepath.Join(base, "lower"),
					SourceParts: []string{"lower"},
				}},
			},
		},
		{
			name: "a mount point inside the staging tree that nothing says was absent",
			says: "does not say it was absent",
			job: privileged.Job{
				Version: 1, Action: privileged.ActionMount, Base: base,
				StagingParts: []string{"work", "staging"},
				LiveParts:    []string{"live"},
				Mounts: []privileged.JobMount{{
					Kind:        string(plan.BindRO),
					Target:      filepath.Join(base, "work", "staging", "point"),
					TargetParts: []string{"work", "staging", "point"},
					Source:      filepath.Join(base, "lower"),
					SourceParts: []string{"lower"},
					SourceIdent: "1:1",
					SourceType:  string(pathx.Dir),
				}},
			},
		},
		{
			name: "a bind whose source carries no identity",
			says: "no identity for the mount source",
			job: privileged.Job{
				Version: 1, Action: privileged.ActionMount, Base: base,
				StagingParts: []string{"work", "staging"},
				LiveParts:    []string{"live"},
				Mounts: []privileged.JobMount{{
					Kind:         string(plan.BindRO),
					Target:       filepath.Join(base, "work", "staging", "point"),
					TargetParts:  []string{"work", "staging", "point"},
					TargetAbsent: true,
					Source:       filepath.Join(base, "lower"),
					SourceParts:  []string{"lower"},
					SourceType:   string(pathx.Dir),
				}},
			},
		},
		{
			name: "a bind whose source carries no kind",
			says: "camp binds directories and regular files",
			job: privileged.Job{
				Version: 1, Action: privileged.ActionMount, Base: base,
				StagingParts: []string{"work", "staging"},
				LiveParts:    []string{"live"},
				Mounts: []privileged.JobMount{{
					Kind:         string(plan.BindRO),
					Target:       filepath.Join(base, "work", "staging", "point"),
					TargetParts:  []string{"work", "staging", "point"},
					TargetAbsent: true,
					Source:       filepath.Join(base, "lower"),
					SourceParts:  []string{"lower"},
					SourceIdent:  "1:1",
				}},
			},
		},
		{
			name: "an overlay whose upper layer carries no identity",
			says: "composed tree's upper layer",
			job: privileged.Job{
				Version: 1, Action: privileged.ActionMount, Base: base,
				StagingParts: []string{"work", "staging"},
				LiveParts:    []string{"live"},
				Mounts: []privileged.JobMount{{
					Kind:        string(plan.Overlay),
					Target:      filepath.Join(base, "work", "staging"),
					TargetParts: []string{"work", "staging"},
					// The mount point is inside the staging tree and the job says
					// it was not there yet, so the refusal this measures is the
					// upper layer's and not the target's.
					TargetAbsent: true,
					Lower:        []string{filepath.Join(base, "lower")},
					LowerParts:   [][]string{{"lower"}},
					LowerIdents:  []string{"1:1"},
					Upper:        filepath.Join(base, "upper"),
				}},
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			asInvoker(t)
			reply := askJob(t, privileged.ActionMount, test.job)
			if reply.Rule != "helper-operand-unchecked" {
				t.Fatalf("the helper answered %q (%s), wanted helper-operand-unchecked",
					reply.Rule, reply.Error)
			}
			// Each case has to be refused for its own reason, or a job that
			// fixed the field this one is about would still be refused and the
			// test would say nothing about it.
			if !contains(reply.Error, test.says) {
				t.Errorf("the refusal does not mention %q:\n%s", test.says, reply.Error)
			}
			if len(reply.Results) != 0 {
				t.Errorf("the helper acted before refusing: %+v", reply.Results)
			}
		})
	}
}

// And an operand that is no longer what the front end looked at stops the
// job, whatever the path still says.
func TestTheHelperRefusesAnOperandThatChanged(t *testing.T) {
	base := t.TempDir()
	testenv.MkDir(t, filepath.Join(base, "work", "staging"))
	testenv.MkDir(t, filepath.Join(base, "lower"))
	testenv.MkDir(t, filepath.Join(base, "upper"))
	testenv.MkDir(t, filepath.Join(base, "work", "overlay"))

	asInvoker(t)
	reply := askJob(t, privileged.ActionMount, privileged.Job{
		Version: 1, Action: privileged.ActionMount, Base: base,
		StagingParts: []string{"work", "staging"},
		LiveParts:    []string{"live"},
		Mounts: []privileged.JobMount{{
			Kind:         string(plan.Overlay),
			Target:       filepath.Join(base, "work", "staging"),
			TargetParts:  []string{"work", "staging"},
			TargetAbsent: true,
			Lower:        []string{filepath.Join(base, "lower")},
			LowerParts:   [][]string{{"lower"}},
			// What no directory on this machine will answer as.
			LowerIdents: []string{"999999:999999"},
			Upper:       filepath.Join(base, "upper"),
			UpperParts:  []string{"upper"},
			UpperIdent:  "999999:999999",
			Work:        filepath.Join(base, "work", "overlay"),
			WorkParts:   []string{"work", "overlay"},
			WorkIdent:   "999999:999999",
		}},
	})
	if !contains(reply.Error, "is not the object camp checked") {
		t.Fatalf("the helper answered %q (%s)", reply.Rule, reply.Error)
	}
	if len(reply.Results) != 0 {
		t.Errorf("the helper acted before refusing: %+v", reply.Results)
	}
}

// A source that is not the kind of thing the front end saw stops the job
// before anything is mounted.
//
// A bind puts one object over another, and the kernel refuses a directory
// over a file or a file over a directory. So the kind is part of what
// makes an operand the operand camp planned: a source that was a
// directory when the front end looked and is a file now is not the same
// source, even where the identity check cannot see it -- and an identity
// somebody wrote into a hand-made job proves nothing at all. The kind
// travels with the job and is compared against the descriptor the helper
// opened.
func TestTheHelperRefusesASourceThatIsNotTheKindCampChecked(t *testing.T) {
	base := t.TempDir()
	testenv.MkDir(t, filepath.Join(base, "work", "staging"))
	testenv.MkDir(t, filepath.Join(base, "directory"))
	testenv.Write(t, filepath.Join(base, "file"), "")

	for _, test := range []struct {
		name    string
		source  string
		claimed pathx.Type
		says    string
	}{
		{"a file where the job says directory", "file", pathx.Dir, "is a file and camp checked a directory"},
		{"a directory where the job says file", "directory", pathx.File, "is a directory and camp checked a file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			asInvoker(t)
			source := filepath.Join(base, test.source)
			reply := askJob(t, privileged.ActionMount, privileged.Job{
				Version: 1, Action: privileged.ActionMount, Base: base,
				StagingParts: []string{"work", "staging"},
				LiveParts:    []string{"live"},
				Mounts: []privileged.JobMount{{
					Kind:         string(plan.BindRO),
					Target:       filepath.Join(base, "work", "staging", "point"),
					TargetParts:  []string{"work", "staging", "point"},
					TargetAbsent: true,
					Source:       source,
					SourceParts:  []string{test.source},
					// The identity is the real one, so the only thing left to
					// refuse this is the kind.
					SourceIdent: identityOf(t, source),
					SourceType:  string(test.claimed),
				}},
			})

			if !contains(reply.Error, test.says) {
				t.Fatalf("the helper answered %q (%s)", reply.Rule, reply.Error)
			}
			if len(reply.Results) != 0 || reply.Moved {
				t.Errorf("the helper acted before refusing: %+v", reply)
			}
			if !reply.RolledBack {
				t.Error("a refusal before the first mount reported a machine " +
					"still carrying something")
			}
		})
	}
}

// identityOf spells a path's device and inode the way the wire does.
func identityOf(t *testing.T, path string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
