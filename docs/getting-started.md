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

`.camp/` will end up holding six things, and `config.yml` is the only one
you edit. The others are camp's working material: the `inventory` it
compares against, and four directories of scratch, machine-local state,
session output and camp's own log.

The log is always written. Every line camp prints to a terminal is
written to `.camp/logs/camp.log` as well, with the time in front of it,
and the file rotates by size so it cannot grow without bound. Nothing
switches it on, because a log you have to remember to switch on is
missing on exactly the run that surprised you.

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
camp run -- bash
```

You are now in `~/work/shop-live`, and it holds both repositories at
once:

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
no command to run.

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

If you used to start tmux *inside* — `camp run -- tmux new-session -d -s
shop` — that now ends the session the moment the tmux client exits: the
server is asked to leave and the composition goes. Move tmux outside as
above. A second pane of that tmux is a process outside the session and
sees the plain directory, like every other process started outside.

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
now.

Every refusal names the path, says what is true on each side, says which
side matters and gives you the command that repairs it. If one of them
does not, that is a bug worth reporting.
