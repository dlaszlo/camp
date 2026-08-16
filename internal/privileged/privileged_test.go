package privileged_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dlaszlo/camp/internal/compose"
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
	return state.FromPlan(built, "test", "", "", os.Getuid(), os.Getgid())
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
	// Every recorded mount, and after them the points the helper bound onto
	// themselves so the composition could be moved into place.
	if len(job.Targets) != len(record.Mounts)+len(record.Detached) {
		t.Fatalf("the job has %d targets, and the record %d mounts and %d "+
			"detached points", len(job.Targets), len(record.Mounts),
			len(record.Detached))
	}
	if last := job.Targets[len(job.Targets)-1]; last.Path != record.Live {
		t.Errorf("the last thing to come down is %s, and it should be the live "+
			"path itself: the composition was standing on it", last.Path)
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

// A configuration that gains a session: section changes nothing about a
// teardown. The record is the authority, the section describes something
// this mode never started, and down must not become sensitive to it: a
// composition that could be brought up and then not taken down would wall
// the user in.
func TestATeardownIsUnaffectedByASessionSection(t *testing.T) {
	plain := fixture(t)
	withSection := recordFor(t, sessionYAML)

	targets := func(record state.Record) []string {
		job := privileged.UnmountJob(record)
		names := make([]string, 0, len(job.Targets))
		for _, target := range job.Targets {
			names = append(names, strings.TrimPrefix(target.Path, record.Env))
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
		len(withSection.Mounts)+len(withSection.Detached) {
		t.Errorf("the teardown job has %d targets and the record %d mounts",
			len(job.Targets), len(withSection.Mounts))
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

// A recorded path is not proof that camp's mount is what stands there.
//
// If camp's mount went away and something else took the same name, an
// unmount by path alone would have root remove a stranger's mount on
// camp's say-so. The identity travels with the job and is compared before
// the syscall -- so this test runs as an ordinary user, and would fail on
// the permission rather than on the check if that order ever moved.
func TestATargetThatIsNotWhatCampMountedIsNotUnmounted(t *testing.T) {
	asInvoker(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("somebody else's\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	job := privileged.Job{
		Version: 1,
		Action:  privileged.ActionUnmount,
		Base:    directory,
		Targets: []privileged.JobTarget{{Path: target, Device: 1, Inode: 1}},
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	reply := ask(t, privileged.ActionUnmount, encoded)

	if len(reply.Results) != 1 {
		t.Fatalf("%d results came back, wanted 1: %+v", len(reply.Results), reply)
	}
	if reply.Results[0].Outcome != "mismatch" {
		t.Errorf("the outcome is %q; a path holding something that is not "+
			"camp's mount must never read as absent or unmounted",
			reply.Results[0].Outcome)
	}
	if !strings.Contains(reply.Results[0].Error, target) {
		t.Errorf("the mismatch does not name the path: %q", reply.Results[0].Error)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the target was disturbed: %v", err)
	}
}

// The identity that does match is passed through to the unmount. What
// the kernel then says depends on privilege -- this test runs as an
// ordinary user, so the syscall is refused -- and what is being measured
// is that the check let it through rather than what came after.
func TestATargetWhoseIdentityMatchesIsActedOn(t *testing.T) {
	asInvoker(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("camp's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("this platform does not report a device and inode")
	}

	job := privileged.Job{
		Version: 1,
		Action:  privileged.ActionUnmount,
		Base:    directory,
		Targets: []privileged.JobTarget{
			{Path: target, Device: uint64(st.Dev), Inode: st.Ino},
		},
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
		t.Errorf("the object camp recorded was refused as somebody else's: %q",
			reply.Results[0].Error)
	}
}
