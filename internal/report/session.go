package report

import (
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/plan"
)

// OwnershipClause is the fact of a session that a reader of a captured
// log needs later, in one sentence.
//
// It rides on the identity line rather than standing alone. An
// unconditional warning before every session would be noise by the second
// day, and a warning nobody reads any more is the protection that looks
// installed and does nothing. As a clause of a line that is already being
// printed it costs nothing and it is in the transcript when somebody is
// working out why an archive built in here has the wrong owners.
const OwnershipClause = "only your own uid is mapped, so a file owned by " +
	"anyone else appears as nobody"

// Session renders what a session's workload will be handed. The heading is
// the caller's, because plan and explain are two different documents about
// the same thing.
func Session(p plan.Plan, heading string) string {
	var b strings.Builder

	identity := nsx.For(p.Config.Session.Identity)
	b.WriteString(heading + "\n\n")
	fmt.Fprintf(&b, "  identity: %s.\n", wrap(identity.Describe(), "            "))
	fmt.Fprintf(&b, "            %s\n\n",
		wrap("Note: "+OwnershipClause+".", "            "))

	width := len("CAMP_LIVE")
	for _, variable := range p.Environment {
		if len(variable.Name) > width {
			width = len(variable.Name)
		}
	}
	fmt.Fprintf(&b, "  %-*s = %q   camp's own, and always the last word\n",
		width, "CAMP_LIVE", p.Live)
	fmt.Fprintf(&b, "  %-*s = %q   camp's own: the workload starts here\n",
		width, "PWD", p.Live)
	for _, variable := range p.Environment {
		note := "new"
		if variable.Overrides {
			note = "replaces an inherited value"
		}
		fmt.Fprintf(&b, "  %-*s = %s   (%s)\n", width, variable.Name, variable.Shown, note)
	}

	b.WriteString("\n  Everything else is inherited from the terminal that " +
		"starts the session,\n  byte for byte. These are applied to the workload " +
		"after camp gives the\n  mount capability back, and to nothing else: not " +
		"to camp's own processes,\n  not to the generation step, which runs before " +
		"any workload exists.\n")
	if len(p.Environment) > 0 {
		b.WriteString("  An inherited value is shown as <inherited NAME> and " +
			"never as its bytes.\n  This output is routinely captured -- terminals, " +
			"pasted issues, agent\n  transcripts -- and what a variable holds is not " +
			"camp's to copy into one.\n")
	}
	return b.String()
}

// Ownership describes what a session changes about who owns a file.
//
// The whole class, not the one program somebody meets first: the ones that
// refuse an owner they cannot attribute, the ones a session deliberately
// cannot do at all, and the silent ones -- the artefact builders that
// record the projected owner and report nothing. The last group is the
// dangerous one precisely because nothing fails.
//
// It recommends no host-side change. Not a global git setting, not a shell
// alias, not a file outside the composition: a session is something you
// are inside, and wiring the outside to repair the inside creates
// incompatibilities in places that have nothing to do with camp.
func Ownership(p plan.Plan) string {
	var b strings.Builder

	b.WriteString("Ownership view\n\n")
	paragraph(&b, "Only your own user id is mapped in here, so every file on "+
		"the machine owned by anyone else -- root included -- is shown as "+
		"'nobody'. Reading and writing are unaffected; what changes is what a "+
		"program sees when it asks who owns a file. No mapping can fix this: a "+
		"user namespace lets you map the ids you own, and root's is not one of "+
		"them.")
	paragraph(&b, "Three kinds of program notice, and they need different "+
		"answers.")
	paragraph(&b, fmt.Sprintf("Programs that refuse an owner they cannot "+
		"attribute. ssh is where this is met: it will not read a system-wide "+
		"configuration file belonging to neither root nor you, so 'ssh' and "+
		"'git push' over ssh stop before any connection is opened. The "+
		"composition adapts such a program through the control the program "+
		"already has -- an option variable, or command resolution through a "+
		"prepended PATH -- declared in the session: section of %s. The "+
		"documentation carries the complete OpenSSH arrangement, which lives "+
		"in the workspace repository and not on this machine.", p.Config.Source))
	paragraph(&b, "Operations a session simply does not have. A setuid binary "+
		"owned by an unmapped id confers nothing, so sudo and pkexec cannot "+
		"elevate in here, and 'chown' to anyone else fails because no other id "+
		"exists to name. That is the session being rootless, not a fault to "+
		"repair.")
	paragraph(&b, "Programs that record ownership into what they produce. tar, "+
		"rsync -a, an image build: a file that is root's outside is written "+
		"into the artefact as 65534. Nothing fails, nothing warns, and the "+
		"artefact is simply wrong -- which surfaces somewhere else, much later. "+
		"No variable repairs this one, and camp has no other way of running "+
		"that would: build such an artefact from outside a session, from the "+
		"repositories' own directories.")

	return b.String()
}

// paragraph writes one indented, folded paragraph of an explain section.
func paragraph(b *strings.Builder, text string) {
	b.WriteString("  " + wrap(text, "  ") + "\n\n")
}
