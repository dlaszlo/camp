// Package mountinfo reads what the kernel says is mounted.
//
// It is the cross-check, never the authority. Two measured facts decide
// that: a covered mount stays listed in mountinfo while being unreachable
// by any path, so presence proves nothing about what a process sees; and
// the overlay's super options come back with the kernel's own defaults
// added to whatever was passed, so comparison has to be per option and
// never by string equality. The path -- statvfs, stat, a write attempt --
// is what a process would experience, and that is what camp verifies
// against. This package answers the questions a path cannot: which mounts
// exist under a prefix, what propagation a mount has, and which flags a
// filesystem has locked.
package mountinfo

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Self is the mount table of the calling process.
const Self = "/proc/self/mountinfo"

// Entry is one line of the table.
type Entry struct {
	ID     int
	Parent int
	Major  int
	Minor  int
	// Root is the part of the filesystem that is mounted.
	Root string
	// Point is where it is mounted.
	Point string
	// Options are the per-mount flags: rw or ro, and nosuid, nodev,
	// noexec and the atime family when the mount carries them.
	Options []string
	// Optional carries the propagation fields -- shared:N, master:N,
	// propagate_from:N, unbindable. Empty means private propagation, which
	// is what every camp mount has to be.
	Optional []string
	// FSType is what the kernel calls the filesystem: overlay, ext4,
	// tmpfs.
	FSType string
	// Source is the device or the string the mount was made with.
	Source string
	// Super is the filesystem's own options, split into key and value.
	Super map[string]string
	// SuperRaw is the same options unsplit, for a message.
	SuperRaw string
	// Line is the whole record, so that a report can quote it verbatim.
	Line string
}

// Private reports whether this mount propagates nowhere.
//
// Mounts propagate by default on a systemd machine -- "/" is shared -- and
// propagation once turned eight planned mounts into twelve, four of them
// on the workspace's own path. Every camp mount is made private as it is
// created, and then verified private, because "we asked for it" is not
// evidence.
func (e Entry) Private() bool { return len(e.Optional) == 0 }

// ReadOnly reports what the per-mount options say. It is a cross-check on
// statvfs, which is the authority.
func (e Entry) ReadOnly() bool {
	for _, option := range e.Options {
		if option == "ro" {
			return true
		}
	}
	return false
}

// Has reports whether a per-mount option is set.
func (e Entry) Has(option string) bool {
	for _, candidate := range e.Options {
		if candidate == option {
			return true
		}
	}
	return false
}

// Read parses a mount table.
//
// There is no size limit here, and no number that could be the wrong one.
// A bufio.Scanner needs a maximum token length decided in advance, and
// the 1 MiB this used to carry was nobody's measurement: the mount API
// takes an overlay's lower layers one at a time, so the option string the
// kernel prints back is bounded by how many layers a composition has and
// not by the single page an option string was once written in. A legal
// line refused for its length would be the same refusal this reader
// exists to prevent, so the line is read with bufio.Reader.ReadString,
// which grows to whatever the kernel wrote and stops at nothing else.
//
// A record ends at \n and at no other byte. bufio.ScanLines also drops a
// trailing \r, and \r is a byte the kernel does not escape: an overlay
// whose lowerdir ends in one puts it at the end of the super options,
// which is the last field of the line. Scanning would have taken it off
// and renamed that layer quietly -- the same class of bug as splitting on
// it, one field further along.
func Read(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the mount table %s: %w", path, err)
	}
	defer file.Close()

	var entries []Entry
	reader := bufio.NewReader(file)
	number := 0
	for {
		line, readErr := reader.ReadString('\n')
		// ReadString returns what it read before the error, so the last
		// record of a file that does not end in a newline is still a record.
		if line != "" {
			number++
			record := strings.TrimSuffix(line, "\n")
			entry, err := parse(record)
			if err != nil {
				// The whole snapshot goes, and camp says why. A dropped line is a
				// partial answer presented as a complete one: the line camp could
				// not read may be the mount that completeness, the residue scan,
				// the guard against a second composition or a teardown needed to
				// see, and every one of those decides by what the table does not
				// contain.
				return nil, fmt.Errorf("the mount table %s could not be read: line "+
					"%d does not parse: %w.\n%s\ncamp reads the whole table or none "+
					"of it: a line it cannot read may be the mount a check is looking "+
					"for, and dropping it would turn a partial answer into a complete "+
					"one", path, number, err, record)
			}
			entries = append(entries, entry)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return entries, nil
			}
			return nil, fmt.Errorf("reading the mount table %s: %w", path, readErr)
		}
	}
}

// mandatory is how many fields stand before the optional ones: id,
// parent, major:minor, root, mount point, per-mount options. The search
// for the separator starts past them because those six are positional --
// a "-" standing in one of them is a name, not this record's separator.
const mandatory = 6

// parse reads one record.
//
// The layout: id parent major:minor root point options [optional...] -
// fstype source superoptions. The optional fields are what make this
// worth a parser rather than a field index -- there may be none, and the
// separator is the only thing that says where they end.
//
// The grammar is the kernel's, not Go's. show_mountinfo writes one
// literal 0x20 between fields and escapes exactly " \t\n\\" inside the
// pathnames it prints; carriage return, vertical tab and form feed are
// legal in a filename and go out raw. strings.Fields splits on every byte
// Unicode calls whitespace and coalesces runs of them, so one unrelated
// mount whose name carried a \r turned into an extra field or a shifted
// separator -- and since a record that does not parse stops the whole
// table, that one mount made every camp command on the machine refuse,
// 'camp down' among them, with no way to get out of it. So: split on
// 0x20 and on nothing else, and hand every other byte to the escape
// decoder exactly as the kernel wrote it.
func parse(line string) (Entry, error) {
	if line == "" {
		return Entry{}, errors.New("the record is empty")
	}
	fields := strings.Split(line, " ")
	// Nothing is coalesced. The kernel writes one space between fields, so
	// two in a row are an empty field: a record camp cannot read rather
	// than one to absorb quietly, because absorbing it would mean guessing
	// which mount the line describes.
	for index, field := range fields {
		if field == "" {
			return Entry{}, fmt.Errorf("field %d of %d is empty: the kernel "+
				"separates fields with one space and never with two, so this "+
				"record is not one it wrote", index+1, len(fields))
		}
	}
	if len(fields) < 10 {
		return Entry{}, fmt.Errorf("a record has at least ten fields and this "+
			"one has %d", len(fields))
	}

	separator := -1
	for index := mandatory; index < len(fields); index++ {
		if fields[index] == "-" {
			separator = index
			break
		}
	}
	if separator == -1 {
		return Entry{}, fmt.Errorf("there is no \"-\" separating the optional " +
			"fields from the filesystem type")
	}
	// Exactly three, in both directions.
	//
	// Not fewer: the guard here read separator+3 > len(fields), which is
	// false when two fields remain, so a record whose super options were
	// missing parsed with an empty one -- and an empty super options field
	// is how the overlay comparison reads "no upperdir".
	//
	// Not more either, and that is safe because none of the three can
	// carry a raw separator byte. show_mountinfo prints the filesystem
	// type and the source through mangle(), which is seq_escape over
	// " \t\n\\"; the super options go out through the helpers a
	// filesystem's show_options is written against, and seq_show_option in
	// include/linux/seq_file.h escapes ", \t\n\\" -- the space is in every
	// one of those sets. So a fourth field after "-" is not a source with
	// a space in its name, which arrives as \040. It is a line no reader
	// of this ABI can assign fields to, and refusing it is the same answer
	// as refusing any other malformed record.
	if got := len(fields) - separator - 1; got != 3 {
		return Entry{}, fmt.Errorf("the separator is followed by %d fields and "+
			"a record has exactly three after it: the filesystem type, the "+
			"source and the super options", got)
	}

	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return Entry{}, fmt.Errorf("the mount id %q is not a number", fields[0])
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return Entry{}, fmt.Errorf("the parent id %q is not a number", fields[1])
	}
	major, minor, err := device(fields[2])
	if err != nil {
		return Entry{}, fmt.Errorf("the device field %q: %w", fields[2], err)
	}

	entry := Entry{
		ID:      id,
		Parent:  parent,
		Major:   major,
		Minor:   minor,
		Root:    Unescape(fields[3]),
		Point:   Unescape(fields[4]),
		Options: strings.Split(fields[5], ","),
		// Carried, not vetted. The kernel is free to add a tag, and a parser
		// that refused an unknown one would refuse a legal host on a newer
		// kernel for a field camp reads only as "is there anything here at
		// all" -- which is what Private() asks, and any tag answers it.
		Optional: fields[mandatory:separator],
		FSType:   Unescape(fields[separator+1]),
		Source:   Unescape(fields[separator+2]),
		Line:     line,
		Super:    map[string]string{},
	}
	entry.SuperRaw = fields[separator+3]
	for key, value := range SplitOptions(entry.SuperRaw) {
		entry.Super[key] = value
	}
	return entry, nil
}

func device(field string) (int, int, error) {
	major, minor, found := strings.Cut(field, ":")
	if !found {
		return 0, 0, fmt.Errorf("%q is not major:minor", field)
	}
	majorValue, err := strconv.Atoi(major)
	if err != nil {
		return 0, 0, err
	}
	minorValue, err := strconv.Atoi(minor)
	if err != nil {
		return 0, 0, err
	}
	return majorValue, minorValue, nil
}

// SplitOptions splits a comma-separated option string into key and value.
//
// The escaping matters here: overlayfs escapes a backslash, a colon and a
// comma inside lowerdir=, so splitting naively on "," would tear a path
// containing a comma in half and then compare two halves against a whole.
func SplitOptions(raw string) map[string]string {
	options := map[string]string{}
	for _, option := range splitEscaped(raw, ',') {
		key, value, _ := strings.Cut(option, "=")
		options[key] = value
	}
	return options
}

// splitEscaped splits on a separator that a backslash may escape.
func splitEscaped(raw string, separator byte) []string {
	var parts []string
	var current strings.Builder
	escaped := false
	for index := 0; index < len(raw); index++ {
		character := raw[index]
		switch {
		case escaped:
			current.WriteByte('\\')
			current.WriteByte(character)
			escaped = false
		case character == '\\':
			escaped = true
		case character == separator:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(character)
		}
	}
	if escaped {
		current.WriteByte('\\')
	}
	parts = append(parts, current.String())
	return parts
}

// escaped is the whole vocabulary show_mountinfo can write, built once
// because it is applied to four fields of every line of a table that has
// hundreds.
var escaped = strings.NewReplacer(
	`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`,
)

// Unescape undoes mountinfo's octal escaping of space, tab, newline and
// backslash.
//
// Those four and no others, because the kernel escapes exactly the set
// " \t\n\\". The bytes a filename may carry that Go also calls
// whitespace -- \r, \v, \f -- arrive raw and belong in the name
// unchanged; that is the same fact the field split rests on.
//
// Anything else that looks like an escape is left byte for byte as it
// was read. The kernel cannot have written it, so decoding it would be
// guessing at a name, and a name camp guessed at is one it goes on to
// compare against a real path. There is no ambiguity to resolve: a
// backslash of its own is always \134, so a file genuinely called \040
// arrives as \134040 and comes back out as the four characters it is.
func Unescape(field string) string { return escaped.Replace(field) }

// At returns every mount whose point is exactly this path. There can be
// several: a mount stacked on another stays listed, and only the topmost
// one is reachable. Which one that is, Top answers.
func At(entries []Entry, path string) []Entry {
	var found []Entry
	for _, entry := range entries {
		if entry.Point == path {
			found = append(found, entry)
		}
	}
	return found
}

// Top returns the mount a path would actually resolve to: of the mounts
// stacked at that point, the one nothing else stands on.
//
// The stack is read from the parent field and never from the order of the
// lines. Mounts are listed roughly as they were made, and MS_MOVE keeps a
// mount's identity, so a mount moved onto a point appears *before* the
// mount it now covers -- which is exactly the privileged mode's shape,
// where the live path first gets a self-bind to give the move a private
// parent and then receives the composed tree on top of it. Reading the
// last line as the top one there returns the bind underneath, whose
// filesystem is ext4 and whose overlay options are all empty, and every
// privileged 'camp up' failed its own post-move check with four refusals
// about a mount that was correct.
//
// The parent field says it without ambiguity: a mount stacked on another
// has that other one as its parent, so the top of the stack is the one
// that is no other stacked mount's parent.
func Top(entries []Entry, path string) (Entry, bool) {
	at := At(entries, path)
	if len(at) == 0 {
		return Entry{}, false
	}
	covered := map[int]bool{}
	for _, entry := range at {
		covered[entry.Parent] = true
	}
	for index := len(at) - 1; index >= 0; index-- {
		if !covered[at[index].ID] {
			return at[index], true
		}
	}
	return at[len(at)-1], true
}

// Under returns every mount at or below a prefix.
func Under(entries []Entry, prefix string) []Entry {
	var found []Entry
	for _, entry := range entries {
		if entry.Point == prefix || strings.HasPrefix(entry.Point, prefix+"/") {
			found = append(found, entry)
		}
	}
	return found
}

// Containing returns the mount a path sits on: the longest mount point
// that is a prefix of it.
//
// This is how the flags a filesystem has locked are found, which a
// read-only remount inside a user namespace has to replicate or fail
// EPERM.
func Containing(entries []Entry, path string) (Entry, bool) {
	best := Entry{}
	found := false
	for _, entry := range entries {
		if entry.Point != "/" && entry.Point != path &&
			!strings.HasPrefix(path, entry.Point+"/") {
			continue
		}
		if !found || len(entry.Point) >= len(best.Point) {
			best = entry
			found = true
		}
	}
	return best, found
}

// Overlays returns every overlay mount whose upperdir is this directory.
//
// One upper must serve one overlay: the kernel does not enforce it -- a
// second overlay on the same upper mounts without complaint -- and
// sharing an upper corrupts data. In the privileged mode there is one
// mount table for the machine, so this scan is the steady-state guard,
// and it is why a lazy unmount could never be allowed: a detached mount
// leaves the table while it is still alive, and the table is the only
// guard there is.
func Overlays(entries []Entry, upper string) []Entry {
	var found []Entry
	for _, entry := range entries {
		if entry.FSType != "overlay" {
			continue
		}
		if UpperOf(entry) == upper {
			found = append(found, entry)
		}
	}
	return found
}

// AllOverlays returns every overlay in the table, whatever it is built
// on.
//
// The caller decides which of them are about one upper, because that
// question is about inodes and not about strings: two paths routinely
// name one directory, and a bind alias of a repository is exactly how a
// second composition would name the same upper differently. This package
// parses; it does not stat.
func AllOverlays(entries []Entry) []Entry {
	var found []Entry
	for _, entry := range entries {
		if entry.FSType == "overlay" {
			found = append(found, entry)
		}
	}
	return found
}

// UpperOf returns an overlay's upper directory, whichever spelling the
// kernel used.
//
// "upperdir" from the option string, and the same key from the mount API,
// which reports the layers it was given as descriptors under the key that
// sets them.
func UpperOf(entry Entry) string {
	if value, found := entry.Super["upperdir"]; found {
		return UnescapeOption(value)
	}
	return UnescapeOption(entry.Super["upperdir+"])
}

// UnescapeOption undoes overlayfs's escaping inside a path option.
func UnescapeOption(value string) string {
	var out strings.Builder
	escaped := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			out.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		out.WriteByte(character)
	}
	return out.String()
}
