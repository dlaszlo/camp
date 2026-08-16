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

	"github.com/dlaszlo/camp/internal/fsx"
)

// SeenSuffix marks a report that has been printed.
const SeenSuffix = ".seen"

// Dir is where reports live for an environment.
func Dir(campDir string) string { return filepath.Join(campDir, "reports") }

// Write leaves one report behind.
//
// Named by the composition and the moment, so several sessions of the
// same composition do not overwrite one another, and so the file itself
// says when it was written.
func Write(campDir, hash string, body string) (string, error) {
	area := fsx.Reports(Dir(campDir))
	if err := area.Ensure(0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d", hash, time.Now().Unix())
	if err := area.Write(name, []byte(body), 0o644); err != nil {
		return "", err
	}
	return filepath.Join(area.Root(), name), nil
}

// Unseen returns the reports nobody has been shown yet, oldest first.
func Unseen(campDir string) []string {
	entries, err := os.ReadDir(Dir(campDir))
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
		paths = append(paths, filepath.Join(Dir(campDir), name))
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
func MarkSeen(path string) error {
	area := fsx.Reports(filepath.Dir(path))
	name := filepath.Base(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := area.Write(name+SeenSuffix, data, 0o644); err != nil {
		return err
	}
	return area.Remove(name)
}

// Seen returns the reports that have been shown, for doctor to list.
func Seen(campDir string) []string {
	entries, err := os.ReadDir(Dir(campDir))
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), SeenSuffix) {
			paths = append(paths, filepath.Join(Dir(campDir), entry.Name()))
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
func Show(campDir string, out func(string)) {
	for _, path := range Unseen(campDir) {
		body, err := Read(path)
		if err != nil {
			continue
		}
		out(fmt.Sprintf("a session that ended left this behind (%s):\n\n%s\n",
			path, strings.TrimRight(body, "\n")))
		_ = MarkSeen(path)
	}
}
