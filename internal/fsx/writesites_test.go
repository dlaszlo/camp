package fsx_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/testenv"
)

// The first invariant -- camp only composes, it never modifies a
// repository -- is a property of the source code, and this is the guard
// on it.
//
// The rule it enforces: every filesystem write in camp goes through the
// fsx package, whose only addressing is an Area, and no Area can be
// constructed from a repository path. So a write target cannot be derived
// from a repository path even by accident, and a reviewer checking the
// write sites has one file to read instead of a whole tree to search.
//
// This does not replace reading the code once. It keeps the answer true
// afterwards, which is the part a person cannot do by hand every time.
var writeCalls = regexp.MustCompile(`\b(os\.(Create|CreateTemp|WriteFile|Mkdir|MkdirAll|MkdirTemp|Remove|RemoveAll|Rename|Chmod|Chown|Lchown|Truncate|Symlink|Link)|unix\.(Mkdir|Mkdirat|Unlink|Unlinkat|Rmdir|Renameat|Fchmod|Fchmodat|Fchown|Fchownat|Symlink|Symlinkat|Link|Linkat))\(`)

// openWrite catches os.OpenFile with a writing flag, which the pattern
// above cannot see from the name alone.
var openWrite = regexp.MustCompile(`os\.OpenFile\([^)]*O_(CREATE|WRONLY|RDWR|APPEND|TRUNC)`)

// allowed are the files where writing is the job.
//
//   - internal/fsx: the one door. Its whole purpose is to hold these.
//   - cmd, and any _test.go: tests build the trees they measure, and that
//     is not camp writing anything at run time.
var allowed = []string{
	"internal/fsx/fsx.go",
	"internal/testenv/testenv.go",
}

func TestEveryFilesystemWriteGoesThroughOnePackage(t *testing.T) {
	root := testenv.RepoRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, exempt := range allowed {
			if relative == exempt {
				return nil
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for number, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if writeCalls.MatchString(line) || openWrite.MatchString(line) {
				offenders = append(offenders,
					relative+":"+strconv.Itoa(number+1)+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("filesystem writes outside the fsx package:\n  %s\n\n"+
			"Every write camp makes has to be addressed through an fsx.Area, "+
			"because an Area cannot be constructed from a repository path -- "+
			"that is what makes 'camp never modifies a repository' a property of "+
			"the code rather than a promise. If one of these really belongs "+
			"where it is, move it into fsx or add it to the allowed list with "+
			"the reason.", strings.Join(offenders, "\n  "))
	}
}
