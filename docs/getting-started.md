# Getting started

This walks you from nothing to a working composition. Everything in it is
real: you can type it, and at the end you will have a directory that
behaves the way the rest of the documentation describes.

Before you start, install camp — see [install.md](install.md) — and check
`camp doctor` says this machine can run it.

## The example

Say you are working on a web service. The code is in one repository. The
things your editor and your coding agent read — the instructions, the
agent definitions, the prompts, the notes — are useful, are worth
versioning, and have no business in the service's history.

You want one directory that holds both, and two repositories that each
hold only their own.

```
~/work/
├── shop/                the product
├── shop-env/            the development environment
└── shop-live/           where you will work
```

## 1. The two repositories

Nothing camp-specific here — these are ordinary repositories.

```
mkdir -p ~/work && cd ~/work

git init shop
cd shop
mkdir src && echo 'package main' > src/main.go
echo '/node_modules' > .gitignore
git add -A && git commit -m 'the service'
cd ..

git init shop-env
cd shop-env
echo '# How to work on the shop service' > INSTRUCTIONS.md
mkdir notes && echo 'why the cart is a separate service' > notes/decisions.md
echo '/scratch' > .gitignore
git add -A && git commit -m 'the development environment'
cd ..
```

The composed tree's directory, `shop-live`, camp makes for you at the
first start: git cannot record an empty directory, so a clone of an
environment could never bring one. If you make it yourself it has to be
empty — camp will not lay a tree over content it would then hide.

## 2. The configuration

One file, and it is the only one you write.

```
mkdir -p ~/work/.camp
```

Put this in `~/work/.camp/config.yml`:

```yaml
env: /home/you/work            # the one absolute path in the file
merged: shop-live              # where the composed tree appears

repositories:
  - { name: env,  path: shop-env }
  - { name: code, path: shop }

overlayfs:
  lower: [env]                 # read-only underneath
  upper: code                  # on top, and where writes land

allow_overlap: [.gitignore]    # each repository needs its own

steps:
  - mount_rw:
      - { source: "code/.git", target: ".git" }
  - git_exclude
```

Two lines deserve a word.

`allow_overlap: [.gitignore]` — both repositories have one, and a name in
both roots normally stops camp. This says the overlap is intended. The
composed tree shows the code repository's copy, which is correct: the
tree is governed by the product's ignore rules.

The `.git` line — both repositories also have a `.git`, and directories
merge, so without this bind the two histories would union and `git` in
the composed tree could resolve a branch to the wrong repository. camp
does not add it for you; leaving it out is caught before anything is
mounted.

### One file is yours

`config.yml` is the only thing in `.camp/` you edit. Everything else
that appears there is camp's working material: the `inventory` it
compares against, written by `camp accept`, and four directories — the
overlay's scratch, machine-local storage, end-of-session reports and
camp's own log — each made when something first has to be written into
it.

The log is always written. Every line camp says about a run on stderr —
the step lines, warnings, refusals, what a session found when it ended —
is written to `.camp/logs/camp.log` as well, with the time in front of
it, and the file rotates by size so it cannot grow without bound. Nothing
switches it on, because a log you have to remember to switch on is
missing on exactly the run that surprised you. What a command prints as
its product — a plan, a status — goes to stdout and is not copied into
it.

If you had run `camp init` instead of writing the file by hand, camp
would have left a `README.md` in there saying exactly that, and a
`.gitignore` that keeps the four directories out of version control
while leaving `config.yml` and `inventory` in. Worth doing:

```
camp init ~/work        # in an environment that does not have one yet
```

The ignore rules live inside `.camp` rather than in a repository's own
`.gitignore`, because the environment root may belong to a repository, to
a different one later, or to none at all — and the answer is the same
every time.

## 3. Record the starting point, and read the plan

```
cd ~/work
camp accept
```

That records what the two repositories have at their roots right now.
camp compares against this snapshot at every start, because a new name
appearing at the top of your environment repository changes what is
protected — and that should be a change you decided.

```
camp plan
```

This prints every mount it would make, in order, with the reason for
each; the exclude it would generate; and anything that would stop it.
**Nothing is mounted.** Read it once — it is the clearest description of
what the tool is about to do.

If something is wrong, `camp plan` says so in sentences, names the paths,
and tells you which way out you have. Fix it and run it again. Nothing is
mounted while you do, so there is nothing to undo.

## 4. Work in it

```
camp shell
```

camp says what it does as it does it, one line per step on stderr, with
the outcome in the first column:

```
[OK]    created: /home/you/work/shop-live, the composed tree's directory
[OK]    locks: /home/you/work/shop, /home/you/work/shop-live
[OK]    checked: 7 mounts, gate clean, nothing refused
[OK]    generated: the exclude and the islands lists
[OK]    identity: uid 1000 and gid 1000 map to themselves
[NOTE]  only your own id is mapped, so files owned by anyone else show as nobody
[OK]    mounted: 7 at /home/you/work/shop-live
[OK]    verified: 7 mounts at /home/you/work/shop-live
```

The `created` line appears whenever the composed tree's directory is not
there yet — the first start, or any start after you removed it. Seven
mounts, for
this configuration: the workspace held read-only at its own path, the
overlay, one read-only bind for each of the two workspace root names the
tree shows (`INSTRUCTIONS.md` and `notes`), the `.git` bind, the
generated exclude, and the code repository held read-only at its own
path. `camp plan` lists the same seven with the reason for each.

Then your shell's prompt, in `~/work/shop-live`, which holds both
repositories at once:

```
$ ls
INSTRUCTIONS.md  notes  src
```

Try the four things that matter:

```
$ echo 'a new file' > src/cart.go        # lands in the shop repository
$ git status --short
?? src/cart.go

$ echo 'edit' >> INSTRUCTIONS.md
bash: INSTRUCTIONS.md: Read-only file system
```

The last one is the point. `INSTRUCTIONS.md` comes from the environment
repository, and camp holds it read-only so that editing it *here* fails
loudly instead of quietly copying it into the product's repository, where
it would look applied and belong to the wrong history. To change it, edit
it where it lives — `~/work/shop-env/INSTRUCTIONS.md`, from another
terminal. What the composed tree then shows depends on what was bound: a
*directory* bound into the tree (`notes/` here) is a live view, and a
change inside it appears immediately; a *single file* bound into it, like
`INSTRUCTIONS.md`, is a bind of one inode, so the tree keeps showing the
file that existed when the session started — editors save by rename, and
the replacement appears at the next start.

One thing not to do from that other terminal: edit the *shop* repository
at `~/work/shop`. Inside the session its path is read-only, and outside
camp cannot make it so; a save by rename there replaces a file behind the
overlay's back, and the composed tree shows the old content at that path
for the rest of the session. Write through the tree — that is what it is
for — or end the session first.

Notice also what `git status` did **not** say: `INSTRUCTIONS.md` and
`notes/` are not listed as untracked, because camp generated an exclude
for them and mounted it over this tree's copy. Outside the composition,
the shop repository's own exclude file is untouched.

Type `exit`. The session ends, the kernel discards every mount with it,
and `~/work/shop-live` is empty again. There is nothing to clean up and
no command to run. A shell that exits with nothing left running behind it
prints nothing new; if something you started in there had drifted — a
worktree made through the tree, a file staged with `git add -f` — camp
says so here, once, with the repair.

While a session runs, `camp status` from inside it lists the mounts,
checks each against what the configuration derives now, says whether the
file would now be refused, and names what has moved under the session —
a new name at the workspace root, a root file replaced by an editor
outside, a file staged that belongs to the workspace — with what to do
about each. A running session is built once from one reading of the file
and does not follow it; an edit that changes no mount and causes no
refusal is one `status` cannot see, and it takes effect at the next
start.

## 5. Ending a session, and surviving a disconnected terminal

A session ends when what you started exits: the shell, or the command you
gave `camp run`. If something you started inside is still running at that
moment — a browser your tooling opened, a server — camp names it, sends
it `SIGTERM` (and `SIGCONT`, so a stopped process can act on it), and
waits up to ten seconds. It sends nothing stronger. If something is still
there when the time is up, camp says so, by pid and command, and exits;
the kernel then ends every process left in the session with `SIGKILL`.
A shell that exits with nothing behind it prints nothing new.

If you want a session to outlive the terminal you started it in, put
tmux *outside* and camp in a pane:

```
tmux new-session -s shop        # or: tmux new-session -d -s shop 'camp shell'
camp shell                      # inside the pane
```

The pane's shell is the session's workload, so the session lives as long
as the pane does, and `tmux attach -t shop` reaches it from any terminal.

The other way round does not work: `camp run -- tmux new-session -d -s
shop` ends the session the moment the tmux client exits, because the
client was the workload — the server is asked to leave and the
composition goes. Keep tmux outside, as above. A second pane of that tmux
is a process outside the session and sees the plain directory, like every
other process started outside.

To give that second pane the composed tree, join the running session:

```
camp shell --join               # a shell inside the running session
camp run --join -- <command>    # one command inside it
```

`--join` is camp's `docker exec`: it finds the session running for this
configuration — its init, from `/proc`, after proving that the init is
yours and composes this tree — enters its namespaces through util-linux
`nsenter`, and hands you the tree, building and locking nothing. A joined
shell is a visitor: its own exit does not end the session, and when the
session ends it ends too, with one line saying so. Run it from a terminal
that is not already inside a session; typed inside one, it tells you that
you are already there.

To end a session you cannot reach, send `SIGTERM` to camp's own process,
the one resident as the session's first: it reaches the shell or command
inside, whose exit ends the session the same way. If you do not know its
pid, ask for a second session in the same tree: camp refuses and names
what is holding it.

## Where to go next

- **[how-it-works.md](how-it-works.md)** — what camp actually mounts, in
  what order, and what it checks afterwards. Read this before trusting it
  with anything you care about.
- **`camp explain`**, run from inside a composition — it describes *that*
  tree: what is read-only and why, where each real file is, and what
  happens to a git worktree made in there.
- **The README** for the configuration language in full: the other mount
  kinds, and `mount_islands` for a directory that is half repository and
  half machine state.

## When something goes wrong

`camp doctor` reports the machine and the environment. `camp plan` says
what would stop a composition. `camp status` says what is mounted right
now, as seen from where you run it, and what has changed under a running
session. `camp explain`, from inside, describes the tree you are standing
in. Everything camp said about a run on stderr is also in
`.camp/logs/camp.log`, with the time in front of it.

Every refusal is written for somebody who has not read the
documentation: it names the path, says what is true and which side of it
matters, says what repairs it — the exact command where there is one, a
program to install or a session to end where that is the repair — and
says whose move it is. If one of them does not, that is a bug worth
reporting.
