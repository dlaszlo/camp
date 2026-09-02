# How camp works

This describes the mechanism: what camp mounts, in what order, why each
piece is there, and what it checks afterwards. Read it before trusting
the tool with anything you care about — and read it if a refusal ever
surprises you, because most of them are one of the rules below.

Nothing here is aspirational. Where a sentence says a kernel or git
behaves in a particular way, that behaviour was measured; where a design
choice looks odd, the reason is the paragraph next to it.

## The pieces

```
$ENV/
├── .camp/
│   ├── config.yml            the configuration — intent, and the only file you write
│   ├── inventory             the accepted snapshot of both repositories' root entries
│   ├── work/<id>/            DISPOSABLE: the overlay's workdir, generated files
│   ├── storage/<id>/         PERSISTENT: machine-local files, worktrees — never removed
│   ├── reports/              what a session found, waiting to be read once
│   └── logs/                 every line camp wrote to stderr, timestamped and rotated
├── <workspace repository>    the lower layer
├── <code repository>         the upper layer
└── <merged>                  the composed tree
```

`<id>` is the first twelve hex characters of SHA-256 over the composed
tree's real path. Derived rather than random, so that anything left by a
crash can be attributed to the composition it belonged to; and stable,
because the two stores are named from it.

The two stores never share a parent, because their lifecycles are
opposite. `work/` may be swept whenever nothing is mounted. `storage/`
holds unfinished worktrees and machine-local files and is **never**
removed by camp.

## What is yours, and what is camp's

Five things live in `.camp`, and exactly one of them is yours to edit.
`camp init` writes a README saying so into the directory itself, because
that is the only place a person meets the answer without going looking
for it.

| | what it is | who writes it | committed? |
|---|---|---|---|
| `config.yml` | **yours** — what you want composed | you, by hand | yes |
| `inventory` | the accepted snapshot of both repositories' root entries | `camp accept`, and nothing else | yes |
| `work/` | scratch for one composition | camp; swept when nothing is mounted | no |
| `storage/` | machine-local files and worktrees | camp — and never removed by it | no |
| `reports/` | what a session found | camp | no |

The distinction that matters is not generated-versus-written; it is
**whose it is**. `inventory` is generated, and committing it is the point
of it — but editing it by hand defeats the one job it has. camp compares
against it at every start, so a name that appeared at a repository's root
while you were not looking stops the composition instead of passing
unnoticed. Change it by editing the repositories and running
`camp accept`, never by editing the file.

`storage/` is camp's and is never removed by camp, which sounds
contradictory until you look at what is in it: worktrees and
machine-local files, which are yours and may be unfinished. camp will not
delete your unfinished work to tidy up. Move or remove what you want from
there yourself; `camp doctor` lists any storage whose composition no
longer exists.

One thing in `work/` looks alarming and is not: a directory with no
permissions at all, owned by you. OverlayFS creates it inside the workdir
it is given and makes it unreadable so that nothing wanders into it. It
holds nothing of yours. camp cannot remove it while the composition is
up — it is in use — and a namespace session has no teardown step of its
own, so the next run sweeps it.

## The mount sequence

A run has two halves: a **frame** that always executes in a fixed order,
and the configuration's own **steps** in the middle of it. The split is
the point — everything the safety rests on is frame, so no configuration
can move it, weaken it or leave it out.

Before anything is mounted:

**1. Take two locks.** One on the code repository's directory, one on the
composed tree's directory — the inodes themselves, with `flock`, not a
lock file anywhere. Both exclusive, both non-blocking, upper first, so
two camps racing can only refuse each other and a deadlock is not
reachable.

Why an inode and not a file: a lock *file* under the environment
directory meant two environment directories naming the same repository
locked two different files and neither saw the other. Every path to one
directory is one inode. Why something *held* and not something written: a
record can go stale after `kill -9` and would then need exactly the
`--force` this tool refuses, while the kernel releases a lock when its
holder dies.

**1a. Run the `prepare:` commands**, where the configuration has any —
the environment's own programs, described in their own section below —
and then look again at what they were able to change: the
configuration's own bytes, and whether the two locks are still on the
directories the configuration names. A program running in that window can
rename a repository and put another directory in its place, and it can
edit the file camp is working from. Either refuses the composition with
nothing mounted.

**2. Validate and gate**, statically, while nothing is mounted. That
moment matters: a repository can still be repaired by hand with an
ordinary editor, and nothing anyone does can land in the wrong place.
Every refusal that can be made here is made here.

Validation plays the sequence through on paper. It walks the steps in
their own order over a virtual tree, so every mount point is judged in
the state its own step will really meet — a mount point an earlier bind
supplies counts as present, and a later mount that would silently cover
an earlier one is refused before either exists.

**3. Generate** the artefacts — the exclude, the islands lists — into
`work/`. Nothing is mounted, so the output is still only data.

**4. Validate that output as hostile data.** More on this below.

Then the mounts, in this order:

**5. The workspace, bound read-only onto its own path.** First, so there
is no window in which the lower is both visible and writable. While the
composition is up, a process inside cannot write the workspace even by
absolute path.

**6. The overlay** at the composed tree: workspace below, code repository
above, camp's work directory as the overlay's workdir.

**7. A read-only bind over every workspace root entry** that no mount
target covers and `allow_overlap` does not name. Derived from the raw
listing — no names in the configuration. This is what makes a write to a
workspace-provided path fail loudly with `EROFS` instead of copying up
into the code repository.

Per-file mounting was considered and rejected. A bind is a live view, so
protecting the directory already covers files born in the workspace
mid-session; per-file coverage would cost thousands of mounts; and a
writable store under a content directory would silently absorb stray
writes — "looks applied, exists nowhere" is the failure this design
exists to prevent.

**8. The configuration's steps**, in their declared order.

**9. Verify everything.** Any failure unmounts in reverse, names the
check that failed, and exits non-zero.

There is no teardown step: the mounts exist only inside the session's
namespace, and the kernel discards them with it when the last process
exits. The one unmount camp ever performs is that rollback of a start
that failed, and it is never lazy.

## Why `steps:` is an ordered list

Its order is the mount order, and that is what lets validation walk it.
An earlier mount's target may not lie inside a later one's, because the
later would silently cover the earlier — the covered mount stays in the
kernel's table and is reachable by nothing. Parent first, then child.

The everyday case where this bites: `git_exclude` puts its file at
`.git/info/exclude`, which is inside the `.git` mount's target. Listed
before the `.git` bind it is refused, with the reason, instead of
mounting an exclude that the next mount covers.

An earlier design had three sibling keys — `mount_ro`, `mount_rw`,
`mount_islands` — and a rule saying they ran "in file order". That was
not implementable: in YAML a sequence carries order and a mapping's keys
do not, so there was no defined interleaving between three keys at the
same level. One sequence makes the order true by definition.

## The overlap gate

If a name exists in both repositories' roots and `allow_overlap` does not
name it, the composition does not start. Directories count as much as
files: a directory overlap is a merge, and a merge is a decision about
which repository owns what.

Names a mount target covers completely are exempt, and the descent stops
there — `.git` is in both roots and always will be, and without the
exemption the gate would be unsatisfiable. Inside an allow-listed
directory the check keeps going one level down, because a file present on
both sides of a merged directory is exactly the trace of a copy-up.

There is no `--force`. The escape hatch is `allow_overlap` in the
configuration: the same decision, recorded and diffable. Nothing can wall
you in, because the repositories stay ordinary directories reachable
without camp.

This is also what makes the `.git` bind's absence safe to leave to you.
camp never adds that mount itself — a core that reaches for the name
`.git` carries git knowledge, and the generation step exists to keep git
out of the core. Leaving the entry out is not a hole: `.git` exists in
both roots, nothing covers it, and the gate refuses the composition by a
rule that knows nothing about git.

## The exclude

The read-only binds stop *writes*. git *reads* through them — so without
an exclude, `git status` in the composed tree lists every workspace name
as untracked and `git add .` stages their content into the code
repository.

camp generates one at every start and **binds it over the composed
tree's** `.git/info/exclude`. The repository's own file is never written:
outside the composition git reports exactly what it always did, and after
teardown there is no trace. The payload is the repository's own bytes,
unchanged and complete, followed by camp's block; verification compares
the mounted file against the whole of that, because a marker-only match
would accept a payload whose repository half had been dropped.

The lines are **coarse — one per workspace root name — and every one is
anchored with a leading slash.** Both halves are load-bearing.

Coarse, because a workspace file born mid-session arrives instantly
through the binds, which are live views rather than snapshots, and its
directory's line covers it automatically. A file-level enumeration would
have no line for it, in exactly the window camp cannot re-check. It is
lossless because of the zero-overlap invariant the gate re-verifies at
every start.

Anchored, because the gate compares **root entries only** — so a
workspace root name and a same-named directory deep in the code
repository never meet in that comparison. An unanchored line `scripts`
would hide new files under a real `frontend/scripts` and no gate would
ever fire. The slash is the only guard for that class.

**What the exclude does not do:** nothing prevents `git add -f`. It reads
the file through the tree and stages its bytes; the mounts stop writes,
not reads. That is detected rather than prevented — see below.

## `mount_islands`

Some directories are half repository and half machine state. What the
repository tracks belongs to it; the runtime files next to them exist on
this machine only. Neither an overlay nor a plain bind gets that right.

An islands mount covers the whole target with camp's own storage — the
*water* — and stands each entry the source **contributes** in it
read-only — the *islands*. Runtime files land in the water: machine-local,
and they survive the session. Editing a contributed entry is `EROFS`.

"Contributes" means what the source repository *tracks* there, not what
the directory happens to contain — the raw listing would hand islands to
the source's own runtime junk.

The source need not be a repository root. It may sit inside a larger
repository — one that holds the whole environment, with the workspace as
a subdirectory — and camp asks git in that repository's own frame, so the
answer is the same either way. How the repositories are arranged is your
decision, not something camp gets to require. A source in no repository
at all falls back to the directory listing, and camp says so where the
difference is decided: every entry the directory happens to hold becomes
an island, the source's own runtime files included.

This shape is not a preference. A machine-local file exists in no
repository, so a plain writable hole has nothing to bind onto — a bind
cannot create its own mount point — and creating the attachment point
through the overlay would copy the whole directory up into the code
repository. Only a store covering the entire directory can provide
attachment points without touching a repository.

The alternative that looks tidier — making the directory its own small
overlay with camp's storage as the upper — was rejected on mechanism:
there, editing a *tracked* entry copies it up silently into scratch
storage, and the change looks applied while existing in no repository.

camp records every attachment point it creates in a manifest beside the
store, written *before* the object is created. That is what lets a second
run accept its own scaffolding while still refusing to hide anything of
yours: present but unrecorded is refused, recorded but modified is
refused, and an island that disappears from the source has its still-empty
scaffold removed.

## `prepare:` — the environment's own code, before the composition

Some environments have work to do before a composition may be built at
all: check that every checkout is on the branch it should be, fetch the
rules the session will run under, refuse if a tree is dirty. None of that
is composition, and camp used to offer no place for it — so it ended up
in a wrapper script around camp, which is one more thing to write and one
more thing to get wrong.

`prepare:` is that place. It is a top-level list of programs, in the
order they run:

```yaml
prepare:
  - command: [bin/check-the-checkouts]
    timeout: 120
  - command: [bin/fetch-the-rules]
```

Each is an argv vector executed directly — never through a shell, so
nothing is split on spaces and nothing is expanded between the file and
the process. They run with the environment root as their working
directory, with `CAMP_ENV` and `CAMP_LIVE` in their environment, with
stdin on `/dev/null` and their output on your terminal. There is no
default timeout, because camp is driven from a terminal by somebody who
can interrupt it; `timeout: <seconds>` kills the command's process group
when it expires.

**They run after camp has taken the locks and before it derives the
plan.** By then camp has read the configuration, taken both locks and
made the composed tree's directory, and nothing else. After the locks, so
that whatever they check or fetch cannot be raced by a second composition
starting on the same tree — which is the job an environment's own lock
file used to do. Before the plan, because a command that changes a
repository has to be seen by the gate, the inventory and the generation
step, all of which read the repositories and have to read them as they
will be mounted.

Two things they can reach in that window are the ones camp's own
guarantees rest on, so camp looks at both afterwards: **the
configuration's bytes**, because the process that mounts reads the file
again for itself, and **the two locked directories**, because a rename
plus a new directory at the same path would leave camp holding a lock on
an inode nothing mounts. Either one changed refuses the composition, with
nothing mounted and the reason named.

**The first one that does not succeed refuses the composition**, and the
ones after it do not run. A non-zero exit, a timeout or a fatal signal
all count. Nothing has been mounted at that point — but what a command
changed before it stopped is still changed, and camp says so rather than
letting "nothing was mounted" read as "nothing happened".

They always run as you, never as root. And they are the one thing in a
real run that `camp plan` will not do for you: it lists
them and says it did not run them, because a plan that quietly performed
them would not be a plan.

This is worth being plain about. camp itself never writes to a
repository — every write it makes goes through one internal door that
cannot be pointed at one. A `prepare:` command is not camp: it is your
program, running as you, and it can write anywhere you can. That is the
point of it.

## Generation: keeping git out of the core

camp needs git knowledge in exactly two artefacts — the exclude payload
and the islands lists — and both come from **one generation step** with a
shipped default (`git_exclude`). A composition that is not git-based
simply does not list one.

A configuration may have at most one: there is one exclude payload, and
two steps claiming it cannot both be right. The custom form is
`- generate: { command: [...] }` — an argv vector executed directly,
never through a shell, with camp's scratch as its working directory so
that a naive generator's relative writes land there and not in a
repository. It always runs as the invoking user: camp holds no privilege
when it runs, and the process that does hold the mount capability, the
session's init, never executes a configured command.

**Its output is hostile data until checked.** Whoever can edit the
configuration chooses the program that runs, and the mounts that follow
are made on what it produced. So camp refuses: an entry that is not exactly one
path component, a declared type that disagrees with what is really there,
a name the source does not have, a duplicate, an unsupported type, or an
exclude payload that is not byte-identical to camp's own assembly of it.
Then the order and tracked-content rules re-run over the mounts the
expansion created.

## The inventory

`camp accept` records both repositories' root entries, with their types.
Only that command writes it — never a start, because a refresh on the way
past would swallow the very signal the file exists to raise.

At every start a new workspace root entry **blocks** and a type change
**blocks**: both change what the derived binds protect and what the
exclude covers, and both were derived from the snapshot you accepted. A
disappearance, or a change on the code side, only warns.

The file is one record per line, byte-sorted, so its diff is meant to be
read. Every record camp writes uses one reversible escaping, because a
Linux name may legally contain spaces, tabs and bytes that are not valid
UTF-8 — and a snapshot that cannot represent what it saw will one day
report a change that did not happen.

## Verification

Two measured facts shape the whole of it. A covered mount stays listed in
`/proc/self/mountinfo` while being unreachable by any path, so presence
in the table proves nothing about what a process sees. And path-based
syscalls see exactly what a process would. **The path is the authority;
the mount table is the cross-check.**

The second habit: never trust the call, inspect the result.
`MS_BIND|MS_RDONLY` in one `mount(2)` silently ignores the read-only
flag, so a bind that reports success can be writable. Every read-only
bind is therefore two calls, and `statvfs` is what decides afterwards.

After mounting, before declaring anything up, camp checks each mount for:

- **identity** — for a bind, source and target must be the same device
  and inode. This one check also catches every ordering and shadowing
  mistake, because a covered mount fails it;
- **writability** — `statvfs` matches the plan, which is what catches a
  one-step read-only bind;
- **propagation** — every mount private. Mounts propagate by default on a
  systemd machine, and a propagating mount inside the composed tree
  travels back out onto the backing store's own path;
- **the generated exclude** — byte-equal to the validated payload;
- **completeness** — the set of mounts present equals the plan exactly.
  Fewer means a mount failed; more means residue, or interference;
- **ownership** — camp's storage belongs to the invoking user.

`camp status` is this same pass run read-only: one code path, two exits.
It answers for the process that runs it, which from outside every session
is "nothing is mounted" -- true, and the reason it is worth saying.

## The session

The composition is built inside a user and mount namespace, and in no
other way. No privilege is needed; nothing outside can see it; and when
the last process exits the kernel discards the namespace and every mount
in it. Teardown cannot fail and there is nothing to take down.

A session is two processes. The **launcher** locks, validates, gates and
generates — all as you, with nothing privileged existing yet. The
**init** is camp resident as the first process of a new pid namespace: it
maps your uid to itself, gives the namespace its own `/proc`, makes the
mounts, verifies them, gives back the mount capability, and only then
starts what you asked for. It stays for the whole session, reaping
everything that reparents to it and holding the locks.

That last part is why it exists. A daemonising program routinely closes
the descriptors it inherited, so a design that let the workload carry the
locks would be trusting the workload's habits. And it is what makes
`camp run -- tmux new-session -d` return at once while the composition
stays open.

Which means a session can outlive the terminal it was started from, and
you may want one gone. **Send `SIGTERM` to that init.** It means "end
this session", and it reaches every process inside the namespace and
nothing outside it; a `SIGCONT` follows, because a stopped process holds
the request pending and would otherwise never act on it. The composition
comes down when the last of them goes, and the kernel takes the mounts
with it. Nothing is escalated to `SIGKILL` — camp asks, once, and a
program that ignores the request keeps the session alive on purpose or on
a bug, which is yours to look at rather than camp's to overrule. If you
do not know the pid, ask for a second session in the same tree: camp
refuses and names what is holding it, by pid and command.

That is the contract of a session that is *running*. A signal arriving
while camp is still mounting meets no supervisor: it ends the init, and
the kernel discards the namespace with every mount in it — which is the
right answer to "stop" at that point, and a different one.

Your uid maps to itself rather than to 0, so `id` inside shows the real
user and files you create are yours. The cost is that the effective uid
is not zero, so `execve` drops every capability — which is why the mount
capability is carried in the ambient set and dropped as soon as the
mounts are verified. The overlay keeps working afterwards because the
kernel recorded the mounter's credentials at mount time.

The other cost is that exactly one id is mapped, so every file on the
machine owned by anyone else — root included — is shown inside with the
kernel's overflow id, as `nobody`. Reading and writing are unaffected;
what changes is what a program sees when it asks who owns a file. This is
not a gap to be closed: an unprivileged process may map the ids it owns,
its own and any range assigned to it in `/etc/subuid`, and host root's is
not one of them. That is the property the mode rests on.

Where you meet it is ssh, which refuses a system-wide configuration file
it cannot attribute to root or to you — so `ssh` and `git push` over ssh
fail inside a session until ssh is pointed at your own configuration
with `-F`, which is also what makes it skip the system-wide one.

That repair belongs to the composition rather than to the machine, and
the `session:` section of the configuration is where it goes: a
`GIT_SSH_COMMAND` declaration covers git, and a launcher directory in the
workspace repository, prepended to the session's `PATH`, covers `ssh`,
`scp` and `sftp` typed by hand. camp carries no program's name and writes
no launcher; it applies what the configuration declares, and the next
program that breaks the same way is fixed by the same key.
[install.md](install.md) has the complete arrangement.

Two other members of the same class need saying, because they are not
repaired by any variable. Rootless mode has no elevation at all — a
setuid binary owned by an unmapped id confers nothing, so `sudo` and
`pkexec` cannot work inside a session. And a program that *records*
ownership into what it builds — `tar`, `rsync -a`, an image build — will
write `65534` for a file that is root's outside, with nothing failing and
nothing warning. Build such an artefact outside a session, from the
repositories' own directories; nothing camp offers keeps the real owners
inside one. `camp explain` says all of this in its "Ownership view"
section.

**What a session cannot do, plainly.** A program started outside it
cannot see the composed tree: the mounts exist only in the session's
namespace, so an editor already running, a language server, a daemon, sees
the composed tree's directory empty. Start it inside -- `camp run --
<program>` -- and it sees the tree. The tree is never visible machine-wide,
and no record of a session survives it: when the last process exits the
kernel takes the mounts, and there is nothing left to describe or to take
apart. camp once had a second way of running that mounted the tree for
the whole machine, with root, for exactly the already-running editor. It
was removed on 2026-09-02: nothing used it, every change to the session
would have had to be designed and measured twice, and it carried the
whole `sudo` surface -- a root helper, records of what was mounted, a
teardown command and the recovery story around them. What that gives up
is what this paragraph names, and it is given up knowingly.

## Rollback

A start that fails after its first mount unwinds what it made, in reverse,
before the refusal is reported. There is no lazy unmount anywhere in camp:
a mount that cannot be removed stays mounted, is named in the refusal, and
goes when the session's namespace goes. A detached mount would leave the
kernel's table while it is still alive and still being written through,
and a rollback that used one would report a clean namespace over a mount
something is still writing to.

Nothing else is ever unmounted by camp. A composition that came up ends
with its session, and the kernel takes every mount with it.

## What a session reports when it ends

Four read-only scans, at the end of a session -- the moment when the
cause is still fresh. They never block; there is nothing left to refuse
with.

The gate's comparison and the inventory's re-run, so a change made during
the session is named the same day. The code repository's untracked paths
whose names belong to the workspace, as suspected copy-up residue. And
the index — `git ls-files --stage` — because a forced add leaves an
*indexed* path with no working-tree file at all, which no untracked scan
can see.

Worktrees registered inside the composed tree get their exact repair
command. Git records a worktree's git directory as an absolute path and
compares it as a string, so one created inside the composition stops
resolving when the composition comes down; the files are fine, git simply
cannot see them. Left unrepaired, git prunes the registration at an
ordinary auto-gc after three months — the failure that happens while
nobody is looking.

A namespace session has nowhere to print this by the time its last window
closes, so it writes it to a file, and the next camp command in that
environment prints it once and marks it read.

## What camp is not

**Not a security boundary.** The read-only mounts prevent accidental
writes and copy-up. A process inside can still walk to the backing
directories and read anything on the machine. camp does not pretend to
sandbox.

**Not a filesystem.** Writing the composition as a FUSE filesystem was
considered and rejected. A bad mount composition produces a wrong *view*,
which is visible and reversible; a bad filesystem produces wrong *data on
disk*, and git is the least forgiving client there is. A deadlocked FUSE
daemon hangs the mountpoint and blocks the editor, the language server
and every agent uninterruptibly, which a kernel mount cannot do.

**Not something that modifies your repositories.** Every filesystem write
camp makes goes through one package whose addressing cannot be
constructed from a repository path, and a test fails the build if a write
appears anywhere else.
