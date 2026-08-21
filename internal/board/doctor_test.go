package board

import (
	"context"
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

// fakeHerdrSessions writes a herdr that reports three running sessions where two of them —
// default and "be" — serve the SAME agents, which is what this machine actually does. The
// third has its own. `agent list` is answered per session: `herdr agent list` for default,
// `herdr --session <name> agent list` for the rest.
func fakeHerdrSessions(t *testing.T, shared, bob string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"session list\") printf 'NAME STATUS\\ndefault running\\nbe running\\nbob running\\n' ;;\n" +
		"  \"agent list\") printf '%s\\n' '" + shared + "' ;;\n" +
		"  \"--session be agent list\") printf '%s\\n' '" + shared + "' ;;\n" +
		"  \"--session bob agent list\") printf '%s\\n' '" + bob + "' ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// doctor reported "14 lead(s) visible to herdr" for seven real ones, because herdr's default
// socket serves the agents of some named sessions too and Agents appended every session's
// answer without asking whether it had seen that pane already.
//
// The unnamed lead is the case that matters and the case that regresses: `workspace create`
// names nothing, so most leads arrive with an empty name and are labelled by pane id. Any
// dedupe keyed on the name would either miss them or, worse, collapse two distinct unnamed
// panes into one — losing a live lead, which is the failure the count was hiding in reverse.
func TestAnAgentInTwoSessionsIsCountedOnce(t *testing.T) {
	const shared = `{"result":{"agents":[` +
		`{"name":"","pane_id":"w22:p1","agent_status":"idle","cwd":"/Users/jsc/Developer/timecop"},` +
		`{"name":"","pane_id":"w16:p1","agent_status":"done","cwd":"/Users/jsc/Developer/herdrer"},` +
		`{"name":"cone-lead","pane_id":"w28:p1","agent_status":"working","cwd":"/Users/jsc/Developer/cone"}` +
		`]}}`
	const bob = `{"result":{"agents":[` +
		`{"name":"","pane_id":"w3:p2","agent_status":"idle","cwd":"/Users/jsc/Developer/bob"}` +
		`]}}`

	agents, err := Agents(context.Background(), fakeHerdrSessions(t, shared, bob))
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for _, a := range agents {
		seen[a.PaneID]++
	}
	want := map[string]int{"w22:p1": 1, "w16:p1": 1, "w28:p1": 1, "w3:p2": 1}
	for pane, n := range want {
		if seen[pane] != n {
			t.Errorf("pane %s appears %d time(s), want %d", pane, seen[pane], n)
		}
	}
	if len(agents) != len(want) {
		t.Fatalf("got %d agents, want %d: %+v", len(agents), len(want), agents)
	}

	// The unnamed ones still carry a usable label, since that is what the poke addresses.
	for _, a := range agents {
		if a.Name == "" {
			t.Errorf("pane %s has no name to address it by", a.PaneID)
		}
	}

	// And the symptom the bug was reported as: the number a human reads under pressure.
	msg := strings.Join(findings(checkOrchestrators(fakeHerdrSessions(t, shared, bob)), OK), " ")
	if !strings.Contains(msg, "4 lead(s)") {
		t.Errorf("doctor said %q, want 4 lead(s)", msg)
	}
	if strings.Count(msg, "w22:p1") != 1 {
		t.Errorf("doctor listed w22:p1 %d times: %q", strings.Count(msg, "w22:p1"), msg)
	}
}
