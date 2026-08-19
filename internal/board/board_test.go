package board

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// The claim race is the one property everything else depends on. If two agents can hold the
// same task, the board is worse than no board — so this test asserts it under real
// concurrency rather than trusting that rename is atomic.
func TestClaimIsExclusiveUnderConcurrency(t *testing.T) {
	b := tmpBoard(t)
	task, err := b.New(Task{Title: "contended work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}

	const racers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins, losses int

	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_, err := b.Claim(task.ID, "racer")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrLostRace):
				losses++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one agent must win the claim, got %d", wins)
	}
	if losses != racers-1 {
		t.Fatalf("every other agent must lose cleanly, got %d of %d", losses, racers-1)
	}
	if list, _ := b.List(Doing); len(list) != 1 {
		t.Fatalf("doing/ must hold exactly one copy, got %d", len(list))
	}
	if list, _ := b.List(Ready); len(list) != 0 {
		t.Fatalf("ready/ must be empty after the claim, got %d", len(list))
	}
}

// An id must be unique across every state, or Find returns whichever directory it happens to
// look in first and the board disagrees with itself about what an id means.
func TestNewRejectsIDAlreadyInAnotherState(t *testing.T) {
	b := tmpBoard(t)
	first, err := b.New(Task{Title: "same title"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.New(Task{Title: "same title"}); err == nil {
		t.Fatal("expected a collision error for an id already in ready/")
	}
}

func TestRoundTripPreservesBodyAndAwkwardTitle(t *testing.T) {
	b := tmpBoard(t)
	const title = `fix: the "quoted" thing: with colons`
	const body = "line one\n\n## Done when\n\n- it works\n"
	created, err := b.New(Task{Title: title, Body: body, Repo: "be", Priority: "high"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Find(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != title {
		t.Errorf("title mangled:\n want %q\n  got %q", title, got.Title)
	}
	if !strings.Contains(got.Body, "## Done when") {
		t.Errorf("body lost its structure: %q", got.Body)
	}
	if got.Priority != "high" || got.Repo != "be" {
		t.Errorf("frontmatter lost: repo=%q priority=%q", got.Repo, got.Priority)
	}
}

func TestLifecycleTransitions(t *testing.T) {
	b := tmpBoard(t)
	task, _ := b.New(Task{Title: "walk the states"})

	if _, err := b.Claim(task.ID, "someone"); err == nil {
		t.Fatal("an untriaged inbox task must not be claimable")
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(task.ID, "someone"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Block(task.ID, "waiting on a decision"); err != nil {
		t.Fatal(err)
	}
	released, err := b.Release(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.ClaimedBy != "" {
		t.Errorf("release must clear the claim, still %q", released.ClaimedBy)
	}
	if _, err := b.Claim(task.ID, "another"); err != nil {
		t.Fatalf("a released task must be claimable again: %v", err)
	}
	if _, err := b.Complete(task.ID); err != nil {
		t.Fatal(err)
	}
	done, _ := b.List(Done)
	if len(done) != 1 {
		t.Fatalf("expected the task in done/, got %d", len(done))
	}
}

func TestSearchFindsTasksAndMessages(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "vitest isolation list is wrong"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Post("be-2099", "solver", "slowness is in the LP build, not the solve"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := b.Search("isolation", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("expected a task hit: %v (%d hits)", err, len(hits))
	}
	hits, err = b.Search("slowness", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("expected a message hit: %v (%d hits)", err, len(hits))
	}
	if hits[0].Kind != "message" {
		t.Errorf("expected a message, got %q", hits[0].Kind)
	}
}

func tmpBoard(t *testing.T) *Board {
	t.Helper()
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}
