# camp

Compose several git repositories into one working directory, without any
of them learning about the others.

```
camp run -- <your editor, your shell, your coding agent>
```

No root, nothing written into any repository, and nothing left behind
when the session ends.

---

## What it is for

Modern development directories hold more than the product. They hold the
instructions your tools read, the agent definitions, the skills, the
prompts, the notes, the record of what was decided and why. All of it is
real, all of it is worth versioning — and none of it belongs in the
product's history.

That has become sharpest with **AI coding agents**. Claude Code reads
`CLAUDE.md` and `.claude/`; other agents read their own files at the
project root; all of them expect one project directory and look for their
configuration in it. So the choice appears to be: commit your agent setup
into the product's repository, or give the agent a directory that is
missing half of what it needs.

camp is the third option, and it is not specific to any one tool. The
repositories stay separate — separately cloned, separately committed,
separately owned — and the filesystem presents them as one directory:

- the **code repository** stays the product, and only the product;
- a **workspace repository** carries the development environment, and can
  be shared across every project you work on;
- writes land in the code repository or in machine-local storage —
  **never** in the workspace, and never silently in the wrong place.

Each tool sees one project root. Each repository sees only its own
content. Nothing is copied, and nothing is generated into anyone's
working tree.

**New here? Start with [docs/getting-started.md](docs/getting-started.md)** —
it walks from two empty repositories to a working composition in about
five minutes.

## What it actually does

```
~/work/
├── .camp/
│   ├── config.yml            the configuration — the only file you write
│   ├── inventory             the accepted snapshot of both repositories' roots
│   ├── work/<id>/            disposable: the overlay's workdir, generated files
│   ├── storage/<id>/         persistent: machine-local files, worktrees
│   └── logs/                 every line camp printed here, with timestamps
├── shop/                     the code repository       (writes land here)
├── shop-env/                 the workspace repository  (read-only inside)
├── shop-records/             a third repository        (writable, its own)
└── shop-live/                the composed tree         (where you work)
```

Inside `shop-live` you see one project. `git` there is the code
repository's git. A new file you create lands in the code repository. A
write to anything the workspace provides fails with `EROFS` — loudly, on
purpose, because the alternative is a change that looks applied and
exists in no repository.

## The three guarantees

**camp only composes; it never modifies a repository.** No file, no
xattr, no hook, no exclude line is ever written into one. The generated
exclude is *mounted* over the composed tree's copy, so the repository
keeps reading its own. This is a property of the source code: every
filesystem write in camp goes through one package whose addressing cannot
be constructed from a repository path, and a test fails the build if a
write appears anywhere else.

**The workspace is never written, by any route.** It is the overlay's
lower layer, so no copy-up can reach it; no writable mount may source
from it; and while the composition is up it is bound read-only onto its
own path, so a process inside cannot write it even by absolute path.
After a session the workspace is byte-identical.

**You delete; camp checks.** camp never removes a repository, a checkout
or a branch, never clones, never commits. It removes only what it made.
If the composed tree's directory is not empty after unmounting, that is
evidence of a problem and it is reported, never cleaned away.

## The session

The composition is built inside a user and mount namespace, for the
processes camp starts there and for nothing else. No privilege is needed;
nothing outside the session can see it; and when the last process exits
the kernel discards the namespace and every mount in it. Teardown cannot
fail, and there is no command to take a composition down.

```
camp run -- claude              # or your editor, your shell, your test suite
camp shell                      # a shell in the composed tree
```

**Several terminals in the same tree** — start something that stays
inside, and attach to it from anywhere:

```
camp run -- tmux new-session -d -s work    # returns at once
tmux attach -t work                        # from any other terminal
```

The tmux *client* is only a pipe; every window it opens is a child of the
server, and therefore inside. `tmux kill-server` ends the session and
everything comes down with it.

**A session lives as long as any process is inside it**, so something
started detached keeps it open after you have left the terminal. Send
`SIGTERM` to camp's own process, the one resident as the session's first
process: it means "end this session", it reaches every process in the
namespace and nothing outside it, and a `SIGCONT` follows so a stopped
one can act on it. Nothing is escalated to `SIGKILL`. If you do not know
its pid, ask for a second session in the same tree: camp refuses and
names what is holding it, by pid and command.

camp does not depend on tmux, and it does not depend on tmux keeping any
descriptor open either: the locks that guarantee one composition per
repository are held by camp itself, resident as the session's first
process.

**A program has to be started inside the session to see the tree.** That
includes your editor: start it with `camp run -- <editor>`, or from a
shell `camp shell` opened, and it sees the composed tree. An editor that
was already running when the session started sees the composed tree's
directory empty, and there is no other way to run camp that would show it
the tree. The [limits](#limits-plainly) below say so again, because it is
the one thing about camp that surprises people.

## Requirements and install

Linux with OverlayFS, and `git` when the composition is git-based —
camp reads git to work out what each repository contributes; two plain
directories need none of it. Nothing else at run time: camp makes its
mounts by syscall and asks `/proc` for state, so there is no `mount(8)`,
`fuser` or similar to install. Go 1.25+ only if you build it yourself.

On Debian and Ubuntu, a package does all of it at once — the binary, the
AppArmor profile with the path already pointing at it, and the overlay
module at boot. Take the one from the latest release:

```
gh release download --repo dlaszlo/camp --pattern '*.deb'
sudo dpkg -i camp_*.deb
camp doctor                     # says whether this machine can run camp
```

Or from [the releases page](https://github.com/dlaszlo/camp/releases), if
you would rather not have `gh`. Every release is built from a tagged
commit after the whole suite has passed on the machine that built it. The
same package is built from a checkout with `sudo dpkg -i
"$(packaging/deb/build)"`, which is what the tests do.

By hand, anywhere — this is the route that needs a Go toolchain:

```
go build -o camp ./cmd/camp
sudo install -m 755 camp /usr/local/bin/camp
camp doctor
```

On most distributions that is all of it: unprivileged user namespaces
are permitted by default, and `camp run` works. Ubuntu 23.10 and later
restrict them per binary, and camp ships an AppArmor profile for that one
case:

```
sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp
sudo apparmor_parser -r /etc/apparmor.d/camp
```

camp does not depend on AppArmor — the profile exists because Ubuntu's
restriction is per-binary, and granting one path is narrower than turning
the restriction off for every program on the machine. Where a different
switch is in the way, `camp doctor` names it and the repair, because it
finds out by trying rather than by reading switches.
[docs/install.md](docs/install.md) has the full table. There is no other
way to run camp: every session needs the namespace, and a machine that
refuses one cannot compose.

### One more step if you use ssh

A `camp run` session maps only your own user id, so every file owned by
anyone else — root included — appears as `nobody` inside it. ssh refuses
a system-wide configuration file it cannot attribute to root or to you,
so `ssh` and `git push` over ssh fail in a session until ssh is pointed
at your own configuration with `-F`, which is also what makes it skip the
system-wide one.

The repair belongs to the composition, not to your machine. camp changes
nothing outside a session — not your shell's startup file, not your
global git configuration — so the setting goes in the `session:` section
of the configuration, where it is versioned and diffable:

```yaml
session:
  environment:
    GIT_SSH_COMMAND: "ssh -F ${HOME}/.ssh/config"
```

That is git covered, including where no shell is started — the case for a
program `camp run` starts directly. `ssh`, `scp` and `sftp` typed by hand
have no option variable of their own, so they are reached the other way:
a directory in your workspace repository, prepended to the session's
`PATH`, holding a small launcher for each. [docs/install.md](docs/install.md)
has the complete arrangement, including how a launcher finds the real
program without naming a distribution path.

## The configuration

One file, `$ENV/.camp/config.yml`. It states intent; the mount plan, the
exclude and the inventory are all derived from it.

```yaml
env: /home/you/work               # the one absolute path in the file
merged: shop-live                 # where the composed tree appears

repositories:
  - { name: workspace, path: shop-env }
  - { name: code,      path: shop }
  - { name: records,   path: shop-records }

overlayfs:
  lower: [workspace]              # read-only underneath
  upper: code                     # on top, and the only place writes land

allow_overlap: [.gitignore]       # the only names allowed in both roots

prepare:                          # optional: your own programs, run
  - command: [bin/check-my-trees] # before anything is composed
    timeout: 120                  # optional, in seconds

steps:
  - mount_rw:
      - { source: "code/.git",         target: ".git" }
      - { source: "records",           target: ".records" }
  - mount_islands:
      - { source: "workspace/.claude", target: ".claude" }
  - git_exclude

session:                          # optional, and only 'camp run' reads it
  # identity: uidmap              # optional; the default maps your own uid
  environment:
    GIT_SSH_COMMAND: "ssh -F ${HOME}/.ssh/config"
```

Those keys are the whole file. `examples/config.yml` is the same file with
every one of them written out and commented, and `camp plan` prints what
any of it would do without doing it.

**`prepare:` is your own code, before the composition.** An ordered list
of programs camp runs after it has taken its two locks and before it
derives anything: guards over your checkouts, a fetch of something the
session must not be a day older than, whatever your environment needs
established before it exists. Each entry is an argument vector executed
directly — no shell, so nothing is split on spaces and nothing is
expanded — with the environment root as its working directory and
`CAMP_ENV` and `CAMP_LIVE` in its environment. The first one that does
not succeed refuses the composition with nothing mounted, and the ones
after it do not run. They always run as you and never as root; `camp
plan` lists them and says it did not run them. Note the one
boundary this crosses: camp itself never writes into a repository, and
one of these is your program, running as you, which can. That is what it
is for.

**`steps:` is one ordered sequence, and its order is the mount order.** An
earlier mount's target may not lie inside a later one's, because the later
would silently cover the earlier — so `git_exclude`, whose target is
inside `.git`, has to come after the `.git` bind. Parent first, then
child. camp checks this before anything is mounted.

| kind | what it does |
|---|---|
| `mount_ro` | a source, read-only, at a target |
| `mount_rw` | a source, writable — or, with no source, an empty writable hole backed by camp's storage |
| `mount_islands` | a writable machine-local floor, with the source's *contributed* entries standing in it read-only |
| `git_exclude` | the shipped generation step: reads git, produces the exclude and the islands expansions |
| `generate` | the same contract with a program of your own |

Three things worth knowing before you write one:

**`.git` is declared, never derived.** Both repositories have one, and
directories merge, so without that bind the two histories would union.
camp does not add it for you — a core that reaches for the name `.git`
carries git knowledge, and the generation step exists to keep git out of
it. Leaving it out is not a hole: the overlap gate refuses the
composition first, by a rule that knows nothing about git.

**The overlap gate.** If a name exists in both roots and `allow_overlap`
does not name it, the composition does not start. There is no `--force`:
the escape hatch is that line in the configuration — the same decision,
recorded and diffable. Nothing can wall you in, because the repositories
stay ordinary directories reachable without camp.

**`mount_islands` is for a directory that is half repository.** `.claude`
holds what the workspace tracks (agents, skills, settings) *and* what only
this machine has (`settings.local.json`, worktrees, locks). An islands
mount covers the whole directory with camp's storage — the *water* — and
stands each tracked entry in it read-only — the *islands*. Runtime files
land in the water and survive the session; editing a tracked entry is
`EROFS`.

**`session:` describes processes, not the tree.** `environment:` declares
what a session's workload receives, and through inheritance everything
descended from it. `$NAME` and `${NAME}` insert what that name held in
the environment camp was started with, `$$` is one literal dollar, and
`$CAMP_LIVE` is the composed tree. There is no shell in any of it: no
command substitution, no `~`, no word splitting, and inserted bytes are
never scanned again. A name that is not set **refuses** rather than
becoming empty text, because an empty value looks applied. Names
beginning `CAMP_` are camp's own, and so is `PWD`. `identity:` selects
how you are mapped inside the namespace: left out, your own uid maps to
itself and the mount capability is dropped before anything runs, which is
the route you want unless you know otherwise; `uidmap` uses `newuidmap`
and your subuid range instead.

A `prepare:` command does **not** receive these declarations, and that is
deliberate: they are the workload's, and they are resolved against a
composed tree that does not exist yet — `$CAMP_LIVE/records` names
nothing at prepare time. A prepare command that needs variables for its
own children exports them itself.

## Everyday commands

```
camp plan          what would be mounted, in order, with the reason for each
camp doctor        what this machine and this environment lack
camp accept        record the two repositories' root entries as they are now
camp explain       describe the composed tree to whoever is standing in it
camp status        what is mounted and what is not, from where you run it
camp init          write a configuration skeleton
```

`camp status` answers for the process that runs it. From outside every
session it reports nothing mounted, which is true: a session's mounts
exist only inside its namespace. From inside a session it describes that
session and checks it against the configuration as it now stands.

`camp accept` is the only thing that writes the inventory. camp compares
against it at every start, because a new name at the workspace root
changes what the read-only binds protect and what the exclude covers —
and that should be a change somebody decided, not one that happened.

## What the exclude does, and what it does not

Without it, `git status` in the composed tree lists every workspace name
as untracked and `git add .` stages their content into the code
repository. So camp generates one — one anchored line per workspace root
name — and mounts it over the composed tree's `.git/info/exclude`. The
repository's own file is untouched.

Three levels of defence, stated honestly:

- the kernel stops writes (the read-only binds);
- the exclude stops *accidental* staging (`git add .`);
- **nothing stops `git add -f`.** It reads a workspace file through the
  tree and stages its bytes; the mounts stop writes, not reads.

That last one is detected rather than prevented. When a session ends,
camp scans the index — `git ls-files --stage`, because a forced add
leaves an indexed path with no working-tree file at all, which no
untracked-file scan can see. The point of no return for a shared history
is *push*, not commit, so a leak caught then is usually still free to
undo.

## Limits, plainly

**Not a sandbox.** The read-only mounts prevent accidental writes and
copy-up. A process inside can still walk to the backing directories and
read anything on the machine. camp does not pretend otherwise.

**Linux only.** It is built on OverlayFS and bind mounts.

**A program started outside a session cannot see the composed tree.** The
mounts exist only inside the session's namespace, so an editor, a
language server or a daemon that was already running sees the composed
tree's directory empty, and nothing camp offers shows it the tree. Start
such programs inside — `camp run -- <program>`, or from a `camp shell` —
and they see it. The tree is never visible machine-wide, and no record of
a session survives it: when the last process exits, the kernel takes
everything, and there is nothing left to describe. camp once had a second
way of running that mounted the tree for the whole machine; it was
removed, because nothing used it and every change to the session would
have had to be designed twice.

**Worktrees made through the tree need one repair.** Git records a
worktree's git directory as an absolute path and compares it as a string,
so one created inside the composition stops resolving when the
composition comes down. The files are fine; git simply cannot see them.
camp prints the exact `git worktree repair` command when the session
ends, and after that the worktree is composition-independent.

**A single-instance GUI editor started from inside** may hand the path to
its instance outside, which then opens the raw directory. "Start it from
inside" does not work for those.

## Documentation

- **[docs/getting-started.md](docs/getting-started.md)** — from two empty
  repositories to a working composition.
- **[docs/install.md](docs/install.md)** — requirements, build, install,
  the namespace permission, and how to check it worked.
- **[docs/how-it-works.md](docs/how-it-works.md)** — what camp mounts, in
  what order, why, and what it verifies afterwards. Read this before
  trusting it with anything you care about.

## License, and no warranty

MIT — see [LICENSE](LICENSE), copyright David Laszlo.

camp mounts filesystems. That is what it is for, and it is done carefully:
it refuses rather than guessing, it verifies what the kernel actually did
rather than what it was asked to do, and it never writes into your
repositories. It is still a tool that changes what a directory looks
like, on a machine you care about.

So, in the licence's words, the software is provided **as is, without
warranty of any kind**, and the authors are not liable for any claim,
damage or other liability arising from its use. Meant plainly: read
`camp plan` before your first `camp run`, keep your work committed, and
satisfy yourself that it does what you expect before you rely on it.
