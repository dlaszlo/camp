package report

import (
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/plan"
)

// Explain describes the composed tree to whoever is standing in it.
//
// Generated from the live configuration rather than written by hand, so
// it cannot go stale: every path in it is the path this composition
// actually uses. It answers the four questions somebody working in a
// composed tree eventually asks -- what is read-only and why, where the
// real file is, what can never end up in a commit, and what happens to a
// worktree made in here.
func Explain(p plan.Plan) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are in %s, a tree camp composed out of several "+
		"repositories.\nNone of them knows about the others.\n\n", p.Live)

	fmt.Fprintf(&b, "Where a write goes\n\n")
	fmt.Fprintf(&b, "  Almost everywhere: into %s, the code repository. That is "+
		"the product,\n  and it is the only place ordinary writes land. A file "+
		"you create anywhere\n  camp has not covered is a file in that "+
		"repository.\n\n", p.Config.UpperPath())

	var guarded, stores, writable []plan.Mount
	for _, mount := range p.Mounts {
		switch mount.Role {
		case plan.RootGuard, plan.Island:
			guarded = append(guarded, mount)
		case plan.Store:
			stores = append(stores, mount)
		case plan.Declared:
			if mount.Kind == plan.BindRW {
				writable = append(writable, mount)
			} else {
				guarded = append(guarded, mount)
			}
		}
	}

	if len(guarded) > 0 {
		b.WriteString("What is read-only, and why\n\n")
		for _, mount := range guarded {
			fmt.Fprintf(&b, "  %-28s the real file is %s\n",
				mount.Rel.String(), mount.Source)
		}
		fmt.Fprintf(&b, "\n  These come from %s. Writing one through this tree "+
			"fails with EROFS,\n  on purpose: without that, the write would copy "+
			"the file up into the code\n  repository, and the change would look "+
			"applied while living in the wrong\n  place. Edit the real file "+
			"instead -- the path above -- and the change\n  appears here "+
			"immediately, because these are live views and not copies.\n\n",
			p.Config.LowerPath())
	}

	if len(writable) > 0 {
		b.WriteString("What is writable but goes somewhere else\n\n")
		for _, mount := range writable {
			fmt.Fprintf(&b, "  %-28s writes land in %s\n",
				mount.Rel.String(), mount.Source)
		}
		b.WriteString("\n")
	}

	if len(stores) > 0 {
		b.WriteString("What is machine-local\n\n")
		for _, mount := range stores {
			fmt.Fprintf(&b, "  %-28s kept in %s\n", mount.Rel.String(), mount.Source)
		}
		b.WriteString("\n  Files here belong to this machine and to no " +
			"repository. They survive the\n  session and they are never " +
			"committed anywhere, because there is nothing\n  to commit them to. " +
			"Inside these directories, whatever a repository\n  contributes " +
			"stands read-only; everything else is yours to write.\n\n")
	}

	if _, has := p.Config.GenerationStep(); has {
		fmt.Fprintf(&b, "What git sees\n\n")
		fmt.Fprintf(&b, "  git run in here is %s's git. Its .git/info/exclude "+
			"carries a generated\n  block listing everything the workspace "+
			"provides, so 'git status' stays\n  quiet and 'git add .' cannot "+
			"pick those names up.\n\n", p.Config.UpperPath())
		b.WriteString("  That is convenience, not a boundary. 'git add -f' " +
			"still reads a workspace\n  file through this tree and stages its " +
			"bytes -- the read-only mounts stop\n  writes, and that only reads. " +
			"camp detects it rather than preventing it:\n  the index is scanned " +
			"when the session ends. The point of no return is\n  push, not " +
			"commit, so a leak caught then is usually still free to undo.\n\n")
		fmt.Fprintf(&b, "  The generated exclude exists only through this tree. "+
			"In %s\n  git keeps reading that repository's own file, unchanged.\n\n",
			p.Config.UpperPath())
	}

	b.WriteString("Worktrees\n\n")
	b.WriteString("  A worktree made from in here records this tree's path on " +
		"both sides, and\n  git compares those paths as strings -- so when the " +
		"composition comes down,\n  git stops being able to see the checkout. " +
		"The files are fine. Run\n  'camp down' and read what it says: it prints " +
		"the exact repair command for\n  each one, and after that repair the " +
		"worktree is independent of the\n  composition.\n\n")

	if p.Mode == plan.Privileged {
		fmt.Fprintf(&b, "This mode\n\n  The composition is visible to every "+
			"process on this machine, and %s\n  is read-only for all of them "+
			"until 'camp down' -- your editor included.\n  That is the price of "+
			"this mode. The namespace mode ('camp run') keeps both\n  promises, "+
			"and is where normal work happens.\n\n", p.Config.LowerPath())
	} else {
		b.WriteString("This mode\n\n  This composition exists only for the " +
			"processes inside it. Nothing outside\n  can see it, nothing has to " +
			"be cleaned up, and when the last process here\n  exits the kernel " +
			"removes every mount with it. There is no 'camp down'.\n\n")
	}

	b.WriteString("What camp is not\n\n  Not a sandbox. The read-only mounts " +
		"prevent accidental writes and copy-up.\n  A process in here can still " +
		"walk to the backing directories and read\n  anything on the machine. " +
		"camp does not pretend otherwise.\n")

	return b.String()
}
