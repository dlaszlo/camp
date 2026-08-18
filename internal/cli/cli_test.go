package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/cli"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/testenv"
)

// TestMain points the state directory somewhere of its own, for every
// test in this package.
//
// These tests invoke the real commands, and the commands that recover a
// composition now find one by the directory the process is standing in --
// which, for a test binary, is a directory inside a real environment.
// Without this, one of them read the record of the machine's own
// composition and got as far as calling sudo on it.
func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "camp-cli-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", directory)
	code := m.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}

// run invokes a command the way a terminal does, and returns what each
// stream received.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// refusing builds an environment whose composition cannot start: the same
// name at both repository roots, which the gate refuses. The plan itself
// still derives, which is exactly the situation this test is about.
func refusing(t *testing.T) string {
	t.Helper()
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Code, "AGENTS.md"), "the code's own\n")
	env.Config(t, "")
	return config.Path(env.Path)
}

// explain describes a tree to whoever is standing in it. Rendered beside a
// standing refusal it would describe a tree that will not exist, and a
// description reads as authority in a way a plan does not -- so every
// refusal stops it, not only the ones that left no plan behind.
func TestExplainDescribesNothingWhileARefusalStands(t *testing.T) {
	path := refusing(t)

	for _, mode := range [][]string{
		{"explain", "-f", path},
		{"explain", "--privileged", "-f", path},
	} {
		out, errOut, code := run(t, mode...)
		if code == 0 {
			t.Errorf("%v exited 0 with a refusal standing", mode)
		}
		if strings.Contains(out, "You are in") {
			t.Errorf("%v described the tree anyway:\n%s", mode, out)
		}
		if !strings.Contains(errOut, "AGENTS.md") {
			t.Errorf("%v did not name what stops the composition:\n%s", mode, errOut)
		}
	}
}

// A configuration that no longer parses must not stand between the user
// and a teardown.
//
// down tears down from its record and reads the file only to learn which
// record. A file edited while the composition was up -- and the session:
// section adds a whole class of ways to get that wrong -- would otherwise
// leave somebody behind mounts camp made and now refuses to remove, which
// is the one thing down is never allowed to do.
func TestATeardownIsNotBlockedByAConfigurationThatWillNotParse(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, "")
	path := config.Path(env.Path)

	broken, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Everything that names the tree still parses; the session section does
	// not.
	testenv.Write(t, path, string(broken)+"\nsession:\n  environment:\n    PORT: 8080\n")

	out, errOut, code := run(t, "down", "-f", path)

	// There is no record here, so the teardown ends by saying so -- which is
	// exactly the point: it got past the configuration to the record, and
	// stopped on the record's own terms.
	if !strings.Contains(errOut, "no record for") {
		t.Errorf("down stopped on the configuration instead of reaching the "+
			"record:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut, "environment-shape") && !strings.Contains(errOut, "PORT") {
		t.Errorf("down did not say what it could not read in the file:\n%s", errOut)
	}
	if !strings.Contains(errOut, "goes ahead anyway") {
		t.Errorf("down did not say that the teardown is unaffected:\n%s", errOut)
	}
	if code == 0 {
		t.Error("down exited 0 with nothing to tear down")
	}
}

// The same file stops every command that genuinely needs to understand it.
// Tolerating a broken configuration is a property of the teardown alone,
// not a general loosening.
func TestABrokenSectionStillStopsTheCommandsThatNeedIt(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, "")
	path := config.Path(env.Path)

	broken, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, path, string(broken)+"\nsession:\n  environment:\n    PORT: 8080\n")

	for _, command := range [][]string{
		{"plan", "-f", path},
		{"explain", "-f", path},
		{"status", "-f", path},
	} {
		_, errOut, code := run(t, command...)
		if code == 0 {
			t.Errorf("%v accepted a configuration it cannot read", command)
		}
		if !strings.Contains(errOut, "PORT") {
			t.Errorf("%v did not name the entry it could not read:\n%s", command, errOut)
		}
	}
}

// plan is the other half of the same rule, and it behaves differently on
// purpose: it prints the derived plan and then what stops it, because the
// plan is what somebody repairing the configuration needs to look at.
func TestPlanStillPrintsThePlanBesideTheRefusal(t *testing.T) {
	path := refusing(t)

	out, errOut, code := run(t, "plan", "-f", path)
	if code == 0 {
		t.Error("plan exited 0 with a refusal standing")
	}
	if !strings.Contains(out, "mount sequence, in order:") {
		t.Errorf("plan printed no sequence:\n%s", out)
	}
	// The two are printed together and go to different streams: the plan is
	// the command's product, which somebody pipes, and what stops it is
	// about the run.
	if !strings.Contains(errOut, "would not start") {
		t.Errorf("plan did not say the composition would not start:\n%s", errOut)
	}
	if strings.Contains(out, "would not start") {
		t.Errorf("the refusal is on stdout, where the plan is:\n%s", out)
	}
}

// init writes the skeleton into camp's own directory inside the
// environment, and there was nothing holding it there.
//
// It took the configuration's path and went one directory up for the base
// -- which is $ENV/.camp, camp's own directory, not the environment. So
// the area became $ENV/.camp/.camp: the command needed a directory it was
// supposed to create, failed on a fresh environment for that reason, and
// on an environment where .camp happened to exist would have written the
// three files a level too deep while printing the path it did not write.
func TestInitWritesTheSkeletonIntoTheEnvironment(t *testing.T) {
	env := t.TempDir()

	out, errOut, code := run(t, "init", env)
	if code != 0 {
		t.Fatalf("init exited %d in an empty directory:\n%s\n%s", code, out, errOut)
	}
	for _, name := range []string{"config.yml", ".gitignore", "README.md"} {
		if _, err := os.Stat(filepath.Join(env, config.Dir, name)); err != nil {
			t.Errorf("%s is not where init said it wrote it: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(env, config.Dir, config.Dir)); err == nil {
		t.Errorf("init made a second %s inside camp's own directory", config.Dir)
	}
	if !strings.Contains(out, config.Path(env)) {
		t.Errorf("init did not name the file it wrote:\n%s", out)
	}
}
