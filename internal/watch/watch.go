// Package watch is the heartbeat.
//
// The problem it solves: an agent session is turn-based. It cannot watch a folder, and an
// agent that polls costs a turn every tick to usually learn nothing. So the watching happens
// here — in a process that is free to be idle — and a model is woken only when all of these
// hold:
//
//  1. tasks/ready is non-empty
//  2. an orchestrator is idle (not working, not blocked)
//  3. we are under the concurrent-worker cap
//  4. we have not already poked about this exact set of tasks
//
// Cost while idle: zero tokens. Cost when work appears: one turn.
//
// Polling is used rather than fsnotify deliberately. The interval is short enough that
// latency is irrelevant against a human-scale queue, a stat of one directory is free, and it
// works identically over a network filesystem — where fsnotify silently does not. The
// expensive check (asking Herdr for agent states) only runs once the free one says there is
// something to do.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
)

type Options struct {
	Interval   time.Duration
	MaxWorkers int
	HerdrBin   string
	Prompt     string // what the woken orchestrator is asked to run
	Verbose    bool
	DryRun     bool
}

func (o *Options) defaults() {
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.MaxWorkers <= 0 {
		o.MaxWorkers = 4
	}
	if o.HerdrBin == "" {
		o.HerdrBin = "herdr"
	}
	if o.Prompt == "" {
		o.Prompt = "/tasks"
	}
}

type Watcher struct {
	b    *board.Board
	opt  Options
	last string // signature of the ready set we last poked about
	log  func(string, ...any)
}

func New(b *board.Board, opt Options) *Watcher {
	opt.defaults()
	w := &Watcher{b: b, opt: opt, log: func(string, ...any) {}}
	if opt.Verbose {
		w.log = func(f string, a ...any) {
			fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(f, a...))
		}
	}
	return w
}

func (w *Watcher) Run(ctx context.Context) error {
	w.log("watching %s every %s (cap %d workers)", w.b.Root, w.opt.Interval, w.opt.MaxWorkers)
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
	if err != nil || len(ready) == 0 {
		return // the free check said no. Nothing else runs.
	}

	sig := signature(ready)
	if sig == w.last {
		return // same set as last poke; nagging is how a heartbeat gets muted
	}

	doing, _ := w.b.List(board.Doing)
	if len(doing) >= w.opt.MaxWorkers {
		w.log("holding: %d in flight, cap %d", len(doing), w.opt.MaxWorkers)
		return
	}

	sess, agent, err := w.idleOrchestrator(ctx)
	if err != nil || agent == "" {
		w.log("%d ready but no idle orchestrator", len(ready))
		return
	}

	msg := fmt.Sprintf(`%s

(cone: %d task(s) ready, %d in flight, cap %d. Triage per ~/cone/AGENTS.md.
Claim only what you will actually start now — a claimed task nobody is working
is worse than an unclaimed one.)`, w.opt.Prompt, len(ready), len(doing), w.opt.MaxWorkers)

	if w.opt.DryRun {
		fmt.Printf("would poke %s (session %s) about %d ready task(s)\n", agent, sess, len(ready))
		return
	}
	if err := w.poke(ctx, sess, agent, msg); err != nil {
		w.log("prompt to %s did not land: %v", agent, err)
		return
	}
	w.last = sig
	w.log("poked %s (session %s) about %d ready task(s)", agent, sess, len(ready))
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

// idleOrchestrator finds an agent sitting in a MAIN checkout. Workers live under a worktrees
// directory; anything else running an agent is a lead. Sessions are enumerated because a
// machine may still have more than one.
func (w *Watcher) idleOrchestrator(ctx context.Context) (session, name string, err error) {
	for _, s := range w.sessions(ctx) {
		out, err := w.herdr(ctx, s, "agent", "list")
		if err != nil {
			continue
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
		if json.Unmarshal(out, &env) != nil {
			continue
		}
		for _, a := range env.Result.Agents {
			if strings.Contains(a.CWD, "/worktrees/") {
				continue
			}
			if a.Status != "idle" && a.Status != "done" {
				continue
			}
			n := a.Name
			if n == "" {
				n = a.PaneID
			}
			return s, n, nil
		}
	}
	return "", "", nil
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

func (w *Watcher) poke(ctx context.Context, session, agent, msg string) error {
	args := []string{"agent", "prompt", agent, msg, "--wait", "--until", "working", "--timeout", "15000"}
	_, err := w.herdr(ctx, session, args...)
	return err
}
