package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/testenv"
)

// The composed tree's directory is the one thing camp makes for the
// reader rather than asking them to.
//
// git cannot record an empty directory, and a placeholder file in this
// one would make camp refuse the composition -- so no clone of an
// environment can bring it, and every fresh checkout used to meet a
// refusal for the one directory camp can safely create itself.
func TestASessionCreatesTheComposedTreesDirectory(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	if err := os.Remove(env.Live); err != nil {
		t.Fatal(err)
	}

	if err := makeLive(cfg, nil); err != nil {
		t.Fatalf("the directory was not created: %v", err)
	}
	info, err := os.Stat(env.Live)
	if err != nil {
		t.Fatalf("nothing is there: %v", err)
	}
	if !info.IsDir() {
		t.Error("what was created is not a directory")
	}
	entries, err := os.ReadDir(env.Live)
	if err != nil || len(entries) != 0 {
		t.Errorf("the composed tree's directory has to be empty: %v %v", entries, err)
	}

	// And it is not created twice: a second call finds it and says nothing.
	if err := makeLive(cfg, nil); err != nil {
		t.Errorf("the second call failed: %v", err)
	}
}

// It refuses to create one inside a repository, and it has to check that
// itself: this runs before the validation that refuses the same thing
// properly, and camp writing into a repository is the invariant the whole
// tool is built around.
func TestNoComposedTreeIsCreatedInsideARepository(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, strings.Replace(env.YAML(),
		"merged: live", "merged: code/live", 1))

	err := makeLive(cfg, nil)
	if err == nil {
		t.Fatal("camp created a directory inside a repository")
	}
	if !strings.Contains(err.Error(), "code") {
		t.Errorf("the refusal does not name the repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Code, "live")); err == nil {
		t.Error("the directory was created anyway")
	}
}
