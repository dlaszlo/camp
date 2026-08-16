package privileged_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/privileged"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/testenv"
)

func fixture(t *testing.T) state.Record {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
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
	if len(job.Targets) != len(record.Mounts) {
		t.Fatalf("the job has %d targets and the record %d mounts",
			len(job.Targets), len(record.Mounts))
	}
	if job.Targets[0] != record.Mounts[len(record.Mounts)-1].Target {
		t.Error("the job's first target is not the last mount made; teardown " +
			"is the mount order reversed")
	}
	if len(job.WorkParts) == 0 {
		t.Error("the job does not name camp's work directory; the kernel's " +
			"leftover there is root-owned and only the helper can remove it")
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
	job := privileged.Job{Version: 1, Action: privileged.ActionUnmount}
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
	directory := t.TempDir()
	job := privileged.Job{
		Version: 1,
		Action:  privileged.ActionUnmount,
		Targets: []string{filepath.Join(directory, "nothing-here")},
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
