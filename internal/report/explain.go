package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/dlaszlo/camp/internal/plan"
)

// Tree is what a description needs, taken from the plan a configuration
// derives -- the one source there is, and the one the session standing
// here was built from.
type Tree struct {
	// Live is the composed tree's directory, Upper the code repository,
	// Lower the workspace.
	Live  string
	Upper string
	Lower string
	// Mounts are the ones worth describing, in plan order.
	Mounts []TreeMount
	// Generated says the tree carries a generated exclude.
	Generated bool
	// Ownership and Session are the two blocks about the processes inside
	// rather than about the tree.
	Ownership string
	Session   string
	// Grace is how long the session's init waits for what is still inside
	// once its workload has exited.
	Grace time.Duration
}

// TreeMount is one mount as a description shows it: where it appears, and
// what is really there.
type TreeMount struct {
	// Path is where it appears in the tree, relative to Live.
	Path   string
	Source string
	Role   plan.Role
	Kind   plan.Kind
}

// Explain describes the composed tree a plan derives, to whoever is
// standing in it.
//
// It answers the four questions somebody working in a composed tree
// eventually asks -- what is read-only and why, where the real file is,
// what can never end up in a commit, and what happens to a worktree made
// in here.
func Explain(p plan.Plan, grace time.Duration) string {
	tree := Tree{
		Live:      p.Live,
		Upper:     p.Config.UpperPath(),
		Lower:     p.Config.LowerPath(),
		Ownership: Ownership(p),
		Session:   Session(p, "Session environment"),
		Grace:     grace,
	}
	_, tree.Generated = p.Config.GenerationStep()
	for _, mount := range p.Mounts {
		tree.Mounts = append(tree.Mounts, TreeMount{
			Path:   mount.Rel.String(),
			Source: mount.Source,
			Role:   mount.Role,
			Kind:   mount.Kind,
		})
	}
	return Describe(tree)
}

// Describe writes the description itself.
func Describe(p Tree) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are in %s, a tree camp composed out of several "+
		"repositories.\nNone of them knows about the others.\n\n", p.Live)

	fmt.Fprintf(&b, "Where a write goes\n\n")
	fmt.Fprintf(&b, "  Almost everywhere: into %s, the code repository. That is "+
		"the product,\n  and it is the only place ordinary writes land. A file "+
		"you create anywhere\n  camp has not covered is a file in that "+
		"repository.\n\n", p.Upper)

	var guarded, stores, writable []TreeMount
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
			fmt.Fprintf(&b, "  %-28s the real path is %s\n",
				mount.Path, mount.Source)
		}
		fmt.Fprintf(&b, "\n  These come from %s. Writing one through this tree "+
			"fails with EROFS,\n  on purpose: without that, the write would copy "+
			"the file up into the code\n  repository, and the change would look "+
			"applied while living in the wrong\n  place. Edit the real file "+
			"instead -- the path above.\n\n", p.Lower)
		paragraph(&b, "What you then see here depends on what was bound. A "+
			"directory bound into this tree is a live view: a change inside it "+
			"appears here immediately. A single file bound into it -- a root "+
			"file such as CLAUDE.md, an island file -- is a bind of one inode, "+
			"pinned to the file that existed when the session started; a "+
			"replacement saved outside, which is how editors save (by rename), "+
			"appears at the next start and not before.")
	}

	fmt.Fprintf(&b, "The code repository's own path\n\n")
	paragraph(&b, fmt.Sprintf("Inside this session, %s -- the code "+
		"repository's own directory -- refuses writes with EROFS. Write "+
		"through this tree instead; that is what it is for. The reason: the "+
		"overlay holds the paths this tree has resolved, and a write behind "+
		"its back at the raw path -- a save by rename, which is how git and "+
		"every editor write -- would leave this tree showing the old file at "+
		"that path for the rest of the session, and failing the next delete "+
		"there with 'Stale file handle'.", p.Upper))
	paragraph(&b, fmt.Sprintf("That guard exists only inside the session. A "+
		"terminal outside it, an editor started from the desktop, a cron job "+
		"-- anything not in the session -- can still write %s while the "+
		"session is up, with exactly that effect on this tree. Do not write "+
		"the code repository from outside while a session is up; end the "+
		"session first.", p.Upper))

	if len(writable) > 0 {
		b.WriteString("What is writable but goes somewhere else\n\n")
		for _, mount := range writable {
			fmt.Fprintf(&b, "  %-28s writes land in %s\n",
				mount.Path, mount.Source)
		}
		b.WriteString("\n")
	}

	if len(stores) > 0 {
		b.WriteString("What is machine-local\n\n")
		for _, mount := range stores {
			fmt.Fprintf(&b, "  %-28s kept in %s\n", mount.Path, mount.Source)
		}
		b.WriteString("\n  Files here belong to this machine and to no " +
			"repository. They survive the\n  session and they are never " +
			"committed anywhere, because there is nothing\n  to commit them to. " +
			"Inside these directories, whatever a repository\n  contributes " +
			"stands read-only; everything else is yours to write.\n\n")
	}

	if p.Generated {
		fmt.Fprintf(&b, "What git sees\n\n")
		fmt.Fprintf(&b, "  git run in here is %s's git. Its .git/info/exclude "+
			"carries a generated\n  block listing everything the workspace "+
			"provides, so 'git status' stays\n  quiet and 'git add .' cannot "+
			"pick those names up.\n\n", p.Upper)
		b.WriteString("  That is convenience, not a boundary. 'git add -f' " +
			"still reads a workspace\n  file through this tree and stages its " +
			"bytes -- the read-only mounts stop\n  writes, and that only reads. " +
			"camp detects it rather than preventing it:\n  the index is scanned " +
			"when the session ends. The point of no return is\n  push, not " +
			"commit, so a leak caught then is usually still free to undo.\n\n")
		fmt.Fprintf(&b, "  The generated exclude exists only through this tree. "+
			"In %s\n  git keeps reading that repository's own file, unchanged.\n\n",
			p.Upper)
	}

	b.WriteString("Worktrees\n\n")
	b.WriteString("  A worktree made from in here records this tree's path on " +
		"both sides, and\n  git compares those paths as strings -- so when the " +
		"session ends, git stops\n  being able to see the checkout. The files " +
		"are fine. When the session ends\n  camp prints the exact repair command " +
		"for each one -- and, if no terminal is\n  attached by then, the next " +
		"camp command in this environment prints it --\n  and after that repair " +
		"the worktree is independent of the composition.\n\n")

	b.WriteString("This session\n\n")
	paragraph(&b, "This composition exists only for the processes inside it. "+
		"Nothing outside can see it -- a program started outside the session, "+
		"an editor already running, sees the composed tree's directory empty.")
	paragraph(&b, Ending(p.Grace)+" Nothing has to be cleaned up: the kernel "+
		"removes every mount with the namespace.")

	b.WriteString(p.Ownership)

	if session := p.Session; session != "" {
		b.WriteString(session)
		b.WriteString("\n")
	}

	b.WriteString("What camp is not\n\n  Not a sandbox. The read-only mounts " +
		"prevent accidental writes and copy-up.\n  A process in here can still " +
		"walk to the backing directories and read\n  anything on the machine. " +
		"camp does not pretend otherwise.\n")

	return b.String()
}
