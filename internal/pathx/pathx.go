// Package pathx is the path language every configuration field is read
// through, and the identity questions that language deliberately does not
// answer by comparing strings.
//
// Two kinds of question are kept apart here, because they need different
// instruments:
//
//   - Shape and containment -- is this field well formed, is this target
//     inside that one -- are answered lexically, over the component list,
//     without touching the filesystem. That is what lets the composition
//     be validated on paper, in the order the mounts will really happen,
//     while nothing is mounted yet.
//   - Identity -- are these two repositories the same directory, is the
//     composed tree inside one of them -- is answered by realpath and by
//     the (device, inode) pair. Two different strings routinely name one
//     directory, and a check that compares strings would let exactly the
//     configuration mistake through that corrupts an upper layer.
//
// Nothing here follows a symlink except the one deliberate resolution of
// the environment root at startup.
package pathx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Rel is a relative path that has passed the grammar: at least one
// component, every component nonempty, none of them "." or "..", no NUL
// anywhere. It is stored split, because every question asked of it is a
// question about components.
type Rel struct {
	parts []string
}

// Components returns the path split, for callers that walk it.
func (r Rel) Components() []string { return append([]string(nil), r.parts...) }

// Empty reports whether this is the zero value -- a repository's own root,
// where a source path may legitimately be absent.
func (r Rel) Empty() bool { return len(r.parts) == 0 }

// String renders the path with separators, as it appeared.
func (r Rel) String() string { return strings.Join(r.parts, "/") }

// First returns the leading component, which is what the overlap gate and
// the exclude compare. It is empty only for the zero value.
func (r Rel) First() string {
	if len(r.parts) == 0 {
		return ""
	}
	return r.parts[0]
}

// Join resolves this path against an absolute base.
func (r Rel) Join(base string) string {
	if len(r.parts) == 0 {
		return base
	}
	return filepath.Join(append([]string{base}, r.parts...)...)
}

// Append extends the path by one already-validated component.
func (r Rel) Append(component string) Rel {
	return Rel{parts: append(append([]string(nil), r.parts...), component)}
}

// Equal reports whether two paths are the same path.
func (r Rel) Equal(other Rel) bool {
	if len(r.parts) != len(other.parts) {
		return false
	}
	for i := range r.parts {
		if r.parts[i] != other.parts[i] {
			return false
		}
	}
	return true
}

// Inside reports whether r lies strictly below other.
//
// Component-wise, never by string prefix: ".claude-local" starts with
// ".claude" as a string and is not inside it, and a check that got that
// wrong would refuse compositions that are perfectly legal.
func (r Rel) Inside(other Rel) bool {
	if len(other.parts) == 0 {
		return len(r.parts) > 0
	}
	if len(r.parts) <= len(other.parts) {
		return false
	}
	for i := range other.parts {
		if r.parts[i] != other.parts[i] {
			return false
		}
	}
	return true
}

// ParseRel reads one relative path field.
//
// field names the configuration key for the message, because a refusal
// that says only what is wrong and not where leaves the reader searching.
func ParseRel(field, raw string) (Rel, error) {
	if raw == "" {
		return Rel{}, fmt.Errorf("%s is empty; it has to name a path relative to "+
			"the directory it is resolved against", field)
	}
	if strings.HasPrefix(raw, "/") {
		return Rel{}, fmt.Errorf("%s is %q, an absolute path. Every path in the "+
			"configuration except env: is relative -- a repository path and merged: "+
			"to env:, a source to its repository's root, a target to the merged "+
			"root. Write it without the leading slash", field, raw)
	}
	if strings.HasPrefix(raw, "~") {
		return Rel{}, fmt.Errorf("%s is %q; only env: may start with ~/. Every "+
			"other path is relative to something camp already knows", field, raw)
	}

	parts := strings.Split(raw, "/")
	for _, part := range parts {
		switch {
		case part == "":
			return Rel{}, fmt.Errorf("%s is %q, which has an empty component -- a "+
				"doubled or trailing slash. Write each component once", field, raw)
		case part == ".":
			return Rel{}, fmt.Errorf("%s is %q, which contains a %q component. camp "+
				"resolves no path relatively: write the path out", field, raw, ".")
		case part == "..":
			return Rel{}, fmt.Errorf("%s is %q, which contains a %q component. A path "+
				"that can climb out of what it is resolved against could name any "+
				"directory on the machine, so camp refuses it outright", field, raw, "..")
		case strings.ContainsRune(part, 0):
			return Rel{}, fmt.Errorf("%s contains a NUL byte, which no filesystem "+
				"name may hold", field)
		}
	}
	return Rel{parts: parts}, nil
}

// ParseComponent reads a field that has to be exactly one path component:
// a repository name, or an allow_overlap entry.
//
// allow_overlap is a set of root names because the gate compares root
// entries, and a deeper path could never match one -- a rule that only
// sometimes bites is worse than a rule that always does.
func ParseComponent(field, raw string) (string, error) {
	rel, err := ParseRel(field, raw)
	if err != nil {
		return "", err
	}
	if len(rel.parts) != 1 {
		return "", fmt.Errorf("%s is %q, which is a path. It has to be a single "+
			"name, without %q", field, raw, "/")
	}
	return rel.parts[0], nil
}

// Identity is a directory as the kernel knows it: the pair that answers
// "is this the same directory as that one" whatever it was called on the
// way in.
type Identity struct {
	Device uint64
	Inode  uint64
}

// String renders the pair for a report.
func (i Identity) String() string { return fmt.Sprintf("%d:%d", i.Device, i.Inode) }

// IdentityOf returns the identity of a path, following no symlink of its
// own accord -- lstat, so a symlink is itself and not its target.
func IdentityOf(path string) (Identity, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return Identity{}, fmt.Errorf("looking at %s: %w", path, err)
	}
	return Identity{Device: uint64(st.Dev), Inode: st.Ino}, nil
}

// Real resolves a path to its canonical form, following symlinks.
//
// Used in exactly one place by design: the environment root, once, at
// startup. Everything below it is then addressed relative to the resolved
// root and opened without following anything.
func Real(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", path, err)
	}
	return filepath.Clean(resolved), nil
}

// ExpandHome turns a leading ~/ into the user's home directory. It is the
// only expansion camp performs anywhere, and it applies only to env:.
func ExpandHome(raw string) (string, error) {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: this process has no home "+
				"directory: %w", raw, err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/")), nil
	}
	return raw, nil
}

// Under reports whether an absolute path is at or below a directory,
// compared component-wise on already-resolved paths.
func Under(path, directory string) bool {
	if path == directory {
		return true
	}
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// HasNewline reports whether a name contains a line break.
//
// Such a name is refused at up: it cannot be written as a gitignore
// pattern at all -- the attempt silently ignores the intended file and
// hides two unrelated names instead -- and it makes every line-oriented
// report ambiguous.
func HasNewline(name string) bool {
	return strings.ContainsAny(name, "\n\r")
}
