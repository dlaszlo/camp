package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/report"
)

// heard records what a log said about itself, the way the sink a command
// narrates through would say it.
type heard struct {
	mutex sync.Mutex
	said  []string
}

func (h *heard) Trouble(format string, args ...any) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.said = append(h.said, fmt.Sprintf(format, args...))
}

// On a filesystem whose flock does nothing, two writers keep every line
// and rotate nothing, and each says so once.
//
// The lock on the log's directory is the only thing that makes the
// rotation -- several renames over one set of names -- happen one
// process at a time. Where there is no lock, both writers were
// performing that sequence at once: a generation renamed twice, or over
// a file the other had just moved into place, and the lines in it gone.
// So the run appends instead, which loses nothing but the size bound,
// and the sentence saying so is in the log and on the terminal.
func TestWithoutInterprocessLocksNothingRotatesAndNothingIsLost(t *testing.T) {
	for _, refusal := range []error{unix.EOPNOTSUPP, unix.ENOSYS} {
		t.Run(refusal.Error(), func(t *testing.T) {
			pinned := refusal
			original := flock
			// The state this is about is one no test can arrange on the
			// filesystem it is run on.
			flock = func(int, int) error { return pinned }
			t.Cleanup(func() { flock = original })

			env, err := pathx.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()

			// Two tagged writers, the way a session has two: the launcher and
			// the init it re-executes.
			said := []*heard{{}, {}}
			var writers []*Log
			for _, listener := range said {
				log, err := Open(env, listener)
				if err != nil {
					t.Fatalf("opening the log: %v", err)
				}
				defer log.Close()
				writers = append(writers, log)
			}

			// Enough for several rotations, so that the generations the lock
			// protects are exactly what is being asked about.
			const lines = 300
			padding := strings.Repeat("x", 4096)
			var running sync.WaitGroup
			for index, log := range writers {
				running.Add(1)
				go func(tag int, log *Log) {
					defer running.Done()
					for number := 0; number < lines; number++ {
						if _, err := log.Write([]byte(fmt.Sprintf(
							"writer-%d-line-%d %s\n", tag, number, padding))); err != nil {
							t.Errorf("writing the log: %v", err)
							return
						}
					}
				}(index, log)
			}
			running.Wait()

			// Every line that was written is still there, and there is one
			// place to find it: nothing was moved aside.
			kept, err := os.ReadFile(Path(env))
			if err != nil {
				t.Fatalf("reading the log: %v", err)
			}
			for tag := range writers {
				for number := 0; number < lines; number++ {
					want := fmt.Sprintf("writer-%d-line-%d ", tag, number)
					if strings.Count(string(kept), want) != 1 {
						t.Fatalf("%q appears %d times in the log, and it was "+
							"written once", want, strings.Count(string(kept), want))
					}
				}
			}
			if _, err := os.Stat(Path(env) + ".1"); err == nil {
				t.Errorf("a generation was rotated with no lock to serialize " +
					"the renames, so what the other writer moved could be moved " +
					"again or moved over")
			}

			// And each writer said it once. Once, because a sentence about
			// the log repeated on every line buries the run's own work.
			for index, listener := range said {
				if len(listener.said) != 1 {
					t.Fatalf("writer %d said %d things about its log and owes "+
						"exactly one:\n%s", index, len(listener.said),
						strings.Join(listener.said, "\n"))
				}
				if !strings.Contains(listener.said[0], "does not rotate") {
					t.Errorf("writer %d did not say what changed about the "+
						"log:\n%s", index, listener.said[0])
				}
			}
		})
	}
}

// A lock that cannot be given back is the same finding as one that
// cannot be taken, and it must not be left standing.
//
// A process holding the directory for the rest of its life is every
// other camp process on that log waiting for it. So the unlock is
// checked too: rotation goes off, and the one sentence covers both
// directions.
func TestAnUnlockThatFailsAlsoStopsTheRotation(t *testing.T) {
	original := flock
	flock = func(_ int, how int) error {
		if how == unix.LOCK_UN {
			return unix.ENOLCK
		}
		return nil
	}
	t.Cleanup(func() { flock = original })

	env, err := pathx.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()

	listener := &heard{}
	log, err := Open(env, listener)
	if err != nil {
		t.Fatalf("opening the log: %v", err)
	}
	defer log.Close()

	line := strings.Repeat("y", 4096) + "\n"
	for written := 0; written <= Limit; written += len(line) {
		if _, err := log.Write([]byte(line)); err != nil {
			t.Fatalf("writing the log: %v", err)
		}
	}

	if _, err := os.Stat(Path(env) + ".1"); err == nil {
		t.Error("the log rotated after the lock could not be given back")
	}
	if len(listener.said) != 1 {
		t.Fatalf("%d things were said about the log and exactly one is owed:\n%s",
			len(listener.said), strings.Join(listener.said, "\n"))
	}
}

// The lock and the file are of one directory, whatever happens to the
// name in between.
//
// The lock is on the directory because the file is what rotation
// renames. That argument only holds if the directory being locked is the
// directory the file is written in -- and it was opened by name, from a
// string built out of the environment root, while the file was opened
// through the root camp holds. A directory swapped in at that name gave
// two processes a lock on one inode and a file in another, which is no
// lock at all.
//
// Arranged the way it can happen: the environment is opened, and then
// somebody moves it aside and puts another directory at its name.
func TestTheLockIsOnTheDirectoryTheFileIsWrittenIn(t *testing.T) {
	base := t.TempDir()
	env := filepath.Join(base, "env")
	if err := os.MkdirAll(env, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// The environment camp opened is now over here ...
	moved := filepath.Join(base, "moved")
	if err := os.Rename(env, moved); err != nil {
		t.Fatal(err)
	}
	// ... and something else stands at its name, with a logs directory of
	// its own for a resolution by name to land in.
	if err := os.MkdirAll(filepath.Join(env, ".camp", "logs"), 0o755); err != nil {
		t.Fatal(err)
	}

	log, err := Open(root, nil)
	if err != nil {
		t.Fatalf("opening the log: %v", err)
	}
	defer log.Close()
	if _, err := log.Write([]byte("a line of this run\n")); err != nil {
		t.Fatalf("writing the log: %v", err)
	}

	written := filepath.Join(moved, ".camp", "logs", "camp.log")
	kept, err := os.ReadFile(written)
	if err != nil || !strings.Contains(string(kept), "a line of this run") {
		t.Fatalf("the line was not written in the environment camp opened "+
			"(%v):\n%s", err, kept)
	}
	if _, err := os.Stat(filepath.Join(env, ".camp", "logs", "camp.log")); err == nil {
		t.Error("camp wrote a log into the directory that replaced the " +
			"environment's name")
	}

	var held, beside unix.Stat_t
	if err := unix.Fstat(int(log.directory.Fd()), &held); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Dir(written), &beside); err != nil {
		t.Fatal(err)
	}
	if held.Dev != beside.Dev || held.Ino != beside.Ino {
		t.Errorf("the lock is taken on directory %d:%d and the log is written "+
			"in %d:%d, so two writers can hold that lock and rotate two "+
			"different files", held.Dev, held.Ino, beside.Dev, beside.Ino)
	}
}

// The sentence about the log goes through the sink, and the sink writes
// back into the log.
//
// That is a loop worth measuring rather than reasoning about: the
// complaint is composed while a line is being written, and a version of
// this that said it under the mutex would have deadlocked on the line it
// was itself producing. It has to terminate, the line that caused it has
// to land, and the sentence has to be in the file -- a run whose log
// stopped rotating is a fact about that run, and belongs in the record
// of it.
func TestTheSentenceAboutTheLogReachesTheLogItself(t *testing.T) {
	original := flock
	flock = func(int, int) error { return unix.EOPNOTSUPP }
	t.Cleanup(func() { flock = original })

	env, err := pathx.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()

	var terminal strings.Builder
	sink := report.To(&terminal)
	log, err := Open(env, sink)
	if err != nil {
		t.Fatalf("opening the log: %v", err)
	}
	sink.Keep(log)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fmt.Fprintln(sink, "[OK] locks: taken")
		fmt.Fprintln(sink, "[OK] checked: 9 mounts")
		sink.Close()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("saying what changed about the log did not return: the " +
			"sentence is composed while a line is being written, and it goes " +
			"through the sink that is writing it")
	}

	kept, err := os.ReadFile(Path(env))
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	for _, want := range []string{"locks: taken", "checked: 9 mounts", "does not rotate it"} {
		if !strings.Contains(string(kept), want) {
			t.Errorf("the log does not carry %q:\n%s", want, kept)
		}
	}
	if strings.Count(string(kept), "does not rotate it") != 1 {
		t.Errorf("the log carries the sentence about itself %d times:\n%s",
			strings.Count(string(kept), "does not rotate it"), kept)
	}
	if !strings.Contains(terminal.String(), "[WARN]") {
		t.Errorf("the person at the terminal was not told:\n%s", terminal.String())
	}
}
