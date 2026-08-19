# measure — the instruments, and the machine to run them on

Not part of camp, and deliberately: **a separate Go module that imports
nothing of camp's.** These read the kernel's mount table and the trees on
disk with their own eyes, because an instrument built out of camp's own
parsing would agree with camp by construction, and agreement is what is
being tested. They live in this repository so that what measures camp
arrives with camp, and so that continuous integration needs one checkout
rather than two.

What is here:

- `killmatrix` and `renamerace`, the two drivers the code review asked
  for;
- `measure`, one command that runs the terminal-gated group on the
  machine you are sitting at;
- `vm`, a machine that exists for the length of one run, on which all of
  it runs with no person at all.

Everything except the ssh and keyring group can run unattended now. The
`vm` directory is how, and `.github/workflows/ci.yml` runs the same stage
files on a hosted runner, which is the same thing by another route: a
machine that is thrown away when the job ends.

## On a machine of its own — nobody needed

```
vm/run
```

Boots a machine that exists for one run, copies this repository into it,
and runs every stage in `vm/guest` in name order. It needs qemu and KVM,
uses no root on your machine, and leaves a log behind. `vm/run --shell`
leaves you an ssh session in it instead.

That is where sudo's password, the AppArmor install gate and the
machine-wide mounts all stop being a problem: inside a machine that is
thrown away, passwordless sudo is not a hole, an install is free, and the
machine is the only thing on the machine.

**Adding a measurement is adding a file to `vm/guest`.** They run in name
order, each is a program that exits non-zero when what it measures does
not hold, and both the virtual machine and the hosted runner discover them
the same way.

## On the machine you are sitting at

```
./measure
```

Builds both binaries, does one ordinary `camp up` and `camp down` to show
the mount paths work at all, then runs both drivers — stopping at the
first stage that fails, because a stage measuring what the one before it
left measures nothing. Everything it printed is also in `measure.log`, so
somebody who was not at the terminal can read it afterwards.

A single stage on its own: `./measure build`, `./measure run`,
`./measure killmatrix`, `./measure renamerace`. It takes the environment
from `CAMP_ENV` and the camp repository from `CAMP_REPO` if the defaults
(`~/campcheck` and the sibling checkout) are not where they are.

The rest of this file is what the two drivers do and how to run them by
hand.

## Building the camp they drive

Both need a camp with the barrier protocol compiled in. That build is not
one anybody ships: a pause inside the root helper that the invoking user
can trigger is the attack the measurements exist to prove camp is safe
from, so it exists only under a build tag.

```
cd <the camp repository>
go build -tags camptest -o ~/campcheck/camp-barriers ./cmd/camp
```

The helper is the same executable, so no install is needed: run *that*
binary and its `sudo camp helper-mount` is itself.

## `killmatrix` — recovery from the record alone

Interrupts the privileged helper at each of the eight boundaries the
review lists, deletes the configuration, and requires `camp down` to take
the composition apart from its record alone. `mount-made` fires once per
nested mount and each one is measured separately.

```
go run ./killmatrix -env ~/campcheck -camp ~/campcheck/camp-barriers
```

What it requires at every boundary: every mount camp made is gone, no
unrelated mount is gone, the repositories and the storage hash the same
before and after, the record survives exactly as long as something is
still standing, and anything that could not be removed is named.

## `renamerace` — the environment's name swapped underneath root

Renames the environment away at four of the helper's resolutions and
leaves a symbolic link to a root-owned trap tree at its name, then lets
the helper carry on.

```
go run ./renamerace -env ~/campcheck -camp ~/campcheck/camp-barriers
```

The assertion is **not** that camp errors. camp may refuse and camp may
carry on; what it may never do is act at the link's target. So what is
measured is the trap tree and the rest of the machine: no mount id
outside the environment changes, no mount attribute outside it changes,
no inode mode outside it changes, and the trap tree hashes identically
before and after.

## Both of them

- Run them as yourself, not with sudo. camp's front end refuses to run as
  root on purpose, and these drive the front end; camp elevates for the
  one step that needs it.
- Point them at a scratch composition. They take it up and down many
  times, rename its root, and kill things in the middle of it.
- They print a verdict, not a log: one line per case while they run, and
  at the end what failed, with what was seen and what was required.
- Exit 0 means every case held, 1 means something failed, 2 means
  something could not be measured — which is not a pass.

## Reading a failure

Every failure names the object rather than describing it: a mount is
quoted as its whole `/proc/self/mountinfo` line, a tree as the two hashes
that differ, a mode as the path and the two values. That is deliberate.
These are run when somebody wants to know whether camp survives a thing,
and a verdict that only said "failed" would send them back to do the
measurement again by hand.
