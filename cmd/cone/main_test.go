package main

import (
	"flag"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// `cone new "why is the solver slow" -kind investigate` filed a task titled
// "why is the solver slow -kind investigate" and silently ignored the kind, because Go's flag
// package stops at the first positional. Every command here takes a sentence, so that default
// is wrong for all of them.
func TestFlagsCanFollowFreeText(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("new", flag.ContinueOnError)
		fs.String("kind", "investigate", "")
		fs.String("repo", "", "")
		fs.Bool("ready", false, "")
		return fs
	}

	cases := []struct {
		name  string
		args  []string
		title string
		kind  string
		ready bool
	}{
		{"flags after the title", []string{"why is it slow", "-kind", "chore"}, "why is it slow", "chore", false},
		{"flags before the title", []string{"-kind", "chore", "why is it slow"}, "why is it slow", "chore", false},
		{"flags on both sides", []string{"-repo", "be", "why", "-ready"}, "why", "investigate", true},
		{"equals form", []string{"why", "-kind=review"}, "why", "review", false},
		{"a bool does not eat the next word", []string{"-ready", "why is it slow"}, "why is it slow", "investigate", true},
		{"an unknown dash word stays in the text", []string{"why -kind matters", "-x"}, "why -kind matters -x", "investigate", false},
		{"-- ends the flags", []string{"--", "-kind", "is part of the title"}, "-kind is part of the title", "investigate", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newFS()
			if err := fs.Parse(reorder(fs, c.args)); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fs.Args(), " "); got != c.title {
				t.Errorf("title = %q, want %q", got, c.title)
			}
			if got := fs.Lookup("kind").Value.String(); got != c.kind {
				t.Errorf("kind = %q, want %q", got, c.kind)
			}
			if got := fs.Lookup("ready").Value.String() == "true"; got != c.ready {
				t.Errorf("ready = %v, want %v", got, c.ready)
			}
		})
	}
}

// A value that itself looks like a flag must survive: `-result -1 is not a finding` is text.
func TestAFlagValueIsTakenVerbatim(t *testing.T) {
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	fs.String("result", "", "")
	got := reorder(fs, []string{"20260819-x", "-result", "-1 was the culprit"})
	want := []string{"-result", "-1 was the culprit", "--", "20260819-x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reorder = %q, want %q", got, want)
	}
}

// `cone new --help` filed a task titled "help". An agent's first guess at an unfamiliar CLI
// is --help — `cone --help` works — and reorder's rule that an unknown dash word is title
// text turned that guess into a silent junk row on the board. These three cases are the
// contract: help answers, a mistyped flag refuses, and -- still buys a literal dash title.
func TestNewRefusesFlagsItDoesNotKnow(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantErr   bool
		wantHelp  bool
		wantTitle string // the task that must exist afterwards; empty means the board stays empty
	}{
		{name: "help is answered, not filed", args: []string{"--help"}, wantHelp: true},
		{name: "the short form too", args: []string{"-h"}, wantHelp: true},
		{name: "an unknown flag is a usage error", args: []string{"--dry-run"}, wantErr: true},
		{name: "and is not rescued by a title beside it", args: []string{"why is it slow", "-x"}, wantErr: true},
		{name: "-- buys a literal dash title", args: []string{"--", "-literal-dash-title"}, wantTitle: "-literal-dash-title"},
		{name: "a known flag still parses", args: []string{"-kind", "chore", "tidy up"}, wantTitle: "tidy up"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CONE_HOME", t.TempDir())

			out, err := captureStdout(t, func() error { return cmdNew(c.args) })
			if c.wantErr != (err != nil) {
				t.Fatalf("cmdNew(%q) error = %v, want error: %v", c.args, err, c.wantErr)
			}
			if c.wantHelp && !strings.Contains(out, "usage: cone new") {
				t.Errorf("cmdNew(%q) printed %q, want usage", c.args, out)
			}

			b, err := board.Open(os.Getenv("CONE_HOME"))
			if err != nil {
				t.Fatal(err)
			}
			filed, err := b.List(board.Inbox)
			if err != nil {
				t.Fatal(err)
			}
			if c.wantTitle == "" {
				if len(filed) != 0 {
					t.Fatalf("cmdNew(%q) filed %q; nothing should reach the board", c.args, filed[0].Title)
				}
				return
			}
			if len(filed) != 1 {
				t.Fatalf("cmdNew(%q) filed %d tasks, want 1", c.args, len(filed))
			}
			if filed[0].Title != c.wantTitle {
				t.Errorf("title = %q, want %q", filed[0].Title, c.wantTitle)
			}
		})
	}
}

// captureStdout runs fn with os.Stdout redirected, because "prints usage" and "files nothing"
// are the same assertion seen from two sides.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	fnErr := fn()
	os.Stdout = saved
	w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), fnErr
}
