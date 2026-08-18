# Installing camp

camp is a single binary with no runtime dependencies of its own. What it
needs is a Linux kernel with OverlayFS, `git`, and — for the mode you
will use every day — permission to create a user namespace.

## What the machine has to have

**Linux.** camp is built on OverlayFS and bind mounts. There is no macOS
or Windows version and there will not be one; the nearest equivalent is
to run the composition inside a Linux VM or container.

**OverlayFS in the kernel.** Every distribution kernel of the last decade
has it. To check:

```
grep overlay /proc/filesystems        # should print: nodev  overlay
```

If it is missing, `sudo modprobe overlay` usually loads it.

**git**, for a git-based composition. camp reads git — `rev-parse`,
`ls-files`, `worktree list` — to work out what each repository
contributes and to report what a session changed. It never writes git.

```
sudo apt install git                  # Debian/Ubuntu
sudo dnf install git                  # Fedora
```

A composition of two directories that are not repositories needs none of
that and works without git; `camp doctor` reports its absence as
something worth knowing rather than as a failure. The one place where it
really is required — the shipped `git_exclude` step — refuses with the
reason rather than quietly reading raw directory listings instead.

**Nothing else.** camp does not call `mount(8)`, `umount(8)`, `fuser` or
any other tool: it makes the mounts by syscall, and asks `/proc` for the
state. There is nothing to install for those.

To build it you need **Go 1.25 or newer**.

## Build and install

```
git clone <this repository> camp
cd camp
go build -o camp ./cmd/camp
sudo install -m 755 camp /usr/local/bin/camp
```

You can stop here if you only want the system-wide mode (`camp up`). For
the everyday mode, one more step.

## The namespace permission

`camp run` builds the composition inside a user namespace, which is what
lets it need no privilege and leave nothing behind.

**On most distributions this already works and there is nothing to do.**
Unprivileged user namespaces are permitted by default on Fedora, RHEL and
its rebuilds, Debian, Arch, and most others. Run `camp doctor`: if it
says the namespace mode is available, skip the rest of this section.

Where it does not work, `camp doctor` names the reason and the repair,
because it finds out by *trying* rather than by reading switches — there
are several, they interact, and one of them stays on system-wide even
when a particular binary has been granted an exception. The cases camp
knows about:

| what the machine does | where you meet it | the repair |
|---|---|---|
| AppArmor restricts unprivileged user namespaces per binary | Ubuntu 23.10 and later, and its derivatives | the profile below |
| `user.max_user_namespaces` is 0 | hardened kernels | `sudo sysctl -w user.max_user_namespaces=15000` |
| `kernel.unprivileged_userns_clone` is 0 | older Debian, hardened kernels | `sudo sysctl -w kernel.unprivileged_userns_clone=1` |
| something else denied it | SELinux in a confined domain, a container runtime | `camp doctor` says where to look |

**camp does not depend on AppArmor.** It ships a profile because Ubuntu's
restriction is per-binary and a profile is the narrow way to satisfy it —
narrower than turning the restriction off for every program on the
machine. On a system with no AppArmor at all, none of this applies.

And in every case there is the other way out: `camp up` needs no
namespace, only `sudo`.

### The Ubuntu case

```
sysctl kernel.apparmor_restrict_unprivileged_userns   # 1 means restricted
```

Where that is `1`, the failure is quiet in an unhelpful way: the
namespace **is** created, and the process inside it is then confined to a
profile that denies every mount. camp ships an AppArmor profile that
grants the permission to one binary path and to nothing else:

```
sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp
sudo apparmor_parser -r /etc/apparmor.d/camp
```

The system-wide restriction stays on — that is the point of doing it this
way rather than turning the sysctl off, which would remove the protection
from every program on the machine.

Two things worth knowing about the profile. It attaches to
`/usr/local/bin/camp`; **a copy of the binary anywhere else is not
covered by it**, which is why the build above installs before this step.
And the binary stays otherwise unconfined: a development tool that runs
your editor, your shell and your test suite cannot be meaningfully
confined, and a profile that pretends to is worse than an honest one.

If you install camp somewhere else, edit both the attachment path and the
profile name in the file.

## ssh inside a session

`camp run` maps exactly one user id — yours — into the session. Every
file on the machine owned by anyone else is then shown with the kernel's
overflow id, which is to say as `nobody`:

```
$ stat -c '%U %n' /etc/ssh/ssh_config      # inside a session
nobody /etc/ssh/ssh_config
```

Reading and writing are unaffected. What changes is what a program sees
when it asks who owns a file — and ssh asks, because it refuses a
system-wide configuration file it cannot attribute to root or to you:

```
$ ssh nas
Bad owner or permissions on /etc/ssh/ssh_config.d/20-systemd-ssh-proxy.conf
```

**No mapping fixes this, and none is coming.** A user namespace lets an
unprivileged process map the ids it owns — its own, and any range
assigned to it in `/etc/subuid`. Host `root` is not one of them, and that
is the property the whole rootless mode rests on. Rootless podman shows
the same thing; it is less visible there only because a container brings
its own `/etc`.

The repair is to point ssh at your own configuration. `-F` does two
things: it names the file to read, **and it skips the system-wide one**.
Your host aliases, keys and options all keep working; only the file that
cannot be attributed is left unread.

The question is how `-F` gets there, and the answer camp takes is that it
belongs to the composition. Nothing here touches your machine: not your
shell's startup file, not your global git configuration, not
`~/.local/bin`. A session is something you are inside, and wiring the
outside to repair the inside breaks things in places that have nothing to
do with camp. So the setting lives in the configuration's `session:`
section, where it is versioned, diffable, and travels with the
environment it serves.

### git

One line, and git is covered — including where no shell is started, which
is the case for a program `camp run` starts directly:

```yaml
session:
  environment:
    GIT_SSH_COMMAND: "ssh -F ${HOME}/.ssh/config"
```

`${HOME}` is read from the environment you started camp in. camp knows
nothing about ssh or git here: it sets what the configuration says, and
the next program that breaks the same way is fixed by the same key.

### ssh, scp and sftp typed by hand

These have no option variable of their own — ssh's own manual lists only
the variables it *sets* — so the only control left is which program the
name resolves to. Prepend a directory your workspace repository owns, and
put a launcher in it for each entry point. `scp` and `sftp` need their
own: they start ssh from a compiled-in absolute path, so wrapping `ssh`
alone does not reach them.

```yaml
session:
  environment:
    GIT_SSH_COMMAND: "ssh -F ${HOME}/.ssh/config"
    PATH: "$CAMP_LIVE/.workspace/bin:$PATH"
    # The path as it was outside, saved under a name of this
    # composition's choosing. The launchers find the real programs
    # through it -- see below.
    OUTER_PATH: "$PATH"
```

`$CAMP_LIVE` is the composed tree, so the directory is
`.workspace/bin/` in your workspace repository, reached through the
tree. Three files go in it, committed like anything else there:

```sh
#!/bin/sh
# .workspace/bin/ssh -- and the same file as scp and sftp, with the
# program name changed in both places.
original=$(PATH="$OUTER_PATH" command -v ssh) || exit 127
exec "$original" -F "$HOME/.ssh/config" "$@"
```

Three things about that script are deliberate:

- **It finds the original through `$OUTER_PATH`**, the path as it was
  before the launcher directory was prepended. Not a fixed directory such
  as `/usr/bin`: whichever one you pick is right on your distribution and
  wrong on the next, and nothing in this arrangement should assume a
  filesystem layout it does not have to.
- **It tests nothing about camp.** There is no `if inside a session`
  switch, because a launcher that changes behaviour depending on an
  exported marker changes it for reasons nobody can see. It is reached
  only when the session's `PATH` puts it first, and that is the whole
  condition.
- **It fails loudly.** If the original is not found it exits 127 rather
  than doing something approximate, and if the launcher itself is missing
  the command is simply not found — camp never reports a success that did
  not happen.

camp neither writes these files nor blesses them. They are ordinary
content of your workspace repository, reviewed and versioned by you,
because the moment camp generated a program-specific wrapper it would be
carrying ssh knowledge in a tool that has none.

`camp up` creates no namespace, so none of this applies there — and `camp
up` says so when the configuration has a `session:` section, rather than
leaving you to wonder whether it took effect. If you need the system-wide
ssh configuration read as itself, that is the mode for it.

## Check that it worked

```
camp doctor
```

The line to look for is:

```
  ok   user namespaces  permitted, and a real overlay in one mounts, copies up and whiteouts, with userxattr
```

`doctor` does not read the switches and guess — it creates a namespace
with the same identity mapping a real session uses, builds a real overlay
inside it, writes through it and removes through it. If any of that
fails, it says which restriction stopped it and what to do about it. The
scratch tree it uses lives inside that namespace and goes with it.

The system-wide mode is reported as far as it can honestly be:

```
  ok   privileged behaviour  not tested; it needs a terminal
```

Answering that one would mean running `sudo` to find out whether `sudo`
works. What that mode does on this machine is measured the first time you
run `camp up`.

Run it with no configuration anywhere and it reports only the machine;
run it inside an environment and it also reports that environment.

## Uninstall

```
sudo rm /usr/local/bin/camp
sudo rm /etc/apparmor.d/camp
sudo systemctl reload apparmor
```

Nothing else is left behind: camp writes only inside its own `.camp`
directory in an environment, and inside your user's state directory
(`~/.local/state/camp`) when the system-wide mode is used. Removing those
two removes every trace. It never wrote anything into a repository.
