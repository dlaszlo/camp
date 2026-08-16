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
// by the tool's own switch. In the privileged mode that table is the only
// steady-state guard there is.
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
		if !strings.HasSuffix(path, ".go") {
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
