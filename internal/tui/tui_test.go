package tui

import (
	"strings"
	"testing"

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
