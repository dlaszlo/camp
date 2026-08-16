package mountx_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	root := repositoryRoot(t)

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
					offenders = append(offenders, relative+":"+itoa(number+1)+": "+trimmed)
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

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test's directory")
		}
		directory = parent
	}
}
