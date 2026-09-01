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
	"github.com/jclement/cone/internal/human"
	"github.com/jclement/cone/internal/inbox"
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

	// Reraise is how long to wait before mentioning the SAME outstanding work to the same lead
	// again. Zero means the default. An agent that is told once and stops is the common way
	// work dies here, so silence after one telling is not a virtue — but neither is a poke
	// every interval, which is how a heartbeat gets ignored.
	Reraise time.Duration

	// StaleClaim is how long a claim may sit untouched before its holder is nudged about it.
	// A lead that claimed something and moved on looks identical to one mid-way through it;
	// only time separates them, and the reaper cannot help because the claimant is alive.
	StaleClaim time.Duration
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
	if o.Reraise <= 0 {
		o.Reraise = 10 * time.Minute
	}
	if o.StaleClaim <= 0 {
		o.StaleClaim = time.Hour
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

	// poked remembers, per orchestrator, the task set it was last woken about — with when, and
	// how many times. Keyed by agent so two orchestrators are not treated as one, and persisted
	// so a KeepAlive restart does not re-poke about a set someone is already triaging.
	//
	// The count is what makes persistence bounded rather than permanent. Telling a lead once
	// and never again assumes it acted; the common failure is that it answered, did part of the
	// work, and stopped, after which nothing in the system would ever mention the rest again.
	poked map[string]pokeRecord

	complaint string // the last thing that stopped a poke
	since     time.Time

	// tries counts unconfirmed pokes per agent+signature. Confirmation is not free of
	// ambiguity — an agent that takes the turn and finishes it inside the same second reads
	// as "still idle", i.e. as a prompt that never landed — so retrying forever would nag a
	// perfectly healthy lead every interval. Bounded retries, then take the win.
	tries map[string]int
}

// pokeRecord is what was said to one lead, when, and how many times without the work moving.
type pokeRecord struct {
	Sig   string    `json:"sig"`
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

const maxPokeAttempts = 3

// maxRaises bounds how often the same unchanged work is raised with the same lead. Persistence
// past this is not persistence, it is noise: if a lead has been told four times over the better
// part of an hour and the work has not moved, the problem is not that it has not heard.
const maxRaises = 4

func New(b *board.Board, opt Options) *Watcher {
	opt.defaults()
	w := &Watcher{b: b, opt: opt, poked: map[string]pokeRecord{}, tries: map[string]int{}, log: func(string, ...any) {}}
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
	// The two remote pulls run first, so a task queued from a phone or an answer that arrived
	// overnight is on the board before this tick counts the work — offered now, not next tick.
	// Both are failure-isolated: an unreachable service is logged and retried next tick, and
	// must never stop the heartbeat. Skipped under --dry-run because both mutate the board.
	if !w.opt.DryRun {
		w.syncInboxes(ctx)
		w.sweepQuestions(ctx)
	}

	ready, err := w.b.List(board.Ready)
	if err != nil {
		w.hold("cannot read %s: %v", w.b.Root, err)
		return
	}
	doing, err := w.b.List(board.Doing)
	if err != nil {
		w.hold("cannot read %s: %v", w.b.Root, err)
		return
	}
	blocked, err := w.b.List(board.Blocked)
	if err != nil {
		w.hold("cannot read %s: %v", w.b.Root, err)
		return
	}
	// Held work is a task whose pane went away with output captured on it: it is waiting on a
	// person, not on a worker. Filtering it here rather than counting all of blocked/ keeps a
	// task parked on a human decision from costing a subprocess every tick for the rest of time.
	held := heldForReview(blocked)

	// Untriaged work was a black hole: inbox/ is where `cone new` files by default and where
	// `cone sync` lands a queue, and nothing read it. Every task filed by an agent that did not
	// know to pass -ready went there and was never offered to anyone, so a board could hold a
	// week of real work and look idle. It has to be counted HERE, before the cheap exit below,
	// or a board whose only work is untriaged still returns as though it were empty.
	untriaged, err := w.b.List(board.Inbox)
	if err != nil {
		untriaged = nil
	}

	// Four directory reads, and on an empty board that is the whole tick: no subprocesses, no
	// tokens, nothing. Work in flight counts as well as work waiting — this used to return
	// here whenever ready/ was empty, which is the *normal* state at 2am while a worker is
	// finishing, so the one moment harvesting exists for was the one it never ran in.
	if len(ready) == 0 && len(doing) == 0 && len(held) == 0 && len(untriaged) == 0 {
		w.clear()
		return
	}

	agents, aerr := board.Agents(ctx, w.opt.HerdrBin)
	if aerr != nil {
		w.hold("cannot ask herdr (%s) who is running: %v", w.opt.HerdrBin, aerr)
		return
	}

	if len(doing) > 0 && !w.opt.DryRun {
		// Capture before reaping. A worker that finished and one that crashed look identical
		// once the pane is gone; the snapshot is what tells them apart, and taking it has to
		// happen while the pane still exists.
		w.harvest(ctx, agents)

		// A claim held by an agent Herdr no longer knows about is not work in progress, it
		// is a slot lost forever: the cap counts files in doing/, so a few dead claims wedge
		// the heartbeat permanently with no trace.
		if !w.opt.NoReap {
			if reaped, err := w.b.Reap(w.opt.HerdrBin, false); err == nil && len(reaped) > 0 {
				for _, t := range reaped {
					w.say("worker %q for %s is gone; it is now %s", t.Agent, t.ID, t.State)
				}
				ready, _ = w.b.List(board.Ready)
			}
		}
	}

	// Work that needs a lead and will never reach ready/: a worker that has finished, and a
	// task held for review after its pane went away. The tick used to return here whenever
	// ready/ was empty, so the system captured a finished worker's output and then told nobody
	// it had — the task sat in doing/ until a human happened to open /tasks. Arrival was the
	// only thing that could wake anyone, which is why work stalled one hop short of done.
	review := reviewable(agents, doing, held)

	// A claim older than the threshold is not work in progress; it is work that stopped. The
	// reaper cannot help, because it only rescues claims whose worker herdr has FORGOTTEN, and
	// this claimant is alive and simply moved on. Nothing else in the system says anything.
	stale, err := w.b.Stale(w.opt.StaleClaim)
	if err != nil {
		stale = nil
	}

	if len(ready) == 0 && len(review) == 0 && len(untriaged) == 0 && len(stale) == 0 {
		w.clear()
		return // nothing to say; the in-flight housekeeping above was the point of this tick
	}

	inFlight := w.b.ActiveClaims(w.opt.HerdrBin)
	// The cap gates NEW work only. Reviewable work is how a claim gets closed and the cap comes
	// back down, so staying silent about it at the cap is the one refusal that cannot clear
	// itself: every slot full of finished-but-unlanded work and nobody told to land it.
	atCap := inFlight >= w.opt.MaxWorkers

	cands := availableOrchestrators(agents)
	if len(cands) == 0 {
		w.hold("%d task(s) ready, %d waiting on a lead, %d untriaged, but no orchestrator is free to take them",
			len(ready), len(review), len(untriaged))
		return
	}

	// Match work to orchestrator by repo. A lead sitting in ~/Developer/be cannot act on a
	// task filed against another repo — poking it produces a confused turn and burns the
	// signature, so the real owner never hears about it.
	for _, c := range cands {
		mine := forRepo(ready, c.cwd)
		if atCap {
			mine = nil // at the cap there is nothing to start, but plenty to finish
		}
		theirs := forRepo(review, c.cwd)
		triage := scopedTo(untriaged, c.cwd)
		stalled := stalledFor(stale, c)
		all := concat(mine, theirs, triage, stalled)
		if len(all) == 0 {
			continue
		}
		sig := signature(all)

		// Told about this exact set already. Raise it again only after a cooldown, and only a
		// bounded number of times: an agent that answers one poke, does part of the work and
		// stops is the ordinary way work dies here, and one telling assumes it acted.
		rec := w.poked[c.name]
		if rec.Sig == sig {
			if rec.Count >= maxRaises {
				continue // said enough; see the give-up notice below
			}
			if time.Since(rec.At) < w.opt.Reraise {
				continue // still inside the cooldown; nagging is how a heartbeat gets muted
			}
		}

		msg := fmt.Sprintf(`%s

(cone: %s. Triage per %s/AGENTS.md.
Claim only what you will actually start now — a claimed task nobody is working
is worse than an unclaimed one.)`, w.opt.Prompt, workLine(len(mine), len(theirs), len(triage), len(stalled), inFlight, w.opt.MaxWorkers, atCap), w.b.Root)

		if w.opt.DryRun {
			fmt.Printf("would poke %s (session %q, cwd %s): %s\n", c.name, c.session, c.cwd,
				workLine(len(mine), len(theirs), len(triage), len(stalled), inFlight, w.opt.MaxWorkers, atCap))
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
		raised := 1
		if rec.Sig == sig {
			raised = rec.Count + 1
		}
		w.poked[c.name] = pokeRecord{Sig: sig, At: time.Now().UTC(), Count: raised}
		delete(w.tries, c.name+"\x00"+sig)
		w.saveState()
		w.clear()
		if raised == 1 {
			w.say("poked %s (session %q): %s", c.name, c.session,
				workLine(len(mine), len(theirs), len(triage), len(stalled), inFlight, w.opt.MaxWorkers, atCap))
		} else {
			w.say("raised the same work with %s again (%d of %d): %s", c.name, raised, maxRaises,
				workLine(len(mine), len(theirs), len(triage), len(stalled), inFlight, w.opt.MaxWorkers, atCap))
		}
		if raised == maxRaises {
			// Loud, once, and unconditional. Everything else here is designed so that silence
			// means nothing to do; a lead that has been told this many times and has not moved
			// the work is the one case where silence would mean the opposite.
			w.say("%s has now been told %d times about the same work and it has not moved — nothing further will be said about this set",
				c.name, maxRaises)
		}
		return
	}
	if atCap {
		w.hold("%d claim(s) in flight, cap %d, and nothing finished is waiting on a lead here",
			inFlight, w.opt.MaxWorkers)
		return
	}
	w.hold("%d task(s) ready, %d waiting on a lead, %d untriaged, but nothing is outstanding for a lead in a matching repo",
		len(ready), len(review), len(untriaged))
}

// workLine is the status line the woken lead reads. At the cap it deliberately leads with the
// cap rather than with a ready count of zero: "0 task(s) ready" next to a full board is a lie
// about why it was woken, and the thing it is being asked to do is land something, not start.
func workLine(ready, review, triage, stalled, inFlight, max int, atCap bool) string {
	head := fmt.Sprintf("%d task(s) ready, %d in flight, cap %d", ready, inFlight, max)
	if atCap {
		head = fmt.Sprintf("%d claim(s) in flight, AT the cap of %d — landing one is what frees a slot",
			inFlight, max)
	}
	if review > 0 {
		head += fmt.Sprintf(", %d finished and waiting on you", review)
	}
	if stalled > 0 {
		head += fmt.Sprintf(", %d claimed by you and not moving", stalled)
	}
	if triage > 0 {
		head += fmt.Sprintf(", %d untriaged", triage)
	}
	return head
}

// scopedTo is forRepo's stricter sibling: it requires an actual repo match rather than also
// accepting the unscoped. Untriaged work is offered this way because an inbox task with no
// repo cannot be routed — offering it to whichever lead the loop reaches first is how two
// agents end up doing the same job. Those are reported by `cone doctor` instead.
func scopedTo(tasks []*board.Task, cwd string) []*board.Task {
	var out []*board.Task
	for _, t := range tasks {
		if t.Repo != "" && matchesRepo(t.Repo, cwd) {
			out = append(out, t)
		}
	}
	return out
}

// stalledFor is the claims this lead should be nudged about: the ones it holds itself, and the
// unattributed ones in its repo.
//
// A claim with no agent recorded cannot be attributed to anybody, and it is the worst case
// rather than an exempt one — nothing will ever reap it, so it holds a worker slot forever. It
// goes to whichever lead owns the repo, because somebody has to hear about it.
func stalledFor(stale []*board.Task, c candidate) []*board.Task {
	var out []*board.Task
	for _, t := range stale {
		switch {
		case t.Agent != "" && t.Agent == c.name:
			out = append(out, t)
		case t.Agent == "" && t.Repo != "" && matchesRepo(t.Repo, c.cwd):
			out = append(out, t)
		}
	}
	return out
}

func concat(lists ...[]*board.Task) []*board.Task {
	var out []*board.Task
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// heldForReview is the blocked tasks carrying a captured snapshot: their worker's pane went
// away and reap parked them here rather than re-queueing work that may already be done.
func heldForReview(blocked []*board.Task) []*board.Task {
	var out []*board.Task
	for _, t := range blocked {
		if board.HasWorkerOutput(t.Body) {
			out = append(out, t)
		}
	}
	return out
}

// reviewable is work that is waiting on a person rather than on a worker.
//
// Two states qualify, and the system already knew about both while telling nobody: a claim
// whose worker herdr reports finished — the pane is still there so nothing reaps it, and it
// stays in doing/ until a lead lands it — and a task held for review after its pane went away.
//
// A task with no agent recorded cannot be judged this way, which is the practical reason to
// close the triangle at delegation time: `cone set <id> agent <name>` is what lets the
// heartbeat notice that this particular piece of work has finished.
func reviewable(agents []board.Agent, doing, held []*board.Task) []*board.Task {
	status := map[string]board.Agent{}
	for _, a := range agents {
		status[a.Name] = a
	}
	var out []*board.Task
	for _, t := range doing {
		if t.Agent == "" {
			continue
		}
		a, ok := status[t.Agent]
		if !ok || a.Status != "done" {
			continue
		}
		// Only DELEGATED work counts. Claim stamps the herdr identity of whoever claimed, so a
		// lead that takes a task on itself is recorded as its own agent — and a lead sits at
		// `done` between the turns of work it is in the middle of. Reporting that back to it as
		// "finished and waiting on you" would interrupt the very turn-by-turn work it describes.
		// A lead's own abandoned claim is a real problem, but it is the one this cannot see;
		// releasing before you stop is what covers it.
		if a.IsLead() {
			continue
		}
		out = append(out, t)
	}
	return append(out, held...)
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

// syncInboxes pulls configured inboxes onto the board, so a task queued from a phone arrives
// without anyone having to run `cone sync`. A host with no inboxes.json pays one file stat.
func (w *Watcher) syncInboxes(ctx context.Context) {
	filed, _, err := inbox.SyncConfigured(ctx, w.b)
	for _, t := range filed {
		w.say("filed %s (from %s)", t.ID, t.Source)
	}
	if err != nil {
		w.log("inbox sync: %v", err)
	}
}

// sweepQuestions closes the human loop. Every blocked task carrying a question id gets its
// question checked (no wait); an answered one comes back to ready with the answer noted, where
// the ordinary wake machinery offers it like any ready task — that is the point: no new poke
// path. Expired and cancelled come back too, loudly, because a lead must re-triage with eyes
// open — expiry is not consent. Once a task moves it leaves this set naturally; there is no
// ack round-trip and no per-question state outside the board.
func (w *Watcher) sweepQuestions(ctx context.Context) {
	blocked, err := w.b.List(board.Blocked)
	if err != nil {
		return
	}
	var waiting []*board.Task
	for _, t := range blocked {
		// Applied questions are skipped: the question key is kept for history, so a task
		// blocked again later for an unrelated reason must not have its old answer redelivered.
		if t.Question != "" && !human.Applied(t) {
			waiting = append(waiting, t)
		}
	}
	if len(waiting) == 0 {
		return
	}
	svc, err := human.Configured()
	if err != nil {
		w.log("human service: %v", err)
		return
	}
	if svc == nil {
		w.log("%d blocked task(s) carry a question but no human service is configured", len(waiting))
		return
	}
	for _, t := range waiting {
		ans, err := svc.Question(ctx, t.Question, 0)
		if err != nil {
			w.log("question %s (task %s): %v", t.Question, t.ID, err)
			continue
		}
		switch ans.Status {
		case human.StatusAnswered:
			if _, err := human.Answered(w.b, t.ID, t.Question, ans); err != nil {
				w.log("could not apply the answer to %s: %v", t.ID, err)
				continue
			}
			w.say("question %s answered — %s is ready again", t.Question, t.ID)
		case human.StatusExpired, human.StatusCancelled:
			if _, err := human.Unanswered(w.b, t.ID, t.Question, ans.Status); err != nil {
				w.log("could not record the %s question on %s: %v", ans.Status, t.ID, err)
				continue
			}
			w.say("question %s %s unanswered — %s is back in ready for re-triage", t.Question, ans.Status, t.ID)
		}
	}
}

// harvest captures a worker's recent terminal output onto the task it is working.
//
// This is the only part of the system that runs when nobody is awake, and it exists for one
// moment: a worker finishes at 02:10, nobody reads it, and by morning the pane is gone — a
// herdr restart, a `/land --all-done`, a laptop that slept — taking the only account of what
// happened with it. The task then looks abandoned and the work gets offered to somebody else.
//
// It reads on `done`, which herdr sets at the end of every turn rather than once at the end of
// the work, so the snapshot REPLACES rather than appends and an unchanged tail writes nothing.
// `agent read` is the passive read: it does not clear herdr's own unread flag, so /crew still
// shows the worker as needing attention.
//
// A snapshot is not a result. `cone done --result` is still the deliverable; this is the
// safety net under it, and it is stored marked unverified and kept out of the search index.
func (w *Watcher) harvest(ctx context.Context, agents []board.Agent) {
	doing, err := w.b.List(board.Doing)
	if err != nil || len(doing) == 0 {
		return
	}
	status := map[string]board.Agent{}
	for _, a := range agents {
		status[a.Name] = a
	}
	for _, t := range doing {
		a, ok := status[t.Agent]
		if !ok || a.Status != "done" {
			continue
		}
		out, err := w.herdr(ctx, a.Session, "agent", "read", a.Name,
			"--source", "recent-unwrapped", "--lines", "200")
		if err != nil {
			w.log("could not read %s: %v", a.Name, err)
			continue
		}
		changed, err := w.b.Snapshot(t.ID, string(out))
		if err != nil {
			w.log("could not store output for %s: %v", t.ID, err)
			continue
		}
		if changed {
			w.say("captured %s output onto %s", a.Name, t.ID)
		}
	}
}

// availableOrchestrators picks the agents sitting in a MAIN checkout. Workers live under a
// worktrees directory; anything else running an agent is a lead.
//
// Only "idle" counts. "done" was accepted here once and should not be: an agent that has
// finished and exited still lists, and prompting it is a message into a dead pane.
func availableOrchestrators(agents []board.Agent) []candidate {
	var out []candidate
	for _, a := range agents {
		if !a.IsLead() || !a.ReadyForInput() {
			continue
		}
		out = append(out, candidate{session: a.Session, name: a.Name, cwd: a.CWD})
	}
	return out
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
		// Still waiting for input means it never took the turn. This has to use the same
		// definition as the picker: once `done` counts as reachable, a prompt into a pane whose
		// session has ended comes back `done` too, and reading that as success would burn the
		// signature on a message nobody received — the exact failure that made `done` ineligible
		// in the first place. Detecting it here protects the invariant without hiding the lead.
		if (board.Agent{Status: a.Status}).ReadyForInput() {
			return fmt.Errorf("still waiting for input after the prompt — it did not take the turn")
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
	if json.Unmarshal(data, &w.poked) != nil {
		// Written by a version that stored a bare signature per lead. Carry it over rather than
		// discarding it: a dropped memory means one gratuitous re-poke about a set somebody may
		// already be working, on every machine, at upgrade.
		var old map[string]string
		if json.Unmarshal(data, &old) != nil {
			w.poked = nil
		} else {
			w.poked = map[string]pokeRecord{}
			for name, sig := range old {
				w.poked[name] = pokeRecord{Sig: sig, At: time.Now().UTC(), Count: 1}
			}
		}
	}
	if w.poked == nil {
		w.poked = map[string]pokeRecord{}
	}
}

func (w *Watcher) saveState() {
	data, err := json.Marshal(w.poked)
	if err != nil {
		return
	}
	_ = os.WriteFile(w.statePath(), data, 0o644)
}
