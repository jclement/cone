package board

import (
	"strings"
	"testing"
)

func snapshotted(t *testing.T, text string) *Task {
	t.Helper()
	b := tmpBoard(t)
	task, err := b.New(Task{Title: "delegated work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Snapshot(task.ID, text); err != nil {
		t.Fatal(err)
	}
	got, err := b.Find(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A worker can emit megabytes. The task file is read by humans and by models with finite
// context, and an unbounded paste would make both unusable.
func TestASnapshotIsCapped(t *testing.T) {
	huge := strings.Repeat("x", 200_000) + "\nthe last line, which is the interesting one\n"
	got := snapshotted(t, huge)

	if len(got.Body) > snapshotCap+2000 {
		t.Fatalf("stored %d bytes of terminal output; the cap is %d", len(got.Body), snapshotCap)
	}
	if !strings.Contains(got.Body, "the last line, which is the interesting one") {
		t.Fatal("truncation kept the beginning — the END is the part that says what happened")
	}
	if !strings.Contains(got.Body, "truncated") {
		t.Error("truncation was silent, so the tail reads as the whole story")
	}
}

// Snapshot must not report a change when nothing changed: the heartbeat calls it every minute,
// and rewriting the file would touch the mtime the search index watches, so an idle worker
// would trigger a full reindex once a minute forever.
func TestAnUnchangedSnapshotWritesNothing(t *testing.T) {
	b := tmpBoard(t)
	task, _ := b.New(Task{Title: "delegated work"})

	changed, err := b.Snapshot(task.ID, "same output\n")
	if err != nil || !changed {
		t.Fatalf("first snapshot: changed=%v err=%v", changed, err)
	}
	changed, err = b.Snapshot(task.ID, "same output\n")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("an identical snapshot reported a change, so the file is rewritten every tick")
	}
}

// Everything the task already said must survive; a snapshot is added, not substituted.
func TestASnapshotPreservesTheTaskAroundIt(t *testing.T) {
	b := tmpBoard(t)
	task, _ := b.New(Task{Title: "delegated work", Body: "the brief\n\n## Done when\n\nthe suite is green\n"})
	if _, err := b.Snapshot(task.ID, "build output\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Snapshot(task.ID, "later output\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Find(task.ID)

	for _, want := range []string{"the brief", "## Done when", "the suite is green", "later output"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("%q was lost:\n%s", want, got.Body)
		}
	}
	if strings.Contains(got.Body, "build output") {
		t.Error("the superseded snapshot is still there")
	}
}

// An empty read is not a snapshot — a worker that printed nothing must not get an empty
// section that makes it look finished to the reaper.
func TestAnEmptySnapshotIsNotStored(t *testing.T) {
	b := tmpBoard(t)
	task, _ := b.New(Task{Title: "delegated work"})
	changed, err := b.Snapshot(task.ID, "  \n\t\n")
	if err != nil || changed {
		t.Fatalf("stored an empty snapshot (changed=%v err=%v)", changed, err)
	}
	got, _ := b.Find(task.ID)
	if HasWorkerOutput(got.Body) {
		t.Fatal("a task with no output looks harvested, so the reaper will hold it for review")
	}
}
