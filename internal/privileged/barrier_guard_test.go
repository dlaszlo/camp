package privileged_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/testenv"
)

// The barrier must be impossible in a camp anybody runs, and one line
// keeps it that way.
//
// The privileged half can be stopped at named points, so that the kill
// matrix and the rename-race measurements can be made at all. That is a
// pause, inside the process that is root, triggered by a file in a
// directory the invoking user owns -- which is not a test seam but the
// primitive those measurements exist to prove camp is safe from. What
// makes it safe is that the body carrying the protocol is compiled only
// under -tags camptest, so a shipped binary has the empty one and there
// is nothing in it to find.
//
// The whole guarantee rests on a build constraint at the top of one file.
// A constraint is easy to lose in an edit and its loss is silent: the
// tests still pass, the build still works, and the seam is in the binary.
// So this reads the file as data and fails the build if the line is not
// there -- the way the other guards in this repository hold a rule that
// nothing else would notice breaking.
func TestTheBarrierCannotBeBuiltIntoAnOrdinaryCamp(t *testing.T) {
	root := testenv.RepoRoot(t)

	for _, probe := range []struct {
		file string
		tag  string
		why  string
	}{
		{
			file: "internal/privileged/barrier_camptest.go",
			tag:  "//go:build camptest",
			why: "this file carries the pause protocol, and without the " +
				"constraint every camp would carry it too",
		},
		{
			file: "internal/privileged/barrier.go",
			tag:  "//go:build !camptest",
			why: "this file is the empty body an ordinary build gets, and " +
				"without the constraint it would collide with the other one " +
				"-- which is a loud failure, but the constraint is what says " +
				"which build gets which",
		},
	} {
		t.Run(filepath.Base(probe.file), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, probe.file))
			if err != nil {
				t.Fatalf("%s: %v", probe.file, err)
			}
			// The first line, and not merely somewhere in the file: a build
			// constraint the compiler honours has to stand before the package
			// clause, and one further down is a comment that looks like a
			// guarantee.
			first, _, _ := strings.Cut(string(data), "\n")
			if strings.TrimSpace(first) != probe.tag {
				t.Errorf("%s opens with %q and it has to open with %q.\n%s",
					probe.file, first, probe.tag, probe.why)
			}
		})
	}
}

// And the empty body stays empty.
//
// A barrier that grew a real body in the untagged file would put the
// pause back into every build without anybody having to touch a build
// constraint. The function is one line and it is meant to stay one line;
// if it ever needs more, that is a decision to make deliberately rather
// than to arrive at.
func TestTheOrdinaryBarrierDoesNothing(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testenv.RepoRoot(t),
		"internal/privileged/barrier.go"))
	if err != nil {
		t.Fatal(err)
	}
	const body = "func barrier(Job, string) {}"
	if !strings.Contains(string(data), body) {
		t.Errorf("the barrier an ordinary build gets is no longer %q.\n"+
			"Whatever it does now, every camp does it, and the point of the "+
			"split is that a shipped binary can be stopped by nobody.", body)
	}
}
