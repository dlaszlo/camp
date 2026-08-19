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
│   ├── work/<id>/            DISPOSABLE: the overlay's workdir, generated files, staging
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

Teardown is that list reversed, and never lazy.

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
repository. It always runs as the invoking user; in the system-wide mode
that is true by construction, because generation happens in the
unprivileged front end and no privileged process ever executes a
configured command.

**Its output is hostile data until checked.** Whoever can edit the
configuration chooses the program that runs, and the mounts that follow
may be made by root. So camp refuses: an entry that is not exactly one
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

## The two modes

**The namespace mode (`camp run`) is primary.** The composition is built
inside a user and mount namespace. No privilege is needed; nothing
outside can see it; and when the last process exits the kernel discards
the namespace and every mount in it. Teardown cannot fail and there is no
`camp down`.

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
nothing warning. Build such an artefact outside a session, or with `camp
up`, which creates no user namespace and so sees every owner as it really
is. `camp explain` says all of this in its "Ownership view" section.

**The system-wide mode (`camp up`)** exists for when something started
*outside* must see the tree — a GUI editor, most often. Here there is one
mount table for the machine, so two effects are machine-wide for as long
as the composition is up: the tree appears at the composed path for every
process, and **the workspace is read-only for every process, your editor
included.** Both promises cannot hold at once here, and the protection
wins; camp prints that as a line of its own before it starts.

`camp up` runs as you from start to finish. `sudo` wraps only a narrow
helper that executes the already-validated plan, handed to it on stdin —
never argv, which `/proc` exposes machine-wide. The helper re-resolves
every operand itself, descriptor-relative, following no symlink, and
compares each endpoint against what the front end saw: a component
swapped between the check and the mount fails closed. It builds the whole
tree in a staging directory, verifies it there, and moves it onto the
composed path only then, so nothing outside ever sees a half-built tree.
Running `sudo camp up` directly is refused.

## Recovery

The system-wide mode writes a record before the helper mounts anything,
carrying the complete concrete plan. `down` and `status` read that
record and **never the configuration**, which may have been edited while
the composition was up. So there is no moment at which something is
mounted and nothing knows what.

There is no lazy unmount anywhere in camp. A mount that cannot be removed
stays mounted, is reported as still mounted with the holding process
named from `/proc`, and makes the command exit non-zero. A detached mount
leaves the kernel's table while it is still alive and still being written
through — and in the system-wide mode that table is the only guard
against a second composition on the same repository.

### How a mount is removed

`umount2` takes a path, and the kernel resolves it. That is a problem the
helper cannot ignore: it decides *what* to remove by looking at a
descriptor it resolved beneath the environment root it pinned, and the
owner of every directory above that path is the person the helper is
acting for. Between the decision and the call, a name can be made to
reach something else.

So the mount is not named to the kernel by the path it was recorded as.
It is named by descriptor: `open_tree` takes a handle on the mount the
descriptor holds, `move_mount` takes that same mount into a directory
under `/run` that root makes for the purpose, and the unmount happens
there — at a path no part of which anybody else can rename. A mount that
will not come down is moved back where it was and reported busy, so the
record, `camp status` and the next `camp down` all go on meaning what
they meant.

Two of camp's mounts cannot go that way. The staging and live points are
bound onto themselves so that what is built on them cannot propagate, and
the kernel refuses to move a mount whose parent is shared — which theirs
is, on any machine where `/` is. Those are unmounted through the parent
directory's own descriptor instead: `umount2` is given
`/proc/self/fd/<the directory>/<one name>`, which the kernel resolves to
the directory that descriptor holds rather than by walking the name it was
opened by, and a mount point cannot be renamed while it is mounted.

And a teardown whose environment root has moved since the record was
written stops with nothing unmounted. The mount table reports a mount at
the path it is at *now*, so after a rename every recorded path would
answer "nothing is mounted there" — a true answer about a name and a
false one about the machine. The base is a descriptor and knows where it
is; when that is not where the record says, camp keeps the record and asks
for the directory back.

## What a session reports when it ends

Four read-only scans, at `camp down` and at the end of a namespace
session — the moment when the cause is still fresh. They never block; a
teardown that refused would wall you in.

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
