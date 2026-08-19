//go:build camptest

package privileged

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// This build stops the privileged half at named points, so that the two
// measurements the review asks for can be made at all: killing the helper
// at each boundary and recovering from the record alone, and swapping the
// environment's name under it at each resolution.
//
// Only this build. barrier.go is the empty body every ordinary camp has,
// and the reason for the split is in its comment: a pause the invoking
// user can trigger inside the root helper is the attack, not a seam.
//
// **The signal is a file and not an environment variable**, because sudo
// resets the environment and the helper is on the far side of it. Both
// halves already know camp's work directory for this composition, so that
// is where the two names live:
//
//	<work>/camp-barrier     armed by the driver: "<name> <kill|wait>"
//	<work>/camp-barrier.go  written by the driver to release a wait
//
// **This file only ever reads.** Announcing arrival by writing a file
// would be a write outside fsx, which the source guard refuses and is
// right to: every write camp makes is addressed through an Area, and a
// seam is not a reason to make an exception. The barrier says where it is
// on stderr instead, which the driver is already reading -- sudo passes
// the helper's stderr straight through to the front end's.
//
// kill sends this process SIGKILL where it stands, which is a kill and
// not an exit: no defer runs, no reply is written, and what the machine
// is left holding is exactly what the boundary left it holding. wait
// announces and then blocks until the driver says go, which is the window
// a rename needs.

// barrierWait bounds how long a wait barrier holds the helper. A driver
// that died mid-measurement must not leave root asleep inside a
// composition for ever; past this the helper carries on, and the driver
// sees an unreleased barrier in the reached file and calls the run void.
const barrierWait = 60 * time.Second

// barrier stops at a named point when the driver armed that name.
//
// Every call site names a boundary the review lists. A name nobody armed
// costs one failed stat, which is the whole price of this file being
// compiled in at all.
func barrier(job Job, name string) {
	work := barrierWork(job)
	if work == "" {
		return
	}
	armed, mode := barrierArmed(work)
	if armed != name {
		return
	}

	// The one line the driver waits for. On stderr and not in a file, for
	// the reason the comment above gives.
	fmt.Fprintf(os.Stderr, "camp barrier: reached %s (%s)\n", name, mode)

	if mode == "kill" {
		// Where it stands. Not os.Exit, which would run what is deferred
		// and could unwind the very state the boundary exists to leave.
		_ = unix.Kill(os.Getpid(), unix.SIGKILL)
		select {} // unreachable; the signal is not catchable
	}

	release := filepath.Join(work, "camp-barrier.go")
	deadline := time.Now().Add(barrierWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			fmt.Fprintf(os.Stderr, "camp barrier: released %s\n", name)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "camp barrier: %s was never released, carrying on\n", name)
}

// barrierWork is camp's work directory for this composition, which both
// halves can name and the driver can write into.
//
// The helper has it as components beneath the job's base; the front end
// fills the same field before it hands the job over. A job that names no
// work directory -- a teardown built from a record that predates one --
// simply has no barriers.
func barrierWork(job Job) string {
	if job.Base == "" || len(job.WorkParts) == 0 {
		return ""
	}
	return filepath.Join(append([]string{job.Base}, job.WorkParts...)...)
}

// barrierArmed reads which barrier the driver armed, and how it wants it
// answered. An unreadable or empty file arms nothing.
func barrierArmed(work string) (name, mode string) {
	data, err := os.ReadFile(filepath.Join(work, "camp-barrier"))
	if err != nil {
		return "", ""
	}
	fields := strings.Fields(string(data))
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], "wait"
	default:
		return fields[0], fields[1]
	}
}
