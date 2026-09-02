// Command camp composes several git repositories into one working
// directory, without any of them learning about the others.
package main

import (
	"os"

	"github.com/dlaszlo/camp/internal/cli"
	"github.com/dlaszlo/camp/internal/preflight"
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
		session.InitMain(os.Args[2:])
		return
	}

	// The joined process: this same binary, execed by nsenter inside a
	// session it has already entered. Answered here, before any flag
	// parsing or configuration discovery, for the same reason as the init:
	// it has one job -- become the shell or command inside the session --
	// and nothing may run before it.
	if len(os.Args) > 1 && os.Args[1] == session.JoinedArg {
		session.JoinedMain(os.Args[2:])
		return
	}

	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
