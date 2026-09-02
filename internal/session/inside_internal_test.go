package session

import (
	"os"
	"path/filepath"
	"testing"
)

// What pid 1's command line has to say before camp concludes it is inside
// a session of this configuration: camp's init argument, and a
// configuration path that names the same file -- by what it resolves to,
// not by its spelling.
func TestOnlyCampsOwnInitForTheSameConfigurationMeansInside(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".camp", "config.yml")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("env: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other.yml")
	if err := os.WriteFile(other, []byte("env: y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	init := func(args ...string) []byte {
		var line []byte
		for _, arg := range args {
			line = append(line, arg...)
			line = append(line, 0)
		}
		return line
	}
	campInit := func(config string) []byte {
		return init("/usr/bin/camp", InitArg, config, "1000", "1000", "--")
	}

	// camp's init running this configuration: inside.
	if given, state := initOf(campInit(source), source); state != sameComposition || given != source {
		t.Errorf("camp's init running this configuration: state %v, given %q", state, given)
	}
	// The same file under another spelling still resolves to it: inside.
	spelled := filepath.Join(root, "alias", ".camp", "config.yml")
	if given, state := initOf(campInit(spelled), source); state != sameComposition || given != spelled {
		t.Errorf("the same file under another spelling: state %v, given %q", state, given)
	}
	// A different configuration camp can resolve: not inside, and allowed.
	if _, state := initOf(campInit(other), source); state != notInside {
		t.Errorf("an init running another configuration was taken for this one's: %v", state)
	}
	// The machine's own init: not inside.
	if _, state := initOf(init("/sbin/init", "splash"), source); state != notInside {
		t.Errorf("the machine's own init was taken for a camp session: %v", state)
	}
	// camp's init naming a path that resolves to nothing -- the renamed
	// environment case -- cannot be compared, so it is refused, not passed.
	if _, state := initOf(campInit(filepath.Join(root, "gone.yml")), source); state != unresolvedComposition {
		t.Errorf("an init whose configuration resolves to nothing was not treated "+
			"as uncertain: %v", state)
	}
	// A source camp cannot resolve is uncertain for the same reason.
	if _, state := initOf(campInit(source), filepath.Join(root, "gone.yml")); state != unresolvedComposition {
		t.Errorf("an unresolvable source was not treated as uncertain: %v", state)
	}
	// An empty command line: not inside.
	if _, state := initOf(nil, source); state != notInside {
		t.Errorf("an empty command line was matched: %v", state)
	}
}
