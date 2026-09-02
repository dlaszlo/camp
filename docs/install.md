# Installing camp

camp is a single static binary. It needs a Linux kernel with OverlayFS
and the mount API, permission to create a user namespace, `git`, and —
for joining a running session — util-linux `nsenter`. It needs no
privilege: there is no root mode, and nothing here asks you to run camp
itself as anything but yourself. With the default identity no other
program takes part in a session; the optional `uidmap` identity is the
one case that involves setuid helpers, and it is described below.

## What the machine has to have

**Linux.** camp is built on OverlayFS, bind mounts and Linux namespaces.
There is no macOS or Windows version and there will not be one; the
nearest equivalent is to run the composition inside a Linux VM or
container.

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
`fsopen`, `fsconfig`, `fsmount` and `move_mount` rather than through an
option string handed to `mount(2)`: each layer is given to the kernel as
an open descriptor, one call per layer, and the kernel records the
layers' real paths in the mount table — which is what lets camp's own
verification, and anyone reading `/proc/self/mountinfo`, see what was
actually mounted. `fsopen` and `move_mount` are Linux 5.2; giving
OverlayFS its layers this way — the `lowerdir+` form — is 6.7. A kernel
older than that cannot do it, and camp says so rather than falling back
to the option string, which would give up the readable table silently.

`camp doctor` answers all of this for the machine you are on, and its
answer is a real overlay mounted in a real namespace rather than a version
comparison.

**git.** Every composition needs it, whatever the configuration says:
planning asks git whether the code repository is a working tree and what
it tracks under each mount target — the rule that no mount may cover
tracked content — and without git that question has no answer, so a
check that could not run is not a check that passed, and the composition
is refused, including one whose directories are not repositories at all.
The same planning runs for `camp plan`, `camp status`, `camp explain`
and `camp doctor`. A git-based composition also needs it for the shipped
`git_exclude` step and for the scans a session runs when it ends. `camp
accept`, `camp init`, the two joins, help and version need no git. camp
never writes git. `camp doctor` reports a missing git as a failure.

```
sudo apt install git                  # Debian/Ubuntu
sudo dnf install git                  # Fedora
```

**nsenter**, from util-linux, for `camp shell --join` and `camp run
--join`. A join enters a running session's namespaces with `setns`, which
the kernel refuses to a multithreaded process — and a Go program is one
before its own code runs — so camp hands the namespace descriptors to
`nsenter`, which is single-threaded. Composing needs none of this: a
machine without `nsenter` runs every camp command except the two joins.
It is an essential package on every Debian-derived system, so its absence
is unusual; `camp doctor` reports it as a warning and names the package.

**`newuidmap` and `newgidmap`, only for `identity: uidmap`.** The
default identity maps your own uid to itself and involves no other
program. A configuration that declares `identity: uidmap` maps a whole
range in podman's `keep-id` shape, through those two setuid helpers (the
`uidmap` package on Debian and Ubuntu) and a subordinate range assigned
to you in `/etc/subuid` and `/etc/subgid`. camp refuses to start that
route when the programs are missing; `camp doctor` does not check for
them or for the ranges.

**Nothing else.** camp does not call `mount(8)`, `umount(8)`, `fuser` or
any other tool: it makes the mounts by syscall, and asks `/proc` for the
state. There is nothing to install for those.

To build it you need **Go 1.25 or newer**; to build the Debian package,
`dpkg-deb` as well.

## Install from a package

On Debian and Ubuntu, a package does all of it at once — the binary at
`/usr/bin/camp`, the AppArmor profile that grants the namespace
permission with the path already pointing at it, and the overlay module
at boot — the three steps the sections below describe by hand. Take the
one from the latest release:

```
gh release download --repo dlaszlo/camp --pattern '*.deb'
sudo dpkg -i camp_*.deb
camp doctor                           # says whether this machine can run camp
```

Or from [the releases page](https://github.com/dlaszlo/camp/releases), if
you would rather not have `gh`. A release is built by the release
workflow — from a pushed `v*` tag, or by hand with a version given to
it — which runs `go build`, `go vet` and `go test ./...` on the runner
before it builds the package, and publishes nothing that failed them.

The same package is built from a checkout, which is what the tests do:

```
git clone https://github.com/dlaszlo/camp
cd camp
sudo dpkg -i "$(packaging/deb/build)"
```

That needs `dpkg-deb` and a Go toolchain to build, and declares `git` as
its one dependency. To remove it: `sudo apt remove camp`, which takes the
profile with it.

## Build and install by hand

```
git clone https://github.com/dlaszlo/camp
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
mounts with root instead. A machine that refuses the namespace cannot
compose.

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

The join needs nothing more from the profile. The restriction mediates
*creating* a user namespace; joining one that your own session created
needs only the capabilities your uid already holds in it, and `nsenter`
started from the profiled binary inherits its label.

## ssh inside a session

By default a session maps exactly one user id — yours — into its
namespace (the `uidmap` identity maps a range, and this section is about
the default). Every file on the machine owned by anyone else is then
shown with the kernel's overflow id, which is to say as `nobody`:

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
is the property the whole approach rests on. Rootless podman shows the
same thing; it is less visible there only because a container brings its
own `/etc`.

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
environment it serves. A joined shell gets the same declarations,
resolved against its own terminal's environment.

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

It checks seven things about the machine — the platform, `/proc`,
OverlayFS, the mount API, `git`, `nsenter`, and user namespaces. All but
`nsenter` are failures when missing, because camp cannot compose without
them; `nsenter` is a warning, because only the two joins need it. The
line to look for is:

```
  ok   user namespaces  permitted, and a real overlay in one mounts, copies up and whiteouts, with userxattr
```

`doctor` does not read the switches and guess — it creates a namespace
with the same identity mapping a real session uses, builds a real overlay
inside it, writes through it and removes through it. If any of that
fails, it says which restriction stopped it and what to do about it. The
scratch tree it uses lives inside that namespace and goes with it.

Run it with no configuration anywhere and it reports only the machine;
run it inside an environment and it also reports that environment: the
locale, which filesystems its paths sit on and what is locked there,
storage whose composition no longer exists, work directories a session
left for the next start to sweep, worktrees git considers prunable, and
session reports waiting to be read.

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
the binary — and ends the way every session ends, when its shell or
command exits.

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
