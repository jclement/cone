package board

import (
	"context"
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
	case "question":
		// The human service's id for the question this task waits on. `cone ask` writes it;
		// the watch sweep is what reads it, so a task asked about by hand still comes back.
		t.Question = value
	default:
		return nil, fmt.Errorf("cannot set %q (worktree|agent|branch|repo|priority|kind|question). "+
			"Other fields are not round-tripped and would be lost on the next write", key)
	}
	if err := b.Save(t); err != nil {
		return nil, err
	}
	b.touchIndex()
	return t, nil
}

// Agent is one row of `herdr agent list`, in whichever session it was found.
type Agent struct {
	Session string
	Name    string // the agent name, or its pane id when it has none
	PaneID  string // herdr's identity for this agent: one pane hosts at most one
	Status  string // idle | working | done | unknown
	CWD     string
}

// IsLead reports whether this agent is an orchestrator rather than a worker: a worker sits in
// a linked worktree, a lead sits in a main checkout.
//
// It asks git rather than matching on the path. The string test was "/worktrees/", which
// silently missed every checkout under the older layouts — `~/Developer/barreleye.wt/review`
// read as a lead, so the heartbeat would offer it tasks and a worker would be handed work
// meant for the session that delegates it. Worktrees on this machine live under at least
// three different naming schemes; git knows which are linked and no pattern does.
func (a Agent) IsLead() bool { return !isLinkedWorktree(a.CWD) }

// ReadyForInput reports whether this agent could take a prompt right now.
//
// Herdr has two words for the same readiness. `idle` is an agent waiting with its tab seen;
// `done` is that same agent when its tab has NOT been seen in the focused UI — and a CLI read
// never marks a tab seen, only focusing does. Requiring `idle` therefore made a lead invisible
// from the end of its first turn onward: it answered one poke, settled back to `done` unseen,
// and was skipped from then on. Not just at bootstrap — every turn a lead completed unseen took
// it out of the running again, permanently, and the symptom was a heartbeat holding with
// "no idle orchestrator in a matching repo" while a perfectly reachable lead sat in that repo.
//
// harvest already reads `done` the other way, where it means "this worker has finished, take
// its output". One status cannot mean finished for a worker and unreachable for a lead.
//
// `working` and `unknown` stay out: the first is mid-turn, and the second means herdr could not
// classify it, which is not the same as ready.
func (a Agent) ReadyForInput() bool { return a.Status == "idle" || a.Status == "done" }

// isLinkedWorktree is true for a checkout whose git directory is not the repository's common
// one — the definition of a linked worktree. A path that is not a repository at all is not a
// worktree, so an agent parked in ~ or /tmp still counts as a lead.
func isLinkedWorktree(dir string) bool {
	if dir == "" {
		return false
	}
	if strings.Contains(dir, "/worktrees/") {
		return true // fast path for the common layout; also correct when git is unavailable
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--absolute-git-dir", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return false
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		return false
	}
	return lines[0] != lines[1]
}

// Agents asks every running Herdr session who it has.
//
// A session that cannot be listed fails the WHOLE call rather than being skipped: its agents
// would read as absent, and "absent" is what makes a claim look abandoned. Callers must treat
// an error as "unknown", never as "none".
func Agents(ctx context.Context, herdrBin string) ([]Agent, error) {
	if herdrBin == "" {
		herdrBin = "herdr"
	}
	sessions, err := sessions(ctx, herdrBin)
	if err != nil {
		return nil, err
	}
	// First session wins each pane. sessions() lists the default session first, and that is
	// the socket serving the widest set, so the surviving row is the one most likely to be
	// reachable when the heartbeat pokes it.
	var out []Agent
	seen := map[string]bool{}
	for _, s := range sessions {
		args := []string{}
		if s != "" {
			args = append(args, "--session", s)
		}
		raw, err := exec.CommandContext(ctx, herdrBin, append(args, "agent", "list")...).Output()
		if err != nil {
			return nil, fmt.Errorf("could not list agents in session %q: %w", s, err)
		}
		var env struct {
			Result struct {
				Agents []struct {
					Name   string `json:"name"`
					PaneID string `json:"pane_id"`
					Status string `json:"agent_status"`
					CWD    string `json:"cwd"`
				} `json:"agents"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		for _, a := range env.Result.Agents {
			n := a.Name
			if n == "" {
				n = a.PaneID
			}
			// Sessions overlap: herdr's default socket serves the agents of some named
			// sessions too, so the same pane comes back from more than one `agent list` and
			// doctor reported "14 lead(s)" for seven real ones. Keyed on the pane because
			// that is the identity herdr itself uses and a pane hosts at most one agent —
			// keyed on the name, two unnamed agents in one session would collapse into one,
			// which is the direction that loses a live worker rather than merely repeating it.
			key := a.PaneID
			if key == "" {
				key = s + "\x00" + n + "\x00" + a.CWD
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Agent{Session: s, Name: n, PaneID: a.PaneID, Status: a.Status, CWD: a.CWD})
		}
	}
	return out, nil
}

func sessions(ctx context.Context, herdrBin string) ([]string, error) {
	out, err := exec.CommandContext(ctx, herdrBin, "session", "list").Output()
	if err != nil {
		// Without the session list we would see only the default session, and every agent in
		// every other session would read as dead. Liveness is unknown, not empty.
		return nil, fmt.Errorf("could not list herdr sessions: %w", err)
	}
	var names []string
	for i, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 2 || f[1] != "running" {
			continue
		}
		if f[0] == "default" {
			names = append(names, "")
		} else {
			names = append(names, f[0])
		}
	}
	if len(names) == 0 {
		names = []string{""}
	}
	return names, nil
}

// LiveAgents returns the set of agent names Herdr currently knows about, across every session.
// An empty set with no error means Herdr answered and there are none; an error means we could
// not ask, which callers must treat as "unknown", never as "none".
func LiveAgents(herdrBin string) (map[string]bool, error) {
	agents, err := Agents(context.Background(), herdrBin)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, a := range agents {
		live[a.Name] = true
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

		// A worker that finished at 2am and one that crashed at minute three look identical
		// from here — both are simply gone. The captured output is the only thing that tells
		// them apart, and releasing a finished task to ready/ offers work that is already
		// done to somebody else. Finished-looking work goes to blocked/, where a human reads
		// the snapshot and closes it; only work with nothing to show is re-offered.
		verb, to := "Released to ready", Ready
		if HasWorkerOutput(t.Body) {
			verb, to = "Held for review; its output is above", Blocked
		}
		t.Body = strings.TrimRight(t.Body, "\n") +
			fmt.Sprintf("\n\n## Worker gone %s\n\nWorker %q is no longer known to Herdr. %s.\n",
				time.Now().UTC().Format(time.RFC3339), t.Agent, verb)
		if err := b.Save(t); err != nil {
			continue
		}
		if to == Ready {
			if _, err := b.Release(t.ID); err != nil {
				continue
			}
			// Release works on its own loaded copy, so correct the state we hand back: the
			// caller logs it, and reporting the state we just moved away from is how a log
			// line ends up contradicting the board it describes. Agent and claimant are
			// deliberately LEFT on this copy even though the file no longer carries them —
			// "worker %q is gone" is the whole message, and a caller reading it off a
			// cleared struct prints an empty name.
			t.State = Ready
		} else if err := b.move(t, to); err != nil {
			// Blocked keeps the claim stamps: it is not unowned, it is waiting on a human.
			continue
		}
		b.touchIndex()
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
