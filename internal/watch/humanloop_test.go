package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// TestMain exists for one reason: the tick now reads this machine's real inboxes.json and
// human.json. A test run that pulled from the developer's actual phone queue would CLAIM tasks
// there — destructively — and file them onto a t.TempDir() board about to be deleted. Every
// test in this package therefore starts pointed at config files that do not exist; tests that
// want a config set their own with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cone-watch-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("CONE_INBOXES", filepath.Join(dir, "absent-inboxes.json"))
	os.Setenv("CONE_HUMAN", filepath.Join(dir, "absent-human.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// humanService fakes the ask-the-human endpoint: every question GET answers with the given
// status/value/note.
func humanService(t *testing.T, status, value, note string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"status": status, "answer_value": value, "answer_note": note,
		})
	}))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(t.TempDir(), "human.json")
	if err := os.WriteFile(cfg, []byte(`{"name":"svc","url":"`+srv.URL+`","token":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONE_HUMAN", cfg)
	return srv
}

// blockedOnQuestion files a task, walks it to blocked, and stamps a question id on it — the
// state `cone ask` leaves behind.
func blockedOnQuestion(t *testing.T, b *board.Board, qid string) *board.Task {
	t.Helper()
	task, err := b.New(board.Task{Title: "needs a human"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Block(task.ID, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}
	blocked, err := b.Set(task.ID, "question", qid)
	if err != nil {
		t.Fatal(err)
	}
	return blocked
}

func TestAnAnsweredQuestionReturnsItsTaskToReady(t *testing.T) {
	humanService(t, "answered", "yes, drop the index", "and rebuild it after")
	b, err := board.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task := blockedOnQuestion(t, b, "q_1")
	bin, _ := fakeHerdr(t, agents())
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())

	got, err := b.Find(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != board.Ready {
		t.Fatalf("an answered task is %s, want ready", got.State)
	}
	if !strings.Contains(got.Body, "yes, drop the index") || !strings.Contains(got.Body, "and rebuild it after") {
		t.Fatalf("the answer was not noted verbatim:\n%s", got.Body)
	}
	if got.Question != "q_1" {
		t.Fatalf("the question key is history and must survive; got %q", got.Question)
	}
}

// Expiry is not consent: the task must come back for re-triage, with that said in so many
// words, never silently and never as a green light.
func TestAnExpiredQuestionComesBackForRetriage(t *testing.T) {
	humanService(t, "expired", "", "")
	b, _ := board.Open(t.TempDir())
	task := blockedOnQuestion(t, b, "q_2")
	bin, _ := fakeHerdr(t, agents())
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())

	got, _ := b.Find(task.ID)
	if got.State != board.Ready {
		t.Fatalf("an expired question left its task %s, want ready", got.State)
	}
	if !strings.Contains(got.Body, "expiry is not consent") {
		t.Fatalf("expiry was not called out:\n%s", got.Body)
	}
}

// The question key is kept for history, so a task blocked again later — for an unrelated
// reason — must not have its already-delivered answer applied a second time.
func TestADeliveredAnswerIsNotDeliveredAgain(t *testing.T) {
	humanService(t, "answered", "the answer", "")
	b, _ := board.Open(t.TempDir())
	task := blockedOnQuestion(t, b, "q_3")
	bin, _ := fakeHerdr(t, agents())
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())
	if _, err := b.Block(task.ID, "now waiting on a PR instead"); err != nil {
		t.Fatal(err)
	}
	w.Tick(context.Background())

	got, _ := b.Find(task.ID)
	if got.State != board.Blocked {
		t.Fatalf("a re-blocked task was yanked back to %s by its old answer", got.State)
	}
	if strings.Count(got.Body, "the answer") != 1 {
		t.Fatalf("the answer was applied more than once:\n%s", got.Body)
	}
}

// An unreachable service must never break a tick: the blocked task stays put and is retried
// next tick, and the rest of the tick — offering ready work to a lead — still happens.
func TestAnUnreachableHumanServiceDoesNotBreakTheTick(t *testing.T) {
	srv := humanService(t, "answered", "x", "")
	srv.Close() // configured, but nobody answers the phone line itself
	b, _ := board.Open(t.TempDir())
	task := blockedOnQuestion(t, b, "q_4")
	ready, err := b.New(board.Task{Title: "ordinary ready work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(ready.ID); err != nil {
		t.Fatal(err)
	}
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "idle", "/Users/x/Developer/be"}))
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())

	got, _ := b.Find(task.ID)
	if got.State != board.Blocked {
		t.Fatalf("an uncheckable question moved its task to %s", got.State)
	}
	if !strings.Contains(calls(t, dir), "agent prompt lead") {
		t.Fatal("a dead human service stopped the rest of the tick: the ready task was never offered")
	}
}

// The launchd unit runs `cone watch`; nothing runs `cone sync`. The tick is what makes a task
// queued from a phone actually arrive.
func TestTheTickPullsConfiguredInboxes(t *testing.T) {
	served := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		served = true
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
			"id": "mp-7", "title": "from the phone", "body": "queued remotely",
		}})
	}))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(t.TempDir(), "inboxes.json")
	os.WriteFile(cfg, []byte(`[{"name":"phone","url":"`+srv.URL+`","claim_path":"/claim","token":"t"}]`), 0o644)
	t.Setenv("CONE_INBOXES", cfg)

	b, _ := board.Open(t.TempDir())
	bin, _ := fakeHerdr(t, agents())
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())

	inboxed, err := b.List(board.Inbox)
	if err != nil || len(inboxed) != 1 {
		t.Fatalf("the phoned-in task was not filed (got %d, %v)", len(inboxed), err)
	}
	if inboxed[0].SourceRef != "mp-7" {
		t.Fatalf("filed the wrong thing: %+v", inboxed[0])
	}
}
