package locks

import (
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/refusal"
)

// ScanUpper is the privileged mode's steady-state guard.
//
// The flocks only cover the transitions there: no camp process outlives
// an up, so between up and down nothing is holding anything. What does
// last is the mount itself, and in that mode there is one mount table for
// the whole machine, so an overlay already standing on our upper can be
// seen. That is why a lazy unmount could never be permitted: a detached
// mount leaves the table while it is still alive and still writing, and
// the table is the only guard this mode has.
//
// The namespace mode needs none of this and gets none: another
// namespace's mounts are invisible from here, which is exactly why the
// primary guard had to be something held rather than something seen.
func ScanUpper(table []mountinfo.Entry, upper string) refusal.List {
	var refused refusal.List
	existing := mountinfo.Overlays(table, upper)
	if len(existing) == 0 {
		return refused
	}

	points := make([]string, 0, len(existing))
	for _, entry := range existing {
		points = append(points, entry.Point)
	}
	refused.Add("upper-already-composed",
		"an overlay is already mounted on the code repository %s, at %s.\n"+
			"One upper may serve one overlay: the kernel does not enforce that "+
			"-- a second overlay on the same upper mounts without complaint -- "+
			"and two compositions writing one upper corrupt each other's data.\n"+
			"Take that composition down first ('camp down'), or work in it. If "+
			"it is the residue of a run that did not finish, 'camp status' says "+
			"what is still mounted and 'camp down' removes it.",
		upper, strings.Join(points, ", "))
	return refused
}

// Residue refuses to start on top of what a previous run left behind.
//
// Anything already mounted under the composed tree's path, or on the
// workspace's own path, is either a crash leftover or another
// composition's. Either way it is not something to build on: the new
// mounts would stack on it, the verification would find more mounts than
// the plan has, and the teardown list would be wrong from the first
// moment.
func Residue(table []mountinfo.Entry, live, workspace string) refusal.List {
	var refused refusal.List

	var found []string
	for _, entry := range mountinfo.Under(table, live) {
		found = append(found, fmt.Sprintf("%s (%s)", entry.Point, entry.FSType))
	}
	for _, entry := range mountinfo.At(table, workspace) {
		found = append(found, fmt.Sprintf("%s (%s)", entry.Point, entry.FSType))
	}
	if len(found) == 0 {
		return refused
	}

	refused.Add("residue",
		"something is already mounted where this composition would go: %s.\n"+
			"That is either a run that did not come down cleanly or another "+
			"composition. camp will not stack on top of it: the new mounts would "+
			"pile onto the old ones, and the list camp would have to take down "+
			"afterwards would be wrong from the first moment.\n"+
			"Run 'camp status' to see whose they are, then 'camp down' to remove "+
			"them.", strings.Join(found, ", "))
	return refused
}
