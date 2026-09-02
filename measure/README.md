# measure — the machine camp is measured on

Not part of camp: nothing here is imported by the tool, and nothing here
ships. It is the machine on which camp's own suite runs the way nobody's
laptop can run it, and the list of what runs there.

What is here:

- `vm`, a machine that exists for the length of one run, on which
  everything runs with no person at all;
- `vm/guest`, the stages it runs, in name order.

`.github/workflows/ci.yml` runs the same stage files on a hosted runner,
which is the same thing by another route: a machine that is thrown away
when the job ends.

## On a machine of its own — nobody needed

```
vm/run
```

Boots a machine that exists for one run, copies this repository into it,
and runs every stage in `vm/guest` in name order. It needs qemu and KVM,
uses no root on your machine, and leaves a log behind. `vm/run --shell`
leaves you an ssh session in it instead.

That is where the AppArmor install gate stops being a problem: the
namespace permission is granted to one *installed* binary path, so the
tests that need a real composition skip from a checkout and run only
through an installed camp. Inside a machine that is thrown away, an
install is free, and the machine is the only thing on the machine.

**Adding a measurement is adding a file to `vm/guest`.** They run in name
order, each is a program that exits non-zero when what it measures does
not hold, and both the virtual machine and the hosted runner discover them
the same way.

## What the stages measure

The suite in the ordinary build; camp installed from the package it builds
itself, with the profile naming the installed path; a composition camp
accepted on that machine; `camp doctor` proving a real overlay inside a
real namespace; one ordinary session, and that nothing of camp's is left
mounted after it; what a session hands its workload and what it never
hands a pipe; and the suite twice more -- inside a composition camp
opened, where nothing may skip, and in a namespace of its own with the
restriction turned off, on ext4 and on tmpfs both.

Two drivers used to live here as well, `killmatrix` and `renamerace`.
They interrupted and raced a root helper that camp no longer has: camp
composes only inside a user namespace of its own, holds no privilege, and
leaves the kernel to take the composition apart. What they measured is
recorded in the design record beside this repository; nothing they
measured exists to be measured any more.

## Reading a failure

Every stage names the object rather than describing it: a mount is quoted
as its `/proc/self/mountinfo` line, a variable as the value that arrived.
That is deliberate. These are run when somebody wants to know whether camp
survives a thing, and a verdict that only said "failed" would send them
back to do the measurement again by hand.
