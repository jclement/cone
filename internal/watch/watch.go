// Package watch is the heartbeat.
//
// The problem it solves: an agent session is turn-based. It cannot watch a folder, and an
// agent that polls costs a turn every tick to usually learn nothing. So the watching happens
// here — in a process that is free to be idle — and a model is woken only when all of these
// hold:
//
//  1. tasks/ready is non-empty
//  2. an orchestrator is idle, and the ready work is work *it* could take
//  3. we are under the concurrent-worker cap, counting only live claims
//  4. we have not already poked that orchestrator about this exact set of tasks
//
// Cost while idle: zero tokens. Cost when work appears: one turn.
//
// Polling is used rather than fsnotify deliberately. The interval is short enough that
// latency is irrelevant against a human-scale queue, a stat of one directory is free, and it
// works identically over a network filesystem — where fsnotify silently does not. The
// expensive check (asking Herdr for agent states) only runs once the free one says there is
// something to do.
//
// A note on failure. The first installed version of this never fired: launchd hands a user
// agent PATH=/usr/bin:/bin:/usr/sbin:/sbin, `herdr` lives in /opt/homebrew/bin, every exec
// failed with ENOENT — and all three places that could have said so logged only under
// --verbose, which the installed unit did not pass. `.watch.log` was zero bytes and the job
// showed as loaded. A heartbeat that fails silently is indistinguishable from a heartbeat
// with nothing to do. So: anything that *stops* a poke that would otherwise have happened is
// reported unconditionally, and repeated only when it changes or every 15 minutes, so the log
// stays readable without going quiet.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
)

// renagInterval is how long a stable complaint stays quiet before it is repeated. Long enough
// that a machine idle overnight does not produce a wall of text; short enough that someone
// looking at the log after lunch sees the problem is current, not historical.
const renagInterval = 15 * time.Minute

type Options struct {
	Interval   time.Duration
	MaxWorkers int
	HerdrBin   string
	Prompt     string // what the woken orchestrator is asked to run
	Verbose    bool
	DryRun     bool
	NoReap     bool // leave claims held by dead agents alone
}

func (o *Options) defaults() {
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.MaxWorkers <= 0 {
		o.MaxWorkers = 4
	}
	if o.Prompt == "" {
		o.Prompt = "/tasks"
	}
	// A bare name is resolved once, here, so the failure is reported at startup with a fix in
	// it rather than as a bare ENOENT on every tick for the life of the process.
	if o.HerdrBin == "" {
		o.HerdrBin = "herdr"
	}
	if !strings.ContainsRune(o.HerdrBin, filepath.Separator) {
		if p, err := exec.LookPath(o.HerdrBin); err == nil {
			o.HerdrBin = p
		}
	}
}

type Watcher struct {
	b   *board.Board
	opt Options
	log func(string, ...any)

	// poked remembers, per orchestrator, the signature of the task set it was last woken
	// about. Keyed by agent so two orchestrators are not treated as one, and persisted so a
	// KeepAlive restart does not re-poke about a set someone is already triaging.
	poked map[string]string

	complaint string // the last thing that stopped a poke
	since     time.Time

	// tries counts unconfirmed pokes per agent+signature. Confirmation is not free of
	// ambiguity — an agent that takes the turn and finishes it inside the same second reads
	// as "still idle", i.e. as a prompt that never landed — so retrying forever would nag a
	// perfectly healthy lead every interval. Bounded retries, then take the win.
	tries map[string]int
}

const maxPokeAttempts = 3

func New(b *board.Board, opt Options) *Watcher {
	opt.defaults()
	w := &Watcher{b: b, opt: opt, poked: map[string]string{}, tries: map[string]int{}, log: func(string, ...any) {}}
	if opt.Verbose {
		w.log = w.say
	}
	w.loadState()
	if _, err := exec.LookPath(w.opt.HerdrBin); err != nil {
		w.say("herdr not found at %q (%v) — nothing can be woken. Pass --herdr /full/path, or re-run `cone install` to bake it into the unit.", w.opt.HerdrBin, err)
	}
	return w
}

func (w *Watcher) say(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(f, a...))
}

// hold reports a reason a poke did not happen. Unconditional, but de-duplicated: the same
// reason is printed once and then at most every renagInterval.
func (w *Watcher) hold(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	if msg == w.complaint && time.Since(w.since) < renagInterval {
		return
	}
	w.complaint, w.since = msg, time.Now()
	w.say("holding: %s", msg)
}

// clear cancels a standing complaint, so the next occurrence of the same problem is reported
// immediately rather than swallowed by the renag window.
func (w *Watcher) clear() { w.complaint, w.since = "", time.Time{} }

func (w *Watcher) Run(ctx context.Context) error {
	w.say("watching %s every %s (cap %d workers, herdr %s)", w.b.Root, w.opt.Interval, w.opt.MaxWorkers, w.opt.HerdrBin)
	t := time.NewTicker(w.opt.Interval)
	defer t.Stop()
	w.Tick(ctx) // act immediately rather than after one interval
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one cycle. Exported so `cone watch --once` and tests can drive it directly.
func (w *Watcher) Tick(ctx context.Context) {
	ready, err := w.b.List(board.Ready)
	if err != nil {
		w.hold("cannot read %s: %v", w.b.Root, err)
		return
	}
	if len(ready) == 0 {
		w.clear()
		return // the free check said no. Nothing else runs.
	}

	// A claim held by an agent Herdr no longer knows about is not work in progress, it is a
	// slot lost forever: the cap counts files in doing/, so four dead claims wedge the
	// heartbeat permanently with no trace.
	if !w.opt.NoReap && !w.opt.DryRun {
		if reaped, err := w.b.Reap(w.opt.HerdrBin, false); err == nil && len(reaped) > 0 {
			for _, t := range reaped {
				w.say("released %s: claimant %q is gone", t.ID, t.ClaimedBy)
			}
			ready, _ = w.b.List(board.Ready)
		}
	}

	inFlight := w.b.ActiveClaims(w.opt.HerdrBin)
	if inFlight >= w.opt.MaxWorkers {
		w.hold("%d claim(s) in flight, cap %d", inFlight, w.opt.MaxWorkers)
		return
	}

	cands, err := w.idleOrchestrators(ctx)
	if err != nil {
		w.hold("cannot ask herdr (%s) who is idle: %v", w.opt.HerdrBin, err)
		return
	}
	if len(cands) == 0 {
		w.hold("%d task(s) ready, no idle orchestrator to take them", len(ready))
		return
	}

	// Match work to orchestrator by repo. A lead sitting in ~/Developer/be cannot act on a
	// task filed against another repo — poking it produces a confused turn and burns the
	// signature, so the real owner never hears about it.
	for _, c := range cands {
		mine := forRepo(ready, c.cwd)
		if len(mine) == 0 {
			continue
		}
		sig := signature(mine)
		if w.poked[c.name] == sig {
			continue // same set as last poke; nagging is how a heartbeat gets muted
		}

		msg := fmt.Sprintf(`%s

(cone: %d task(s) ready, %d in flight, cap %d. Triage per %s/AGENTS.md.
Claim only what you will actually start now — a claimed task nobody is working
is worse than an unclaimed one.)`, w.opt.Prompt, len(mine), inFlight, w.opt.MaxWorkers, w.b.Root)

		if w.opt.DryRun {
			fmt.Printf("would poke %s (session %q, cwd %s) about %d ready task(s)\n", c.name, c.session, c.cwd, len(mine))
			return
		}
		if err := w.poke(ctx, c.session, c.name, msg); err != nil {
			attempt := c.name + "\x00" + sig
			w.tries[attempt]++
			if w.tries[attempt] < maxPokeAttempts {
				w.hold("prompt to %s did not land: %v", c.name, err)
				return
			}
			// Three unconfirmed attempts: either it is landing and finishing faster than we
			// can observe, or it never will. Both are better served by moving on than by
			// prompting the same lead about the same tasks every interval forever.
			w.say("prompt to %s could not be confirmed after %d attempts (%v) — recording it anyway",
				c.name, maxPokeAttempts, err)
		}
		// Only now: a signature recorded for a prompt that never arrived means this exact
		// set is never mentioned again, which is the worst possible failure for a queue.
		w.poked[c.name] = sig
		delete(w.tries, c.name+"\x00"+sig)
		w.saveState()
		w.clear()
		w.say("poked %s (session %q) about %d ready task(s)", c.name, c.session, len(mine))
		return
	}
	w.hold("%d task(s) ready, but no idle orchestrator is in a matching repo", len(ready))
}

// forRepo returns the tasks a lead in cwd could actually pick up: those with no repo set (any
// lead may take them) and those whose repo names this checkout.
func forRepo(tasks []*board.Task, cwd string) []*board.Task {
	var out []*board.Task
	for _, t := range tasks {
		if t.Repo == "" || matchesRepo(t.Repo, cwd) {
			out = append(out, t)
		}
	}
	return out
}

// matchesRepo is deliberately loose. A task's repo: is written by a human or another agent and
// may be a bare name ("be"), a path, or a path with ~ in it; cwd is absolute. Comparing path
// elements handles all three without demanding that anyone normalise anything.
func matchesRepo(repo, cwd string) bool {
	repo = strings.TrimSuffix(strings.TrimSpace(repo), "/")
	if repo == "" {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(repo, "~/") {
		repo = filepath.Join(home, repo[2:])
	}
	if filepath.IsAbs(repo) {
		return cwd == repo || strings.HasPrefix(cwd, repo+string(filepath.Separator))
	}
	for _, part := range strings.Split(filepath.ToSlash(cwd), "/") {
		if strings.EqualFold(part, filepath.Base(repo)) {
			return true
		}
	}
	return false
}

func signature(tasks []*board.Task) string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:8])
}

type candidate struct{ session, name, cwd string }

// idleOrchestrators finds agents sitting in a MAIN checkout. Workers live under a worktrees
// directory; anything else running an agent is a lead. Sessions are enumerated because a
// machine may still have more than one.
//
// Only "idle" counts. "done" was accepted here once and should not be: an agent that has
// finished and exited still lists, and prompting it is a message into a dead pane.
func (w *Watcher) idleOrchestrators(ctx context.Context) ([]candidate, error) {
	var out []candidate
	var asked bool
	var lastErr error
	for _, s := range w.sessions(ctx) {
		raw, err := w.herdr(ctx, s, "agent", "list")
		if err != nil {
			lastErr = err
			continue
		}
		asked = true
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
			if strings.Contains(a.CWD, "/worktrees/") || a.Status != "idle" {
				continue
			}
			n := a.Name
			if n == "" {
				n = a.PaneID
			}
			out = append(out, candidate{session: s, name: n, cwd: a.CWD})
		}
	}
	if !asked {
		if lastErr == nil {
			lastErr = fmt.Errorf("no session answered")
		}
		return nil, lastErr
	}
	return out, nil
}

func (w *Watcher) sessions(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, w.opt.HerdrBin, "session", "list").Output()
	if err != nil {
		return []string{""}
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
		return []string{""}
	}
	return names
}

func (w *Watcher) herdr(ctx context.Context, session string, args ...string) ([]byte, error) {
	a := []string{}
	if session != "" {
		a = append(a, "--session", session)
	}
	return exec.CommandContext(ctx, w.opt.HerdrBin, append(a, args...)...).Output()
}

// poke sends the prompt and then confirms the agent actually started working. --wait --until
// working is asked for, but a zero exit is not on its own proof the model took the turn, and
// the signature we are about to record is the only reason this set is ever mentioned again.
func (w *Watcher) poke(ctx context.Context, session, agent, msg string) error {
	args := []string{"agent", "prompt", agent, msg, "--wait", "--until", "working", "--timeout", "15000"}
	if _, err := w.herdr(ctx, session, args...); err != nil {
		return err
	}
	raw, err := w.herdr(ctx, session, "agent", "list")
	if err != nil {
		return nil // it went out and we cannot check; do not re-send into a working agent
	}
	var env struct {
		Result struct {
			Agents []struct {
				Name   string `json:"name"`
				PaneID string `json:"pane_id"`
				Status string `json:"agent_status"`
			} `json:"agents"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	for _, a := range env.Result.Agents {
		if a.Name != agent && a.PaneID != agent {
			continue
		}
		if a.Status == "idle" {
			return fmt.Errorf("still idle after the prompt — it did not take the turn")
		}
		return nil
	}
	return fmt.Errorf("%s is no longer listed", agent)
}

// The poke memory is persisted because launchd KeepAlive restarts this process — on upgrade,
// on crash, on logout — and an in-memory signature means every restart re-pokes about a queue
// someone is already triaging.
func (w *Watcher) statePath() string { return filepath.Join(w.b.Root, ".watch.state") }

func (w *Watcher) loadState() {
	data, err := os.ReadFile(w.statePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &w.poked)
	if w.poked == nil {
		w.poked = map[string]string{}
	}
}

func (w *Watcher) saveState() {
	data, err := json.Marshal(w.poked)
	if err != nil {
		return
	}
	_ = os.WriteFile(w.statePath(), data, 0o644)
}
