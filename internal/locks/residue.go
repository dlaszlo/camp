package locks

import (
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Residue refuses to start on top of something already mounted.
//
// A session's own mounts cannot be met here: they exist only inside its
// namespace and go with it. So anything already mounted under the
// composed tree's path, or on the workspace's or the code repository's
// own path -- the three places the plan puts mounts -- as seen from the
// process starting a session, was put there by something other than camp
// -- and it is not something to build on: the new mounts would stack on
// it, the verification would find more mounts than the plan has, and the
// rollback list would be wrong from the first moment.
func Residue(table []mountinfo.Entry, live, workspace, upper string) refusal.List {
	var refused refusal.List

	var found []string
	for _, entry := range mountinfo.Under(table, live) {
		found = append(found, fmt.Sprintf("%s (%s)", entry.Point, entry.FSType))
	}
	for _, path := range []string{workspace, upper} {
		for _, entry := range mountinfo.At(table, path) {
			found = append(found, fmt.Sprintf("%s (%s)", entry.Point, entry.FSType))
		}
	}
	if len(found) == 0 {
		return refused
	}

	refused.Add("residue",
		"something is already mounted where this composition would go: %s.\n"+
			"camp did not make it: a session's mounts exist only inside that "+
			"session's namespace and go with it, so what stands here was mounted "+
			"by something else. camp will not stack on top of it -- the new "+
			"mounts would pile onto the old ones, and the verification would be "+
			"comparing the plan against a tree it does not describe.\n"+
			"'camp status' lists what is there. Unmount it the way it was "+
			"mounted, then start again.", strings.Join(found, ", "))
	return refused
}
