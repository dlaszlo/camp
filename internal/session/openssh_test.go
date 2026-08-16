package session_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/envx"
	"github.com/dlaszlo/camp/internal/session"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The OpenSSH arrangement the documentation recommends, measured.
//
// It is an application of the mechanism and no part of camp's grammar:
// camp writes none of these files, knows no program's name, and would
// apply the same map to anything else that needed it. What is being
// checked is that the arrangement a composition can write actually works
// -- that git receives the declared command, that each entry point typed
// directly receives -F through a launcher, and that a launcher finds the
// original program without naming a distribution's directory.
//
// Nothing here needs a namespace: the launcher directory is an ordinary
// path on the session's PATH. The same arrangement reached through the
// composed tree is measured by the install-gated session tests.

// openssh is a scratch machine: the real programs somewhere on the
// inherited path, the composition's launchers somewhere else.
type openssh struct {
	Root string
	// Host is where the real ssh, scp and sftp live -- a stand-in for
	// /usr/bin, which nothing in this arrangement may name.
	Host string
	// Launchers is the directory the workspace repository would own.
	Launchers string
	// Log is where the fake programs record how they were invoked.
	Log string
	// SSHConfig is the user's own configuration, which is the file -F
	// names.
	SSHConfig string
}

func newOpenSSH(t *testing.T) *openssh {
	t.Helper()
	root := testenv.Root(t)
	machine := &openssh{
		Root:      root,
		Host:      filepath.Join(root, "host-bin"),
		Launchers: filepath.Join(root, "workspace-bin"),
		Log:       filepath.Join(root, "invocations.log"),
		SSHConfig: filepath.Join(root, "home", ".ssh", "config"),
	}
	testenv.Write(t, machine.SSHConfig, "Host fakehost\n  User nobody\n")

	// The real programs. ssh is a working transport: it runs the command
	// its caller asked the far end to run, locally, which is enough to
	// carry a git fetch.
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("asking git where its helpers live: %v", err)
	}
	machine.executable(t, filepath.Join(machine.Host, "ssh"),
		"#!/bin/sh\n"+
			"printf 'ssh %s\\n' \"$*\" >> "+machine.Log+"\n"+
			"for last; do :; done\n"+
			"PATH=\""+strings.TrimSpace(string(execPath))+":$PATH\"\n"+
			"export PATH\n"+
			"exec /bin/sh -c \"$last\"\n")
	for _, program := range []string{"scp", "sftp"} {
		machine.executable(t, filepath.Join(machine.Host, program),
			"#!/bin/sh\nprintf '"+program+" %s\\n' \"$*\" >> "+machine.Log+"\n")
	}

	// The launchers, as docs/install.md writes them: the original is found
	// through the path saved under a name this composition chose, never
	// through a hard-coded directory, and there is no test of any kind for
	// "am I inside camp".
	for _, program := range []string{"ssh", "scp", "sftp"} {
		machine.executable(t, filepath.Join(machine.Launchers, program),
			"#!/bin/sh\n"+
				"original=$(PATH=\"$OUTER_PATH\" command -v "+program+") || exit 127\n"+
				"exec \"$original\" -F \"$HOME/.ssh/config\" \"$@\"\n")
	}
	return machine
}

func (m *openssh) executable(t *testing.T, path, body string) {
	t.Helper()
	testenv.Write(t, path, body)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// environment resolves the session environment this arrangement declares.
func (m *openssh) environment(t *testing.T) []string {
	t.Helper()
	env := testenv.NewEnv(t)
	cfg := env.Config(t, env.YAML()+`
session:
  environment:
    GIT_SSH_COMMAND: "ssh -F ${HOME}/.ssh/config"
    PATH: "`+m.Launchers+`:$PATH"
    OUTER_PATH: "$PATH"
`)
	resolved, err := session.Resolve(cfg, env.Live, []string{
		"PATH=" + m.Host,
		"HOME=" + filepath.Join(m.Root, "home"),
	})
	if err != nil {
		t.Fatalf("the declarations did not resolve: %v", err)
	}
	return resolved.Env
}

func (m *openssh) invocations(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(m.Log)
	if err != nil {
		return ""
	}
	return string(data)
}

// Each entry point, typed by name, reaches the original program with -F.
// scp and sftp need their own launchers: they start ssh from a compiled-in
// absolute path, so wrapping ssh alone would never reach them.
func TestEachOpenSSHEntryPointReceivesTheUsersConfiguration(t *testing.T) {
	machine := newOpenSSH(t)
	environment := machine.environment(t)

	for _, program := range []string{"ssh", "scp", "sftp"} {
		path, err := envx.Command(program, envx.Value(environment, "PATH"))
		if err != nil {
			t.Fatalf("%s was not found on the session's PATH: %v", program, err)
		}
		if path != filepath.Join(machine.Launchers, program) {
			t.Fatalf("%s resolved to %q; the launcher directory is first on the "+
				"session's PATH and is what has to be selected", program, path)
		}

		command := exec.Command(path, "fakehost", "true")
		command.Env = environment
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("the %s launcher failed: %v\n%s", program, err, out)
		}
	}

	log := machine.invocations(t)
	for _, program := range []string{"ssh", "scp", "sftp"} {
		line := program + " -F " + machine.SSHConfig
		if !strings.Contains(log, line) {
			t.Errorf("the original %s was not reached with the user's "+
				"configuration.\nwanted a line starting %q, log:\n%s",
				program, line, log)
		}
	}
}

// git resolves ssh itself, and the declared command is what it uses --
// which is what makes git work in a session with no shell involved
// anywhere. The remote here is a real repository reached through the fake
// transport, so this is a fetch that either happens or does not.
func TestGitReachesARemoteThroughTheDeclaredCommand(t *testing.T) {
	machine := newOpenSSH(t)
	environment := machine.environment(t)

	remote := testenv.GitRepo(t, filepath.Join(machine.Root, "remote"))
	testenv.Write(t, filepath.Join(remote, "file"), "content\n")
	testenv.Commit(t, remote, "the remote's history")

	command := exec.Command("git", "ls-remote", "fakehost:"+remote)
	command.Env = append(environment,
		"LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git could not reach the remote through the declared command: "+
			"%v\n%s\ninvocations:\n%s", err, out, machine.invocations(t))
	}
	if !strings.Contains(string(out), "refs/heads/main") {
		t.Errorf("git listed no refs:\n%s", out)
	}
	if log := machine.invocations(t); !strings.Contains(log, "-F "+machine.SSHConfig) {
		t.Errorf("the declared command was not what git used:\n%s", log)
	}
}

// A launcher that cannot find the original fails loudly. camp reports no
// success that did not happen, and neither does the arrangement it
// documents: 127 is the shell's own "command not found", and a missing
// launcher makes the lookup itself fail.
func TestAnUnusableLauncherFailsLoudly(t *testing.T) {
	machine := newOpenSSH(t)

	// The original is nowhere: OUTER_PATH names an empty directory.
	command := exec.Command(filepath.Join(machine.Launchers, "ssh"), "fakehost")
	command.Env = []string{
		"OUTER_PATH=" + filepath.Join(machine.Root, "empty"),
		"HOME=" + filepath.Join(machine.Root, "home"),
	}
	err := command.Run()
	if err == nil {
		t.Fatal("a launcher that could not find the original reported success")
	}
	if !strings.Contains(err.Error(), "127") {
		t.Errorf("the launcher exited %v, wanted 127 -- the shell's own "+
			"'command not found'", err)
	}

	// And with no launcher at all, the lookup fails rather than quietly
	// selecting the host's program: the session's PATH is what decides.
	if _, err := envx.Command("ssh", filepath.Join(machine.Root, "empty")); err == nil {
		t.Error("a command that is on no directory of the session's PATH was " +
			"resolved anyway")
	}
}

// The arrangement names no distribution directory. /usr/bin/ssh is right
// on one machine and wrong on the next, and camp assumes no filesystem
// layout it does not have to.
func TestNoDistributionBinaryPathIsWrittenDown(t *testing.T) {
	root := testenv.RepoRoot(t)
	// An absolute path to one of the entry points, where a path starts
	// rather than as the tail of a longer one -- a workspace's own
	// .workspace/bin/ssh is the arrangement, not a violation of it.
	forbidden := regexp.MustCompile(`(^|[\s"'` + "`" + `=(])/(usr/)?(local/)?bin/(ssh|scp|sftp)\b`)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".yml", ".yaml":
		default:
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		if relative == filepath.Join("internal", "session", "openssh_test.go") {
			return nil // this test names them in order to forbid them
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if found := forbidden.FindString(string(data)); found != "" {
			offenders = append(offenders, relative+" ("+strings.TrimSpace(found)+")")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("a distribution's own path is written down in %v", offenders)
	}
}
