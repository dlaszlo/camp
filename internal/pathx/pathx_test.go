package pathx_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/pathx"
)

func rel(t *testing.T, raw string) pathx.Rel {
	t.Helper()
	parsed, err := pathx.ParseRel("test", raw)
	if err != nil {
		t.Fatalf("%q should have parsed: %v", raw, err)
	}
	return parsed
}

func TestGrammarRefusals(t *testing.T) {
	for _, raw := range []string{"", "/etc", "~/x", "a//b", "a/./b", "a/../b", "..", "."} {
		if _, err := pathx.ParseRel("field", raw); err == nil {
			t.Errorf("%q was accepted and should not have been", raw)
		}
	}
	for _, raw := range []string{"a", ".git", "a/b/c", ".git/info/exclude", "a b", "a\tb"} {
		if _, err := pathx.ParseRel("field", raw); err != nil {
			t.Errorf("%q should have been accepted: %v", raw, err)
		}
	}
}

func TestComponentMustBeOneName(t *testing.T) {
	if _, err := pathx.ParseComponent("field", "a/b"); err == nil {
		t.Error("a path was accepted where a single name is required")
	}
	if _, err := pathx.ParseComponent("field", ".gitignore"); err != nil {
		t.Errorf("a plain name was refused: %v", err)
	}
}

// Containment is component-wise, never a string prefix: ".claude-local"
// starts with ".claude" as a string and is not inside it.
func TestInsideIsComponentWise(t *testing.T) {
	claude := rel(t, ".claude")
	cases := []struct {
		path string
		want bool
	}{
		{".claude/agents", true},
		{".claude/agents/x.md", true},
		{".claude-local/agents", false},
		{".claude", false},
		{".clau", false},
	}
	for _, test := range cases {
		if got := rel(t, test.path).Inside(claude); got != test.want {
			t.Errorf("%q inside .claude: got %v, wanted %v", test.path, got, test.want)
		}
	}
	if !rel(t, "anything").Inside(pathx.Rel{}) {
		t.Error("every target is inside the merged root")
	}
	root := pathx.Rel{}
	if root.Inside(rel(t, "anything")) {
		t.Error("the merged root is inside nothing")
	}
}

func TestStatBeneathReportsTypesWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		want pathx.Type
	}{
		{"dir", pathx.Dir},
		{"file", pathx.File},
		{"link", pathx.Symlink},
		{"absent", pathx.Absent},
	}
	for _, test := range cases {
		info, err := pathx.StatBeneath(root, []string{test.name})
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if info.Type != test.want {
			t.Errorf("%s came out as %q, wanted %q", test.name, info.Type, test.want)
		}
	}

	info, err := pathx.StatBeneath(root, []string{"link"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Link != "/etc" {
		t.Errorf("the link's target came out as %q", info.Link)
	}
}

// A symlink anywhere in the path is refused, not followed. This is the
// check that has no race inside it: a component the user owns can be
// swapped between validation and the mount, and refusing the whole class
// is the only answer that stays true.
func TestASymlinkInThePathIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "hop")); err != nil {
		t.Fatal(err)
	}

	_, err := pathx.StatBeneath(root, []string{"hop", "secret"})
	if !errors.Is(err, pathx.ErrSymlinkInPath) {
		t.Errorf("resolution through a symlink returned %v, wanted a refusal", err)
	}
}

func TestReadDirBeneathSortsByBytes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b", "A", "a", "_", "Z"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := pathx.ReadDirBeneath(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "Z", "_", "a", "b"}
	for index, entry := range entries {
		if entry.Name != want[index] {
			t.Fatalf("entry %d is %q, wanted %q -- a locale sort and a byte sort "+
				"silently disagree, and every comparison camp makes has to be the "+
				"same order", index, entry.Name, want[index])
		}
	}
}

func TestHasNewline(t *testing.T) {
	if !pathx.HasNewline("a\nb") || !pathx.HasNewline("a\rb") {
		t.Error("a line break was not detected")
	}
	if pathx.HasNewline("a b") {
		t.Error("an ordinary space was reported as a line break")
	}
}

// A component that is a file is not the same answer as a component that
// is not there, and the composed tree's paper walk turns on the
// difference.
//
// Files do not merge: a file in the code repository where the workspace
// has a directory covers that whole directory. Reading the code side's
// "not a directory" as "nothing there" sent the walk to the workspace,
// found the directory, and accepted a mount point the real overlay cannot
// reach -- which then failed at mount time, after generation and after
// earlier mounts had been made.
func TestAFileInThePathIsNotAnAbsence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "shadow"), []byte("a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := pathx.StatBeneath(root, []string{"shadow", "inside"})
	if !errors.Is(err, pathx.ErrNotDirectory) {
		t.Fatalf("a file on the way down answered (%v, %v), and it has to say "+
			"the component is not a directory", info.Type, err)
	}
	if !strings.Contains(err.Error(), "shadow") {
		t.Errorf("the error does not name the component: %v", err)
	}

	// And a name that is genuinely not there is still an ordinary answer.
	info, err = pathx.StatBeneath(root, []string{"nothing", "inside"})
	if err != nil || info.Exists() {
		t.Errorf("an absent name answered (%v, %v), and absence is not an error",
			info.Type, err)
	}
}
