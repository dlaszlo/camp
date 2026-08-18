package fsx_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/fsx"
)

// The capability probe builds a real tree, and it must not build it in a
// repository. os.MkdirTemp honours $TMPDIR, so a person who has pointed
// $TMPDIR at a repository would have the probe write lower/upper/work/merged
// into it -- a write camp's first invariant forbids, however briefly, and
// one a crash makes durable. Scratch chooses its base itself and never from
// $TMPDIR.
func TestScratchIsNotChosenByAmbientTmpdir(t *testing.T) {
	// A repository a careless or hostile $TMPDIR points at.
	repository := t.TempDir()
	t.Setenv("TMPDIR", repository)
	before := names(t, repository)

	area, cleanup, err := fsx.Scratch("camp-probe-")
	if err != nil {
		t.Fatalf("Scratch: %v", err)
	}

	if strings.HasPrefix(area.Root(), repository) {
		t.Errorf("Scratch built its tree inside the repository $TMPDIR named: %s",
			area.Root())
	}
	if err := cleanup(); err != nil {
		t.Errorf("the scratch cleanup failed: %v", err)
	}
	if after := names(t, repository); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("Scratch wrote into the repository $TMPDIR named:\nbefore %v\nafter %v",
			before, after)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	sort.Strings(out)
	return out
}
