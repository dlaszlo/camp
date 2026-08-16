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

**git**, for a git-based composition. camp reads git — `ls-files`,
`ls-tree`, `worktree list` — to work out what each repository contributes
and to report what a session changed. It never writes git.

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

## Check that it worked

```
camp doctor
```

The line to look for is:

```
  ok   user namespaces  permitted, and a mount inside one succeeds
```

`doctor` does not read the switches and guess — it creates a namespace
with the same identity mapping a real session uses and tries to mount
something inside it. If it cannot, it says which restriction stopped it
and what to do about it.

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
