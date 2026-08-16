package report

// What 'camp init' leaves in the environment's own directory, beside the
// configuration.
//
// Both files exist for the same reason. Somebody opening .camp finds five
// things and no way to tell which of them is theirs. **One of them is**;
// the rest is camp's working material, and editing it by hand ranges from
// pointless to harmful. Saying so in the directory itself is the cheapest
// place to put that, and the only one a person meets without going
// looking for it.

// CampIgnore keeps camp's scratch out of version control.
//
// It lives inside .camp rather than in any repository's .gitignore,
// because the environment root may belong to a repository, to a different
// one later, or to none at all -- and the answer is the same in every
// case.
const CampIgnore = `# camp's own directory. See README.md here for what each of these is.
#
# Worth committing: config.yml (what you want composed) and inventory
# (a record whose diff is meant to be read).
#
# Not worth committing, and ignored below.

/work/
/storage/
/reports/
`

// CampReadme says which of the things in .camp belongs to the reader.
const CampReadme = `# camp's directory

Everything camp needs for this environment lives here. **One file is
yours.**

| | what it is | who writes it |
|---|---|---|
| ` + "`config.yml`" + ` | **yours** -- what you want composed | you, by hand |
| ` + "`inventory`" + ` | a record of both repositories' root entries, compared against at every start | ` + "`camp accept`" + `, and nothing else |
| ` + "`work/`" + ` | scratch for one composition | camp; swept when nothing is mounted |
| ` + "`storage/`" + ` | machine-local files and git worktrees, kept between sessions | camp -- and **never removed** by it, because it holds unfinished work |
| ` + "`reports/`" + ` | what a session found, printed once by the next camp command | camp |

## Editing

Edit ` + "`config.yml`" + `. Run ` + "`camp plan`" + ` to see what it would
do. Do not edit the rest by hand.

` + "`inventory`" + ` is generated on purpose and only on request. camp
compares against it at every start, so a name that appears at a
repository's root while you were not looking stops the composition instead
of passing unnoticed -- and editing the file by hand defeats exactly that.
When the change is one you meant, look at it and run ` + "`camp accept`" + `.

` + "`work/`" + `, ` + "`storage/`" + ` and ` + "`reports/`" + ` are camp's
own. If something in ` + "`storage/`" + ` matters to you -- a worktree, a
machine-local settings file -- it is yours to move or delete, and camp will
not do it for you. Everything else in those three is safe to lose.

You may also find a directory in ` + "`work/`" + ` that you cannot open, with
no permissions at all. That one is the kernel's, not camp's: OverlayFS
makes it unreadable so that nothing wanders into it. It holds nothing of
yours, and the next run removes it.

## Version control

` + "`config.yml`" + ` and ` + "`inventory`" + ` are worth committing: the
first is intent, the second has a diff meant to be read. The three
directories are not, and the ` + "`.gitignore`" + ` here says so.
`
