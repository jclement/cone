package board

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Outcomes, linkage, and reaping.
//
// The board originally recorded only what was *asked*. `Complete` stamped a timestamp and
// renamed the file, so `done/` accumulated requests and nothing else — which quietly gutted
// the feature the whole thing exists for. `cone search` is supposed to answer "has anyone
// looked at this before, and what did they find?", and the answer was never written down.
//
// So: completing an investigation requires a result, tasks carry a pointer to the worker that
// is doing them, and a claim held by an agent that no longer exists gets released instead of
// silently occupying a slot forever.

// Note appends a timestamped block to a task's body without changing its state. A worker that
// learns something at hour one should not have to survive to hour six for it to be findable.
func (b *Board) Note(id, heading, text string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("a note needs text")
	}
	t.Body = strings.TrimRight(t.Body, "\n") +
		fmt.Sprintf("\n\n## %s %s\n\n%s\n", heading, time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(text))
	if err := b.Save(t); err != nil {
		return nil, err
	}
	b.touchIndex()
	return t, nil
}

// CompleteWith finishes a task, recording what was found.
//
// An investigation with no finding did not finish, so a result is required for kind
// "investigate" — that is the whole point of the board over a to-do list. Other kinds may
// complete bare, because "the code is written and the gate is green" is its own result and
// lives in the branch.
func (b *Board) CompleteWith(id, result string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	if t.State != Doing {
		return nil, fmt.Errorf("only a claimed task can be completed; %s is %s", id, t.State)
	}
	if strings.TrimSpace(result) == "" && t.Kind == "investigate" {
		return nil, fmt.Errorf("%s is an investigation — completing it needs a result "+
			"(--result, or --result-file -). A finding nobody wrote down is the failure this board exists to prevent", id)
	}
	if strings.TrimSpace(result) != "" {
		t.Body = strings.TrimRight(t.Body, "\n") +
			fmt.Sprintf("\n\n## Result %s\n\n%s\n", time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(result))
	}
	t.Completed = time.Now().UTC()
	if err := b.Save(t); err != nil {
		return nil, err
	}
	err = b.move(t, Done)
	b.touchIndex()
	return t, err
}

// Set writes one of the linkage fields an agent legitimately needs to update.
//
// Deliberately not a general frontmatter editor: unknown keys are dropped on the next Save,
// so hand-editing arbitrary fields loses them silently. These four close the triangle —
// task → worker → worktree — so any vertex finds the others, which is what makes an
// abandoned checkout distinguishable from debris.
func (b *Board) Set(id, key, value string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	switch key {
	case "worktree":
		t.Worktree = value
	case "agent":
		t.Agent = value
	case "branch":
		t.Branch = value
	case "repo":
		t.Repo = value
	case "priority":
		t.Priority = value
	case "kind":
		// Filed over MCP, kind was always the default "investigate" — and CompleteWith
		// refuses to close one without a result. Without this the only ways out were to
		// invent a result (poisoning the one thing done/ is for) or abandon the task.
		t.Kind = value
	default:
		return nil, fmt.Errorf("cannot set %q (worktree|agent|branch|repo|priority|kind). "+
			"Other fields are not round-tripped and would be lost on the next write", key)
	}
	if err := b.Save(t); err != nil {
		return nil, err
	}
	b.touchIndex()
	return t, nil
}

// LiveAgents returns the set of agent names Herdr currently knows about, across every session.
// An empty set with no error means Herdr answered and there are none; an error means we could
// not ask, which callers must treat as "unknown", never as "none".
func LiveAgents(herdrBin string) (map[string]bool, error) {
	if herdrBin == "" {
		herdrBin = "herdr"
	}
	out, err := exec.Command(herdrBin, "session", "list").Output()
	if err != nil {
		// Without the session list we would see only the default session, and every agent in
		// every other session would read as dead. Liveness is unknown, not empty.
		return nil, fmt.Errorf("could not list herdr sessions: %w", err)
	}
	var sessions []string
	for i, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 2 || f[1] != "running" {
			continue
		}
		if f[0] == "default" {
			sessions = append(sessions, "")
		} else {
			sessions = append(sessions, f[0])
		}
	}
	if len(sessions) == 0 {
		sessions = []string{""}
	}

	live := map[string]bool{}
	for _, s := range sessions {
		args := []string{}
		if s != "" {
			args = append(args, "--session", s)
		}
		out, err := exec.Command(herdrBin, append(args, "agent", "list")...).Output()
		if err != nil {
			// One session we cannot read is one whose agents would all look dead. Refuse the
			// whole answer rather than return a set that is quietly missing a session.
			return nil, fmt.Errorf("could not list agents in session %q: %w", s, err)
		}
		for _, name := range agentNames(out) {
			live[name] = true
		}
	}
	return live, nil
}

// Reap releases claims whose worker Herdr no longer knows about.
//
// A dead claim must release itself: the worker cap counts tasks in doing/, so a few crashed
// workers stop the heartbeat permanently, and the only trace was a log line suppressed unless
// --verbose — which the installed scheduler did not pass.
//
// It keys on `agent`, NOT on `claimed_by`, and that distinction is the whole safety of this
// function. `claimed_by` is a display label: it can be a hostname (Whoami falls back to one),
// a name a caller invented, or an operator at a shell. None of those will ever appear in
// `herdr agent list`, so keying on it meant "I cannot see you" was read as "you are dead" —
// and the reaper revoked live claims and offered the work to a second agent, which is exactly
// the duplicated work this board exists to prevent.
//
// `agent` is a herdr identity by construction: Claim stamps it from $HERDR_AGENT, and the
// documented delegation flow sets it to the worker's name. **A task with no `agent` is never
// reaped** — nobody has said which agent is doing it, so nothing can say it stopped. Those
// surface through `cone stale` and `cone doctor`, where a human decides.
//
// A task whose worker is still live is left entirely alone regardless of age: slow is not the
// same as abandoned, and that distinction is not the reaper's to make.
func (b *Board) Reap(herdrBin string, dryRun bool) ([]*Task, error) {
	live, err := LiveAgents(herdrBin)
	if err != nil {
		// Cannot ask: do nothing. Releasing every claim because Herdr is down would be
		// far worse than leaving them held.
		return nil, err
	}
	doing, err := b.List(Doing)
	if err != nil {
		return nil, err
	}
	var reaped []*Task
	for _, t := range doing {
		if t.Agent == "" || live[t.Agent] {
			continue
		}
		reaped = append(reaped, t)
		if dryRun {
			continue
		}
		t.Body = strings.TrimRight(t.Body, "\n") +
			fmt.Sprintf("\n\n## Abandoned %s\n\nWorker %q is no longer known to Herdr. Released to ready.\n",
				time.Now().UTC().Format(time.RFC3339), t.Agent)
		if err := b.Save(t); err != nil {
			continue
		}
		if _, err := b.Release(t.ID); err != nil {
			continue
		}
	}
	return reaped, nil
}

// ActiveClaims counts tasks in doing/ whose claimant is still live — the number the worker cap
// should actually be measured against. Falls back to counting files when Herdr cannot be
// reached, which is the conservative direction.
func (b *Board) ActiveClaims(herdrBin string) int {
	doing, err := b.List(Doing)
	if err != nil {
		return 0
	}
	live, err := LiveAgents(herdrBin)
	if err != nil {
		return len(doing)
	}
	n := 0
	for _, t := range doing {
		if t.Agent == "" || live[t.Agent] {
			n++
		}
	}
	return n
}

// agentNames pulls agent identifiers out of `herdr agent list` JSON. It reads both `name` and
// `pane_id` because an agent started by hand has no name, and a claim recorded against a pane
// id must still count as live.
func agentNames(raw []byte) []string {
	var env struct {
		Result struct {
			Agents []struct {
				Name   string `json:"name"`
				PaneID string `json:"pane_id"`
			} `json:"agents"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	var out []string
	for _, a := range env.Result.Agents {
		if a.Name != "" {
			out = append(out, a.Name)
		}
		if a.PaneID != "" {
			out = append(out, a.PaneID)
		}
	}
	return out
}
