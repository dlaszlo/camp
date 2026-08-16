# Getting started

This walks you from nothing to a working composition. Everything in it is
real: you can type it, and at the end you will have a directory that
behaves the way the rest of the documentation describes.

Before you start, install camp — see [install.md](install.md) — and check
`camp doctor` says the namespace mode is available.

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

And an empty directory for the composed tree. It has to exist, and it has
to be empty — camp will not lay a tree over content it would then hide:

```
mkdir shop-live
```

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

`.camp/` will end up holding five things, and `config.yml` is the only one
you edit. The others are camp's working material: the `inventory` it
compares against, and three directories of scratch, machine-local state
and session output.

If you had run `camp init` instead of writing the file by hand, camp
would have left a `README.md` in there saying exactly that, and a
`.gitignore` that keeps the three directories out of version control
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
terminal — and the change appears in the composed tree immediately, because
what you are looking at is a live view and not a copy.

Notice also what `git status` did **not** say: `INSTRUCTIONS.md` and
`notes/` are not listed as untracked, because camp generated an exclude
for them and mounted it over this tree's copy. Outside the composition,
the shop repository's own exclude file is untouched.

Type `exit`. The session ends, the kernel discards every mount with it,
and `~/work/shop-live` is empty again. There is nothing to clean up and
no command to run.

## 5. Working from several terminals

A session lives as long as a process is inside it. If you want more than
one terminal in the same composed tree, start something that stays:

```
camp run -- tmux new-session -d -s shop
```

That returns immediately — the tmux *client* exited — while the server
stays inside and holds the composition open. From any other terminal:

```
tmux attach -t shop
```

Every window it opens is inside. `tmux kill-server` ends the session and
everything comes down with it.

This works because the composition is held open by camp itself, running
as the first process of the session, and not by tmux — a program that
daemonises typically closes what it inherited, and nothing here depends
on it not doing so.

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
