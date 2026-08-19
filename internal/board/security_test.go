package board

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A task id is a filename, never a path. filepath.Join calls Clean, so an id containing ".."
// escapes the board root — and Claim would hardlink the target into doing/ and then DELETE the
// original. This was arbitrary .md deletion anywhere the user can write, reachable from both
// the CLI and the MCP server.
func TestTaskIDCannotEscapeTheBoard(t *testing.T) {
	root := t.TempDir()
	b, err := Open(filepath.Join(root, "board"))
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "NOTES.md")
	if err := os.WriteFile(victim, []byte("---\nid: x\ntitle: precious\n---\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{
		"../../NOTES", "../NOTES", "..%2FNOTES", "a/b", "/etc/passwd", "", ".", "..",
		"foo/../../NOTES",
	} {
		if _, err := b.Claim(id, "attacker"); err == nil {
			t.Fatalf("Claim accepted a traversing id: %q", id)
		}
		if _, err := b.Find(id); err == nil {
			t.Fatalf("Find accepted a traversing id: %q", id)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the file outside the board was destroyed: %v", err)
	}
}

// Only `title` used to be quoted, and quote() ignored newlines entirely — so any other field
// could smuggle extra frontmatter lines. `repo` arrives verbatim from a remote inbox, which
// made `auto: true` reachable by a hostile upstream, defeating the only autonomy control.
func TestFrontmatterCannotBeInjectedThroughAnyField(t *testing.T) {
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := b.New(Task{
		Title: "harmless",
		Repo:  "be\nauto: true",
		Kind:  "investigate\n---\nauto: true",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Find(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Auto {
		t.Fatal("a task authorised itself through an injected field — the autonomy dial is defeated")
	}
	raw, _ := os.ReadFile(got.Path)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "auto: true" {
			t.Fatalf("injected frontmatter reached the file:\n%s", raw)
		}
	}
	if got.Repo != "be\nauto: true" {
		t.Errorf("the value should round-trip intact, escaped: %q", got.Repo)
	}
}

// os.Rename silently destroys whatever is at the destination. Claim uses os.Link precisely to
// avoid that; every other transition used Rename, so a duplicate id turned a state change into
// permanent deletion of a task and its body, while reporting success.
func TestMoveRefusesToClobber(t *testing.T) {
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task, err := b.New(Task{Title: "one", Body: "the original body"})
	if err != nil {
		t.Fatal(err)
	}
	// Forge the collision the way a crash or a hand-edit would.
	decoy := b.path(Ready, task.ID)
	if err := os.WriteFile(decoy, []byte("---\nid: "+task.ID+"\ntitle: decoy\n---\n\nDO NOT LOSE ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err == nil {
		t.Fatal("Promote overwrote an existing task in the destination state")
	}
	data, err := os.ReadFile(decoy)
	if err != nil || !strings.Contains(string(data), "DO NOT LOSE ME") {
		t.Fatalf("the destination file was destroyed: %v %q", err, data)
	}
}

// A doing/ entry with no claim stamp is the worst case, not an exempt one: a failed stamp or a
// hand-mv puts it there, it occupies a worker slot forever, and Stale used to skip it.
func TestStaleReportsUnstampedClaims(t *testing.T) {
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task, _ := b.New(Task{Title: "orphan"})
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}
	t2, _ := b.Find(task.ID)
	if err := b.move(t2, Doing); err != nil { // no claim stamp, as a hand-mv would leave it
		t.Fatal(err)
	}
	stale, err := b.Stale(8 * 60 * 60 * 1e9)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("an unstamped doing/ task must be reported, got %d", len(stale))
	}
}

// Agents are told to accept a lost race and move on, so a typo must not look like one.
func TestClaimDistinguishesMissingFromLostRace(t *testing.T) {
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim("no-such-task", "a"); err == nil || strings.Contains(err.Error(), "claimed it first") {
		t.Fatalf("a missing task must not read as a lost race: %v", err)
	}
	task, _ := b.New(Task{Title: "still in inbox"})
	_, err = b.Claim(task.ID, "a")
	if err == nil || !strings.Contains(err.Error(), "inbox") {
		t.Fatalf("an untriaged task should say where it actually is: %v", err)
	}
}

// Two tasks whose titles agree in their first 48 characters share a slug on the same day. The
// board refused the second, and for a remote inbox it refused it again on every retry — the
// request was simply never filed.
func TestASecondTaskWithTheSameSlugIsFiledNotLost(t *testing.T) {
	b := tmpBoard(t)
	a, err := b.New(Task{Title: "investigate the slow well-loading query in the reporting export"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.New(Task{Title: "investigate the slow well-loading query in the reporting API"})
	if err != nil {
		t.Fatalf("the second request was lost: %v", err)
	}
	if a.ID == c.ID {
		t.Fatalf("both tasks got id %s — one file overwrote the other", a.ID)
	}
	for _, id := range []string{a.ID, c.ID} {
		if _, err := b.Find(id); err != nil {
			t.Errorf("%s is not on the board: %v", id, err)
		}
	}
}

// An explicitly supplied id is a caller's assertion about identity — deduplication depends on
// it, so it must still refuse rather than quietly file a second copy under a new name.
func TestAnExplicitIDStillRefusesToCollide(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{ID: "20260819-fixed", Title: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.New(Task{ID: "20260819-fixed", Title: "two"}); err == nil {
		t.Fatal("filed a second task under an id the caller said was unique")
	}
}

// Two agents posting about the same topic in the same second produced the same filename, and
// os.WriteFile truncates: one message was silently destroyed.
func TestTwoMessagesInTheSameSecondBothSurvive(t *testing.T) {
	b := tmpBoard(t)
	first, err := b.Post("alice", "vitest isolation", "four entries do not need isolating")
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Post("bob", "vitest isolation", "the list in the wiki is stale")
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path {
		t.Fatalf("both messages wrote to %s", first.Path)
	}
	msgs, err := b.Read(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("read %d messages, want 2 — one was overwritten", len(msgs))
	}
}

// The same title typed twice is the same request, not two — that one still refuses, because
// a queue full of accidental duplicates is the failure the board exists to prevent.
func TestTheSameTitleTwiceIsStillRefused(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "look at the solver"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.New(Task{Title: "look at the solver"}); err == nil {
		t.Fatal("filed the same request twice")
	}
}

// A task from an inbox has already been deduplicated upstream by SourceRef, so a repeated
// title is a genuinely new request. Refusing it lost it on every retry, forever.
func TestARepeatedTitleFromAnInboxIsStillFiled(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "check the logs", Source: "meatprompt", SourceRef: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.New(Task{Title: "check the logs", Source: "meatprompt", SourceRef: "2"}); err != nil {
		t.Fatalf("a second, distinct request from the inbox was lost: %v", err)
	}
}
