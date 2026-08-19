package mountinfo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
)

// A table with the shapes that actually turn up: an ordinary mount with
// propagation, a private one with no optional fields, an overlay whose
// super options carry escaped paths, and a mount point containing a
// space.
const table = `25 30 0:23 / /proc rw,nosuid,nodev,noexec,relatime shared:5 - proc proc rw
30 1 252:1 / / rw,relatime - ext4 /dev/sda1 rw,errors=remount-ro
41 30 0:36 / /tmp rw,nosuid,nodev shared:9 - tmpfs tmpfs rw,inode64
88 30 0:52 / /home/x/live rw,relatime - overlay overlay rw,lowerdir=/home/x/ws,upperdir=/home/x/code,workdir=/home/x/.camp/work/abc/work,redirect_dir=nofollow,uuid=on,userxattr
89 30 252:1 /home/x/ws /home/x/ws ro,relatime - ext4 /dev/sda1 rw,errors=remount-ro
90 88 252:1 /home/x/odd /home/x/live/a\040b rw,relatime - ext4 /dev/sda1 rw
91 30 0:52 / /home/y/live rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/home/x/co\,de,workdir=/w
`

func read(t *testing.T) []mountinfo.Entry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := mountinfo.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestOptionalFieldsDecidePropagation(t *testing.T) {
	entries := read(t)

	root, ok := mountinfo.Top(entries, "/")
	if !ok {
		t.Fatal("/ was not parsed")
	}
	if !root.Private() {
		t.Error("/ has no optional fields here and should read as private")
	}
	proc, _ := mountinfo.Top(entries, "/proc")
	if proc.Private() {
		t.Error("shared:5 is propagation and must not read as private")
	}
}

func TestReadOnlyAndLockedFlagsComeFromThePerMountOptions(t *testing.T) {
	entries := read(t)

	workspace, ok := mountinfo.Top(entries, "/home/x/ws")
	if !ok {
		t.Fatal("the workspace self-bind was not parsed")
	}
	if !workspace.ReadOnly() {
		t.Error("a mount with ro in its options should read as read-only")
	}

	tmp, _ := mountinfo.Top(entries, "/tmp")
	for _, flag := range []string{"nosuid", "nodev"} {
		if !tmp.Has(flag) {
			t.Errorf("/tmp should carry %s; a read-only remount inside a user "+
				"namespace has to replicate it or fail EPERM", flag)
		}
	}
	if tmp.Has("noexec") {
		t.Error("noexec was reported for a mount that does not have it")
	}
}

func TestAMountPointWithASpaceIsDecoded(t *testing.T) {
	entries := read(t)
	if _, ok := mountinfo.Top(entries, "/home/x/live/a b"); !ok {
		t.Error("the escaped space in the mount point was not decoded")
	}
}

// The overlay's super options come back with the kernel's own additions,
// so they have to be compared per option and never as one string.
func TestOverlayOptionsAreSplitPerOption(t *testing.T) {
	entries := read(t)
	overlay, ok := mountinfo.Top(entries, "/home/x/live")
	if !ok {
		t.Fatal("the overlay was not parsed")
	}
	if overlay.FSType != "overlay" {
		t.Errorf("the filesystem type came out as %q", overlay.FSType)
	}
	if got := overlay.Super["upperdir"]; got != "/home/x/code" {
		t.Errorf("upperdir came out as %q", got)
	}
	if got := overlay.Super["lowerdir"]; got != "/home/x/ws" {
		t.Errorf("lowerdir came out as %q", got)
	}
	if _, added := overlay.Super["redirect_dir"]; !added {
		t.Error("the kernel's own added options should be visible, so that " +
			"comparison can ignore them deliberately rather than by accident")
	}
	if _, forced := overlay.Super["userxattr"]; !forced {
		t.Error("userxattr should be visible")
	}
}

// A path containing a comma is escaped inside the option string. Splitting
// naively would tear it in half and then compare two halves against a
// whole.
func TestAnEscapedCommaInAPathSurvives(t *testing.T) {
	entries := read(t)
	overlay, ok := mountinfo.Top(entries, "/home/y/live")
	if !ok {
		t.Fatal("the second overlay was not parsed")
	}
	if got := mountinfo.UnescapeOption(overlay.Super["upperdir"]); got != "/home/x/co,de" {
		t.Errorf("upperdir with a comma came out as %q", got)
	}
}

func TestOverlaysFindsEveryCompositionOnAnUpper(t *testing.T) {
	entries := read(t)
	found := mountinfo.Overlays(entries, "/home/x/code")
	if len(found) != 1 || found[0].Point != "/home/x/live" {
		t.Errorf("the scan for an overlay on /home/x/code found %v", found)
	}
	if len(mountinfo.Overlays(entries, "/home/x/co,de")) != 1 {
		t.Error("the escaped upperdir was not matched")
	}
}

// The privileged mode's own shape, copied from a real run: the live path
// carries a self-bind that gives the move a private parent, and the
// composed tree sits on top of it. The overlay was made first and MS_MOVE
// kept its identity, so it is listed first while standing highest -- the
// parent field is the only thing that says so.
const stacked = `33 1 252:1 / /home rw,relatime - ext4 /dev/sda1 rw
3575 3711 0:228 / /home/x/live rw,relatime - overlay overlay rw,lowerdir=/home/x/ws,upperdir=/home/x/code,workdir=/home/x/.camp/work/abc/work,nouserxattr
3711 33 252:1 /home/x/live /home/x/live rw,relatime - ext4 /dev/sda1 rw
`

func TestTheTopOfAStackIsReadFromTheParentAndNotTheOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(stacked), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := mountinfo.Read(path)
	if err != nil {
		t.Fatal(err)
	}

	top, ok := mountinfo.Top(entries, "/home/x/live")
	if !ok {
		t.Fatal("the live path has no mount")
	}
	if top.FSType != "overlay" {
		t.Fatalf("the live path resolves to a %s mount, and the composed tree "+
			"is the overlay standing on top of the self-bind", top.FSType)
	}
	if got := top.Super["upperdir"]; got != "/home/x/code" {
		t.Errorf("upperdir came out as %q", got)
	}
}

func TestUnderAndContaining(t *testing.T) {
	entries := read(t)

	under := mountinfo.Under(entries, "/home/x/live")
	if len(under) != 2 {
		t.Errorf("%d mounts found under the composed tree, wanted 2", len(under))
	}

	// The filesystem a path sits on is the longest mount point that is a
	// prefix of it -- that is where the locked flags come from.
	on, ok := mountinfo.Containing(entries, "/tmp/scratch/x")
	if !ok || on.Point != "/tmp" {
		t.Errorf("/tmp/scratch/x was placed on %q", on.Point)
	}
	on, ok = mountinfo.Containing(entries, "/var/log")
	if !ok || on.Point != "/" {
		t.Errorf("/var/log was placed on %q", on.Point)
	}
}

// A line camp cannot read stops the whole table.
//
// Everything camp asks the table decides by what is not in it:
// completeness compares the mounts that exist with the mounts that were
// planned, the residue scan refuses to build on what is already there,
// the steady-state guard looks for another composition on this upper, and
// a teardown walks what a record says is mounted. A dropped line is a
// partial answer handed over as a complete one, and every one of those
// checks would read it as good news.
func TestAnUnreadableLineRejectsTheWholeTable(t *testing.T) {
	good := "23 1 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1 rw"
	for _, damaged := range []string{
		"23 1 0:22 / /",
		"23 1 0:22 / / rw,relatime shared:1 ext4 /dev/sda1 rw",
		"x 1 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1 rw",
		"23 y 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1 rw",
		"23 1 zero / / rw,relatime shared:1 - ext4 /dev/sda1 rw",
		"23 1 0:22 / / rw,relatime shared:1 - ext4",
	} {
		path := filepath.Join(t.TempDir(), "mountinfo")
		body := good + "\n" + damaged + "\n" + good + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		entries, err := mountinfo.Read(path)
		if err == nil {
			t.Errorf("the damaged line %q was dropped and %d entries came back "+
				"as the table", damaged, len(entries))
			continue
		}
		if !strings.Contains(err.Error(), "line 2") {
			t.Errorf("the error does not say which line it was: %v", err)
		}
	}
}

// The kernel's grammar, not Go's.
//
// show_mountinfo separates fields with one literal 0x20 and escapes
// exactly " \t\n\\" inside the pathnames it prints. Carriage return,
// vertical tab and form feed are legal in a filename and go out raw, so a
// parser that split on Unicode whitespace saw an extra field wherever one
// of them appeared -- and because a record that does not parse stops the
// whole table, one unrelated mount with a \r in its name made every camp
// command on that machine refuse.
func TestRawControlBytesBelongToTheFieldTheyAreIn(t *testing.T) {
	for _, c := range []struct {
		name   string
		line   string
		root   string
		point  string
		source string
	}{
		{
			name:   "carriage return",
			line:   "36 25 0:33 /pull\rreq /mnt/build\rout rw,relatime - ext4 /dev/disk\rone rw",
			root:   "/pull\rreq",
			point:  "/mnt/build\rout",
			source: "/dev/disk\rone",
		},
		{
			name:   "vertical tab",
			line:   "36 25 0:33 /pull\vreq /mnt/build\vout rw,relatime - ext4 /dev/disk\vone rw",
			root:   "/pull\vreq",
			point:  "/mnt/build\vout",
			source: "/dev/disk\vone",
		},
		{
			name:   "form feed",
			line:   "36 25 0:33 /pull\freq /mnt/build\fout rw,relatime - ext4 /dev/disk\fone rw",
			root:   "/pull\freq",
			point:  "/mnt/build\fout",
			source: "/dev/disk\fone",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Named here so the fixture is visibly about the difference: this
			// is what the old grammar did to the same line.
			if len(strings.Fields(c.line)) == len(strings.Split(c.line, " ")) {
				t.Fatalf("the fixture carries no byte the two grammars disagree "+
					"about: %q", c.line)
			}

			entries := parsed(t, c.line)
			if len(entries) != 1 {
				t.Fatalf("%d records came back from one line", len(entries))
			}
			entry := entries[0]
			if entry.Root != c.root {
				t.Errorf("the root came out as %q, wanted %q", entry.Root, c.root)
			}
			if entry.Point != c.point {
				t.Errorf("the mount point came out as %q, wanted %q", entry.Point, c.point)
			}
			if entry.Source != c.source {
				t.Errorf("the source came out as %q, wanted %q", entry.Source, c.source)
			}
			if entry.FSType != "ext4" {
				t.Errorf("the filesystem type came out as %q", entry.FSType)
			}
		})
	}
}

// One mount nobody asked about must not take the rest of the table with
// it. This is the shape the report described: an unrelated mount carrying
// a raw byte, and camp's own overlay two lines further down.
func TestOneRawByteSomewhereElseDoesNotHideCampsOwnMount(t *testing.T) {
	entries := parsed(t,
		"25 30 0:23 / /proc rw,nosuid shared:5 - proc proc rw",
		"90 30 252:1 / /mnt/re\rlease rw,relatime - ext4 /dev/sdb1 rw",
		"91 30 0:52 / /home/x/live rw,relatime - overlay overlay rw,"+
			"lowerdir=/home/x/ws,upperdir=/home/x/code,workdir=/w")

	if len(entries) != 3 {
		t.Fatalf("%d of the three records came back", len(entries))
	}
	if _, found := mountinfo.Top(entries, "/home/x/live"); !found {
		t.Error("camp's own overlay was lost with the unrelated mount")
	}
	if len(mountinfo.Overlays(entries, "/home/x/code")) != 1 {
		t.Error("the guard against a second composition on this upper found nothing")
	}
}

// The four sequences the kernel does write, in the fields it writes them
// in.
func TestTheEncodedControlsAreDecoded(t *testing.T) {
	entries := parsed(t,
		`36 25 0:33 /a\040b /mnt/c\011d rw,relatime - ext4 /dev/e\012f rw`,
		`37 25 0:34 /g\134h /mnt/i\134j rw,relatime - ext4 /dev/k\134l rw`)

	if len(entries) != 2 {
		t.Fatalf("%d of the two records came back", len(entries))
	}
	for _, c := range []struct {
		what string
		got  string
		want string
	}{
		{`\040 in the root`, entries[0].Root, "/a b"},
		{`\011 in the mount point`, entries[0].Point, "/mnt/c\td"},
		{`\012 in the source`, entries[0].Source, "/dev/e\nf"},
		{`\134 in the root`, entries[1].Root, `/g\h`},
		{`\134 in the mount point`, entries[1].Point, `/mnt/i\j`},
		{`\134 in the source`, entries[1].Source, `/dev/k\l`},
	} {
		if c.got != c.want {
			t.Errorf("%s came out as %q, wanted %q", c.what, c.got, c.want)
		}
	}
}

// An escape the kernel cannot have written is left byte for byte.
//
// Decoding it would be guessing at a name, and a name camp guessed at is
// one it goes on to compare against a real path. The decoding is
// unambiguous because a backslash of its own is always \134: a file
// genuinely called \040 arrives as \134040 and comes back out as the four
// characters it is.
func TestAnInvalidOctalEscapeIsLeftAlone(t *testing.T) {
	for _, c := range []struct{ field, want string }{
		{`\04`, `\04`},
		{`\0`, `\0`},
		{`\`, `\`},
		{`\777`, `\777`},
		{`\101`, `\101`},
		{`\13`, `\13`},
		{`\134040`, `\040`},
		{`\1340`, `\0`},
		{`\040`, ` `},
	} {
		if got := mountinfo.Unescape(c.field); got != c.want {
			t.Errorf("%q decoded to %q, wanted %q", c.field, got, c.want)
		}
	}

	// And a record carrying one still parses: the field is a name, not a
	// grammar error.
	entries := parsed(t, `36 25 0:33 /a\099b /mnt/c rw,relatime - ext4 none rw`)
	if len(entries) != 1 {
		t.Fatalf("%d records came back from one line", len(entries))
	}
	if entries[0].Root != `/a\099b` {
		t.Errorf("the root came out as %q", entries[0].Root)
	}
}

// A tag this parser has never heard of is carried, not refused.
//
// The kernel is free to add one, and camp reads that field only to ask
// whether there is anything in it at all -- which is what Private()
// decides by, and any tag answers it. Refusing an unknown one would
// refuse a legal host on a newer kernel.
func TestAnUnknownOptionalFieldIsCarried(t *testing.T) {
	entries := parsed(t,
		"36 25 0:33 / /mnt rw,relatime shared:9 propagate_from:2 newtag:7 - ext4 /dev/sda1 rw")
	if len(entries) != 1 {
		t.Fatalf("the record with an unknown tag was refused")
	}
	if got := len(entries[0].Optional); got != 3 {
		t.Errorf("%d optional fields were kept, wanted 3: %v", got, entries[0].Optional)
	}
	if entries[0].Private() {
		t.Error("a mount with tags on it read as private")
	}
}

// Whatever the kernel wrote is a line camp can read, however long it is.
//
// The 1 MiB scanner cap this replaces was nobody's measured bound. The
// mount API takes an overlay's lower layers one at a time, so the option
// string the kernel prints back is bounded by how many layers a
// composition has, and a legal line refused for its length is the same
// refusal the whole-table policy was meant to make impossible.
func TestALineLargerThanTheOldCapIsRead(t *testing.T) {
	var options strings.Builder
	options.WriteString("rw")
	for options.Len() < 2*1024*1024 {
		options.WriteString(",lowerdir+=/home/x/layers/00000000")
	}
	options.WriteString(",upperdir=/home/x/code")

	line := "42 25 0:99 / /home/x/live rw,relatime - overlay overlay " + options.String()
	if len(line) <= 1024*1024 {
		t.Fatalf("the fixture is %d bytes and has to be larger than the old "+
			"1 MiB cap", len(line))
	}

	entries := parsed(t, line)
	if len(entries) != 1 {
		t.Fatalf("%d records came back from one long line", len(entries))
	}
	if got := entries[0].Super["upperdir"]; got != "/home/x/code" {
		t.Errorf("upperdir came out as %q", got)
	}
	if len(entries[0].Line) != len(line) {
		t.Errorf("the quoted record is %d bytes and the line was %d",
			len(entries[0].Line), len(line))
	}
}

// The kernel writes one space between fields. Two is an empty field, and
// an empty field is a record camp cannot assign meaning to -- absorbing
// it would mean guessing which mount the line describes.
func TestALiteralSpaceThatTheKernelDoesNotWriteIsRefused(t *testing.T) {
	good := "23 1 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1 rw"
	for _, c := range []struct{ name, line string }{
		{"a leading space", " " + good},
		{"a trailing space", good + " "},
		{"two between fields", "23 1 0:22 / /  rw,relatime shared:1 - ext4 /dev/sda1 rw"},
		{"two around the separator", "23 1 0:22 / / rw,relatime shared:1  - ext4 /dev/sda1 rw"},
		{"an empty line", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := readLines(t, c.line); err == nil {
				t.Errorf("%q was accepted", c.line)
			}
		})
	}
}

// Exactly three fields after the separator, in both directions.
//
// Not fewer: the guard read separator+3 > len(fields), which is false
// when two remain, so a record with no super options parsed with an empty
// one -- and an empty super options field is how the overlay comparison
// reads "no upperdir".
//
// Not more either, and that is safe because none of the three can carry a
// raw separator byte: show_mountinfo prints the filesystem type and the
// source through mangle(), and the super options through the seq_escape
// helpers a filesystem's show_options is written against, and the escape
// set of every one of them contains the space.
func TestTheSeparatorIsFollowedByExactlyThreeFields(t *testing.T) {
	for _, c := range []struct {
		name     string
		line     string
		accepted bool
	}{
		{
			name: "none",
			line: "23 1 0:22 / / rw,relatime shared:1 master:2 unbindable -",
		},
		{
			name: "one",
			line: "23 1 0:22 / / rw,relatime shared:1 master:2 - ext4",
		},
		{
			name: "two",
			line: "23 1 0:22 / / rw,relatime shared:1 master:2 - ext4 /dev/sda1",
		},
		{
			name:     "three",
			line:     "23 1 0:22 / / rw,relatime shared:1 master:2 - ext4 /dev/sda1 rw",
			accepted: true,
		},
		{
			name: "four",
			line: "23 1 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1 rw extra",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			entries, err := readLines(t, c.line)
			switch {
			case c.accepted && err != nil:
				t.Fatalf("a legal record was refused: %v", err)
			case !c.accepted && err == nil:
				t.Fatalf("%q was accepted as %+v", c.line, entries)
			}
			if !c.accepted {
				return
			}
			if entries[0].SuperRaw != "rw" {
				t.Errorf("the super options came out as %q", entries[0].SuperRaw)
			}
		})
	}
}

// The cheapest guard there is against a grammar stricter than the
// kernel's: this machine's own table, whatever is on it today.
//
// Reading /proc/self/mountinfo mounts nothing and changes nothing. A
// machine that will not hand it over skips.
func TestThisMachinesOwnTableParses(t *testing.T) {
	raw, err := os.ReadFile(mountinfo.Self)
	if err != nil {
		t.Skipf("%s cannot be read here: %v", mountinfo.Self, err)
	}
	if len(raw) == 0 {
		t.Skipf("%s is empty here", mountinfo.Self)
	}

	entries, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		t.Fatalf("this machine's own mount table does not parse, which means "+
			"camp's grammar is stricter than the kernel's: %v", err)
	}
	// Two reads of /proc are two snapshots, so the counts are compared only
	// as a sanity check on the line splitting, not for equality with the
	// second read.
	if len(entries) == 0 {
		t.Fatal("no records came back from a table that has bytes in it")
	}
	for _, entry := range entries {
		if entry.Point == "" {
			t.Errorf("a record parsed with no mount point: %q", entry.Line)
		}
		if entry.FSType == "" {
			t.Errorf("a record parsed with no filesystem type: %q", entry.Line)
		}
		if entry.SuperRaw == "" {
			t.Errorf("a record parsed with no super options: %q", entry.Line)
		}
	}
}

// readLines parses the given lines as a table and hands back whatever came
// of it, error included -- the fixtures that must be refused need the
// error rather than a failed test.
func readLines(t *testing.T, lines ...string) ([]mountinfo.Entry, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return mountinfo.Read(path)
}

// parsed is the same for the fixtures that have to parse.
func parsed(t *testing.T, lines ...string) []mountinfo.Entry {
	t.Helper()
	entries, err := readLines(t, lines...)
	if err != nil {
		t.Fatalf("a legal record was refused: %v", err)
	}
	return entries
}
