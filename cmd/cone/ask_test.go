package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// These drive cmdAsk end to end: a real board in a temp dir (CONE_HOME) against a real HTTP
// server (CONE_HUMAN), because the durability chain — question posted, id recorded, task
// blocked — is exactly the part unit tests on the pieces would not cover.

// askFixture is a board holding one ready task, and a human service whose question GET
// answers with the given status.
func askFixture(t *testing.T, status, value string) (b *board.Board, taskID string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CONE_HOME", home)
	b, err := board.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := b.New(board.Task{Title: "pick a migration strategy", Repo: "be"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var q map[string]any
			json.NewDecoder(r.Body).Decode(&q)
			// The context must carry the way back to the board.
			ctx, _ := q["context"].(map[string]any)
			if ctx["task"] != task.ID || ctx["board"] != b.Root {
				t.Errorf("context does not point back at the board: %v", ctx)
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
				"id": "q_77", "url": "http://" + r.Host + "/q/q_77",
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": status, "answer_value": value})
	}))
	t.Cleanup(srv.Close)

	cfg := filepath.Join(t.TempDir(), "human.json")
	os.WriteFile(cfg, []byte(`{"name":"svc","url":"`+srv.URL+`","token":"t"}`), 0o644)
	t.Setenv("CONE_HUMAN", cfg)
	return b, task.ID
}

func TestAskRecordsTheQuestionAndBlocksTheTask(t *testing.T) {
	b, id := askFixture(t, "open", "")

	err := cmdAsk([]string{id, "-title", "which strategy?", "-body", "the two diffs"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != board.Blocked {
		t.Fatalf("state = %s, want blocked", got.State)
	}
	if got.Question != "q_77" {
		t.Fatalf("question = %q — the sweep has nothing to poll", got.Question)
	}
	if !strings.Contains(got.Body, "/q/q_77") {
		t.Fatalf("the question URL was not noted:\n%s", got.Body)
	}
}

// Durability first: with no service configured the command explains what to create and
// touches nothing — it must not invent a URL and must not move the task.
func TestAskWithoutAServiceExplainsAndTouchesNothing(t *testing.T) {
	b, id := askFixture(t, "open", "")
	t.Setenv("CONE_HUMAN", filepath.Join(t.TempDir(), "absent.json"))

	err := cmdAsk([]string{id, "-title", "x", "-body", "y"})
	if err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Fatalf("want a one-line explanation naming the config path, got %v", err)
	}
	got, _ := b.Find(id)
	if got.State != board.Ready || got.Question != "" {
		t.Fatalf("a failed ask changed the task: state=%s question=%q", got.State, got.Question)
	}
}

// --wait must land in the same end state the sweep produces: answer noted, task ready.
func TestAskWaitDeliversTheAnswer(t *testing.T) {
	b, id := askFixture(t, "answered", "go with plan b")

	if err := cmdAsk([]string{id, "-title", "which?", "-body", "diffs", "-wait"}); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Find(id)
	if got.State != board.Ready {
		t.Fatalf("state = %s, want ready", got.State)
	}
	if !strings.Contains(got.Body, "go with plan b") {
		t.Fatalf("the answer is not on the task:\n%s", got.Body)
	}
}

// Expiry is not consent: --wait must exit non-zero, say so on the task, and leave it blocked
// for the asker to report rather than quietly requeueing it.
func TestAskWaitExpiryIsLoudAndLeavesTheTaskBlocked(t *testing.T) {
	b, id := askFixture(t, "expired", "")

	err := cmdAsk([]string{id, "-title", "which?", "-body", "diffs", "-wait"})
	if err == nil || !strings.Contains(err.Error(), "expiry is not consent") {
		t.Fatalf("an expired question was not loud: %v", err)
	}
	got, _ := b.Find(id)
	if got.State != board.Blocked {
		t.Fatalf("state = %s, want blocked", got.State)
	}
	if !strings.Contains(got.Body, "expired unanswered") {
		t.Fatalf("expiry was not noted on the task:\n%s", got.Body)
	}
}

// The contract's decision kinds need a recommendation, and the refusal must happen before
// anything is posted or the task is touched.
func TestAskRejectsAChoiceWithoutARecommendationLocally(t *testing.T) {
	b, id := askFixture(t, "open", "")

	err := cmdAsk([]string{id, "-title", "which?", "-body", "diffs", "-kind", "choice",
		"-option", "a:plan a", "-option", "b:plan b"})
	if err == nil || !strings.Contains(err.Error(), "--recommend") {
		t.Fatalf("a choice with no recommendation went through: %v", err)
	}
	got, _ := b.Find(id)
	if got.State != board.Ready || got.Question != "" {
		t.Fatalf("a rejected ask still changed the task: state=%s question=%q", got.State, got.Question)
	}
}
