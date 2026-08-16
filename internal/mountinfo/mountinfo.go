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
	"fmt"
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
func Read(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the mount table %s: %w", path, err)
	}
	defer file.Close()

	var entries []Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		entry, ok := parse(scanner.Text())
		if ok {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the mount table %s: %w", path, err)
	}
	return entries, nil
}

// parse reads one record.
//
// The layout: id parent major:minor root point options [optional...] -
// fstype source superoptions. The optional fields are what make this
// worth a parser rather than a field index -- there may be none, and the
// separator is the only thing that says where they end.
func parse(line string) (Entry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return Entry{}, false
	}

	separator := -1
	for index := 6; index < len(fields); index++ {
		if fields[index] == "-" {
			separator = index
			break
		}
	}
	if separator == -1 || separator+3 > len(fields) {
		return Entry{}, false
	}

	id, err1 := strconv.Atoi(fields[0])
	parent, err2 := strconv.Atoi(fields[1])
	major, minor, err3 := device(fields[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Entry{}, false
	}

	entry := Entry{
		ID:       id,
		Parent:   parent,
		Major:    major,
		Minor:    minor,
		Root:     Unescape(fields[3]),
		Point:    Unescape(fields[4]),
		Options:  strings.Split(fields[5], ","),
		Optional: fields[6:separator],
		FSType:   Unescape(fields[separator+1]),
		Source:   Unescape(fields[separator+2]),
		Line:     line,
		Super:    map[string]string{},
	}
	if separator+3 < len(fields) {
		entry.SuperRaw = fields[separator+3]
		for key, value := range SplitOptions(entry.SuperRaw) {
			entry.Super[key] = value
		}
	}
	return entry, true
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

// Unescape undoes mountinfo's octal escaping of space, tab, newline and
// backslash.
func Unescape(field string) string {
	return strings.NewReplacer(
		`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`,
	).Replace(field)
}

// At returns every mount whose point is exactly this path. There can be
// several: a mount stacked on another stays listed, and only the last one
// is reachable.
func At(entries []Entry, path string) []Entry {
	var found []Entry
	for _, entry := range entries {
		if entry.Point == path {
			found = append(found, entry)
		}
	}
	return found
}

// Top returns the mount a path would actually resolve to -- the last one
// listed at that point, since a later mount covers an earlier one.
func Top(entries []Entry, path string) (Entry, bool) {
	at := At(entries, path)
	if len(at) == 0 {
		return Entry{}, false
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
		if UnescapeOption(entry.Super["upperdir"]) == upper {
			found = append(found, entry)
		}
	}
	return found
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
