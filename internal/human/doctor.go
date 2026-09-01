package human

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
)

// Doctor checks the human loop's health, in the same spirit as the rest of `cone doctor`:
// every failure here is one that is otherwise invisible. A broken human.json makes `cone ask`
// die and the answer sweep silently skip — which looks exactly like a human who never answers.
// And a blocked task whose question cannot be fetched is a task the sweep has been failing on
// every tick, with the failure logged only where nobody looks.
//
// Reports only. It does the same no-wait GET the sweep does, so what it reports is precisely
// what the sweep is experiencing.
func Doctor(b *board.Board) []board.Finding {
	svc, err := Configured()

	blocked, lerr := b.List(board.Blocked)
	if lerr != nil {
		blocked = nil
	}
	var waiting []*board.Task
	for _, t := range blocked {
		if t.Question != "" && !Applied(t) {
			waiting = append(waiting, t)
		}
	}

	if err != nil {
		return []board.Finding{{Severity: board.Broken, Area: "human",
			Message: fmt.Sprintf("human.json exists but cannot be used (%v) — `cone ask` fails and no answer will ever be delivered", err),
			Fix:     "fix or remove " + ConfigPath()}}
	}
	if svc == nil {
		// No service is the normal state on most hosts and says nothing — unless the board
		// carries questions that nothing can ever check.
		if len(waiting) == 0 {
			return nil
		}
		return []board.Finding{{Severity: board.Warn, Area: "human",
			Message: fmt.Sprintf("%d blocked task(s) carry a question id but no human service is configured, so no answer can ever arrive: %s",
				len(waiting), ids(waiting)),
			Fix: "declare the service in " + ConfigPath()}}
	}

	out := []board.Finding{{Severity: board.OK, Area: "human",
		Message: fmt.Sprintf("asking through %q", svc.Name)}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, t := range waiting {
		ans, err := svc.Question(ctx, t.Question, 0)
		switch {
		case err != nil:
			out = append(out, board.Finding{Severity: board.Warn, Area: "human",
				Message: fmt.Sprintf("question %s (task %s) cannot be checked: %v — the heartbeat's sweep is failing the same way, so this task cannot come back", t.Question, t.ID, err)})
		case ans.Settled():
			out = append(out, board.Finding{Severity: board.Warn, Area: "human",
				Message: fmt.Sprintf("question %s (task %s) is %s but the task is still blocked — the heartbeat sweep should have returned it to ready; is the watcher running?", t.Question, t.ID, ans.Status)})
		}
	}
	return out
}

func ids(tasks []*board.Task) string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return strings.Join(out, ", ")
}
