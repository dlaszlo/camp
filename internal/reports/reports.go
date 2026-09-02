// Package reports carries what a session found, out of a namespace that
// is about to stop existing.
//
// A session has no teardown command and leaves no state record: the
// namespace is the state, and it vanishes with its last process. That is
// its whole strength, and it leaves one hole -- the drift report, the
// worktree repairs, the index scan, everything worth saying at the end of
// a session has nowhere to be said. A detached tmux session's terminal is
// long gone by the time the last window closes, so printing them was not
// enough.
//
// So the session's init writes them to a file before it exits, and the
// next camp command run in that environment prints any unseen report once
// and renames it. A report is **output, not authority**: nothing reads it
// back as state, nothing decides anything from it, and "a session leaves
// no state record" still holds. What it leaves is the message that
// otherwise had no way to be delivered.
package reports

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/pathx"
)

// SeenSuffix marks a report that has been printed.
const SeenSuffix = ".seen"

// attempts bounds the names tried when something already holds the one
// this run wanted -- a session ending at the same nanosecond as another,
// or a report already marked as read under the name this mark wants. A
// hundred is far past the point where a collision means something other
// than chance.
const attempts = 100

// ErrAlreadySeen says another camp command marked this report while this
// one was delivering it.
//
// Returned rather than folded into success, because "the report has been
// marked" and "this command marked it" are two different facts and the
// once-only delivery is built on the second.
var ErrAlreadySeen = errors.New("the report was marked as read by something else")

// Dir is where reports live for an environment, as a path for a message.
func Dir(root pathx.Root) string { return filepath.Join(root.Name(), config.Dir, "reports") }

// Write leaves one report behind.
//
// Named by the composition and the moment, so several sessions of the
// same composition do not overwrite one another, and so the file itself
// says when it was written.
//
// The moment is to the nanosecond, and the name appears only once the
// whole body is written and synced. Seconds were not enough: two sessions
// of one composition ending in the same second produced one name, and the
// second report replaced the first -- a report about a session nobody
// would ever see, lost to make room for another. Claiming the name first
// with an empty file was not enough either: between the claim and the
// body the final name existed as an unseen report of nothing, which a
// camp command running at that moment would print and mark, and which a
// failed write left there for good. The name and the body now arrive
// together, so a report that exists is a report with something in it.
func Write(root pathx.Root, hash string, body string) (string, error) {
	area := fsx.Reports(root)
	if err := area.Ensure(0o755); err != nil {
		return "", err
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		name := fmt.Sprintf("%s-%d", hash, time.Now().UnixNano())
		err := area.WriteNew(name, []byte(body), 0o644)
		switch {
		case err == nil:
			return filepath.Join(area.Root(), name), nil
		case errors.Is(err, fsx.ErrExists):
			last = err
		default:
			return "", err
		}
	}
	if last == nil {
		last = fmt.Errorf("every name tried was already taken")
	}
	return "", fmt.Errorf("naming a report for %s: %w", hash, last)
}

// Unseen returns the reports nobody has been shown yet, oldest first.
//
// A name beginning with a dot is not a report. The atomic write builds
// its bytes in a temporary in this very directory, named that way, and a
// session killed mid-write leaves one behind: listing it as a report
// meant printing a fragment of one and marking the fragment as read.
func Unseen(root pathx.Root) []string {
	entries, err := os.ReadDir(Dir(root))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || hidden(entry.Name()) ||
			strings.HasSuffix(entry.Name(), SeenSuffix) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(Dir(root), name))
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
// The environment's own root is passed rather than derived from the path,
// because the write has to be addressed from a directory camp holds
// open: a report is named by whatever listed the directory, and camp does
// not write to a place it worked out from a string it was handed.
//
// The rename refuses to replace anything, and the name it lands on is
// decided by that refusal rather than by a look beforehand: a free name
// found by an Lstat is free until the rename, which is a window in which
// the .seen file somebody kept gets overwritten. A source that is gone by
// then is ErrAlreadySeen, because the once-only delivery this supports
// needs to know it lost rather than to be told it won.
func MarkSeen(root pathx.Root, path string) error {
	area := fsx.Reports(root)
	name := filepath.Base(path)

	// One rename in one directory, rather than a copy and a removal. The
	// copy could land on a .seen file of the same name -- replacing a
	// report somebody kept -- and a crash between the two left the report
	// to be delivered a second time.
	//
	// The counter goes before the suffix: the suffix is what says the
	// report has been read, and a counter after it would hide the file
	// from the listing that looks for it.
	for attempt := 0; attempt < attempts; attempt++ {
		marked := name + SeenSuffix
		if attempt > 0 {
			marked = fmt.Sprintf("%s.%d%s", name, attempt, SeenSuffix)
		}
		err := area.RenameNew(name, marked)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, fsx.ErrExists):
			continue
		case errors.Is(err, unix.ENOENT):
			return fmt.Errorf("marking %s as read: %w", path, ErrAlreadySeen)
		default:
			return fmt.Errorf("marking %s as read: %w", path, err)
		}
	}
	return fmt.Errorf("marking %s as read: no free name beside it", path)
}

// Seen returns the reports that have been shown, for doctor to list.
func Seen(root pathx.Root) []string {
	entries, err := os.ReadDir(Dir(root))
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if hidden(entry.Name()) {
			continue
		}
		if strings.HasSuffix(entry.Name(), SeenSuffix) {
			paths = append(paths, filepath.Join(Dir(root), entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths
}

// hidden says the name belongs to the machinery rather than to a session.
// Everything camp puts in this directory that is not a report is named
// with a leading dot.
func hidden(name string) bool { return strings.HasPrefix(name, ".") }

// afterRead runs between a report being read and it being printed.
//
// It does nothing in a running camp. It is here so a test can start a
// second camp command in precisely the window where two of them would
// otherwise both deliver one report -- the way doctorTable in internal/cli
// exists for a state nobody can arrange on purpose.
var afterRead = func(path string) {}

// Show prints every unseen report once and marks it.
//
// Called at the start of every command that resolves a composition, so a
// session's findings reach whoever comes back to the environment next --
// which, for a detached session, is the only moment there is anybody to
// tell.
//
// The whole delivery -- listing, reading, printing, marking -- happens
// under an exclusive lock on the reports directory itself. Listing and
// reading before marking is what let two camp commands started at once
// both print one report, and the mark is not a thing that can be undone
// afterwards. The lock is on the directory's inode rather than on a file
// beside it, because that inode is the one thing here no rename moves,
// and because the kernel drops it when the holder dies: a command killed
// mid-delivery leaves the report unmarked and the next command delivers
// it, which is the failure this is allowed to have.
func Show(root pathx.Root, out func(string)) {
	// Asked before the lock, and again under it. Most commands run in an
	// environment where no session has left anything, and opening and
	// locking a directory to find that out would be work at the start of
	// every command -- and, before the first report is ever written, a
	// directory that is not there.
	if len(Unseen(root)) == 0 {
		return
	}
	release := hold(fsx.Reports(root))
	defer release()

	for _, path := range Unseen(root) {
		body, err := Read(path)
		if err != nil {
			// Said rather than skipped: this file is the whole of what a
			// session that has already ended found, and it is delivered once.
			// A silent skip is that finding lost.
			out(fmt.Sprintf("a session left a report at %s and it could not be "+
				"read: %v", path, err))
			continue
		}
		afterRead(path)
		out(fmt.Sprintf("a session that ended left this behind (%s):\n\n%s\n",
			path, strings.TrimRight(body, "\n")))
		switch err := MarkSeen(root, path); {
		case err == nil:
		case errors.Is(err, ErrAlreadySeen):
			// Reachable only where the lock above did not hold, and nothing is
			// lost when it happens: the report was delivered, and it is
			// marked. A line about the race would be a complaint about a
			// duplicate the reader is already looking at.
		default:
			out(fmt.Sprintf("that report could not be marked as read (%v), so "+
				"the next camp command in this environment will print it again",
				err))
		}
	}
}

// hold takes the reports directory exclusively for one delivery.
//
// A failure to lock is not a reason to withhold a report. This file is
// the whole of what a session that has already ended found, and its
// terminal is gone; delivering it twice is a nuisance, and never
// delivering it is the finding lost. So a directory that cannot be opened
// or locked -- a filesystem without working flock semantics is the only
// way that happens once the directory exists -- leaves the delivery to
// the rename in MarkSeen, which still refuses to mark a report twice.
func hold(area fsx.Area) func() {
	directory, err := area.OpenDir()
	if err != nil {
		return func() {}
	}
	if !waitForLock(int(directory.Fd())) {
		directory.Close()
		return func() {}
	}
	return func() {
		_ = unix.Flock(int(directory.Fd()), unix.LOCK_UN)
		directory.Close()
	}
}

// lockWait is how long a delivery waits for the one before it.
//
// A whole delivery is a listing, a read, a write to a terminal and a
// rename, so the holder is gone in the time a terminal takes -- unless
// its terminal is not reading, and then it is not gone at all. Waiting
// without a bound would put that process in front of every later camp
// command in this environment, at the point in startup where reports are
// shown, which is before the command has done anything the person asked
// for. The bound turns "somebody is wedged" into the case the comment
// above already covers: the lock is not taken, the report is still
// delivered, and MarkSeen still refuses to mark one twice.
const lockWait = 2 * time.Second

// waitForLock takes the lock without blocking, for as long as lockWait
// allows, and reports whether it got it.
func waitForLock(fd int) bool {
	deadline := time.Now().Add(lockWait)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return true
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
