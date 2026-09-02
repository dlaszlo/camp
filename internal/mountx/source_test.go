package mountx_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/testenv"
)

// A lazy unmount is not in camp, and this is the guard that keeps it out.
//
// Measured, which is why the rule is absolute rather than a preference:
// after a lazy detach the target is gone from the kernel's mount table
// while a shell whose working directory is inside keeps writing through
// it -- and a second overlay then mounts on the same upper and writes
// too, which is exactly the state the locks exist to prevent, re-entered
// by the tool's own switch.
//
// So a mount that cannot be removed is an error: named, reported, and a
// non-zero exit. It was --force wearing another name, and the flag that
// asked for it is deleted.
func TestNothingInCampCanDetachAMountLazily(t *testing.T) {
	root := testenv.RepoRoot(t)

	forbidden := []string{"MNT_DETACH", "umount -l", "allowDetach", "--allow-detach"}
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || !testenv.OwnModule(t, root, path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if relative == "internal/mountx/source_test.go" {
			return nil // this file names them in order to look for them
		}
		for number, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// The prose that explains why it is absent is allowed to name it.
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, word := range forbidden {
				if strings.Contains(line, word) {
					offenders = append(offenders, relative+":"+strconv.Itoa(number+1)+": "+trimmed)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("camp contains a lazy unmount:\n  %s\n\n"+
			"A detached mount leaves the kernel's table while it is still alive "+
			"and still being written through. There is no --force in camp, and "+
			"this was the same thing under another name.",
			strings.Join(offenders, "\n  "))
	}
}

// Every fsconfig call camp makes goes through the described sequence,
// and this is the guard that keeps it there.
//
// The behaviour is measured next door: the calls that fill a filesystem
// context are held against the description they are performing. That
// test can only see the calls that go through the two variables it
// replaces, so this one keeps every other route closed -- a
// unix.FsconfigSetFd written anywhere else is an operand the record does
// not carry, the verification does not expect and 'camp status' cannot
// compare, which is exactly the drift the description exists to make
// impossible.
//
// The two declarations in mountx.go are what everything goes through,
// and they are the only lines allowed to name the calls.
func TestEveryFsconfigCallGoesThroughTheDescription(t *testing.T) {
	root := testenv.RepoRoot(t)

	allowed := map[string]bool{
		"internal/mountx/mountx.go":      true,
		"internal/mountx/source_test.go": true, // it names them to look for them
	}
	var offenders []string
	for _, path := range testenv.Tracked(t) {
		if !strings.HasSuffix(path, ".go") || !testenv.OwnModule(t, root, path) {
			continue
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		if allowed[relative] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // a tracked file that is not in this checkout
		}
		for number, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "unix.FsconfigSet") {
				offenders = append(offenders,
					relative+":"+strconv.Itoa(number+1)+": "+trimmed)
			}
		}
	}

	// And in mountx itself, only where the two variables are declared.
	body, err := os.ReadFile(filepath.Join(root, "internal", "mountx", "mountx.go"))
	if err != nil {
		t.Fatal(err)
	}
	for number, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || !strings.Contains(line, "unix.FsconfigSet") {
			continue
		}
		if trimmed == "fsconfigFd   = unix.FsconfigSetFd" ||
			trimmed == "fsconfigFlag = unix.FsconfigSetFlag" {
			continue
		}
		offenders = append(offenders,
			"internal/mountx/mountx.go:"+strconv.Itoa(number+1)+": "+trimmed)
	}

	if len(offenders) > 0 {
		t.Errorf("an overlay operand reaches the kernel outside the described "+
			"sequence:\n  %s\n\n"+
			"What camp tells the kernel about a composed tree is one object: "+
			"DescribeOverlay derives it, the mount is performed from it and "+
			"the verification compares the mounted filesystem against it. A "+
			"call made anywhere else is an operand nothing else knows about.", strings.Join(offenders, "\n  "))
	}
}
