package privileged_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/privileged"
	"github.com/dlaszlo/camp/internal/testenv"
)

// What a job may ask root to do.
//
// The helper is the only thing sudo wraps, and it was general where it was
// described as narrow: the ids to chown to came out of the job, any
// absolute path could be named for unmounting, and a caller-chosen base
// joined with caller-chosen components was removed and given away. A job
// naming "/" and "etc" made root hand the whole of /etc to whoever asked.
//
// These run the real entry point with hostile jobs. They pass as an
// ordinary user because every one of them has to be refused before any
// syscall that would need privilege -- if a refusal ever moved to after
// the act, these would start failing on the check rather than on the
// permission.

// asInvoker makes the environment look the way sudo leaves it.
func asInvoker(t *testing.T) {
	t.Helper()
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()))
	t.Setenv("SUDO_GID", strconv.Itoa(os.Getgid()))
}

// ask runs one job through the helper and returns what came back.
func ask(t *testing.T, action privileged.Action, body []byte) privileged.Reply {
	t.Helper()
	var out bytes.Buffer
	privileged.Helper(action, bytes.NewReader(body), &out)
	var reply privileged.Reply
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
		t.Fatalf("the reply did not parse: %v\n%s", err, out.String())
	}
	return reply
}

func askJob(t *testing.T, action privileged.Action, job privileged.Job) privileged.Reply {
	t.Helper()
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	return ask(t, action, encoded)
}

// The whole class, one case each, every one refused by rule.
func TestTheHelperRefusesAJobThatIsNotAboutOneCompositionOfYours(t *testing.T) {
	mine := t.TempDir()

	cases := []struct {
		name string
		job  privileged.Job
		rule string
		says string
	}{
		{
			name: "the machine's root as the base",
			job:  privileged.Job{Version: 1, Action: privileged.ActionUnmount, Base: "/", WorkParts: []string{"etc"}},
			rule: "helper-base-invalid",
			says: "root of the filesystem",
		},
		{
			name: "a base somebody else owns",
			job:  privileged.Job{Version: 1, Action: privileged.ActionUnmount, Base: "/etc", WorkParts: []string{"camp"}},
			rule: "helper-base-not-yours",
			says: "belongs to uid",
		},
		{
			name: "a relative base",
			job:  privileged.Job{Version: 1, Action: privileged.ActionUnmount, Base: "etc"},
			rule: "helper-base-invalid",
			says: "absolute",
		},
		{
			name: "an unmount target outside the base",
			job: privileged.Job{Version: 1, Action: privileged.ActionUnmount, Base: mine,
				Targets: []privileged.JobTarget{{Path: "/proc"}}},
			rule: "helper-target-outside",
			says: "/proc",
		},
		{
			name: "an unmount target that only looks like a prefix",
			job: privileged.Job{Version: 1, Action: privileged.ActionUnmount, Base: mine,
				Targets: []privileged.JobTarget{{Path: mine + "-elsewhere/x"}}},
			rule: "helper-target-outside",
			says: "not inside",
		},
		{
			name: "a climbing component",
			job: privileged.Job{Version: 1, Action: privileged.ActionUnmount, Base: mine,
				WorkParts: []string{"..", "..", "etc"}},
			rule: "helper-component-invalid",
			says: "'..'",
		},
		{
			name: "a component carrying a separator",
			job: privileged.Job{Version: 1, Action: privileged.ActionMount, Base: mine,
				StagingParts: []string{"a/b"}},
			rule: "helper-component-invalid",
			says: "one name",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			asInvoker(t)
			reply := askJob(t, test.job.Action, test.job)
			if reply.Rule != test.rule {
				t.Fatalf("the helper answered %q (%s), wanted %q",
					reply.Rule, reply.Error, test.rule)
			}
			if !contains(reply.Error, test.says) {
				t.Errorf("the refusal does not mention %q:\n%s", test.says, reply.Error)
			}
			if len(reply.Results) != 0 {
				t.Errorf("the helper acted before refusing: %+v", reply.Results)
			}
		})
	}
}

// The ids come from sudo. A uid in the job is a request for root to hand
// something to somebody, which is not a request this program answers.
func TestTheHelperTakesTheOwnerFromSudoAndNotFromTheJob(t *testing.T) {
	asInvoker(t)
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	// A job asking for the tree to be handed to somebody else entirely.
	reply := askJob(t, privileged.ActionUnmount, privileged.Job{
		Version: 1, Action: privileged.ActionUnmount, Base: base,
		WorkParts: []string{"work"}, UID: 12345, GID: 12345,
	})
	// It never gets as far as chowning: the directory is not camp's.
	if reply.Rule != "helper-not-camps" {
		t.Fatalf("the helper answered %q (%s), wanted helper-not-camps",
			reply.Rule, reply.Error)
	}

	var st unix.Stat_t
	if err := unix.Stat(work, &st); err != nil {
		t.Fatal(err)
	}
	if int(st.Uid) != os.Getuid() {
		t.Errorf("the directory changed hands: it belongs to uid %d", st.Uid)
	}
}

// The one directory the helper removes and gives away has to be one camp
// made. The marker is what says so; a path that merely spells the same
// components is somebody else's.
func TestTheHelperOnlyClearsADirectoryCampMarked(t *testing.T) {
	asInvoker(t)
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	unmarked := filepath.Join(env.Path, "not-camps")
	if err := os.MkdirAll(filepath.Join(unmarked, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	reply := askJob(t, privileged.ActionUnmount, privileged.Job{
		Version: 1, Action: privileged.ActionUnmount, Base: env.Path,
		WorkParts: []string{"not-camps"},
	})
	if reply.Rule != "helper-not-camps" {
		t.Fatalf("the helper answered %q (%s)", reply.Rule, reply.Error)
	}
	if _, err := os.Stat(filepath.Join(unmarked, "work")); err != nil {
		t.Errorf("the helper removed something in a directory that is not "+
			"camp's: %v", err)
	}

	// And the real one, which camp marked when it created it, is accepted --
	// otherwise this test would pass by refusing everything.
	built, problems := plan.Prepare(cfg, plan.Privileged)
	if !problems.Empty() {
		t.Fatalf("the fixture was refused:\n%v", problems)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	reply = askJob(t, privileged.ActionUnmount, privileged.Job{
		Version: 1, Action: privileged.ActionUnmount, Base: env.Path,
		WorkParts: []string{".camp", "work", built.Hash},
	})
	if reply.Rule != "" {
		t.Errorf("camp's own work directory was refused: %s (%s)",
			reply.Rule, reply.Error)
	}
}

// A field the helper does not understand is a field somebody expected it
// to honour, and this process is root.
func TestTheHelperRefusesAJobItCannotReadExactly(t *testing.T) {
	asInvoker(t)
	base := t.TempDir()

	cases := map[string]string{
		"an unknown field": `{"version":1,"action":"unmount","base":"` + base +
			`","escalate":true}`,
		"a second job after the first": `{"version":1,"action":"unmount","base":"` +
			base + `"}{"version":1,"action":"unmount","base":"/"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			reply := ask(t, privileged.ActionUnmount, []byte(body))
			if reply.Rule != "helper-job-invalid" {
				t.Errorf("the helper answered %q (%s)", reply.Rule, reply.Error)
			}
		})
	}
}

// Nobody to act for is not a case this program has.
func TestTheHelperRefusesWhenNothingSaysWhoInvokedIt(t *testing.T) {
	t.Setenv("SUDO_UID", "")
	t.Setenv("SUDO_GID", "")
	reply := askJob(t, privileged.ActionUnmount, privileged.Job{
		Version: 1, Action: privileged.ActionUnmount, Base: t.TempDir(),
	})
	if reply.Rule != "helper-no-invoker" {
		t.Errorf("the helper answered %q (%s)", reply.Rule, reply.Error)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || bytes.Contains([]byte(haystack), []byte(needle))
}

// The base arrives already resolved, so a symbolic link at its name is
// not somebody's convenience: it was put there after the front end
// looked, and following it would address this composition's operands
// beneath whatever it points at.
//
// Everything the old, name-based check asked is true of this fixture --
// the link points at a directory, and at one the invoking user owns -- so
// the only thing that can refuse it is the descriptor. The base is opened
// following nothing, and it is that open, and the check on what it
// returned, that decide.
func TestTheHelperRefusesABaseThatIsALink(t *testing.T) {
	asInvoker(t)
	scratch := t.TempDir()
	real := filepath.Join(scratch, "env")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(scratch, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	reply := askJob(t, privileged.ActionUnmount, privileged.Job{
		Version: 1, Action: privileged.ActionUnmount, Base: link,
		Targets: []privileged.JobTarget{{Path: filepath.Join(link, "live")}},
	})
	if reply.Rule != "helper-base-invalid" {
		t.Fatalf("the helper answered %q (%s), wanted helper-base-invalid",
			reply.Rule, reply.Error)
	}
	if !contains(reply.Error, "symbolic link") {
		t.Errorf("the refusal does not say the base is a link:\n%s", reply.Error)
	}
	if len(reply.Results) != 0 {
		t.Errorf("the helper acted before refusing: %+v", reply.Results)
	}
}
