// Package logs is camp's own record of what it said.
//
// Every line a command writes to stderr is written here as well, with a
// timestamp, and always -- not on request. A log you have to remember to
// switch on is missing on exactly the run that surprised somebody, which
// is the only run anybody ever wants it for.
//
// It is the same lines, not a second format and not a second severity
// system. What is worth having a week later is what the person at the
// terminal saw, in the words they saw it in, with the time each line was
// said.
//
// **No logging framework.** What zap, logrus or zerolog buy -- runtime
// level filtering, several appenders, pattern layouts, structured fields,
// asynchronous buffering -- is for a long-running process emitting events
// somebody greps later. camp is a short command a person watches, with one
// sink and prose composed for a reader. Adopting one would mean writing a
// custom handler to reproduce these sentences, and gaining levels nobody
// sets. If several sinks and levels are ever genuinely wanted, log/slog is
// in the standard library and needs no dependency. Rotation is the file
// below either way.
package logs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/pathx"
)

// DirName is the directory under .camp, and Name the file camp writes.
const (
	DirName = "logs"
	Name    = "camp.log"
)

// Limit is the size at which the current file is rotated, and Kept how
// many rotated files survive -- camp.log.1 to camp.log.3.
//
// A run says a dozen lines, so a megabyte is thousands of runs and four
// files are the history of a working month. Rotation is by size because
// nothing here knows or should know how long a machine has been running.
const (
	Limit = 1 << 20
	Kept  = 3
)

// Stamp is RFC 3339 with milliseconds and the machine's own offset:
// 2026-08-16T20:09:33.412+02:00.
//
// Local as asked for, and unambiguous to a parser because the offset is in
// every line. The one caveat worth knowing: across a daylight-saving
// change local times repeat for an hour, so a plain lexicographic sort of
// the file is briefly out of order even though no line is ambiguous.
const Stamp = "2006-01-02T15:04:05.000-07:00"

// Trouble is how the log says the one thing that can change about it
// while a command runs, and it is said once.
//
// The sink a command narrates through implements it: what is said goes
// to the terminal and into this file like any other line, because a run
// whose log stopped rotating is a fact about that run and belongs in the
// record of it.
type Trouble interface {
	Trouble(format string, args ...any)
}

// Log is the file behind a command's stderr.
type Log struct {
	area fsx.Area
	// path is the file itself, for a message about it.
	path string
	// directory is held open for the whole run and locked around every
	// write. The lock is on the directory rather than on the file because
	// the file is what rotation renames: two processes holding a lock on
	// the same name could be holding two different files a moment later.
	//
	// It is opened through the same Area the file is, so both descriptors
	// come out of one resolution from one pinned root: a lock taken on a
	// directory that is not the one the file was created in serializes
	// nothing.
	directory *os.File
	mutex     sync.Mutex
	file      *os.File

	// say is where the sentence below goes. Nil for a caller that opened
	// a log outside a command's narration, which then hears nothing --
	// there is nobody to hear it.
	say Trouble
	// locking is whether this filesystem has working interprocess locks.
	// It starts true and goes false the first time flock refuses, taking
	// rotation with it, and what refused is kept for the sentence that
	// says so.
	locking bool
	refused error
}

// flock is how the directory lock is taken and given back.
//
// A variable, and the only one in this package, because the state the
// code below it is written against -- a filesystem whose flock answers
// EOPNOTSUPP or ENOSYS -- is one no test can arrange on purpose: a test
// makes directories on whatever filesystem it is run on, and cannot make
// that filesystem stop locking. It exists for the test, the way
// doctorTable does in internal/cli and afterTypeCheck in internal/fsx,
// and a running camp never replaces it.
var flock = unix.Flock

// Path is where the log lives for an environment, for a message about it.
func Path(root pathx.Root) string {
	return filepath.Join(root.Name(), config.Dir, DirName, Name)
}

// Open prepares the log for one environment.
//
// A failure to open it is returned rather than swallowed, and the caller
// decides -- which is always to carry on without a log and say so once. A
// command that cannot write its own record still has work to do.
//
// The narration is passed in rather than reached for, because the one
// thing this file can have to say about itself has to reach the person
// at the terminal, and this package knows nothing about terminals.
func Open(root pathx.Root, say Trouble) (*Log, error) {
	area := fsx.Logs(root)
	if err := area.Ensure(0o755); err != nil {
		return nil, err
	}
	// Through the area, not through its name: the name would be resolved
	// again, and the whole point of locking the directory is that it is
	// the directory this file is written in.
	directory, err := area.OpenDir()
	if err != nil {
		return nil, err
	}
	file, err := area.Append(Name, 0o644)
	if err != nil {
		directory.Close()
		return nil, err
	}
	return &Log{area: area, path: Path(root), directory: directory, file: file,
		say: say, locking: true}, nil
}

// Write records whole lines, one timestamp each.
//
// It is given complete lines by the sink that feeds it, so nothing here
// has to reassemble a sentence. A blank line stays blank: it separates
// the paragraphs of a refusal, and a timestamp on its own would be a line
// saying nothing.
func (l *Log) Write(p []byte) (int, error) {
	written, lost, err := l.write(p)
	// Said here, after the write and outside the mutex, because saying it
	// goes through the sink and the sink writes straight back into this
	// log: a sentence composed under the mutex would deadlock on the line
	// it is itself producing.
	if lost {
		l.unserialized()
	}
	return written, err
}

// write is one line's whole journey to the file, under this process's
// mutex and -- where the filesystem has one -- the directory lock every
// camp process writing this log takes.
//
// It answers whether this call is the one that found there is no
// interprocess locking here, so that the sentence about it is said
// once, and said outside the mutex.
func (l *Log) write(p []byte) (written int, lost bool, err error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	stamped := stamp(time.Now(), p)
	held, refused := l.hold()
	lost = refused
	defer func() {
		if l.drop(held) {
			lost = true
		}
	}()

	if err := l.reopenIfRotated(); err != nil {
		return 0, lost, err
	}
	// Only under the lock. Rotation is several renames of one set of
	// names, and two writers performing them at once overwrite or skip
	// the generations they are moving -- so without the lock this run
	// appends and lets the file grow. A line lost is worse than a
	// rotation missed, and Write says afterwards which one this run got.
	if held {
		if err := l.rotateIfFull(len(stamped)); err != nil {
			return 0, lost, err
		}
	}
	if _, err := l.file.Write(stamped); err != nil {
		return 0, lost, fmt.Errorf("writing %s: %w", l.path, err)
	}
	return len(p), lost, nil
}

// unserialized says, once, that this run appends without rotating.
func (l *Log) unserialized() {
	if l.say == nil {
		return
	}
	l.say.Trouble("camp's log %s cannot be locked on this filesystem (%v), "+
		"so this run appends to it and does not rotate it. Two processes "+
		"write one log -- a session's launcher and the init it re-executes "+
		"-- and rotation is several renames of one set of names: performed "+
		"at once by both, they overwrite or skip the generations they are "+
		"moving. Every line is still kept, and nothing else about this run "+
		"changes.", l.path, l.refused)
}

// Close releases the file and the directory.
func (l *Log) Close() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.directory != nil {
		l.directory.Close()
	}
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

// stamp puts the time in front of every line of a block.
func stamp(now time.Time, p []byte) []byte {
	prefix := now.Format(Stamp) + " "
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(p, []byte("\n")) {
		body := bytes.TrimSuffix(line, []byte("\n"))
		if len(body) == 0 && len(line) == 0 {
			continue // the empty tail SplitAfter leaves behind
		}
		if len(body) > 0 {
			out.WriteString(prefix)
			out.Write(body)
		}
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// hold takes the lock across processes, and says both whether it got it
// and whether this call is the one that found there is none.
//
// A failure to lock is not a reason to lose the line -- the append still
// lands whole, because the kernel places an appending write at the end
// of the file in one operation whoever else is writing. It is a reason
// to stop rotating: rotation is a sequence of renames over one set of
// names, and this lock is the only thing that makes two processes
// perform it one at a time.
//
// EINTR is not that failure. A signal arriving while this process waits
// for the lock says nothing about the filesystem, so it waits again.
func (l *Log) hold() (held, refused bool) {
	if l.directory == nil || !l.locking {
		return false, false
	}
	for {
		err := flock(int(l.directory.Fd()), unix.LOCK_EX)
		if err == nil {
			return true, false
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		l.locking, l.refused = false, err
		return false, true
	}
}

// drop gives the lock back, and answers the same question hold does.
//
// An unlock that fails is the same finding as a lock that fails, and it
// is worse to leave standing: this process would hold the directory for
// the rest of its life and every other camp process on this log would
// wait for it. So rotation goes off here too, and the one sentence about
// it covers both directions.
func (l *Log) drop(held bool) bool {
	if !held || l.directory == nil {
		return false
	}
	if err := flock(int(l.directory.Fd()), unix.LOCK_UN); err != nil {
		l.locking, l.refused = false, err
		return true
	}
	return false
}

// reopenIfRotated notices that somebody else rotated the file under us.
//
// Two processes write this log -- a session's launcher and the init it
// re-executes -- and the one that does not rotate would otherwise keep
// writing into camp.log.1 for the rest of the session, where nobody looks
// for the current run.
func (l *Log) reopenIfRotated() error {
	current, err := l.area.Path(Name)
	if err != nil {
		return err
	}
	onDisk, err := os.Stat(current)
	ours, statErr := l.file.Stat()
	if err == nil && statErr == nil && os.SameFile(onDisk, ours) {
		return nil
	}
	return l.reopen()
}

func (l *Log) reopen() error {
	file, err := l.area.Append(Name, 0o644)
	if err != nil {
		return err
	}
	l.file.Close()
	l.file = file
	return nil
}

// rotateIfFull moves the current file aside before it grows past the
// limit, so that a line is never split across two files.
func (l *Log) rotateIfFull(incoming int) error {
	info, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("looking at the log: %w", err)
	}
	if info.Size()+int64(incoming) <= Limit {
		return nil
	}
	if err := l.area.Remove(rotated(Kept)); err != nil {
		return err
	}
	for number := Kept - 1; number >= 1; number-- {
		// The older generations are optional, and this is the one place that
		// knows it: the first rotation of a new log has none of them, and a
		// name that has never been written is not a rename that failed. The
		// rename itself no longer suppresses that for everybody, because the
		// same suppression hid the current file below going missing, which is
		// not optional at all -- it was reopened and stat'd a few lines ago,
		// under the lock this rotation holds.
		if err := l.area.Rename(rotated(number), rotated(number+1)); err != nil &&
			!errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	if err := l.area.Rename(Name, rotated(1)); err != nil {
		return err
	}
	return l.reopen()
}

func rotated(number int) string { return fmt.Sprintf("%s.%d", Name, number) }
