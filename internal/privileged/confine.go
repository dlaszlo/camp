package privileged

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
)

// What this file exists for.
//
// The helper is the only thing sudo wraps, and it was described as narrow
// while being, in fact, general: it took the ids to chown to out of the
// job, accepted any absolute path as something to unmount, and joined a
// caller-supplied base with caller-supplied components before removing
// that tree and giving it away. A job naming `/` and `etc` made root hand
// the whole of /etc to whoever asked.
//
// Who that mattered to is worth stating exactly, because it decides how
// the repair is shaped. Somebody who can already run any sudo command
// gains nothing here -- they are root. What it created was a confused
// deputy: a rule scoped to camp with NOPASSWD, which is the obvious way
// to stop `camp up` reprompting, or simply a sudo timestamp still warm
// from one, and then any code running as the user could reach root
// through this entry point without a person deciding anything. The
// specification names that case: whoever can edit the configuration must
// never gain root through it, and a configured generator runs as the user
// moments before `camp up` elevates.
//
// So the helper stops trusting three things it was handed. The ids come
// from sudo. Every operand has to lie beneath one base that the invoking
// user owns -- which is true of every environment root and false of `/`
// and `/etc`. And the one directory it removes and gives away has to
// carry camp's own marker, so it can only ever be a directory camp made.

// ruled is a refusal with a stable identifier, carried out of the
// containment checks so the helper can report both.
type ruled struct {
	rule    string
	message string
}

func (r ruled) Error() string { return r.message }

func refuse(rule, format string, args ...any) error {
	return ruled{rule: rule, message: fmt.Sprintf(format, args...)}
}

// invoker returns the account sudo was invoked by.
//
// A helper started without sudo has nobody to act for. Refusing then also
// means that running it by hand as root -- from a root shell, from a
// script -- does nothing, which is the right answer for a program whose
// entire input arrives on a pipe from an unprivileged front end.
func invoker() (int, int, error) {
	uid, uidErr := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("SUDO_GID"))
	if uidErr != nil || gidErr != nil {
		return 0, 0, fmt.Errorf("this helper is invoked by 'camp up' and 'camp " +
			"down' through sudo, and nothing in the environment says who invoked " +
			"it.\nIt exists to do one narrow thing on behalf of an unprivileged " +
			"camp, and it will not act on behalf of nobody. Run 'camp up' or " +
			"'camp down'; they wrap it themselves.")
	}
	if uid == 0 {
		return 0, 0, fmt.Errorf("sudo says this was invoked by root.\n" +
			"camp's front end runs unprivileged from start to finish and elevates " +
			"only for this step, so an invoking root is not a case camp has: " +
			"everything it created would belong to root, including the storage " +
			"you have to be able to write.")
	}
	return uid, gid, nil
}

// confine refuses a job whose operands are not this composition's.
func (j Job) confine() error {
	base, err := ownedBase(j.Base)
	if err != nil {
		return err
	}

	for _, parts := range [][]string{j.StagingParts, j.LiveParts, j.WorkParts} {
		if err := components(parts); err != nil {
			return err
		}
	}
	for _, mount := range j.Mounts {
		if err := components(mount.TargetParts); err != nil {
			return err
		}
		if err := components(mount.SourceParts); err != nil {
			return err
		}
	}

	for _, target := range j.Targets {
		if !beneath(target, base) {
			return refuse("helper-target-outside",
				"the job asks for %s to be unmounted, and that is not inside %s.\n"+
					"This helper unmounts what one camp composition put up, and "+
					"nothing else on the machine. Every path it touches has to lie "+
					"beneath the environment root the job names.", target, base)
		}
	}
	return nil
}

// ownedBase resolves the one root everything is addressed beneath, and
// refuses a root the invoking user does not own.
//
// An environment root is the user's own directory -- it holds their
// repositories and camp's own storage, and camp refuses to run at all if
// it cannot write there. `/`, `/etc` and every other interesting target
// belong to root. One ownership test therefore separates every real
// composition from every path worth attacking, without camp having to
// keep a list of paths it dislikes.
func ownedBase(base string) (string, error) {
	if base == "" || !filepath.IsAbs(base) || base != filepath.Clean(base) {
		return "", refuse("helper-base-invalid",
			"the job's base is %q, and it has to be an absolute, already "+
				"normalised path: it is the root every operand is resolved "+
				"beneath.", base)
	}
	if base == "/" {
		return "", refuse("helper-base-invalid",
			"the job's base is the root of the filesystem.\nEverything this "+
				"helper does is addressed beneath one environment root, and the "+
				"machine's root is not one.")
	}

	var st unix.Stat_t
	if err := unix.Lstat(base, &st); err != nil {
		return "", refuse("helper-base-invalid",
			"the job's base %s could not be looked at: %v.", base, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", refuse("helper-base-invalid",
			"the job's base %s is not a directory.", base)
	}

	uid, _, err := invoker()
	if err != nil {
		return "", err
	}
	if int(st.Uid) != uid {
		return "", refuse("helper-base-not-yours",
			"the job's base %s belongs to uid %d, and this helper was invoked by "+
				"uid %d.\nIt acts inside one environment root, which is a directory "+
				"the person running camp owns. A base somebody else owns is not a "+
				"composition of theirs, and root will not be pointed at it.",
			base, st.Uid, uid)
	}
	return base, nil
}

// components refuses a path piece that could climb out of the base.
func components(parts []string) error {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." ||
			strings.ContainsAny(part, "/\x00") {
			return refuse("helper-component-invalid",
				"the job names %q as one component of a path.\n"+
					"A component is one name: never empty, never '.' or '..', never "+
					"containing a separator. Anything else could address something "+
					"outside the composition.", part)
		}
	}
	return nil
}

// beneath reports whether a path is the base or inside it, lexically.
//
// Lexically is enough here because it is a gate rather than the
// resolution: what the helper acts on is opened component by component
// beneath the base, following no symlink. This only refuses a job that
// does not even claim to be about this composition.
func beneath(path, base string) bool {
	if path == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return false
	}
	return path == base || strings.HasPrefix(path, base+"/")
}

// campsOwn reports whether a directory carries camp's attribution marker.
//
// The helper removes and gives away exactly one directory -- the overlay's
// leftover work directory, which the kernel creates as root and only root
// can clear. Requiring the marker means that directory can only ever be
// one camp made: the marker is written when the work and storage areas
// are created, and nothing else on the machine has a reason to carry it.
func campsOwn(directory string) error {
	if _, _, err := compose.ReadMarker(directory); err != nil {
		return refuse("helper-not-camps",
			"%s does not carry camp's own marker, so this helper will not remove "+
				"anything in it or change what it belongs to.\n"+
				"The marker is written when camp creates its work and storage "+
				"directories. A directory without one is somebody else's, whatever "+
				"the job says.", directory)
	}
	return nil
}
