package fsx_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/fsx"
)

// What these two measure is the base itself, which is the one part of an
// area the strict component walk never protected.
//
// Everything below the base is resolved by the kernel in the call that
// acts on it, following no symlink and never leaving the base. The base
// was a string, and a string is resolved again in every call that uses
// it -- so between the moment camp validated the environment and the
// moment it wrote anything, the owner of that directory could rename it
// away and leave a symlink to a repository at its name, and camp would
// write through the link. That is a repository modified by ordinary
// unprivileged camp, which the first invariant forbids outright.

// The rename and the swap: what camp holds is a descriptor, so a write
// after the swap lands in the directory camp opened and the repository
// left standing at its name is untouched.
func TestARenamedEnvironmentDoesNotRedirectAWriteIntoARepository(t *testing.T) {
	scratch := t.TempDir()
	env := filepath.Join(scratch, "env")
	mkdir(t, env)
	repository := filepath.Join(scratch, "code")
	mkdir(t, filepath.Join(repository, "src"))
	write(t, filepath.Join(repository, "src", "app.go"), "package main\n")
	write(t, filepath.Join(repository, "README.md"), "the product\n")

	// The environment as the configuration resolved it, opened once.
	area := fsx.Work(environment(t, env), "cbfbbb63ee0d")
	before := tree(t, repository)

	// Everything camp established about the environment is still true of
	// the directory. Only its name has moved, and a repository now answers
	// to the name camp was given.
	moved := filepath.Join(scratch, "moved")
	if err := os.Rename(env, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repository, env); err != nil {
		t.Fatal(err)
	}

	if err := area.Ensure(0o755); err != nil {
		t.Fatalf("the work area could not be made: %v", err)
	}
	if _, err := area.MkdirAll("gen"); err != nil {
		t.Fatalf("a directory inside it could not be made: %v", err)
	}
	if err := area.Write("exclude", []byte("/.claude\n"), 0o644); err != nil {
		t.Fatalf("a file inside it could not be written: %v", err)
	}
	if err := area.RemoveTree("gen"); err != nil {
		t.Fatalf("a directory inside it could not be removed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(moved, ".camp", "work", "cbfbbb63ee0d", "exclude")); err != nil {
		t.Errorf("the write did not land in the directory camp opened: %v", err)
	}
	if after := tree(t, repository); after != before {
		t.Errorf("camp wrote into the repository left at the environment's "+
			"name:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A base that is itself a symlink -- an env: written through one is how
// camp meets it -- is resolved once, when the root is opened, and the root
// holds what it resolved to. Pointing the link somewhere else afterwards
// moves nothing.
func TestARootOpenedOnALinkHoldsWhatTheLinkPointedAt(t *testing.T) {
	scratch := t.TempDir()
	intended := filepath.Join(scratch, "environment")
	mkdir(t, intended)
	repository := filepath.Join(scratch, "code")
	mkdir(t, filepath.Join(repository, "src"))
	write(t, filepath.Join(repository, "src", "app.go"), "package main\n")

	link := filepath.Join(scratch, "link")
	if err := os.Symlink(intended, link); err != nil {
		t.Fatal(err)
	}

	root := environment(t, link)
	if root.Name() != intended {
		t.Errorf("the root is called %q; it has to name the directory it "+
			"resolved to, because that is the one it holds", root.Name())
	}
	area := fsx.Reports(root)
	before := tree(t, repository)

	// The link now names a repository. A base resolved again at every write
	// would follow it.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repository, link); err != nil {
		t.Fatal(err)
	}

	if err := area.Ensure(0o755); err != nil {
		t.Fatalf("the reports area could not be made: %v", err)
	}
	if err := area.Write("cbfbbb63ee0d-1", []byte("a report\n"), 0o644); err != nil {
		t.Fatalf("the report could not be written: %v", err)
	}

	if _, err := os.Stat(filepath.Join(intended, ".camp", "reports", "cbfbbb63ee0d-1")); err != nil {
		t.Errorf("the report did not land where the root resolved to: %v", err)
	}
	if after := tree(t, repository); after != before {
		t.Errorf("the report landed in the repository the link was pointed at "+
			"afterwards:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// tree renders everything under a directory -- every name, its type, its
// mode, its size, when it was last changed, and the bytes of every file --
// so that "the repository is untouched" is a comparison and not an
// impression. A directory camp created inside it moves the directory's own
// modification time, and that shows up here even if the file is removed
// again.
func tree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %s %d %s", relative, info.Mode(), info.Size(),
			info.ModTime().UTC().Format(time.RFC3339Nano))
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += " " + string(data)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	return strings.Join(lines, "\n")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
