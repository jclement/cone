package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"
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
