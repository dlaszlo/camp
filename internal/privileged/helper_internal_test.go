package privileged

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
)

// The one check that stands between root and a stranger's mount, tested
// where every case can be constructed: a synthetic mount table, because
// making a real mount needs privilege and the point of the check is that
// it happens before any syscall that would need it.
//
// The two dangerous mistakes are opposite ones. Refusing too little means
// root unmounting somebody else's mount because camp wrote that path down
// once. Refusing too much means a teardown that will not finish -- which
// is what happened when this only compared identities: after a teardown
// that unmounted everything and died before saying so, every recorded
// path resolved to what was underneath it, and 'camp down' called eleven
// mounts that were already gone somebody else's.
func TestOnlyAMountThatIsNotCampsIsRefused(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("this platform does not report a device and inode")
	}
	its := JobTarget{Path: path, Device: uint64(st.Dev), Inode: st.Ino}
	mounted := []mountinfo.Entry{{Point: path}}

	for _, probe := range []struct {
		name    string
		table   []mountinfo.Entry
		target  JobTarget
		refused bool
	}{
		{
			name:   "nothing is mounted there any more",
			table:  nil,
			target: JobTarget{Path: path, Device: 222, Inode: 6295166},
		},
		{
			name:   "the record carries no identity",
			table:  mounted,
			target: JobTarget{Path: path},
		},
		{
			name:   "the mount is camp's own",
			table:  mounted,
			target: its,
		},
		{
			name:   "the path cannot be looked at",
			table:  []mountinfo.Entry{{Point: filepath.Join(directory, "gone")}},
			target: JobTarget{Path: filepath.Join(directory, "gone"), Device: 1, Inode: 1},
		},
		{
			name:    "something else is mounted there",
			table:   mounted,
			target:  JobTarget{Path: path, Device: 222, Inode: 6295166},
			refused: true,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			mismatch := standsThere(probe.table, probe.target)
			if probe.refused && mismatch == "" {
				t.Error("a mount that is not camp's was let through")
			}
			if !probe.refused && mismatch != "" {
				t.Errorf("this must not be refused: %s", mismatch)
			}
			if probe.refused && !strings.Contains(mismatch, probe.target.Path) {
				t.Errorf("the refusal does not name the path: %q", mismatch)
			}
		})
	}
}
