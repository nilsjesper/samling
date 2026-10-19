package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nilsjesper/samling/internal/library"
)

// survivorHome writes a library with these bookmarks to a fresh SAMLING_HOME.
func survivorHome(t *testing.T, bs ...library.Bookmark) {
	t.Helper()
	t.Setenv("SAMLING_HOME", t.TempDir())
	lib := library.New()
	for _, b := range bs {
		lib.Bookmarks[b.ID] = b
	}
	if err := lib.Save(library.DefaultPath()); err != nil {
		t.Fatal(err)
	}
}

func kept(id string, n int, daysAgo int, tags ...string) library.Bookmark {
	b := library.Bookmark{
		ID:    id,
		URL:   "https://www.example.com/" + id,
		Title: "Article " + id,
		Time:  time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour).Unix(),
		Tags:  tags,
	}
	if n > 0 {
		b.Tags = append(b.Tags, library.KeptTag(n))
	}
	return b
}

func survivors(t *testing.T, args ...string) []string {
	t.Helper()
	var out bytes.Buffer
	if err := runSurvivors(args, &out, flag.ContinueOnError); err != nil {
		t.Fatalf("survivors %v: %v", args, err)
	}
	return strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
}

// titles pulls the article ids back out of survivors lines.
func titles(lines []string) string {
	var ids []string
	for _, l := range lines {
		if i := strings.Index(l, "Article "); i >= 0 {
			ids = append(ids, strings.Fields(l[i+len("Article "):])[0])
		}
	}
	return strings.Join(ids, " ")
}

var survivorFixture = []library.Bookmark{
	kept("a", 1, 10),
	kept("b", 3, 5),
	kept("c", 3, 400), // same count as b, saved earlier
	kept("d", 10, 1, "ifttt"),
	kept("e", 0, 900, "ifttt"),    // never kept
	kept("f", 2, 30, "samling02"), // the hand-made tag doesn't count; samling2 does
}

func TestSurvivorsSortByCountThenOldest(t *testing.T) {
	survivorHome(t, survivorFixture...)
	lines := survivors(t)
	if got := titles(lines); got != "d c b f a" {
		t.Errorf("order = %q, want d c b f a", got)
	}
	// Count, title, domain and age, with counts padded so ×10 and ×3 line up.
	if want := "kept ×10  Article d · example.com · 1 day old"; lines[0] != want {
		t.Errorf("line 0 = %q, want %q", lines[0], want)
	}
	if want := "kept ×3   Article c · example.com · 13 months old"; lines[1] != want {
		t.Errorf("line 1 = %q, want %q", lines[1], want)
	}
}

func TestSurvivorsMinAndLimit(t *testing.T) {
	survivorHome(t, survivorFixture...)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--min", "3"}, "d c b"},
		{[]string{"--min=2"}, "d c b f"},
		{[]string{"2"}, "d c"},
		{[]string{"2", "--min", "3"}, "d c"},
		{[]string{"--min", "3", "1"}, "d"},
		{[]string{"50"}, "d c b f a"},
	} {
		if got := titles(survivors(t, tc.args...)); got != tc.want {
			t.Errorf("survivors %v = %q, want %q", tc.args, got, tc.want)
		}
	}

	for _, args := range [][]string{{"--min", "0"}, {"0"}, {"2", "3"}, {"x"}} {
		if err := runSurvivors(args, io.Discard, flag.ContinueOnError); err == nil {
			t.Errorf("survivors %v: no error", args)
		}
	}
}

func TestSurvivorsWhenThereAreNone(t *testing.T) {
	survivorHome(t, kept("e", 0, 900, "ifttt"))
	if got := strings.Join(survivors(t), "\n"); !strings.HasPrefix(got, "No survivors yet.") {
		t.Errorf("output = %q, want the no-survivors message", got)
	}

	survivorHome(t, kept("a", 1, 10))
	if got := survivors(t, "--min", "4"); len(got) != 1 || got[0] != "Nothing kept 4 times or more yet." {
		t.Errorf("output = %q, want the --min message", got)
	}

	// No library at all yet: same friendly message, not an error.
	t.Setenv("SAMLING_HOME", t.TempDir())
	if got := survivors(t); !strings.HasPrefix(got[0], "No survivors yet.") {
		t.Errorf("output = %q on an empty home", got)
	}
}

func TestFilterMinKept(t *testing.T) {
	for _, tc := range []struct {
		min, kept int
		want      bool
	}{
		{0, 0, true}, {1, 0, false}, {1, 1, true}, {2, 1, false}, {2, 5, true},
	} {
		b := kept("x", tc.kept, 1)
		if got := (library.Filter{MinKept: tc.min}).Match(b); got != tc.want {
			t.Errorf("MinKept %d on kept ×%d = %v, want %v", tc.min, tc.kept, got, tc.want)
		}
	}
}

// parseKept runs pick's handling of --kept and the count together.
func parseKept(args []string) (minKept, count int, err error) {
	leading := -1
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if v, err := strconv.Atoi(args[0]); err == nil {
			leading, args = v, args[1:]
		}
	}
	fs := flag.NewFlagSet("pick", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.Int("n", 1, "")
	fs.Bool("starred", false, "")
	var k keptFlag
	fs.Var(&k, "kept", "")
	if err := fs.Parse(joinKeptValue(args)); err != nil {
		return 0, 0, err
	}
	count, err = resolvePickCount(leading, *n, fs.Args())
	return int(k), count, err
}

func TestKeptFlag(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		minKept, want int
	}{
		{nil, 0, 1},
		{[]string{"3", "--kept", "2"}, 2, 3}, // the example from the issue
		{[]string{"3", "--kept"}, 1, 3},      // bare --kept means 1
		{[]string{"--kept"}, 1, 1},
		{[]string{"--kept", "--starred", "4"}, 1, 4},
		{[]string{"--kept=1", "5"}, 1, 5},
		{[]string{"-kept", "4", "2"}, 4, 2},
		{[]string{"--kept", "2"}, 2, 1}, // a number after --kept is its value
	} {
		minKept, count, err := parseKept(tc.args)
		if err != nil || minKept != tc.minKept || count != tc.want {
			t.Errorf("pick %v = --kept %d, count %d, %v; want %d, %d", tc.args, minKept, count, err, tc.minKept, tc.want)
		}
	}
	for _, args := range [][]string{{"--kept=0"}, {"--kept=x"}, {"--kept", "-1"}} {
		if _, _, err := parseKept(args); err == nil {
			t.Errorf("pick %v: no error", args)
		}
	}
}

// list --kept end to end: only survivors come back.
func TestListKept(t *testing.T) {
	survivorHome(t, survivorFixture...)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"10", "--kept", "3"}, "b c d"},
		{[]string{"10", "--kept"}, "a b c d f"},
	} {
		out := captureStdout(t, func() error { return cmdPick(tc.args, false) })
		var ids []string
		for _, b := range survivorFixture {
			if strings.Contains(out, "Article "+b.ID+"\n") {
				ids = append(ids, b.ID)
			}
		}
		if got := strings.Join(ids, " "); got != tc.want {
			t.Errorf("list %v picked %q, want %q\n%s", tc.args, got, tc.want, out)
		}
	}
}

func captureStdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	ferr := f()
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	if ferr != nil {
		t.Fatal(ferr)
	}
	return string(out)
}
