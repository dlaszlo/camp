package mountx_test

import (
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
)

// The locked flags a remount has to replicate, rendered for the person
// who reads the failure.
//
// A read-only remount inside a user namespace is refused unless it
// carries the source mount's locked flags, so the message that reports
// that refusal has to say which ones camp asked for -- it is the whole
// diagnosis. The strictatime rule is the part worth guarding: a mount
// with no atime option at all is strictatime, and leaving it unsaid reads
// as a request to change it.
func TestTheLockedFlagsAreNamedAsTheMountCarriesThem(t *testing.T) {
	for _, probe := range []struct {
		name    string
		options []string
		want    string
	}{
		{
			name:    "a tmpfs as systemd mounts one",
			options: []string{"rw", "nosuid", "nodev", "relatime"},
			want:    "nosuid,nodev,relatime",
		},
		{
			name:    "nothing about atime, which means strictatime",
			options: []string{"rw", "nosuid"},
			want:    "nosuid,strictatime",
		},
		{
			name:    "an ordinary filesystem with nothing locked",
			options: []string{"rw"},
			want:    "strictatime",
		},
		{
			name:    "an option that is not one of the locked ones",
			options: []string{"rw", "noexec", "someoption"},
			want:    "noexec,strictatime",
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			got := mountx.DescribeFlags(
				mountx.LockedFlags(mountinfo.Entry{Options: probe.options}))
			if got != probe.want {
				t.Errorf("the flags read %q and the mount carries %v; expected %q",
					got, probe.options, probe.want)
			}
		})
	}
	if mountx.DescribeFlags(0) != "none" {
		t.Errorf("an empty flag set has to read as something: %q",
			mountx.DescribeFlags(0))
	}
}
