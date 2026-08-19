package probe_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The instruments measure camp from outside, and this is the guard on it.
//
// They live in camp's own repository so that what measures camp arrives
// with camp -- but they are a module of their own that imports nothing of
// camp's, and that is the part which matters. An instrument built out of
// the parsing it is testing agrees with it by construction: it would read
// the mount table the way camp reads it, unescape the way camp unescapes,
// and call a composition clean for exactly the reasons camp called it
// clean. Agreement is what is being tested, so it has to come from two
// separate readings of the same machine.
//
// go.mod already says it -- there is no require for camp -- and this says
// it where somebody adding an import would see it fail.
func TestTheInstrumentsShareNoCodeWithWhatTheyMeasure(t *testing.T) {
	root := ".."
	for range 3 {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		root = filepath.Join(root, "..")
	}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.HasSuffix(path, "outside_test.go") {
			return nil // it names the import in order to look for it
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "github.com/dlaszlo/camp/internal") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the instruments: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("an instrument imports the code it measures:\n  %s\n\n"+
			"These read the kernel's mount table and the trees on disk with "+
			"their own eyes on purpose. One built out of camp's own parsing "+
			"would agree with camp by construction, and agreement is what is "+
			"being tested.", strings.Join(offenders, "\n  "))
	}
}
