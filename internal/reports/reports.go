// Package reports carries what a namespace session found, out of a
// namespace that is about to stop existing.
//
// The namespace mode has no down and leaves no state record: the
// namespace is the state, and it vanishes with its last process. That is
// its whole strength, and it left one hole -- the drift report, the
// worktree repairs, the index scan, all the things `down` says in the
// other mode had no way to be delivered here. A detached tmux session's
// terminal is long gone by the time the last window closes, so printing
// them was not enough.
//
// So the session's init writes them to a file before it exits, and the
// next camp command run in that environment prints any unseen report once
// and renames it. A report is **output, not authority**: nothing reads it
// back as state, nothing decides anything from it, and "the namespace
// mode leaves no state record" still holds. What it leaves is the message
// the mode otherwise had no way to deliver.
package reports

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
)

// SeenSuffix marks a report that has been printed.
const SeenSuffix = ".seen"

// Dir is where reports live for an environment.
func Dir(env string) string { return filepath.Join(env, config.Dir, "reports") }

// Write leaves one report behind.
//
// Named by the composition and the moment, so several sessions of the
// same composition do not overwrite one another, and so the file itself
// says when it was written.
//
// The moment is to the nanosecond and the name is claimed with O_EXCL
// before anything is written into it. Seconds were not enough: two
// sessions of one composition ending in the same second produced one
// name, and the second report replaced the first -- a report about a
// session nobody would ever see, lost to make room for another.
func Write(env, hash string, body string) (string, error) {
	area := fsx.Reports(env)
	if err := area.Ensure(0o755); err != nil {
		return "", err
	}
	name, err := claim(area, hash)
	if err != nil {
		return "", err
	}
	if err := area.Write(name, []byte(body), 0o644); err != nil {
		return "", err
	}
	return filepath.Join(area.Root(), name), nil
}

// claim reserves a name nothing else holds.
//
// O_EXCL decides it rather than a look beforehand: two sessions ending at
// once would both find the name free and both write it. The nanosecond is
// what makes a second attempt land somewhere else.
func claim(area fsx.Area, prefix string) (string, error) {
	var last error
	for attempt := 0; attempt < 100; attempt++ {
		name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
		_, created, err := area.Touch(name)
		switch {
		case err != nil:
			last = err
		case created:
			return name, nil
		}
	}
	if last == nil {
		last = fmt.Errorf("every name tried was already taken")
	}
	return "", fmt.Errorf("naming a report for %s: %w", prefix, last)
}

// Unseen returns the reports nobody has been shown yet, oldest first.
func Unseen(env string) []string {
	entries, err := os.ReadDir(Dir(env))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), SeenSuffix) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(Dir(env), name))
	}
	return paths
}

// Read returns a report's text.
func Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(data), nil
}

// MarkSeen renames a report so it is printed once and not again.
//
// Renamed rather than removed: it is the record of what a session found,
// and somebody may want to read it a second time. camp simply stops
// putting it in front of them.
//
// The environment is passed rather than derived from the path, because
// the write has to be addressed from a directory camp trusts down: a
// report is named by whatever listed the directory, and camp does not
// write to a place it worked out from a string it was handed.
func MarkSeen(env, path string) error {
	area := fsx.Reports(env)
	name := filepath.Base(path)

	// One rename in one directory, rather than a copy and a removal. The
	// copy could land on a .seen file of the same name -- replacing a
	// report somebody kept -- and a crash between the two left the report
	// to be delivered a second time.
	marked, err := freeName(area, name, SeenSuffix)
	if err != nil {
		return err
	}
	if err := area.Rename(name, marked); err != nil {
		return fmt.Errorf("marking %s as read: %w", path, err)
	}
	return nil
}

// freeName finds a name in the directory that nothing holds yet, keeping
// the suffix at the end -- it is what says the report has been read, and
// a counter after it would hide the file from the listing that looks for
// it.
func freeName(area fsx.Area, base, suffix string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		candidate := base + suffix
		if attempt > 0 {
			candidate = fmt.Sprintf("%s.%d%s", base, attempt, suffix)
		}
		path, err := area.Path(candidate)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for %s%s", base, suffix)
}

// Seen returns the reports that have been shown, for doctor to list.
func Seen(env string) []string {
	entries, err := os.ReadDir(Dir(env))
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), SeenSuffix) {
			paths = append(paths, filepath.Join(Dir(env), entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths
}

// Show prints every unseen report once and marks it.
//
// Called at the start of every command that resolves a composition, so a
// session's findings reach whoever comes back to the environment next --
// which, for a detached session, is the only moment there is anybody to
// tell.
func Show(env string, out func(string)) {
	for _, path := range Unseen(env) {
		body, err := Read(path)
		if err != nil {
			// Said rather than skipped: this file is the whole of what a
			// session that has already ended found, and it is delivered once.
			// A silent skip is that finding lost.
			out(fmt.Sprintf("a session left a report at %s and it could not be "+
				"read: %v", path, err))
			continue
		}
		out(fmt.Sprintf("a session that ended left this behind (%s):\n\n%s\n",
			path, strings.TrimRight(body, "\n")))
		if err := MarkSeen(env, path); err != nil {
			out(fmt.Sprintf("that report could not be marked as read (%v), so "+
				"the next camp command in this environment will print it again",
				err))
		}
	}
}
