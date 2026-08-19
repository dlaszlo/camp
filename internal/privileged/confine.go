package privileged

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
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
// and `/etc` -- and that base is opened once, here, and held: the
// ownership question is asked of the descriptor, and every operand is
// resolved from it, so the directory that answered and the directory
// root acts in cannot come apart. And the one directory it removes and
// gives away has to carry camp's own marker, read through that same
// descriptor, so it can only ever be a directory camp made.

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

// confine refuses a job whose operands are not this composition's, and
// returns the one root everything it goes on to do is resolved beneath.
//
// The root is opened here and nowhere else. Every later step -- the
// precheck, each mount's resolution, the reopen after a bind, both ends
// of the move, the teardown's identity check, the work directory -- starts
// at that descriptor, so the base is resolved exactly once for the whole
// invocation. It used to be a string that every one of those steps
// resolved again, which meant the ownership test decided about one
// directory and the mounts happened beneath whatever the name pointed at
// by then: the invoking user owns the environment root and normally its
// parent, so renaming it away and leaving a symlink at the old name
// pointed root at another tree entirely.
//
// The caller closes it when the job is done.
func (j Job) confine() (pathx.Root, error) {
	root, err := ownedBase(j.Base)
	if err != nil {
		return pathx.Root{}, err
	}
	if err := j.addressed(root); err != nil {
		root.Close()
		return pathx.Root{}, err
	}
	return root, nil
}

// addressed refuses anything the job names that is not addressable inside
// the root confine opened.
func (j Job) addressed(root pathx.Root) error {
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

	if err := j.checkable(); err != nil {
		return err
	}

	for _, target := range j.Targets {
		if _, err := componentsBeneath(root, target.Path); err != nil {
			return err
		}
	}
	return nil
}

// checkable refuses a mount job in which anything would be mounted
// without being compared against what the front end looked at.
//
// Here rather than at the mount itself, so that it is answered before the
// helper's first syscall: a job that cannot be checked is refused while
// the machine is still untouched, and the tests that exercise these
// refusals can run as an ordinary user.
//
// The one operand that may arrive without an identity is a mount point
// inside the staging tree that the job says did not exist when it was
// built -- which is the ordinary case, because most of them are supplied
// by an earlier mount of this same sequence. It has to say so: the field
// is what separates "it was not there" from "camp could not look at it",
// and only the first of those is a mount point anybody checked.
//
// This is also where a hand-written job is stopped from omitting an
// assertion an honest front end would have made. Everything the front end
// fills in is required here, so leaving a field out cannot buy a job an
// unchecked operand.
func (j Job) checkable() error {
	if j.Action != ActionMount {
		return nil
	}
	for _, operation := range j.Mounts {
		if operation.TargetIdent == "" {
			if !insideStaging(j, operation.TargetParts) {
				return refuse("helper-operand-unchecked",
					"the job gives no identity for the mount point %s, and it is not "+
						"inside the staging tree.\nEvery operand this helper mounts is "+
						"compared against what the front end looked at. A mount point "+
						"with nothing to compare against is one nobody checked.",
					operation.Target)
			}
			// Inside the staging tree is where a mount point may legitimately
			// not exist yet, and the job has to say that is why. An identity
			// that is merely empty is two different facts written the same
			// way: "it was not there when I looked" and "I could not look at
			// it". The second is a mount point nobody checked, and the field
			// exists so that root can tell them apart.
			if !operation.TargetAbsent {
				return refuse("helper-operand-unchecked",
					"the job gives no identity for the mount point %s and does not "+
						"say it was absent when the job was built.\nInside the staging "+
						"tree a mount point supplied by an earlier mount of this same "+
						"sequence has no identity yet, and the job says so. A missing "+
						"identity with nothing saying why is an operand the front end "+
						"could not look at.", operation.Target)
			}
		}
		if operation.Kind != string(plan.Overlay) {
			if len(operation.SourceParts) == 0 {
				continue
			}
			if operation.SourceIdent == "" {
				return refuse("helper-operand-unchecked",
					"the job gives no identity for the mount source %s.\nEvery "+
						"operand this helper mounts is compared against what the front "+
						"end looked at, and a source is always there to be looked at: "+
						"a bind cannot create one.", operation.Source)
			}
			// A bind puts one kind of thing over another, and the kernel
			// refuses to put a directory on a file or a file on a directory.
			// Which of the two this is has to arrive from the front end, so
			// that the helper compares the source it opened against something
			// somebody actually saw rather than against whatever it finds.
			switch pathx.Type(operation.SourceType) {
			case pathx.Dir, pathx.File:
			default:
				return refuse("helper-operand-unchecked",
					"the job says the mount source %s is %q, and camp binds "+
						"directories and regular files.\nThe kind the front end saw "+
						"travels with the job and is compared against the source this "+
						"helper opens. A source with no kind recorded is one nobody "+
						"checked.", operation.Source, operation.SourceType)
			}
			continue
		}
		if len(operation.LowerParts) != len(operation.Lower) {
			return refuse("helper-operand-unchecked",
				"the job names %d lower layers for the composed tree and %d of "+
					"them as components beneath the base.",
				len(operation.Lower), len(operation.LowerParts))
		}
		operands := []struct {
			what     string
			path     string
			parts    []string
			identity string
		}{
			{"upper layer", operation.Upper, operation.UpperParts, operation.UpperIdent},
			{"work directory", operation.Work, operation.WorkParts, operation.WorkIdent},
		}
		for index, parts := range operation.LowerParts {
			operands = append(operands, struct {
				what     string
				path     string
				parts    []string
				identity string
			}{"lower layer", operation.Lower[index], parts, identityAt(operation.LowerIdents, index)})
		}
		for _, operand := range operands {
			if operand.path == "" {
				continue // an overlay with no upper has no work directory either
			}
			if operand.identity == "" || len(operand.parts) == 0 {
				return refuse("helper-operand-unchecked",
					"the job gives no identity for the composed tree's %s (%s).\n"+
						"Every operand this helper mounts is compared against what "+
						"the front end looked at, and this one has nothing to compare "+
						"against. The composed tree decides what the whole "+
						"composition shows and where every write lands.",
					operand.what, operand.path)
			}
		}
	}
	return nil
}

// ownedBase opens the one root everything is addressed beneath, and
// refuses a root the invoking user does not own.
//
// An environment root is the user's own directory -- it holds their
// repositories and camp's own storage, and camp refuses to run at all if
// it cannot write there. `/`, `/etc` and every other interesting target
// belong to root. One ownership test therefore separates every real
// composition from every path worth attacking, without camp having to
// keep a list of paths it dislikes.
//
// The test is made on the descriptor and not on the name. A name looked
// at and then used again is two objects whenever somebody renames it in
// between, and the person who can rename this one is exactly the person
// the helper is acting for. What comes back is the directory that
// answered the ownership question, held open, and every later step starts
// there.
func ownedBase(base string) (pathx.Root, error) {
	if base == "" || !filepath.IsAbs(base) || base != filepath.Clean(base) {
		return pathx.Root{}, refuse("helper-base-invalid",
			"the job's base is %q, and it has to be an absolute, already "+
				"normalised path: it is the root every operand is resolved "+
				"beneath.", base)
	}
	if base == "/" {
		return pathx.Root{}, refuse("helper-base-invalid",
			"the job's base is the root of the filesystem.\nEverything this "+
				"helper does is addressed beneath one environment root, and the "+
				"machine's root is not one.")
	}

	root, err := pathx.OpenRootExactly(base)
	if err != nil {
		return pathx.Root{}, baseUnopenable(base, err)
	}
	if err := yours(root, base); err != nil {
		root.Close()
		return pathx.Root{}, err
	}
	return root, nil
}

// yours refuses a base the invoking user does not own.
//
// One look, at the descriptor that was opened, answering both questions
// the base has to pass. Both from the same stat because they are one
// fact: a name asked twice can be two directories, and what the second
// of them belongs to says nothing about the first. The type is asked
// again here although the open already demanded a directory -- the
// descriptor is what the answer has to be about, and reading it off the
// same stat as the owner costs nothing.
func yours(root pathx.Root, base string) error {
	fd, err := root.Open(nil, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return refuse("helper-base-invalid",
			"the job's base %s could not be looked at: %v.", base, err)
	}
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	unix.Close(fd)
	if err != nil {
		return refuse("helper-base-invalid",
			"the job's base %s could not be looked at: %v.", base, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return refuse("helper-base-invalid",
			"the job's base %s is not a directory.", base)
	}

	uid, _, err := invoker()
	if err != nil {
		return err
	}
	if int(st.Uid) != uid {
		return refuse("helper-base-not-yours",
			"the job's base %s belongs to uid %d, and this helper was invoked by "+
				"uid %d.\nIt acts inside one environment root, which is a directory "+
				"the person running camp owns. A base somebody else owns is not a "+
				"composition of theirs, and root will not be pointed at it.",
			base, st.Uid, uid)
	}
	return nil
}

// baseUnopenable words the refusal for a base that could not be opened
// following nothing.
//
// The look it takes decides nothing: the open has already refused, and
// this only lets the message name what is standing there. A symbolic link
// is worth naming outright, because it is the one answer that means
// somebody acted -- the front end resolves the environment root before it
// builds the job, so a link at that name was put there afterwards.
func baseUnopenable(base string, err error) error {
	if info, look := pathx.StatBeneath(base, nil); look == nil && info.Type == pathx.Symlink {
		return refuse("helper-base-invalid",
			"the job's base %s is a symbolic link, and this helper follows none.\n"+
				"The environment root was already resolved when the job was built, "+
				"so a link at that name appeared after camp looked. Root will not "+
				"resolve it and address a composition's operands beneath whatever "+
				"it points at.", base)
	}
	return refuse("helper-base-invalid",
		"the job's base %s could not be opened as a directory, following "+
			"nothing: %v.", base, err)
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

// componentsBeneath writes a path the job names as the components between
// the pinned root and itself, and refuses one that cannot be written that
// way at all.
//
// One derivation, in one place, used by the containment check and again
// by the teardown. A recorded path is the only thing a teardown has, and
// it is a string: turning it into components here is what lets everything
// after it start at the root's descriptor instead of at that string. Two
// readings of the same path would be two objects the moment the name
// stopped meaning what it meant, which is the whole class this repair is
// about.
func componentsBeneath(root pathx.Root, path string) ([]string, error) {
	if !beneath(path, root.Name()) {
		return nil, refuse("helper-target-outside",
			"the job asks for %s to be unmounted, and that is not inside %s.\n"+
				"This helper unmounts what one camp composition put up, and "+
				"nothing else on the machine. Every path it touches has to lie "+
				"beneath the environment root the job names.", path, root.Name())
	}
	if path == root.Name() {
		return nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, root.Name()+"/"), "/")
	if err := components(parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// campsOwn reports whether a directory carries camp's attribution marker.
//
// The helper removes and gives away exactly one directory -- the overlay's
// leftover work directory, which the kernel creates as root and only root
// can clear. Requiring the marker means that directory can only ever be
// one camp made: the marker is written when the work and storage areas
// are created, and nothing else on the machine has a reason to carry it.
//
// Read through the pinned root, from the same components the removal
// uses, and not from the path the message names. A marker read by name
// would be a marker in whatever tree that name reached at that instant,
// while the removal happened in the tree the root holds -- the check and
// the act would be about two directories, which is the shape of the
// whole class of defect this helper exists to refuse.
func campsOwn(root pathx.Root, parts []string, directory string) error {
	if err := marked(root, parts, directory); err != nil {
		return refuse("helper-not-camps",
			"%s does not carry camp's own marker, so this helper will not remove "+
				"anything in it or change what it belongs to.\n"+
				"The marker is written when camp creates its work and storage "+
				"directories. A directory without one is somebody else's, whatever "+
				"the job says.", directory)
	}
	return nil
}

// markerLimit is as much of a marker as root will read.
//
// The file belongs to the invoking user and this process is root: a
// marker is two short lines, and reading an unbounded amount of somebody
// else's file into memory is not something a privileged process should be
// asked to do to find out whether a directory is camp's.
const markerLimit = 64 << 10

func marked(root pathx.Root, parts []string, directory string) error {
	name := filepath.Join(directory, compose.MarkerName)
	fd, err := root.Open(append(append([]string{}, parts...), compose.MarkerName),
		unix.O_RDONLY)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, markerLimit))
	if err != nil {
		return err
	}
	_, _, err = compose.ParseMarker(name, data)
	return err
}
