package board

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func findings(f []Finding, sev Severity) []string {
	var out []string
	for _, x := range f {
		if x.Severity == sev {
			out = append(out, x.Message)
		}
	}
	return out
}

func hasFinding(f []Finding, sev Severity, substr string) bool {
	for _, m := range findings(f, sev) {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// List skips a file it cannot parse, so a broken task does not merely look wrong — it does not
// exist. That is invisible exactly when someone is asking why their task never came up.
func TestDoctorSeesAFileThatDoesNotParse(t *testing.T) {
	b := tmpBoard(t)
	if err := os.WriteFile(filepath.Join(b.dir(Ready), "20260819-broken.md"),
		[]byte("no frontmatter here, just prose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if list, _ := b.List(Ready); len(list) != 0 {
		t.Fatal("precondition: List should have skipped it")
	}
	if !hasFinding(b.Doctor(""), Broken, "does not parse") {
		t.Fatal("doctor did not report an unparseable task")
	}
}

// The same id in two states makes Find return whichever directory it looks in first, so the
// board silently disagrees with itself about what that id means.
func TestDoctorSeesTheSameIDInTwoStates(t *testing.T) {
	b := tmpBoard(t)
	task, err := b.New(Task{Title: "one thing"})
	if err != nil {
		t.Fatal(err)
	}
	// Hand-place a second copy, the way an interrupted move or a stray cp would.
	data, _ := os.ReadFile(task.Path)
	if err := os.WriteFile(filepath.Join(b.dir(Done), task.ID+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasFinding(b.Doctor(""), Broken, "exists in both") {
		t.Fatalf("doctor did not report the duplicate:\n%v", findings(b.Doctor(""), Broken))
	}
}

// A claim with no claimant occupies a worker slot that nothing can ever release: the reaper
// matches on the claimant's name, and there isn't one.
func TestDoctorSeesAClaimWithNoClaimant(t *testing.T) {
	b := tmpBoard(t)
	task, err := b.New(Task{Title: "one thing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(task.ID, "someone"); err != nil {
		t.Fatal(err)
	}
	held, _ := b.Find(task.ID)
	held.ClaimedBy = ""
	if err := b.Save(held); err != nil {
		t.Fatal(err)
	}
	if !hasFinding(b.Doctor(""), Broken, "no claimed_by") {
		t.Fatal("doctor did not report a claim nothing can release")
	}
}

func TestDoctorIsQuietOnAHealthyBoard(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "one thing"}); err != nil {
		t.Fatal(err)
	}
	if got := findings(b.Doctor(""), Broken); len(got) != 0 {
		t.Fatalf("a healthy board reported problems: %v", got)
	}
}

// The index was rebuilt from scratch on every mutation — O(every file on the board) per write,
// with the heartbeat triggering it every minute. It is rebuilt on read instead, which has to
// stay correct for files this binary never saw.
func TestSearchRebuildsWhenTheFilesAreNewerThanTheIndex(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "the first thing", Body: "about widgets\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Search("widgets", 10); err != nil {
		t.Fatal(err)
	}

	// A second task written the way an agent with `mv` and `cat` would write it — never
	// through this binary, so nothing marked the index dirty.
	raw := "---\nid: 20260819-hand-written\ntitle: hand written\nkind: chore\n---\n\nabout sprockets\n"
	if err := os.WriteFile(filepath.Join(b.dir(Ready), "20260819-hand-written.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, err := b.Search("sprockets", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a task written directly to disk is invisible to search — the index is not rebuilt on read")
	}
}

// The index is a cache; a write must not pay for a full rebuild.
func TestWritingATaskDoesNotRebuildTheIndex(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "the first thing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Search("first", 10); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(b.IndexPath())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, err := b.New(Task{Title: fmt.Sprintf("another thing %d", i)}); err != nil {
			t.Fatal(err)
		}
	}

	after, err := os.Stat(b.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("filing a task rebuilt the whole search index")
	}
}

// A worker sits in a linked worktree; a lead sits in a main checkout. The test used to be the
// string "/worktrees/", which silently missed the older layouts on this machine —
// ~/Developer/barreleye.wt/review read as a lead, so the heartbeat would have offered it work
// meant for the session that delegates.
func TestALinkedWorktreeIsNotALead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	root := t.TempDir()
	main := filepath.Join(root, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	run(main, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(main, "f"), []byte("x"), 0o644)
	run(main, "add", "f")
	run(main, "commit", "-qm", "one")

	// A worktree whose path contains no hint of what it is — the case the string test missed.
	wt := filepath.Join(root, "elsewhere", "review")
	run(main, "worktree", "add", "-q", "-b", "review", wt)

	if !(Agent{CWD: main}).IsLead() {
		t.Error("the main checkout was not recognised as a lead")
	}
	if (Agent{CWD: wt}).IsLead() {
		t.Errorf("%s is a linked worktree and was treated as a lead", wt)
	}
	// Somewhere that is not a repository at all is still a lead — an agent parked in ~ or
	// /tmp is not a worker.
	if !(Agent{CWD: t.TempDir()}).IsLead() {
		t.Error("a directory that is not a repository was treated as a worktree")
	}
	if (Agent{CWD: ""}).IsLead() != true {
		t.Error("an unknown cwd should not be assumed to be a worktree")
	}
}
