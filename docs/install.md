# Installing camp

camp is a single binary with no runtime dependencies of its own. What it
needs is a Linux kernel with OverlayFS, `git`, and permission to create a
user namespace, because every session is built inside one.

## What the machine has to have

**Linux.** camp is built on OverlayFS and bind mounts. There is no macOS
or Windows version and there will not be one; the nearest equivalent is
to run the composition inside a Linux VM or container.

**OverlayFS in the kernel.** Every distribution kernel of the last decade
has it. To check:

```
grep overlay /proc/filesystems        # should print: nodev  overlay
```

If it is missing, `sudo modprobe overlay` loads it. On a machine that has
never used an overlay — a fresh cloud image is the usual case — the module
is present but not loaded, and `/proc/filesystems` does not list it until
something asks for one. camp reads that file and refuses, which is right:
a user namespace cannot load a module, so waiting for first use would
refuse every session. Make it load at boot:

```
echo overlay | sudo tee /etc/modules-load.d/overlay.conf
```

**A kernel with the mount API.** camp composes the tree through
`fsopen`, `fsconfig`, `fsmount` and `move_mount` rather than through
`mount(2)`, so that every layer reaches the kernel as a descriptor rather
than as a name something else could replace between the check and the
mount. `fsopen`, `open_tree` and `move_mount` are Linux 5.2; giving
OverlayFS its lower layers as descriptors — the `lowerdir+` form — is 6.7.
A kernel older than that cannot be given the guarantee, and camp says so
rather than falling back to names.

`camp doctor` answers all of this for the machine you are on, and its
answer is a real overlay mounted in a real namespace rather than a version
comparison.

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

## Install from a package

On Debian and Ubuntu, one command builds a package and one installs it:

```
git clone <this repository> camp
cd camp
sudo dpkg -i "$(packaging/deb/build)"
```

That is the whole installation. The package puts the binary at
`/usr/bin/camp`, installs the AppArmor profile that grants the namespace
permission with the path already pointing at it, and asks for the overlay
module at boot — the three steps the sections below describe by hand. It
needs `dpkg-deb` and a Go toolchain to build, and nothing at run time but
`git`.

To remove it: `sudo apt remove camp`, which takes the profile with it.

## Build and install by hand

```
git clone <this repository> camp
cd camp
go build -o camp ./cmd/camp
sudo install -m 755 camp /usr/local/bin/camp
```

`/usr/local/bin` rather than `/usr/bin`, because that is where what a
person installed by hand belongs — and the AppArmor profile names that
path, so the two agree.

## The namespace permission

camp builds every composition inside a user namespace, which is what lets
it need no privilege and leave nothing behind — and which is why it can
do nothing at all on a machine that refuses one.

**On most distributions this already works and there is nothing to do.**
Unprivileged user namespaces are permitted by default on Fedora, RHEL and
its rebuilds, Debian, Arch, and most others. Run `camp doctor`: if its
user namespaces line says permitted, skip the rest of this section.

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

There is no way around the permission itself. camp has no mode that
mounts with root instead: it once had one, and it was removed because
nothing used it. A machine that refuses the namespace cannot compose.

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

Run it with no configuration anywhere and it reports only the machine;
run it inside an environment and it also reports that environment.

## Moving from a hand install to the package

They install to different places on purpose — `/usr/local/bin` is where
what a person put there belongs, `/usr/bin` is the package's — so both can
be present at once, and `/usr/local/bin` wins the PATH. Take the hand
install off first:

```
sudo apparmor_parser -R /etc/apparmor.d/camp    # unload the old profile
sudo rm -f /usr/local/bin/camp /etc/apparmor.d/camp
sudo dpkg -i camp_*.deb
command -v camp                                 # /usr/bin/camp
camp doctor
```

The order matters only for the profile: it names the path the binary is
at, so the old one is unloaded before the package loads its own. Nothing
else of camp's is on the machine: no configuration outside an
environment's own `.camp`, and no state anywhere else.

A running session is unaffected — its mounts are the kernel's and outlive
the binary — and ends the way every session ends, with its last process.

## Uninstall

```
sudo rm /usr/local/bin/camp
sudo rm /etc/apparmor.d/camp
sudo systemctl reload apparmor
```

Nothing else is left behind: camp writes only inside its own `.camp`
directory in an environment. Removing that removes every trace. It never
wrote anything into a repository, and nothing of a session survives the
session.
