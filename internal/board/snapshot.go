package board

import (
	"fmt"
	"strings"
	"time"
)

// A worker's terminal output, captured onto its task.
//
// The problem: an agent finishes at 02:10 and nobody reads it. By morning the pane is gone —
// a herdr restart, a `/land --all-done`, a laptop that slept — and with it the only account of
// what happened. The task then looks abandoned, gets released, and the work is offered to
// somebody else as if it were new. The board's whole promise is that a result outlives the
// session that produced it, and the one moment that matters is the one where nobody is awake.
//
// So the heartbeat snapshots the worker's recent output onto the task while the pane still
// exists. Three properties make that safe rather than merely large:
//
//   - It REPLACES, never appends. Herdr flips an agent to "done" at the end of every turn, not
//     once at the end of the work, so appending would grow the file all night. One section
//     holds the latest snapshot.
//   - It is capped, and marked unverified. Two hundred lines of terminal tail is raw material,
//     not a finding; a stack trace reads like a conclusion and is not one.
//   - It is EXCLUDED FROM THE SEARCH INDEX. `cone search` answers "did anyone look at this and
//     what did they find?", and build output competing for rank in those results would degrade
//     the one feature the board exists for.
//
// The written result is still the deliverable. This is the safety net under it.

const (
	snapshotOpen  = "<!-- cone:worker-output -->"
	snapshotClose = "<!-- /cone:worker-output -->"
	snapshotCap   = 6000
)

// Snapshot stores text as the task's worker-output section, replacing any previous one.
// It reports whether anything changed, so a caller polling every minute does not rewrite an
// unchanged file — and does not touch its mtime, which is what the search index watches.
func (b *Board) Snapshot(id, text string) (bool, error) {
	t, err := b.Find(id)
	if err != nil {
		return false, err
	}
	text = strings.TrimRight(text, " \t\n")
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	if len(text) > snapshotCap {
		// Keep the END: the last thing a worker said is the interesting part, and the first
		// thing it said is the brief we already have.
		text = "… (truncated)\n" + text[len(text)-snapshotCap:]
	}

	if prev, ok := workerOutput(t.Body); ok && prev == text {
		return false, nil
	}

	section := fmt.Sprintf("%s\n## Worker output — unverified, captured %s\n\n```\n%s\n```\n%s",
		snapshotOpen, time.Now().UTC().Format(time.RFC3339), text, snapshotClose)

	if start, end, ok := snapshotBounds(t.Body); ok {
		t.Body = t.Body[:start] + section + t.Body[end:]
	} else {
		t.Body = strings.TrimRight(t.Body, "\n") + "\n\n" + section + "\n"
	}
	if err := b.Save(t); err != nil {
		return false, err
	}
	b.touchIndex()
	return true, nil
}

// HasWorkerOutput reports whether a task carries a captured snapshot. Reap uses it to tell a
// worker that finished from one that crashed: both look identical from herdr, and only one of
// them should have its work offered to somebody else.
func HasWorkerOutput(body string) bool {
	_, ok := workerOutput(body)
	return ok
}

func workerOutput(body string) (string, bool) {
	start, end, ok := snapshotBounds(body)
	if !ok {
		return "", false
	}
	inner := body[start+len(snapshotOpen) : end-len(snapshotClose)]
	if i := strings.Index(inner, "```"); i >= 0 {
		inner = inner[i+3:]
		if j := strings.LastIndex(inner, "```"); j >= 0 {
			return strings.Trim(inner[:j], "\n"), true
		}
	}
	return strings.TrimSpace(inner), true
}

func snapshotBounds(body string) (start, end int, ok bool) {
	start = strings.Index(body, snapshotOpen)
	if start < 0 {
		return 0, 0, false
	}
	e := strings.Index(body[start:], snapshotClose)
	if e < 0 {
		return 0, 0, false
	}
	return start, start + e + len(snapshotClose), true
}

// withoutWorkerOutput is what gets indexed. Terminal tail is evidence, not a finding, and it
// must not compete for rank with what an agent actually concluded.
func withoutWorkerOutput(body string) string {
	start, end, ok := snapshotBounds(body)
	if !ok {
		return body
	}
	return strings.TrimSpace(body[:start]) + "\n" + strings.TrimLeft(body[end:], "\n")
}
