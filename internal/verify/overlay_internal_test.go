package verify

import (
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
)

// composed is the overlay the frame plans, with two lowers in an order
// that means something.
func composed() plan.Mount {
	return plan.Mount{
		Kind:   plan.Overlay,
		Role:   plan.Composed,
		Target: "/env/live",
		Lower:  []string{"/env/workspace", "/env/other"},
		Upper:  "/env/code",
		Work:   "/env/work/overlay",
		Xattr:  "userxattr",
	}
}

// table is what the kernel would report for a mount made from a
// description: the operands it was given, and defaults of its own that
// nobody asked for.
func table(described mountx.OverlayConfig, point string) []mountinfo.Entry {
	entry := mountinfo.Entry{
		Point:  point,
		FSType: described.FSType,
		Super:  map[string]string{"redirect_dir": "on", "uuid": "null"},
	}
	for _, step := range described.Steps {
		if step.Flag() {
			entry.Super[step.Key] = ""
			continue
		}
		if held, found := entry.Super[step.Key]; found {
			entry.Super[step.Key] = held + ":" + step.Path
			continue
		}
		entry.Super[step.Key] = step.Path
	}
	return []mountinfo.Entry{entry}
}

// The pass holds the mounted overlay to the description the mount was
// performed from, and to nothing it composed itself.
//
// This is the half of the repair that lives here. The expectation used
// to be a third rendering of the plan's own fields -- the lowers joined
// with a colon, the upper, the work directory -- written beside the
// mount and the record and compared with neither. A change to what camp
// sends the kernel would have left this pass expecting the old operands
// and refusing every honest composition; a mount made from the
// description passes here because the description is what it is held to.
func TestTheVerificationExpectsExactlyWhatWouldBeMounted(t *testing.T) {
	mount := composed()
	described := mountx.DescribeOverlay(mount)

	if refused := overlayOptions(mount, "/env/live",
		table(described, "/env/live")); !refused.Empty() {
		t.Fatalf("a composed tree mounted from the description was refused:\n%s",
			refused.Error())
	}
}

// And a mount that is not what was described is refused, whichever
// operand differs.
func TestAComposedTreeMadeOfSomethingElseIsRefused(t *testing.T) {
	mount := composed()
	described := mountx.DescribeOverlay(mount)

	for _, probe := range []struct {
		name  string
		spoil func(mountinfo.Entry)
		says  string
	}{
		{"another lower", func(e mountinfo.Entry) {
			e.Super["lowerdir+"] = "/env/somebody-else"
		}, "lowerdir"},
		{"the lowers the other way round", func(e mountinfo.Entry) {
			e.Super["lowerdir+"] = "/env/other:/env/workspace"
		}, "lowerdir"},
		{"another upper", func(e mountinfo.Entry) {
			e.Super["upperdir"] = "/env/somebody-else"
		}, "upperdir"},
		{"the wrong extended-attribute namespace", func(e mountinfo.Entry) {
			delete(e.Super, "userxattr")
			e.Super["nouserxattr"] = ""
		}, "userxattr"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			entries := table(described, "/env/live")
			probe.spoil(entries[0])
			refused := overlayOptions(mount, "/env/live", entries)
			if refused.Empty() {
				t.Fatalf("%s was accepted", probe.name)
			}
			if !strings.Contains(refused.Error(), probe.says) {
				t.Errorf("the refusal does not name %s:\n%s", probe.says,
					refused.Error())
			}
		})
	}
}

// A composed tree the kernel's table has no entry for is refused rather
// than passed for having nothing to compare.
func TestAnOverlayWithNoTableEntryIsRefused(t *testing.T) {
	refused := overlayOptions(composed(), "/env/live", nil)
	if refused.Empty() {
		t.Fatal("a composed tree with no entry in the kernel's table was accepted")
	}
	if !strings.Contains(refused.Error(), "mount table has no entry") {
		t.Errorf("the refusal does not say what is missing:\n%s", refused.Error())
	}
}
