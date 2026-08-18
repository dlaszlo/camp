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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
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
// the file is briefly out of order even though no line is ambiguous. The
// state record stamps UTC and is a different kind of thing.
const Stamp = "2006-01-02T15:04:05.000-07:00"

// Log is the file behind a command's stderr.
type Log struct {
	area fsx.Area
	// path is the file itself, for a message about it.
	path string
	// directory is held open for the whole run and locked around every
	// write. The lock is on the directory rather than on the file because
	// the file is what rotation renames: two processes holding a lock on
	// the same name could be holding two different files a moment later.
	directory *os.File
	mutex     sync.Mutex
	file      *os.File
}

// Path is where the log lives for an environment.
func Path(env string) string {
	return filepath.Join(env, config.Dir, DirName, Name)
}

// Open prepares the log for one environment.
//
// A failure to open it is returned rather than swallowed, and the caller
// decides -- which is always to carry on without a log and say so once. A
// command that cannot write its own record still has work to do.
func Open(env string) (*Log, error) {
	area := fsx.Logs(env)
	if err := area.Ensure(0o755); err != nil {
		return nil, err
	}
	directory, err := os.Open(area.Root())
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", area.Root(), err)
	}
	file, err := area.Append(Name, 0o644)
	if err != nil {
		directory.Close()
		return nil, err
	}
	return &Log{area: area, path: Path(env), directory: directory, file: file}, nil
}

// Write records whole lines, one timestamp each.
//
// It is given complete lines by the sink that feeds it, so nothing here
// has to reassemble a sentence. A blank line stays blank: it separates
// the paragraphs of a refusal, and a timestamp on its own would be a line
// saying nothing.
func (l *Log) Write(p []byte) (int, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	stamped := stamp(time.Now(), p)
	l.hold()
	defer l.drop()

	if err := l.reopenIfRotated(); err != nil {
		return 0, err
	}
	if err := l.rotateIfFull(len(stamped)); err != nil {
		return 0, err
	}
	if _, err := l.file.Write(stamped); err != nil {
		return 0, fmt.Errorf("writing %s: %w", l.path, err)
	}
	return len(p), nil
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

// hold and drop take the lock across processes. A failure to lock is not
// a reason to lose the line: the append still lands whole, and the only
// thing at risk is which side of a rotation it lands on.
func (l *Log) hold() {
	if l.directory != nil {
		_ = unix.Flock(int(l.directory.Fd()), unix.LOCK_EX)
	}
}

func (l *Log) drop() {
	if l.directory != nil {
		_ = unix.Flock(int(l.directory.Fd()), unix.LOCK_UN)
	}
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
		if err := l.area.Rename(rotated(number), rotated(number+1)); err != nil {
			return err
		}
	}
	if err := l.area.Rename(Name, rotated(1)); err != nil {
		return err
	}
	return l.reopen()
}

func rotated(number int) string { return fmt.Sprintf("%s.%d", Name, number) }
