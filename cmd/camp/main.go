// Command camp composes several git repositories into one working
// directory, without any of them learning about the others.
package main

import (
	"os"
	"strconv"

	"github.com/dlaszlo/camp/internal/cli"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/privileged"
	"github.com/dlaszlo/camp/internal/session"
)

func main() {
	// The capability probe: started in a new user namespace to find out
	// whether this machine allows that. Existing and exiting is its whole
	// job -- if it got this far, the answer is yes. Checked before
	// anything else so that no flag parsing, no logging and no
	// configuration discovery can run in between.
	if len(os.Args) > 1 && os.Args[1] == preflight.ProbeArg {
		os.Exit(preflight.Probe())
	}

	// The init: this same binary, re-executed as pid 1 of a fresh
	// namespace. Checked here so that no flag parsing, no configuration
	// discovery and no logging can run before it -- it has one job, and it
	// is holding the session's locks while it does it.
	if len(os.Args) > 1 && os.Args[1] == session.InitArg {
		runInit(os.Args[2:])
		return
	}

	// The privileged helper: the only part of camp sudo ever wraps. It
	// reads its whole instruction from stdin -- never from argv, which
	// /proc exposes to every user on the machine.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case privileged.MountArg:
			os.Exit(privileged.Helper(privileged.ActionMount, os.Stdin, os.Stdout))
		case privileged.UnmountArg:
			os.Exit(privileged.Helper(privileged.ActionUnmount, os.Stdin, os.Stdout))
		}
	}

	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}

// runInit unpacks "<config> <uid> <gid> -- <command>..." and hands over.
func runInit(args []string) {
	if len(args) < 4 || args[3] != "--" {
		os.Stderr.WriteString("camp: the session init was invoked wrongly\n")
		os.Exit(1)
	}
	uid, uidErr := strconv.Atoi(args[1])
	gid, gidErr := strconv.Atoi(args[2])
	if uidErr != nil || gidErr != nil {
		os.Stderr.WriteString("camp: the session init was given a bad identity\n")
		os.Exit(1)
	}
	session.Inside(args[0], uid, gid, args[4:])
}
