# How camp works

This describes the mechanism: what a composition is, what camp mounts, in
what order, why each piece is there, what it checks afterwards, and what
a session does from the moment you type `camp shell` to its last message.
Read it before trusting the tool with anything you care about — and read
it if a refusal ever surprises you, because most of them are one of the
rules below. A reader who has finished it should be able to predict what
camp will do.

Nothing here is aspirational. Where a sentence says a kernel or git
behaves in a particular way, that behaviour was measured; where a design
choice looks odd, the reason is the paragraph next to it.

## What a composition is

A composition is several ordinary directories — usually git
repositories — presented as one, for the processes camp starts and for
nothing else. No file is copied and nothing is generated into any
repository: the kernel is asked to show the directories together, and the
kernel is asked to refuse the writes that would land in the wrong one.

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
because the two stores are named from it. Both stores carry a marker
file, `.camp-target`, naming the composed tree and the configuration they
were made for — that is how the sweep and `camp doctor` tell camp's own
directories from anything else.

The two stores never share a parent, because their lifecycles are
opposite. `work/` may be swept whenever nothing is using it. `storage/`
holds unfinished worktrees and machine-local files and is **never**
removed by camp.

### Where a path in the tree comes from

Every path in a composed tree has a real file behind it in one of four
places, and `camp explain` says which for every path of the tree that is
actually running.

**The code repository — almost everywhere.** The overlay's upper layer,
the product, and where every ordinary write lands. A file you create
anywhere the composition has not covered is a file in that repository.

**The workspace's root names — read-only.** An entry at the workspace
repository's root is shown in the tree and held read-only by a bind mount
over it: a directory bind for a directory, a file bind for a file. Writing
one through the tree fails with `EROFS`. Without the bind the write would
succeed — an overlay copies a lower file up into the upper before
changing it — and the change would look applied while living in the
product's history. Two kinds of root name get no bind: a name a mount
target covers completely, because the mount stands there instead; and a
name in `allow_overlap`, which stays an ordinary overlay merge — for a
file, the code repository's copy shows and the workspace's is shadowed;
for a directory, the two directories' entries are unioned.

**Machine-local storage — writable, and in no repository.** Runtime
files, local settings, worktrees: what a tool writes next to its
configuration and nobody wants committed. camp provides it from
`storage/<id>/`, and it survives the session.

**Another repository, mounted writable at its own path** — a record
repository, a design record. Writes there land in that repository, and
`camp explain` names it.

### What is yours, and what is camp's

Of everything camp keeps in `.camp`, exactly one thing is yours to edit.
`camp init` writes three files there — `config.yml`, a README saying
which of the things in the directory are yours, because that is the only
place a person meets the answer without going looking for it, and a
`.gitignore` that keeps camp's four directories out of version control
while leaving `config.yml` and `inventory` in. `inventory` appears at the
first `camp accept`; the four directories are made when something first
has to be written into them.

| | what it is | who writes it | committed? |
|---|---|---|---|
| `config.yml` | **yours** — what you want composed | you, by hand | yes |
| `inventory` | the accepted snapshot of both repositories' root entries | `camp accept`, and nothing else | yes |
| `work/` | scratch for one composition | camp; swept when nothing uses it | no |
| `storage/` | machine-local files and worktrees | camp — and never removed by it | no |
| `reports/` | what a session found when it ended | camp's init | no |
| `logs/` | every line camp printed, with the time in front of it | every camp command | no |

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

The log is always written: `.camp/logs/camp.log`, rotated by size, three
old files kept. Nothing switches it on, because a log you have to
remember to switch on is missing on exactly the run that surprised you.
What it holds is what a command says *about a run* on stderr — the step
lines, warnings, refusals, the session's end — from the moment the
command knows which environment it is working in. A command's product on
stdout — a plan, a status, an explanation — is not copied into it, and
neither is `camp help` or `camp --version`, which belong to no environment.

One thing in `work/` looks alarming and is not: a directory with no
permissions at all, owned by you. OverlayFS creates it inside the workdir
it is given and makes it unreadable so that nothing wanders into it. It
holds nothing of yours. camp cannot remove it while the composition is
up — it is in use — and a session has no teardown step of its own, so the
next start sweeps it.

## The namespaces

The composition is built inside three Linux namespaces that camp creates
in one `clone`, and in no other way. Each is there for one reason.

**A user namespace, for the capability.** Mounting needs `CAP_SYS_ADMIN`.
A process that creates a user namespace holds every capability inside it
and none outside, which is what lets camp mount without any privilege on
the machine. By default no other program is involved: your own uid maps to
itself, declared on the clone and written by the kernel, so `id` inside
shows the real user and the files you create are yours. The cost is that
only your id is mapped, and every file owned by anyone else — root
included — appears inside as `nobody`; the section on identity below says
what that breaks and what to do about it. The other route, `identity:
uidmap` in the configuration, maps a whole range in podman's `keep-id`
shape through the setuid helpers `newuidmap` and `newgidmap` and your
subordinate ranges in `/etc/subuid` and `/etc/subgid`; camp refuses to
start that route when the two programs are missing, and `camp doctor`
does not check for them.

**A mount namespace, for the tree.** Every mount camp makes exists in this
namespace and nowhere else. That is why no privilege is needed to leave
nothing behind: when the last process in the namespace is gone the kernel
discards the namespace and every mount in it, and nothing on the machine
ever saw them. It is also the one limit of the design — a program that
was not started inside the session cannot see the tree, because the tree
is not in its namespace.

**A pid namespace, so that camp's init is process 1.** camp stays
resident inside the session as the namespace's first process. Process 1
of a pid namespace is the one every orphan reparents to, so a program
that daemonises still has camp as its parent and camp still sees it
exit; a `kill(-1, …)` from process 1 reaches every process in the
namespace and nothing outside it, which is what makes ending a session a
single, safe request; and the kernel ends every process left in a pid
namespace when its first process exits, which is what makes teardown
unable to fail. `/proc` is mounted afresh inside, so the pids camp
reports are the namespace's own — what `ps` inside shows.

The honest comparison a reader will reach for: **this is a thin
container, and it is not a sandbox.** There is no image — the session
sees the machine's own root filesystem, your home directory, every device
and every path exactly as they are, plus camp's mounts. There is no
network or hostname isolation, no IPC namespace, no cgroup: a server
started inside listens on the machine's ports, and nothing limits what a
process inside may consume. The read-only mounts stop accidental writes
and copy-up. They do not stop a process inside from walking to the raw
repositories and reading them, or from reading anything else on the
machine. `camp explain` says the same thing from inside.

## The start, in order

A start is two processes. The **launcher** is the `camp shell` or
`camp run` you typed. It does everything that can be done while nothing is
mounted, all as you, with nothing privileged existing yet. It then clones
the **init** into the three namespaces and hands it the locks; the init
mounts, verifies, gives the capability back, and starts what you asked
for.

```
 you                          the launcher (as you, no namespace)      the init (pid 1 of the session)
 ────                         ─────────────────────────────────       ───────────────────────────────
 camp shell ───────────────▶  refuse if run from inside a session
                              sweep stale work directories (under the work lock)
                              work lock; make the live directory; lock upper, live; release work
                              run prepare: commands, re-check
                              validate and gate, on paper
                              generate the exclude and islands
                              check that output as hostile data
                              clone ──────────────────────────────▶   write uid/gid maps
                              (wait for "up" on the pipe)             mount a private /proc
                                                                      mount, in order; verify
                                                                      drop the capability
                                                                      start the shell or command
                              "up" ◀──────────────────────────────    supervise; reap; forward signals
                              (wait for the init to exit)             … the workload exits …
                                                                      end-of-session report
                                                                      name, ask, wait, exit
 prompt returns ◀──────────── the workload's exit status              (the kernel takes the rest)
```

### What the launcher does

**Refuse to start from inside a session.** Before anything else, the
launcher reads `/proc/1/cmdline`. Inside a session process 1 is camp's
own init, and its command line names the configuration it was started
for; outside, process 1 is the machine's init. If process 1 is the init
for this same configuration, the command was typed into the session it
would compose, and it is refused (`inside-session`) with the way out:
exit this shell, or use a terminal that is not inside a session. If
process 1 is a camp init whose configuration cannot be resolved — what a
renamed environment directory looks like from inside — camp cannot tell
whether it is this one, and refuses that too (`inside-session-unresolved`)
rather than guess.

This check comes first because from inside, the two facts a start
otherwise rests on are false: `/proc/locks` shows no lock whose holder is
outside the reader's pid namespace, and the composed tree's path is the
overlay's root — a different inode from the directory the launcher
locked — so a lock taken on it succeeds. A start from inside that went on
to the sweep below judged the running session ended and swept its
overlay's work directory out from under it. Measured, and the reason for
the order.

**Sweep the work directories that finished sessions left.** A session
has no teardown step, so its `work/<id>/` outlives it and the next start
removes it. Which entries are stale is decided in a fixed order, under
the work lock (below), and the order is the point:

1. **The mount table first.** An entry whose `work/<id>/work` some
   overlay in this process's mount table names as its `workdir=` is in
   use, whatever any lock says, and is skipped without a word.
2. **Then the marker.** An entry is stale when the composed tree its
   `.camp-target` names no longer exists, or when a non-blocking lock on
   that directory succeeds — nobody is composing there.

An entry camp cannot judge is kept and said: a marker that cannot be
read, a `workdir=` spelled in a way camp cannot compare (relative,
unclean, through a symlink camp will not follow, or carrying an escape
the kernel never writes). Each is printed as `[WARN] left alone: …` with
the reason, because not knowing is not stale. A `workdir=` under a tree
camp is not allowed to read is passed over: camp resolves its own from
the environment root down, so an unreadable one is not camp's — a
container runtime's overlays are the usual case. Every removal is
printed: `[OK] swept: …, left by a session that has ended`.

**Take the locks, and make the composed tree's directory.** All three
locks are `flock` on a directory's own inode, exclusive and non-blocking;
the section on locks below has the table. The sweep above ran under the
work lock and released it. Now the launcher takes the work lock again,
makes the composed tree's directory when it is absent — the one directory
camp makes for you, because git cannot record an empty directory and a
clone of an environment could never bring one; printed as `[OK] created:
…, the composed tree's directory` whenever that happens — then takes the
lock on the code repository and the lock on that directory, in that order,
and releases the work lock. The live directory has to exist before its
lock can, which is why the work lock spans its creation: between the two,
a sweep looking at that moment would take this launcher's work directory
for one a finished session left. If the directory already exists it has
to be a real directory, not a symlink, and empty: an overlay over content
would hide it for the whole session.

**Run the `prepare:` commands**, where the configuration has any — the
environment's own programs, described in their own section below — and
then look again at what they were able to change: the configuration's own
bytes, and whether the two locks are still on the directories the
configuration names. Either changed refuses the composition with nothing
mounted.

**Validate and gate**, statically, while nothing is mounted. That moment
matters: a repository can still be repaired by hand with an ordinary
editor, and nothing anyone does can land in the wrong place. Every
refusal that can be made here is made here. Validation plays the sequence
through on paper: it walks the steps in their own order over a virtual
tree, so every mount point is judged in the state its own step will
really meet — a mount point an earlier bind supplies counts as present,
and a later mount that would silently cover an earlier one is refused
before either exists. It also reads this process's mount table for
residue: a mount already standing under the composed tree, on the
workspace's path or on the code repository's path cannot be a session's
— a session's mounts are invisible from outside — so camp did not make
it, and refuses until it is unmounted the way it was mounted.

**Generate** the artefacts — the exclude, the islands lists — into
`work/<id>/`. Nothing is mounted, so the output is still only data.

**Validate that output as hostile data.** Whoever can edit the
configuration chooses the program that generated it; the section on
generation says what is checked.

**Clone the init** into a new user, mount and pid namespace, with a pipe
between the two and the lock descriptors inherited. The uid and gid maps
are the launcher's doing, not the init's: on the default route they are
declared on the clone — your uid and gid to themselves, `setgroups`
denied, which the kernel requires for writing an own-gid map without a
helper — and the kernel writes them; on the `uidmap` route the launcher
runs `newuidmap` and `newgidmap` on the child once it exists. A lock lives
on the open file description, so the inherited descriptors carry it; once
the init confirms, the launcher closes its own copies and waits.

Each of these is printed on stderr as it completes, in the order it
happened, with a marker in the first column:

```
[OK]    locks: /home/you/work/shop, /home/you/work/shop-live
[OK]    prepare: 2 command(s) the configuration names, all succeeded
[OK]    checked: 7 mounts, gate clean, nothing refused
[OK]    generated: the exclude and the islands lists
```

`[WARN]` lines between them are the inventory's warnings — a workspace
root entry that disappeared since the snapshot, a change on the code
side; things worth knowing that stop nothing. A refusal is printed in the
same columns, as `[ERROR]`.

### What the init does

The init is this same binary, re-executed as process 1 of the session.
Before it mounts anything it checks that it *is* pid 1 and refuses
otherwise — the fan-out it will use to end the session is `kill(-1)`,
which outside a pid namespace would reach everything you own. It marks the
three inherited descriptors — the pipe and the two locks — close-on-exec,
so that the shell or command gets no copy: a workload that inherited the
code repository's lock descriptor could write the repository through
`/proc/self/fd/4/…`, because a path resolved through a descriptor uses
the mount the descriptor was opened on, from before the freeze below
existed. Measured; the descriptors stay with the init.

Then, in order:

1. **Re-derive the plan** from the configuration and check that it is
   about the two directories the inherited locks are on — by inode, never
   by path. The launcher validated the same thing, but the file was read
   again in between; a plan that mounted one upper while camp held the
   lock for another is exactly what the locks exist to make impossible.
2. **Resolve the session's environment** from the `session:` section
   against the environment the launcher was started with. Nothing
   declared is installed on the init itself: a configured `PATH` must
   never steer a process that still holds the mount capability.
3. **Make mount propagation private** for the whole namespace, then
   **mount a fresh `/proc`**. Mounts propagate by default on a systemd
   machine, and without the first step a mount made inside would travel
   back out onto the backing directory's own path; without the second,
   every pid camp reports would be the outside world's.
4. **Mount, in order**, and **verify** — the two sections that follow.
5. **Drop the mount capability**, and read the capability sets back to
   prove it is gone; a drop that reported success while the process
   still held the capability starts no workload.
6. **Start the shell or the command**, in the composed tree, with the
   resolved environment. An empty argument list means a shell, chosen
   from the effective `SHELL`; a bare command name resolves against the
   effective `PATH`, so a launcher directory the configuration prepends is
   honoured — the composed tree is standing by then, which is why the
   lookup waits until here. Only now does the init report "up" to the
   launcher.

The init narrates its own half on the same stderr, in the same columns,
and into the same log:

```
[OK]    identity: uid 1000 and gid 1000 map to themselves
[NOTE]  only your own id is mapped, so files owned by anyone else show as nobody
[OK]    mounted: 7 at /home/you/work/shop-live
[OK]    verified: 7 mounts at /home/you/work/shop-live
[OK]    environment: GIT_SSH_COMMAND, PATH, OUTER_PATH
```

The environment line names the variables the configuration declared and
never their values: what a variable holds is between the configuration,
the terminal, and the workload, and this output is routinely captured.

## The mount sequence

A run has two halves: a **frame** that always executes in a fixed order,
and the configuration's own **steps** in the middle of it. The split is
the point — everything the safety rests on is frame, so no configuration
can move it, weaken it or leave it out. Every mount is made private as it
is created.

```
                       outside the tree                      inside the tree
                       ────────────────                      ───────────────
   1. FREEZE           <workspace>  ro bind onto itself
   2. OVERLAY                                                <merged>   lower=workspace, upper=code
   3. ROOT GUARDS                                            <merged>/<each workspace root name>  ro
   4. STEPS                                                  <merged>/.git         rw bind
                                                             <merged>/.records     rw bind
                                                             <merged>/.claude      islands
                                                             <merged>/.git/info/exclude  ro bind
   5. FREEZE           <code>       ro bind onto itself
   6. VERIFY           every one of the above, by path and against the table
```

**1. The workspace, bound read-only onto its own path.** First, so there
is no window in which the lower is both visible and writable. While the
composition is up, a process inside cannot write the workspace even by
absolute path. A read-only bind is two calls — `MS_BIND`, then a remount
with `MS_RDONLY` — because a single `MS_BIND|MS_RDONLY` silently ignores
the read-only flag; the remount also carries whatever flags the kernel has
locked on the source mount (`nosuid`, `nodev`, `noexec`, the atime flags),
without which a remount inside a user namespace fails with `EPERM`.

**2. The overlay** at the composed tree: workspace below, code repository
above, camp's work directory as the overlay's workdir, `userxattr` as the
xattr namespace an unprivileged overlay has to use. It is made through
the kernel's mount API — `fsopen`, `fsconfig`, `fsmount`, `move_mount` —
rather than an option string: each layer is given to the kernel as an
open descriptor, one `fsconfig` call per layer (`lowerdir+`), and the
kernel records the layers' real paths in the mount table, which is what
lets verification and a person reading `/proc/self/mountinfo` see what
was mounted. `fsopen` and `move_mount` are Linux 5.2; giving an overlay
its layers this way is 6.7. There is no fallback to the option string.

**3. A read-only bind over every workspace root entry** that no mount
target covers and `allow_overlap` does not name. Derived from the raw
listing — no names in the configuration. This is what makes a write to a
workspace-provided path fail loudly with `EROFS` instead of copying up
into the code repository.

The guard is one bind per root name, not one per file. A bind is a live
view, so protecting a directory already covers a file born in it
mid-session, where a per-file list would have no entry for it in exactly
the window camp cannot re-check. And a content directory refuses writes
rather than absorbing them into storage: a write that is accepted and
lands in no repository — "looks applied, exists nowhere" — is the failure
this design exists to prevent.

**4. The configuration's steps**, in their declared order.

**5. The code repository, bound read-only onto its own path.** Last, and
the position is forced from both sides. The overlay treats its layers as
its own: a path the tree has resolved is an object the overlay holds, and
a rename over that path in the raw upper — how git and every editor
write — replaces the file behind the overlay's back, so the tree shows
the old content there for the rest of the session and fails the next
delete with `Stale file handle`. Inside the session the raw path now
answers `EROFS`; a `git commit` typed at the raw path fails on
`.git/index.lock`, which is the intended effect, while read-only git
commands with `--no-optional-locks` keep working there. It cannot come
earlier: the kernel refuses to mount an overlay over a read-only upper at
all, and a bind cut from a read-only mount inherits the flag, so the
`.git` bind — and every other step sourcing from the code repository —
has to exist before the freeze does. And the guard exists only inside the
session: a process outside can still write the raw path, with exactly the
effect above.

**6. Verify everything.** Any failure unmounts in reverse, names the
check that failed, and exits non-zero.

There is no teardown step: the mounts exist only inside the session's
namespace, and the kernel discards them with it when the session ends.
The one unmount camp ever performs is that rollback of a start that
failed, and it is never lazy.

### Where the checks stop and the mount begins

Every path camp checks — a source, a target, every component between the
environment root and it — is opened descriptor-relative with
`openat2(RESOLVE_NO_SYMLINKS | RESOLVE_BENEATH)`, and a symbolic link
anywhere in a mount operand refuses the composition. A component the
user owns can be swapped between a check and a mount, and refusing the
whole class is the only check without a race in it.

The mount itself resolves names again, without those flags. A bind is
`mount(2)` given the source and target as paths, which the kernel
resolves. The overlay's layers are opened by name at mount time and
handed to `fsconfig` as descriptors, and the assembled overlay is
attached with `move_mount` to a mount point opened by name just before —
so the kernel's table records the real directories, but the names were
resolved once more between the check and the mount. That split is honest
for exactly one reason, and it is written here so the split cannot
outlive it: there is no privilege boundary between the check and the
mount. The process that mounts is you, in a mount namespace only your own
processes can enter, so a component swapped between the two gains nobody
anything you could not do by hand — and verification measures the outcome
by path afterwards, however the names resolved.

## Why `steps:` is an ordered list

Its order is the mount order, and that is what lets validation walk it.
An earlier mount's target may not lie inside a later one's, because the
later would silently cover the earlier — the covered mount stays in the
kernel's table and is reachable by nothing. Parent first, then child.

The everyday case where this bites: `git_exclude` puts its file at
`.git/info/exclude`, which is inside the `.git` mount's target. Listed
before the `.git` bind it is refused, with the reason, instead of
mounting an exclude that the next mount covers.

It is one sequence rather than one key per kind because in YAML a
sequence carries order and a mapping's keys do not: three sibling keys
would have no defined interleaving, and the order the mounts are made in
is the one thing this section has to define.

| kind | what it does |
|---|---|
| `mount_ro` | a source, read-only, at a target |
| `mount_rw` | a source, writable — or, with no source, an empty writable hole backed by camp's storage |
| `mount_islands` | a writable machine-local floor, with the source's *contributed* entries standing in it read-only |
| `git_exclude` | the shipped generation step: reads git, produces the exclude and the islands expansions |
| `generate` | the same contract with a program of your own |

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
the session there is no trace. The payload is the repository's own bytes,
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
not reads. That is detected rather than prevented — see the end of a
session, below.

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

And it is a store with islands standing in it, not a second small
overlay with camp's storage as the upper, because in an overlay editing a
*tracked* entry copies it up silently into scratch storage — the change
looks applied while existing in no repository, which is the failure the
whole arrangement exists to prevent.

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
is composition, and none of it should need a wrapper script around camp —
one more thing to write and one more thing to get wrong.

`prepare:` is the place for it: a top-level list of programs, in the
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
plan.** By then camp has read the configuration, taken the locks and
made the composed tree's directory, and nothing else. After the locks, so
that whatever they check or fetch cannot be raced by a second composition
starting on the same tree. Before the plan, because a command that changes
a repository has to be seen by the gate, the inventory and the generation
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

They always run as you, never with the mount capability. And they are
the one thing in a real run that `camp plan` will not do for you: it lists
them and says it did not run them, because a plan that quietly performed
them would not be a plan. They do not run for a join, either: a join
enters a composition that already exists.

This is worth being plain about. camp itself never writes to a
repository — every write it makes goes through one internal door that
cannot be pointed at one. A `prepare:` command is not camp: it is your
program, running as you, and it can write anywhere you can. That is the
point of it.

## Generation: keeping git out of the core

camp needs git knowledge in exactly two artefacts — the exclude payload
and the islands lists — and both come from **one generation step** with a
shipped default (`git_exclude`). A composition that is not git-based
simply does not list one — though camp still needs `git` installed, for
the one question planning asks of the code repository, at every start and
in `plan`, `status`, `doctor` and `explain`: what it tracks under each
mount target, which is the rule that no mount may cover tracked content.
Without git that question has no answer, and a check that could not run
is not a check that passed.

A configuration may have at most one generation step: there is one
exclude payload, and two steps claiming it cannot both be right. The
custom form is `- generate: { command: [...] }` — an argv vector executed
directly, never through a shell, with camp's scratch as its working
directory so that a naive generator's relative writes land there and not
in a repository. It always runs as you, in the launcher: the process that
holds the mount capability, the init, never executes a configured
command.

**Its output is hostile data until checked.** Whoever can edit the
configuration chooses the program that runs, and the mounts that follow
are made on what it produced. So camp refuses: an entry that is not
exactly one path component, a declared type that disagrees with what is
really there, a name the source does not have, a duplicate, an
unsupported type, or an exclude payload that is not byte-identical to
camp's own assembly of it. Then the order and tracked-content rules
re-run over the mounts the expansion created. The init checks the output
once more, against the repositories themselves, before it mounts it.

## The inventory

`camp accept` records both repositories' root entries, with their types.
Only that command writes it — never a start, because a refresh on the way
past would swallow the very signal the file exists to raise.

At every start a new workspace root entry **blocks** and a type change
**blocks**: both change what the derived binds protect and what the
exclude covers, and both were derived from the snapshot you accepted. A
disappearance, or a change on the code side, only warns — a `[WARN]` line
on the start's own output.

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

- **identity** — for a bind, `stat` on the target and on the source
  answer the same device and inode; for the overlay, `statfs` at the
  composed tree answers the overlay's magic, and the table's entry there
  carries the planned `lowerdir`, `upperdir`, `workdir` and `userxattr`,
  compared option by option because the kernel appends its own. This one
  check also catches every ordering and shadowing mistake, because a
  covered mount fails it;
- **writability** — `statvfs` reports `ST_RDONLY` exactly where the plan
  says read-only, which is what catches a one-step read-only bind. With
  the code repository's freeze in the plan, the same check proves the
  frame's order: the raw path read-only, the overlay and every writable
  bind still writable;
- **propagation** — every camp mount private, read from the table's
  optional fields. A propagating mount inside the composed tree travels
  back out onto the backing store's own path;
- **the generated exclude** — byte-equal to the validated payload;
- **completeness** — the set of mounts under the composed tree, plus the
  two self-binds at the workspace's and the code repository's paths,
  equals the plan exactly. Fewer means a mount failed; more means residue,
  or interference;
- **ownership** — camp's storage belongs to you.

Each failure has a stable rule name — nineteen in all, `verify-identity`,
`verify-read-only`, `verify-writable`, `verify-exclude`,
`verify-missing-mounts`, `verify-extra-mounts`, `verify-propagation` and
`verify-storage-owner` among them, with `bind-source-replaced` for a file
bind whose source has been replaced by rename and a `-unreachable` or
`-unreadable` form for each thing camp could not look at — and a message
that says what is true on each side. `camp status` is this same pass run read-only,
one code path with the other exit; the section on what a running
composition cannot absorb says what else it does.

## The locks

A composition is guarded by three locks, all `flock` on a directory's
own inode — a descriptor for the directory itself, not a lock file
anywhere. Each is exclusive and non-blocking, and they are taken in one
order, so two camps racing can only refuse each other and a deadlock is
not reachable.

| lock | on | held by | for how long | what it protects |
|---|---|---|---|---|
| work | `.camp/work`, camp's work area | a launcher, only | from reading the mount table to the last removal of a sweep; from making the composed tree's directory to taking the live lock | the sweep's decision and its removal being one moment |
| upper | the code repository's directory | the launcher, then the init | the whole session | one upper, one overlay — the kernel allows a second and it corrupts data |
| live | the composed tree's directory | the launcher, then the init | the whole session | one composition per tree; `work/<id>` is keyed from it |

Why an inode and not a file: a lock *file* under the environment
directory meant two environment directories naming the same repository
locked two different files and neither saw the other. Every path to one
directory is one inode, symlinks included. Why something *held* and not
something written: a record can go stale after `kill -9` and would then
need exactly the `--force` this tool refuses, while the kernel releases a
lock when its holder dies. Locking writes nothing into the directory — no
entry appears, no timestamp changes — which is why locking the code
repository does not touch the rule that camp never modifies a repository.

**The two session locks live on the init**, camp's process 1 inside the
session, for exactly as long as the composition exists. The launcher
takes them and hands the descriptors over; the workload gets no copy. A
daemonising program routinely closes the descriptors it inherited, so a
design that let the workload carry the locks would be trusting the
workload's habits. The init holds them until it exits, and the kernel
releases them then, whatever happened to it. No staleness is possible.

**The work lock is never held by a session.** It exists for an
interleaving the two session locks cannot close: a launcher that had read
the mount table, probed a stale entry's lock and been descheduled would
otherwise remove a work directory a second launcher had swept, recreated
and mounted on in between — in a namespace the first one's table cannot
see. It is only ever met by another camp in the middle of a creation or a
removal, and its refusal (`work-locked`) says to wait and run again.

No directory of yours other than those two is ever locked: the
workspace, a record repository and the other mount sources are ordinary
git-level parallelism, and several different compositions — different
code repositories, different composed trees — run side by side. camp's
own `.camp/logs` and `.camp/reports` are also `flock`ed, by whichever
command is writing into them and only for the moment of the write, so
two camp processes appending to one log or leaving one report cannot
tear each other's output.

**The refusal.** A second `camp shell` on a running composition meets
`upper-locked` or `live-locked`. The message names the directory, says
why one composition per directory is the rule, and names the holder: camp
finds it without parsing any program's output, from the `FLOCK` rows of
`/proc/locks` matched against the directory's device and inode, and from
`/proc/<pid>/fd`, because the row names the pid that *took* the lock —
the launcher, long gone — and the descriptors name whoever holds it now.
When the holder is camp's own init, the message gives the command and
says whose move it is: `kill -TERM <pid>`, and what camp does with it —
the request goes to every process inside, the shell or command exits, the
session ends and releases the lock. `kill -9` of the init is never
needed, and it is not a harmless shortcut: the init is process 1 of the
session's pid namespace, and when process 1 dies the kernel ends every
other process in the namespace with `SIGKILL` at once — no `SIGTERM`, no
ten seconds, nothing saved — and the end-of-session report is never
written. `kill -TERM` is the instrument because it goes through the init,
which asks first and waits. The way in is the running session, or `camp
shell --join`, not a second composition.

## The life of a session

From your terminal, a session looks like this.

**You type `camp shell`** (or `camp run -- <command>`). The launcher's
`[OK]` lines appear, then the init's; the terminal's title becomes
`camp: <environment>` for as long as the session runs, put back when it
ends (a shell whose startup file writes its own title, as Debian's
default `bashrc` does, overwrites it at its first prompt; a `camp run --
<program>` that writes no titles keeps it); and the shell's prompt appears
in the composed tree. For a `camp run`, the command starts there instead. Any end-of-session report a previous session left unread
is printed first — every camp command in that environment does that,
once, then marks it read.

**While it runs**, the init sits above the workload doing three things.
It reaps everything that reparents to it. It ignores `SIGINT`, `SIGQUIT`,
`SIGTTIN` and `SIGTTOU`, so a Ctrl-C reaches the workload — which owns the
terminal's foreground — and never the supervisor holding the locks. And
it treats `SIGTERM` or `SIGHUP` delivered to itself as "end this session":
the signal fans out to every process in the pid namespace, with `SIGCONT`
behind it so a stopped process can act on it, once per signal received.
There is no branch on whether the workload is still alive — forwarding
to the workload's process group was silent in exactly the case that
matters, a workload already gone while a server it started held the
composition open.

**A session ends when its workload ends** — the shell you were given
exits, or the command `camp run` was given returns. The init acts on its
own observation of that exit, never on a message from the launcher, which
may be dead, `nohup`ed or without a terminal by then. Then, in this order:

1. **The end-of-session report**, while the tree is still whole and
   everything inside is still alive. Five read-only scans, described in
   their own section below. Every git command they run carries a
   ten-second deadline, so a git that hangs cannot hold the session
   open; the directory listings and the inventory comparison need no
   subprocess and carry none. Whatever it finds is printed and written
   to `.camp/reports/`. A session with nothing to report prints nothing
   here.
2. **Look.** The init reads the namespace's own `/proc` and lists every
   process but itself, by pid and command line. Children that have
   already exited are reaped first, so a zombie is not mistaken for
   something running. If nothing is left — the ordinary case, a shell
   that exits with nothing behind it — the session is over and nothing
   more is printed.
3. **Say what is being ended, then ask once.** One line per process,
   before the signal is sent:

   ```
   the shell has exited, and 2 process(es) started in this session are still running. They are being asked to end (SIGTERM, with SIGCONT so a stopped one can act on it):
     pid 41: chromium --profile-directory=Default
     pid 57: node server.js
   camp waits up to 10 seconds for them, and sends nothing stronger.
   ```

   ("the command has exited" for `camp run`.) The request is one per
   session end, not one per path: when the session was ended by a
   `SIGTERM` to the init, the handler's fan-out was the request, and the
   message says the processes "were already sent SIGTERM … when this
   session was signalled, and are not sent it again". A program that
   saves on the first `SIGTERM` and aborts on the second would otherwise
   lose the grace it was promised.
4. **Wait until the namespace is empty or ten seconds have passed.**
   "Empty" is two facts, not one: no child left to reap, *and* nobody but
   the init in `/proc`. The second is what sees a process that entered
   the pid namespace from outside — a joined shell — which is not the
   init's child and which `wait4` never reports. A `/proc` that cannot be
   read is not an empty namespace: camp says it cannot list what is
   inside, asks anyway, and waits the whole ten seconds.
5. **At the deadline, say what is left, and exit.** camp sends no
   `SIGKILL`, ever:

   ```
   after 10 seconds, 1 process(es) are still in the session:
     pid 41: chromium --profile-directory=Default
   camp does not kill them. The session's init exits now, and the kernel ends every process left in a pid namespace whose first process has exited, with SIGKILL. Whatever these had not saved is lost. If one needed longer to stop, that is the program to look at.
   ```

   Then the init exits. The kernel ends every process remaining in the
   pid namespace, discards the namespace, and takes every mount with it.

Both messages go to the init's stderr — your terminal while the launcher
is attached, and the session's log either way — so the deadline message
survives a launcher that was gone by then.

**The ten seconds** exist for a program that answers `SIGTERM` by saving
and exiting — a shell its history, an editor its swap file, a browser its
profile. It is a constant, not a configuration key, because no
environment has needed another value; every message that depends on it
prints it. In the ordinary case the wait is zero.

**The launcher returns** when the init has exited — it waits for its own
child, not merely for the pipe to close, because a launcher that returned
on the pipe's close found the locks still held with the init between its
last write and its exit. When `wait4` on a pid namespace's init returns,
the kernel has finished: descriptors closed, so the locks are released;
every other process ended and reaped; the mounts gone. So when `camp
shell` returns, the session is over in every sense the next command
could ask about, and `camp run` exits with the command's own status,
never reinterpreted.

**A signal that arrives while camp is still mounting** meets no
supervisor: it ends the init under the runtime's own disposition, and the
kernel discards the namespace with every mount in it. That is the right
answer to "stop" at that point, and a different one, which is why it is
written down separately.

**To end a session you cannot reach**, send `SIGTERM` to its init. It
fans out to everything inside and nothing outside; the workload dies of
it, and the workload's exit is the same ending as an `exit` typed at the
shell. If you do not know the pid, ask for a second session on the same
tree: camp refuses and names the holder, with the exact command.

**To keep a session across a disconnected terminal**, start tmux outside
and run camp in a pane; the pane's shell is the workload, and `tmux
attach` reaches it from any terminal. The other way round does not work:
`camp run -- tmux new-session -d` ends the session as soon as the tmux
client exits, because the client was the workload, and the server it
left behind is asked to leave with everything else. A second pane of an
outside tmux is a process outside the session and sees the plain
directory — which is what the join is for.

## Joining a running session

A second terminal reaches a running session's tree with `camp shell
--join`, or runs one command in it with `camp run --join -- <command>`.
It is camp's `docker exec`: it builds nothing, mounts nothing and locks
nothing, because the composition already exists and its init already
holds the locks.

**Finding the session.** A session leaves no record, so its init is found
as a process. camp reads every `/proc/<pid>/cmdline` visible from here and
keeps the ones whose first argument is the init's marker and whose
second resolves to this configuration file. Each candidate is then held
open by `pidfd_open`, so that a pid reused between two reads cannot swap
one process for another under the checks, and read:

- `/proc/<pid>/status` — the real uid is yours, and `NSpid` ends in `1`:
  it really is process 1 of a pid namespace;
- `/proc/<pid>/mountinfo` — the init's own mount namespace's table,
  readable from outside for a process of your uid — shows an overlay
  standing at this configuration's composed tree;
- `/proc/<pid>/fd` — the init has open, by device and inode, the
  directories now at this configuration's code repository and composed
  tree. camp's init holds those two directories open because its locks
  are descriptors on them, and that is what this check is looking for;
  what it proves is the open descriptors, not the locks — nothing in
  `/proc` says whether an open file description carries a `flock`.

Argv is text any process can write; a mount table is the kernel's, and an
open descriptor names an inode whatever the pathname now is. Exactly one
candidate passing every check is joined, and a probe through the pidfd
after the reads, and again after the namespace files are opened, proves
they were all about the same process.

**Entering.** camp opens the init's `/proc/<pid>/ns/user`, `ns/mnt` and
`ns/pid` and hands the three descriptors to util-linux `nsenter`:

```
nsenter --user=/proc/self/fd/3 --mount=/proc/self/fd/4 --pid=/proc/self/fd/5 \
        --preserve-credentials --wd=<the composed tree> -- /usr/bin/camp __joined <config> <live> -- <command>
```

The program `nsenter` runs is the same camp binary, by the absolute path
it is running from, with `__joined` as a first argument that is not a
command anyone types.

camp cannot do this in its own process: `setns` into a user namespace is
refused to a multithreaded caller, and a Go program is multithreaded
before its own code runs. `nsenter` joins the files — not the pid, which
could have been reused — forks the child into the pid namespace, waits,
and re-raises a child's fatal signal on itself so the status reaches you
the way a shell would report it. camp's own copies of the namespace
descriptors are closed the moment `nsenter` has them: an open descriptor
to a mount namespace keeps that namespace and the overlay in it alive
after every process has left it, and a joiner still holding one when the
session ended would hold the old overlay open on a code repository whose
lock the init had already released — a second overlay on the same upper
at the next start, which is the corruption the locks exist to prevent.

**What remains is `nsenter` itself**, which stands in the mount namespace
for as long as it lives and exits the moment its child is gone. A
*stopped* `nsenter` pins the old mount namespace for as long as it is
stopped. Ctrl-Z does not do that: the joined shell or command takes the
terminal's foreground group for itself, so Ctrl-Z stops the workload, and
the session's end reaches a stopped workload with `SIGTERM` and `SIGCONT`
like anything else inside, after which `nsenter` exits behind it. What
does it is a `kill -STOP` aimed at `nsenter` directly. That is the price
of the `nsenter` route, and it is said here rather than glossed: a
session that has ended while its joiner's `nsenter` was stopped keeps its
overlay alive until that `nsenter` is continued or killed.

**What the joined process is.** Inside, the re-executed camp reads the
configuration for its `session:` declarations only, resolves them against
*this* terminal's environment — `$HOME`, `$PATH` and the rest mean the
joiner's, because the first shell's effective environment is stored
nowhere camp could read it — marks every inherited descriptor
close-on-exec, takes the terminal's foreground group, and becomes the
shell or command, standing in the composed tree. It gets the
composition's `PATH` prepend, its `GIT_SSH_COMMAND`, `CAMP_LIVE` and
`PWD`, like the first shell did.

**What it is not.** It is not a second composition: the `prepare:`
commands and the generation step do not run — re-running generation would
rewrite files under a live session — and no lock is taken, because both
locks are exclusive and the joiner would refuse itself, and because the
pid namespace already binds the joined process's life to the init's: there
is no interval in which a joined process exists and the locks do not. Its
exit does not end the session. When the session ends, the init's fan-out
reaches it like anything else inside, and whatever ignores that is ended
by the kernel at the init's exit; the joiner then prints one line, `the
session ended; this shell went with it.`, so the sudden exit is not a
mystery.

**Refusals**, each with its repair: `join-no-session` (no init for this
configuration is visible from here — start one; a session that has just
ended shows as nothing, correctly); `join-from-inside` (process 1 here is
this configuration's init — you are already in it); `join-from-another-session`
(process 1 is another configuration's init and nothing was found: a
sibling session's init is invisible from inside this one — join from a
terminal outside every session; a session nested inside this one *is*
visible and would have been joined); `join-other-user` (a session for this
configuration runs as another uid, and only its own user holds the
capabilities its namespaces need); `join-not-this-composition` (an init
names this file but does not hold the locks on the directories the file
now names — the file was edited after the session started); `join-ambiguous`
(more than one passes, which two exclusive locks make unreachable);
`join-init-unreadable` (an init names this file and camp could not read
it — a descriptor limit is the usual cause; said, never counted as no
session); `join-ended` (the init was alive when found and gone before
the namespace files could be opened); `join-tool-missing` (no `nsenter`
on `PATH`; it is in `util-linux`). Those are all nine. A `setns` the kernel refuses is `nsenter`'s own failure,
reported in its own words and exit status; camp reads no program's output
and does not restate them.

On Ubuntu's restricted machines, the shipped AppArmor profile needs no
change for the join: the restriction mediates *creating* a user
namespace, and joining one needs only the capabilities your own uid
already has in it.

## What a running composition cannot absorb

A session is built once, from one reading of the configuration and the
repositories as they stood. Some changes made while it runs are absorbed
live; the rest are not, and `camp status` is how you ask which.

Absorbed live, because a directory bind is a live view: a file created,
edited or removed inside a workspace directory that was bound in appears
in the tree at once, still read-only; a runtime file written into an
islands mount lands in storage and stays.

Not absorbed, until the next start:

- **A replaced root file or island file.** A bind of a *file* is a bind of
  one inode. `CLAUDE.md`, `AGENTS.md`, a settings file standing as an
  island: editors save by rename, so the tree keeps showing the file that
  existed when the session started, and the replacement appears at the
  next start. `status` reports it as `bind-source-replaced`.
- **A new entry at the workspace root.** The overlay shows it at once,
  but no read-only bind covers it and no exclude line names it — so a
  write there copies up into the code repository and `git status` lists
  it. The next start refuses until `camp accept` records it; `status`
  and the end-of-session report name it the same day.
- **A changed configuration, `prepare:` command or `bin/` program.** The
  tree is what the file said when the session started. Nothing snapshots
  the file or the programs, so `status` sees such a change only where it
  shows: a mount that no longer matches what the file derives now, or a
  file that would now be refused. An edit that changes no mount and
  causes no refusal — a new environment declaration, a changed prepare
  command — is invisible to it, and takes effect at the next start.
- **A write to the code repository from outside the session.** The freeze
  is inside the namespace only; a save by rename at the raw path from an
  outside terminal, editor or cron job replaces a file behind the
  overlay's back, and the tree shows the old content there with `Stale
  file handle` on the next delete. Nothing camp can mount changes that.
  End the session first, or reach the tree through `camp shell --join`.
- **A renamed or moved environment directory.** Do not rename or move
  the environment while a session runs. The running session's overlay is
  in its own mount namespace, invisible to any camp command started
  outside it, and its work directory's marker names the composed tree's
  old path, which no longer exists — so a start from another terminal
  takes that work directory for a stale one and sweeps it out from under
  the session. From inside the session camp refuses to start at all,
  saying why; from outside it cannot tell. Put the name back, or end the
  session first.

**`camp status`** answers for the process that runs it. From outside every
session it reports that nothing is mounted under the composed tree as
seen from this process, and why that is the true answer. From inside a
session it lists the mounts, runs the verification pass read-only
against the plan the configuration derives today — expanded to the
islands a generation step contributes, and with the mounted exclude
compared byte for byte against the payload the configuration derives
now — and then runs the same drift pass the end of a session runs: the
gate re-run, the inventory comparison, the untracked and index scans, the
worktree list. Its closing advice is per finding, because the repairs
differ: a mismatched mount — end the session and start it again; a
configuration that would now be refused — repair it before the next
start, what is mounted is unaffected until then; a changed root entry —
`camp accept`, which no restart replaces; a suspected leak — look before
committing; a worktree registered under the tree — the printed repair
command, and *not* ending the session, which is exactly what breaks it.
Every finding but the last makes the command exit non-zero; worktrees
alone are a standing arrangement with a command beside it, reported and
exiting zero.

`status` and `explain` work from inside a session even though the
composed tree's directory is, from there, not empty — the overlay's own
root never is — and that one start-time refusal is set aside for the
session that causes it. "Inside its own session" is decided from two
facts, never from the file's name alone: process 1 is this
configuration's init *and* an overlay stands at the composed tree in this
process's own mount table. Every other refusal is still reported.

## Who you are inside

Your uid maps to itself rather than to 0, so `id` inside shows the real
user and files you create are yours. The cost is that the effective uid
is not zero, so `execve` drops every capability — which is why the mount
capability is carried in the ambient set and dropped as soon as the
mounts are verified. The overlay keeps working afterwards because the
kernel recorded the mounter's credentials at mount time. Your
supplementary groups cannot be changed inside and display as `nogroup`,
but the kernel keeps them and host permission checks keep honouring
them: a socket your group may write, you may still write.

The other cost is that exactly one id is mapped, so every file on the
machine owned by anyone else — root included — is shown inside with the
kernel's overflow id, as `nobody`. Reading and writing are unaffected;
what changes is what a program sees when it asks who owns a file. This is
not a gap to be closed: an unprivileged process may map the ids it owns,
its own and any range assigned to it in `/etc/subuid`, and host root's is
not one of them. That is the property the whole approach rests on.

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
repaired by any variable. A session has no elevation at all — a setuid
binary owned by an unmapped id confers nothing, so `sudo` and `pkexec`
cannot work inside one. And a program that *records* ownership into what
it builds — `tar`, `rsync -a`, an image build — will write `65534` for a
file that is root's outside, with nothing failing and nothing warning.
Build such an artefact outside a session, from the repositories' own
directories; nothing camp offers keeps the real owners inside one. `camp
explain` says all of this in its "Ownership view" section.

## Rollback

A start that fails after its first mount unwinds what it made, in reverse,
before the refusal is reported. A read-only bind is two calls and the
propagation change a third, so a mount is recorded as made the moment it
exists, not when the whole step succeeds — otherwise a failure between
the calls would leave a mount that the unwinding did not know about.

There is no lazy unmount anywhere in camp: a mount that cannot be removed
stays mounted, is named in the refusal, and goes when the session's
namespace goes. A detached mount would leave the kernel's table while it
is still alive and still being written through, and a rollback that used
one would report a clean namespace over a mount something is still
writing to.

Nothing else is ever unmounted by camp. A composition that came up ends
with its session, and the kernel takes every mount with it.

## What a session reports when it ends

Five read-only scans, at the end of a session — the moment when the
cause is still fresh. They never block; there is nothing left to refuse
with. `camp status` runs the same five, so the mid-session answer and the
end-of-session answer cannot disagree.

The gate's comparison and the inventory's re-run, so a change made during
the session is named the same day. The code repository's untracked paths
whose names belong to the workspace, as suspected copy-up residue. The
index — `git ls-files --stage` — because a forced add leaves an *indexed*
path with no working-tree file at all, which no untracked scan can see.
And the worktrees registered inside the composed tree, each with its
exact repair command: git records a worktree's git directory as an
absolute path and compares it as a string, so one created inside the
composition stops resolving when the composition comes down; the files
are fine, git simply cannot see them. Left unrepaired, git prunes the
registration at an ordinary auto-gc after three months — the failure that
happens while nobody is looking.

A scan that could not run is said, never left out: an omitted scan reads
exactly like a scan that found nothing. Every git command in the pass
runs with `--no-optional-locks`, reads the code repository through its
own frozen path, and carries the ten-second deadline; one that has not
finished by then is sent `SIGTERM`, left behind, and met by the ending as
one more process inside.

The launcher that would print this may be gone by the time the session
ends — killed, `nohup`ed, its terminal closed — so the init writes it to
`.camp/reports/` as well, and the next camp command in that environment
prints it once and marks it read. `camp doctor` lists what is waiting and
what has been read. A report is output, not authority: nothing reads it
back as state, and a session still leaves no state record.

## What camp refuses, and why refusing is the design

camp refuses rather than guesses. When it does not recognise something,
it says so and stops; when it cannot find out, not knowing is not
permission. Every refusal carries a stable rule name and is written for
somebody who has not read this document: it names the path, says what is
true and which side of it matters, says what repairs it — the exact
command where there is one — and says whose move it is: you act, camp
checks. A refusal that falls short of that is a bug worth reporting.

Before mounting, among others:

- an overlap not in `allow_overlap`; a mount target with code-tracked
  content at or under it; two mounts on one target; an earlier mount's
  target inside a later one's; a target outside the composed tree; a
  missing mount point, with the placeholder-file remedy named; a source
  or target type mismatch; a symlink anywhere in a mount operand or at a
  root entry; a name containing a newline; a repository nested in
  another, or the composed tree inside a repository;
- a composed tree's directory that is a symlink, not a directory, or not
  empty;
- a second composition on the same code repository or the same composed
  tree (the locks); a start typed from inside a session; mounts already
  standing where camp would mount;
- a new workspace root entry or a type change against the inventory; no
  inventory yet, with `camp accept` named;
- a `prepare:` command that fails, times out or is interrupted; the
  configuration's bytes changing while they ran; either locked directory
  being a different inode afterwards;
- generator output failing the hostile-data checks; a generation step
  alongside `git_exclude`, or two of them;
- a `session:` key camp does not know; an `identity:` other than absent
  or `uidmap`; an environment declaration that names a variable the
  invoking environment does not define, or a `CAMP_`-prefixed or `PWD`
  name, or a malformed `$` expression;
- a machine that cannot create a user namespace, cannot mount an
  overlay in one, has no `fsopen`, or has no `git`.

After mounting: any verification failure refuses the whole composition,
unwound in reverse.

There is no `--force`. Where a refusal is about what you asked for, the
way past it is a change to the configuration or to a repository — the
same decision, recorded where the next reader finds it. Where it is about
the machine or the moment — a program to install, a kernel setting, a
lock another command holds for a second, a session that has to end
first — the message says which. And nothing can wall you in: the
repositories stay ordinary directories, reachable without camp, and the
composition exists only inside a session that ends when you leave it.

## What camp is not

**Not a security boundary.** The read-only mounts prevent accidental
writes and copy-up. A process inside can still walk to the backing
directories and read anything on the machine, reach the network, and use
every device. camp does not pretend to sandbox.

**Not visible from outside.** The composition exists only inside its
session's mount namespace. An editor already running, a language server,
a daemon — anything not started inside — sees the composed tree's
directory empty, and nothing camp offers shows it the tree: there is no
mode that mounts it for the whole machine. Start such programs inside,
with `camp run -- <program>` or from a `camp shell`, or reach the tree
from another terminal with `camp shell --join`.

**Not a filesystem.** camp is kernel mounts and nothing that runs while
you work: no daemon serves the tree, so nothing camp does can hang a
path, and a wrong composition is a wrong *view* — visible, and gone with
the session — never wrong data on disk.

**Not something that modifies your repositories.** Every filesystem write
camp makes goes through one package whose addressing cannot be
constructed from a repository path, and a test fails the build if a write
appears anywhere else.
