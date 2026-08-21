package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jclement/cone/internal/board"
)

// The board is where cone is looked at all day, so a stale install has to be visible there
// and not only from `cone version`.
func TestViewShowsUpdateNotice(t *testing.T) {
	m := &model{w: 80}
	if got := m.View(); strings.Contains(got, "available") {
		t.Errorf("a current install advertised an update:\n%s", got)
	}

	updated, _ := m.Update(updateMsg{tag: "v0.2.0"})
	got := updated.View()
	if !strings.Contains(got, "v0.2.0 available") || !strings.Contains(got, "cone update") {
		t.Errorf("the update notice is missing from the status area:\n%s", got)
	}
}

func testModel(t *testing.T, tasks ...board.Task) *model {
	t.Helper()
	b, err := board.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		nt, err := b.New(task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Promote(nt.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Claim(nt.ID, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	m := &model{b: b, w: 100}
	m.reload()
	return m
}

func press(m *model, key string) *model {
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return next.(*model)
}

// d sits next to j and k, it acted instantly, and done/ is a directory — there is no undo to
// add. So it asks.
func TestCompletingATaskNeedsASecondPress(t *testing.T) {
	m := testModel(t, board.Task{Title: "ship the thing", Kind: "chore"})
	id := m.rows[0].task.ID

	m = press(m, "d")
	if _, err := m.b.Find(id); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.b.List(board.Done); len(list) != 0 {
		t.Fatal("one keypress completed a task")
	}
	if !strings.Contains(m.status, "again") {
		t.Errorf("nothing told the user a second press was wanted (status %q)", m.status)
	}

	m = press(m, "d")
	if list, _ := m.b.List(board.Done); len(list) != 1 {
		t.Fatal("the confirmed press did not complete it")
	}
}

// Confirmation must not survive moving to a different task: the whole point is that the second
// press means "yes, that one".
func TestAPendingConfirmationDoesNotFollowTheCursor(t *testing.T) {
	m := testModel(t,
		board.Task{Title: "first thing", Kind: "chore"},
		board.Task{Title: "second thing", Kind: "chore"})

	m = press(m, "d")
	m = press(m, "j") // move down
	m = press(m, "d")

	if list, _ := m.b.List(board.Done); len(list) != 0 {
		t.Fatal("a confirmation armed on one task fired on another")
	}
}

// There is nowhere to type a result here, and completing an investigation without one is the
// failure the board exists to prevent. Send them to the CLI rather than filing an empty result.
func TestTheTUIWillNotSilentlyCloseAnInvestigation(t *testing.T) {
	m := testModel(t, board.Task{Title: "why is it slow", Kind: "investigate"})

	m = press(m, "d")
	m = press(m, "d")

	if list, _ := m.b.List(board.Done); len(list) != 0 {
		t.Fatal("an investigation was completed with no result")
	}
	if !strings.Contains(m.status, "--result") {
		t.Errorf("the refusal did not say how to finish it properly (status %q)", m.status)
	}
}

// Every mutation reorders the list. Re-seating the cursor by index means completing one row
// slides a different task under it, and the next keypress acts on that one.
func TestTheCursorStaysOnItsTask(t *testing.T) {
	m := testModel(t,
		board.Task{Title: "first thing", Kind: "chore"},
		board.Task{Title: "second thing", Kind: "chore"},
		board.Task{Title: "third thing", Kind: "chore"})

	m = press(m, "j")
	m = press(m, "j")
	want := m.rows[m.cur].task.ID

	// Something else finishes the task above this one.
	if _, err := m.b.CompleteWith(m.rows[0].task.ID, ""); err != nil {
		t.Fatal(err)
	}
	m = press(m, "r")

	if got := m.rows[m.cur].task.ID; got != want {
		t.Fatalf("the cursor moved to %s; it was on %s", got, want)
	}
}

// A stale "press d again to complete X" left on screen while the user does something else
// reads as a live prompt. Any other key stands the confirmation down.
func TestAnyOtherKeyStandsTheConfirmationDown(t *testing.T) {
	m := testModel(t, board.Task{Title: "ship the thing", Kind: "chore"})

	m = press(m, "d")
	if m.pending == "" {
		t.Fatal("d did not arm a confirmation")
	}
	m = press(m, "x")
	if m.pending != "" {
		t.Fatalf("a confirmation stayed armed through an unrelated key: %q", m.pending)
	}
}

// The board was a snapshot of whenever you opened it: no timer, so it redrew only when you
// acted. Watching it work meant tailing a dotfile nobody knew was there.
func TestTheBoardRefreshesOnItsOwn(t *testing.T) {
	m := testModel(t, board.Task{Title: "first"})
	if m.Init() == nil {
		t.Fatal("no commands scheduled at startup — nothing drives a refresh")
	}

	before := len(m.rows)
	if _, err := m.b.New(board.Task{Title: "filed while you were looking at the screen"}); err != nil {
		t.Fatal(err)
	}
	if len(m.rows) != before {
		t.Fatal("test is not measuring what it thinks: rows changed without a tick")
	}

	updated, cmd := m.Update(tickMsg(time.Now()))
	if len(updated.(*model).rows) != before+1 {
		t.Error("a task filed by another agent never appeared")
	}
	if cmd == nil {
		t.Error("the tick did not schedule the next one — it refreshes once and then stops")
	}
}

// A live redraw must never eat the message you are reading, or the confirmation you are
// halfway through giving. d and b sit next to j and k for a reason.
func TestATickDoesNotDisturbWhatYouAreDoing(t *testing.T) {
	m := testModel(t, board.Task{Title: "one"})
	m.say("press d again to complete something")
	m.pending = "d:some-id"

	updated, _ := m.Update(tickMsg(time.Now()))
	got := updated.(*model)
	if got.pending != "d:some-id" {
		t.Error("a background refresh cancelled a pending confirmation")
	}
	if !strings.Contains(got.status, "press d again") {
		t.Errorf("a background refresh cleared the status line: %q", got.status)
	}
}

// The difference between a row that looks busy and one that is.
func TestAClaimShowsWhatItsWorkerIsDoing(t *testing.T) {
	m := testModel(t, board.Task{Title: "delegated"})
	if _, err := m.b.Set(m.rows[0].task.ID, "agent", "be-2175"); err != nil {
		t.Fatal(err)
	}
	m.reload()

	for _, c := range []struct{ status, want string }{
		{"working", "working"},
		{"done", "finished"},
		{"", "gone"},
	} {
		m.agents = map[string]string{"be-2175": c.status}
		if c.status == "" {
			m.agents = map[string]string{"someone-else": "idle"}
		}
		if got := m.worker(m.rows[0]); !strings.Contains(got, c.want) {
			t.Errorf("herdr says %q, board shows %q, want it to mention %q", c.status, got, c.want)
		}
	}
}

// Before herdr has answered, saying nothing beats guessing — an un-annotated claim is honest,
// "worker gone" on a healthy worker is not.
func TestNoWorkerAnnotationBeforeHerdrAnswers(t *testing.T) {
	m := testModel(t, board.Task{Title: "delegated"})
	m.b.Set(m.rows[0].task.ID, "agent", "be-2175")
	m.reload()

	if got := m.worker(m.rows[0]); got != "" {
		t.Errorf("annotated a claim before herdr had answered: %q", got)
	}
}

// The heartbeat going quiet is the failure this project is organised around, so it is on
// screen permanently rather than behind a command.
func TestTheSummaryReportsAQuietHeartbeat(t *testing.T) {
	m := testModel(t, board.Task{Title: "one"})

	if got := m.summary(); !strings.Contains(got, "never run") {
		t.Errorf("a heartbeat that has never run was not called out: %q", got)
	}
	m.beat = time.Now().Add(-8 * time.Hour)
	if got := m.summary(); !strings.Contains(got, "heartbeat 8h ago") {
		t.Errorf("a silent heartbeat was not reported: %q", got)
	}
	m.beat = time.Now().Add(-10 * time.Second)
	if got := m.summary(); !strings.Contains(got, "just now") {
		t.Errorf("a live heartbeat was not reported: %q", got)
	}
}

// A list that silently ends at the fold is how you miss the task you came to find.
func TestALongBoardScrollsAndSaysHowMuchIsHidden(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %d", i)
	}
	got := window(lines, 20, 10)

	if len(got) != 10 {
		t.Fatalf("windowed to %d lines, want 10", len(got))
	}
	if !strings.Contains(got[0], "more above") || !strings.Contains(got[len(got)-1], "more below") {
		t.Errorf("did not say what it was hiding:\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(strings.Join(got, "\n"), "row 20") {
		t.Error("scrolled the cursor off its own screen")
	}
}

// Timestamps are for humans here: the log stamps RFC3339, the screen wants a clock.
func TestActivityLinesReadAsAClock(t *testing.T) {
	stamped := "2026-08-21T21:35:08Z poked cone-lead (session \"be\") about 1 ready task(s)"
	got := activityLine(stamped, 120)

	if strings.Contains(got, "2026-08-21T") {
		t.Errorf("printed a raw RFC3339 stamp: %q", got)
	}
	if !strings.Contains(got, "poked cone-lead") {
		t.Errorf("lost the message: %q", got)
	}
	if plain := activityLine("no timestamp here", 120); !strings.Contains(plain, "no timestamp here") {
		t.Errorf("dropped a line it could not parse: %q", plain)
	}
}
