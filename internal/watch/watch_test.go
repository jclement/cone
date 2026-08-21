package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// The heartbeat is this project's headline feature and it had no tests at all — which is
// exactly how it shipped in a state where it could never fire. herdr is faked with a script,
// so these exercise the real exec path, the real JSON parsing, and the real decision to poke.

// fakeHerdr writes a script standing in for the herdr binary. It logs every invocation to
// $dir/calls, answers `session list` and `agent list`, and flips the agent to "working" once
// prompted — the observable an agent has actually taken the turn.
func fakeHerdr(t *testing.T, agentsJSON string) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake")
	}
	dir = t.TempDir()
	bin = filepath.Join(dir, "herdr")
	script := `#!/bin/sh
echo "$@" >> "` + dir + `/calls"
for a in "$@"; do case "$a" in prompt) touch "` + dir + `/prompted";; esac; done
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list")
      if [ -f "` + dir + `/prompted" ]; then
        cat "` + dir + `/agents.json" | sed 's/"idle"/"working"/'
      else
        cat "` + dir + `/agents.json"
      fi ;;
  "agent prompt") exit 0 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(agentsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func agents(list ...[3]string) string {
	type a struct {
		Name   string `json:"name"`
		Status string `json:"agent_status"`
		CWD    string `json:"cwd"`
	}
	var env struct {
		Result struct {
			Agents []a `json:"agents"`
		} `json:"result"`
	}
	for _, x := range list {
		env.Result.Agents = append(env.Result.Agents, a{Name: x[0], Status: x[1], CWD: x[2]})
	}
	out, _ := json.Marshal(env)
	return string(out)
}

func boardWith(t *testing.T, tasks ...board.Task) *board.Board {
	t.Helper()
	b, err := board.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range tasks {
		if task.Title == "" {
			task.Title = fmt.Sprintf("task %d", i)
		}
		nt, err := b.New(task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Promote(nt.ID); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func calls(t *testing.T, dir string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return string(data)
}

func TestPokesAnIdleOrchestratorWhenWorkIsReady(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "idle", "/Users/x/Developer/be"}))
	b := boardWith(t, board.Task{Title: "look at the solver"})
	w := New(b, Options{HerdrBin: bin, Verbose: true})

	w.Tick(context.Background())

	if !strings.Contains(calls(t, dir), "agent prompt lead") {
		t.Fatalf("no prompt was sent; herdr saw:\n%s", calls(t, dir))
	}
}

// The bug that kept this dead for weeks: launchd's PATH has no Homebrew, every exec failed
// with ENOENT, and all three layers that could have said so were --verbose-gated. An
// unreachable herdr must be loud.
func TestUnreachableHerdrIsReportedNotSwallowed(t *testing.T) {
	b := boardWith(t, board.Task{Title: "something"})
	w := New(b, Options{HerdrBin: "/nonexistent/herdr"})

	r, wr, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = wr
	w.Tick(context.Background())
	os.Stderr = stderr
	wr.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)

	if !strings.Contains(string(buf[:n]), "herdr") {
		t.Fatalf("a heartbeat that cannot reach herdr said nothing (got %q)", buf[:n])
	}
}

func TestDoesNotNagAboutTheSameReadySet(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "idle", "/Users/x/Developer/be"}))
	b := boardWith(t, board.Task{Title: "one thing"})
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())
	before := strings.Count(calls(t, dir), "agent prompt")
	w.Tick(context.Background())
	w.Tick(context.Background())

	if after := strings.Count(calls(t, dir), "agent prompt"); after != before {
		t.Fatalf("poked %d times about an unchanged queue, want %d", after, before)
	}
}

// The signature must only be recorded once the prompt demonstrably landed. If it is recorded
// for a prompt that never arrived, that exact set of tasks is never mentioned again.
func TestSignatureIsNotRecordedWhenThePromptDoesNotLand(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	// Answers, accepts the prompt, but the agent never leaves idle.
	os.WriteFile(bin, []byte(`#!/bin/sh
echo "$@" >> "`+dir+`/calls"
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list") printf '{"result":{"agents":[{"name":"lead","agent_status":"idle","cwd":"/tmp/x"}]}}\n' ;;
esac
`), 0o755)
	b := boardWith(t, board.Task{Title: "one thing"})
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())
	w.Tick(context.Background())

	if n := strings.Count(calls(t, dir), "agent prompt"); n < 2 {
		t.Fatalf("gave up after a prompt that never took effect (%d attempts)", n)
	}
	if len(w.poked) != 0 {
		t.Fatalf("recorded a signature for a prompt that did not land: %v", w.poked)
	}
}

// A restart must not re-poke: launchd KeepAlive respawns this process on upgrade, crash and
// logout, and an in-memory-only memory means a fresh nag every time.
func TestPokeMemorySurvivesARestart(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "idle", "/Users/x/Developer/be"}))
	b := boardWith(t, board.Task{Title: "one thing"})

	New(b, Options{HerdrBin: bin}).Tick(context.Background())
	before := strings.Count(calls(t, dir), "agent prompt")

	os.Remove(filepath.Join(dir, "prompted")) // the agent went idle again
	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if after := strings.Count(calls(t, dir), "agent prompt"); after != before {
		t.Fatalf("a restarted watcher re-poked about the same set (%d then %d)", before, after)
	}
}

func TestWorkersAreNotPokedOnlyLeads(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"worker", "idle", "/Users/x/.superset/worktrees/be/feature-x"}))
	b := boardWith(t, board.Task{Title: "one thing"})

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("poked an agent inside a worktree; workers are not orchestrators")
	}
}

func TestATaskGoesToALeadInItsOwnRepo(t *testing.T) {
	bin, dir := fakeHerdr(t,
		agents([3]string{"be-lead", "idle", "/Users/x/Developer/be"},
			[3]string{"cone-lead", "idle", "/Users/x/Developer/cone"}))
	b := boardWith(t, board.Task{Title: "fix the TUI", Repo: "cone"})

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	got := calls(t, dir)
	if !strings.Contains(got, "agent prompt cone-lead") {
		t.Fatalf("the cone task did not reach the cone lead:\n%s", got)
	}
	if strings.Contains(got, "agent prompt be-lead") {
		t.Fatalf("a cone task was handed to a lead sitting in another repo:\n%s", got)
	}
}

func TestNobodyIsPokedWhenNoLeadIsInAMatchingRepo(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"be-lead", "idle", "/Users/x/Developer/be"}))
	b := boardWith(t, board.Task{Title: "fix the TUI", Repo: "cone"})

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("handed a cone task to a lead that cannot act on it")
	}
}

func TestMatchesRepo(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := []struct {
		repo, cwd string
		want      bool
	}{
		{"be", "/Users/x/Developer/be", true},
		{"BE", "/Users/x/Developer/be", true},          // filed by hand, case is not a decision
		{"be", "/Users/x/Developer/be/packages", true}, // a lead one level in still owns it
		{"be", "/Users/x/Developer/cone", false},
		{"beta", "/Users/x/Developer/be", false}, // prefix, not a match
		{"/Users/x/Developer/be", "/Users/x/Developer/be", true},
		{"/Users/x/Developer/be", "/Users/x/Developer/before", false},
		{"~/Developer/be", filepath.Join(home, "Developer/be"), true},
		{"", "/Users/x/Developer/be", false},
	}
	for _, c := range cases {
		if got := matchesRepo(c.repo, c.cwd); got != c.want {
			t.Errorf("matchesRepo(%q, %q) = %v, want %v", c.repo, c.cwd, got, c.want)
		}
	}
}

func TestUnscopedTasksGoToAnyLead(t *testing.T) {
	ready := []*board.Task{{ID: "a"}, {ID: "b", Repo: "cone"}}
	if got := forRepo(ready, "/Users/x/Developer/be"); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("a lead in be should see exactly the unscoped task, got %v", got)
	}
}

// `done` used to be ineligible, and that made a lead unreachable from the end of its first
// turn: herdr reports `done` for an agent that is waiting for input but whose tab has not been
// SEEN in the focused UI, and a CLI read never marks a tab seen. So a lead answered one poke,
// went back to `done` unseen, and the heartbeat skipped it forever while reporting that no idle
// orchestrator was in a matching repo.
func TestAnUnseenLeadIsStillReachable(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "done", "/Users/x/Developer/be"}))
	b := boardWith(t, board.Task{Title: "one thing"})

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if !strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("a lead waiting for input was never offered the work")
	}
}

// The invariant the old `done` exclusion was really protecting, kept: `done` also covers an
// agent whose session has ENDED but which still lists for a while. Prompting that is a message
// into a dead pane, and recording the signature for it would mean the real orchestrator never
// hears about that set. The prompt is bounded (`--until working`) and the confirmation re-reads
// the status, so a pane that never takes the turn must not burn the signature.
func TestAPokeIntoADeadPaneDoesNotBurnTheSignature(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	// Accepts the prompt and stays `done` forever: it never took the turn.
	os.WriteFile(bin, []byte(`#!/bin/sh
echo "$@" >> "`+dir+`/calls"
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list") printf '{"result":{"agents":[{"name":"lead","agent_status":"done","cwd":"/tmp/x"}]}}\n' ;;
esac
`), 0o755)
	b := boardWith(t, board.Task{Title: "one thing"})
	w := New(b, Options{HerdrBin: bin})

	w.Tick(context.Background())
	if w.poked["lead"] != "" {
		t.Fatal("a prompt that was never taken up recorded its signature; the set is now lost")
	}

	// And it must not retry forever either — the bound still applies.
	for i := 0; i < 6; i++ {
		w.Tick(context.Background())
	}
	if w.poked["lead"] == "" {
		t.Fatal("after the attempt bound it should record and move on")
	}
}

// Confirmation is ambiguous in one direction: an agent that takes the turn and finishes it
// inside the same second reads exactly like one that never started. Retrying forever would
// prompt a healthy lead about the same tasks every interval, for good.
func TestAnUnconfirmablePokeIsNotRetriedForever(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	// Accepts the prompt and reports idle afterwards, every time.
	os.WriteFile(bin, []byte(`#!/bin/sh
echo "$@" >> "`+dir+`/calls"
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list") printf '{"result":{"agents":[{"name":"lead","agent_status":"idle","cwd":"/tmp/x"}]}}\n' ;;
esac
`), 0o755)
	b := boardWith(t, board.Task{Title: "one thing"})
	w := New(b, Options{HerdrBin: bin})

	for i := 0; i < 6; i++ {
		w.Tick(context.Background())
	}

	if n := strings.Count(calls(t, dir), "agent prompt"); n > maxPokeAttempts {
		t.Fatalf("prompted %d times about the same set; the cap is %d", n, maxPokeAttempts)
	}
	if len(w.poked) == 0 {
		t.Fatal("gave up without recording the set, so it will nag again after a restart")
	}
}

// harvestHerdr fakes a worker that has finished a turn and is sitting unread, with terminal
// output to capture. Each `agent read` returns whatever is in $dir/output.
func harvestHerdr(t *testing.T, worker string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "herdr")
	script := `#!/bin/sh
echo "$@" >> "` + dir + `/calls"
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list") printf '{"result":{"agents":[{"name":"` + worker + `","agent_status":"done","cwd":"/Users/x/.superset/worktrees/be/x"}]}}\n' ;;
  "agent read") cat "` + dir + `/output" ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "output"), []byte("$ mise run check\nEXIT=0\nthe N+1 was in loadWells\n"), 0o644)
	return bin, dir
}

func claimedBy(t *testing.T, b *board.Board, worker string) *board.Task {
	t.Helper()
	task, err := b.New(board.Task{Title: "delegated work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(task.ID, "lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Set(task.ID, "agent", worker); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Find(task.ID)
	return got
}

// The 2am case: a worker finishes, nobody reads it, and by morning the pane is gone along with
// the only account of what happened.
func TestAFinishedWorkersOutputIsCapturedOntoItsTask(t *testing.T) {
	bin, _ := harvestHerdr(t, "be-2175")
	b, err := board.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task := claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	got, err := b.Find(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !board.HasWorkerOutput(got.Body) {
		t.Fatalf("nothing was captured:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "the N+1 was in loadWells") {
		t.Fatalf("the worker's output is not on the task:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "unverified") {
		t.Error("terminal tail was stored without being marked unverified — it reads as a finding")
	}
}

// herdr flips an agent to "done" at the end of EVERY turn, not once at the end of the work.
// Appending would grow the file all night.
func TestRepeatedHarvestsReplaceRatherThanAccumulate(t *testing.T) {
	bin, dir := harvestHerdr(t, "be-2175")
	b, _ := board.Open(t.TempDir())
	task := claimedBy(t, b, "be-2175")

	w := New(b, Options{HerdrBin: bin})
	w.Tick(context.Background())
	os.WriteFile(filepath.Join(dir, "output"), []byte("later: it was the cache after all\n"), 0o644)
	w.Tick(context.Background())

	got, _ := b.Find(task.ID)
	if n := strings.Count(got.Body, "## Worker output"); n != 1 {
		t.Fatalf("the task carries %d output sections, want 1:\n%s", n, got.Body)
	}
	if !strings.Contains(got.Body, "the cache after all") {
		t.Error("the latest output did not replace the older snapshot")
	}
}

// Terminal tail is evidence, not a finding. It must not compete for rank with what an agent
// actually concluded — that is the one question the index exists to answer.
func TestCapturedOutputIsNotSearchable(t *testing.T) {
	bin, _ := harvestHerdr(t, "be-2175")
	b, _ := board.Open(t.TempDir())
	claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	hits, err := b.Search("loadWells", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("captured terminal output is in the search index (%d hits)", len(hits))
	}
	if hits, _ := b.Search("delegated", 10); len(hits) == 0 {
		t.Fatal("stripping the snapshot also removed the task itself from the index")
	}
}

// A finished worker and a crashed one look identical once the pane is gone. Releasing the
// finished one to ready/ offers work that is already done to somebody else.
func TestAHarvestedTaskIsHeldForReviewNotReoffered(t *testing.T) {
	bin, dir := harvestHerdr(t, "be-2175")
	b, _ := board.Open(t.TempDir())
	task := claimedBy(t, b, "be-2175")

	w := New(b, Options{HerdrBin: bin})
	w.Tick(context.Background()) // captures
	// The pane goes away: herdr no longer lists the worker at all.
	os.WriteFile(filepath.Join(dir, "herdr"), []byte(`#!/bin/sh
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list") printf '{"result":{"agents":[]}}\n' ;;
esac
`), 0o755)
	w.Tick(context.Background()) // reaps

	got, err := b.Find(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == board.Ready {
		t.Fatal("finished work was put back on the queue as if nobody had done it")
	}
	if got.State != board.Blocked {
		t.Fatalf("state is %s, want blocked — a human has to read the output and close it", got.State)
	}
}

// A worker that crashed with nothing to show has no snapshot, and that task genuinely should
// go back on the queue.
func TestAClaimWithNothingToShowIsStillReleased(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	os.WriteFile(bin, []byte(`#!/bin/sh
case "$1 $2" in
  "session list") printf 'NAME STATUS\ndefault running\n' ;;
  "agent list") printf '{"result":{"agents":[]}}\n' ;;
esac
`), 0o755)
	b, _ := board.Open(t.TempDir())
	task := claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	got, _ := b.Find(task.ID)
	if got.State != board.Ready {
		t.Fatalf("state is %s, want ready — nothing was captured, so the work still needs doing", got.State)
	}
}

// The gap that stopped work one hop short of done: the tick returned as soon as ready/ was
// empty, so a worker could finish, have its output captured, and nobody was ever told. Arrival
// was the only thing that could wake a lead — never completion.
func TestAFinishedWorkerWakesTheLead(t *testing.T) {
	bin, dir := fakeHerdr(t,
		agents([3]string{"lead", "idle", "/Users/x/Developer/be"},
			[3]string{"be-2175", "done", "/Users/x/.herdr/worktrees/be/feature-x"}))
	b := boardWith(t)
	claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	got := calls(t, dir)
	if !strings.Contains(got, "agent prompt lead") {
		t.Fatalf("a finished worker woke nobody; herdr saw:\n%s", got)
	}
	if !strings.Contains(got, "finished and waiting on you") {
		t.Fatalf("the lead was woken without being told what is waiting:\n%s", got)
	}
}

// A worker still working is not something to wake anyone about.
func TestAWorkingWorkerWakesNobody(t *testing.T) {
	bin, dir := fakeHerdr(t,
		agents([3]string{"lead", "idle", "/Users/x/Developer/be"},
			[3]string{"be-2175", "working", "/Users/x/.herdr/worktrees/be/feature-x"}))
	b := boardWith(t)
	claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("woke a lead about work that is still in progress")
	}
}

// The cap gates NEW work only. Landing a finished claim is what frees a slot, so refusing to
// mention finished work at the cap is the one refusal that can never clear itself: every slot
// full of finished-but-unlanded work, and nobody told to land any of it.
func TestTheWorkerCapDoesNotSilenceFinishedWork(t *testing.T) {
	bin, dir := fakeHerdr(t,
		agents([3]string{"lead", "idle", "/Users/x/Developer/be"},
			[3]string{"be-2175", "done", "/Users/x/.herdr/worktrees/be/feature-x"}))
	b := boardWith(t, board.Task{Title: "something new and ready"})
	claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin, MaxWorkers: 1}).Tick(context.Background())

	got := calls(t, dir)
	if !strings.Contains(got, "agent prompt lead") {
		t.Fatalf("at the cap with a finished worker, nobody was told to land it:\n%s", got)
	}
	if !strings.Contains(got, "AT the cap") {
		t.Fatalf("the lead was not told why it was woken at the cap:\n%s", got)
	}
}

// ...but the cap still does its job for work that has not started.
func TestNewWorkIsStillWithheldAtTheCap(t *testing.T) {
	bin, dir := fakeHerdr(t,
		agents([3]string{"lead", "idle", "/Users/x/Developer/be"},
			[3]string{"be-2175", "working", "/Users/x/.herdr/worktrees/be/feature-x"}))
	b := boardWith(t, board.Task{Title: "something new and ready"})
	claimedBy(t, b, "be-2175")

	New(b, Options{HerdrBin: bin, MaxWorkers: 1}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("started new work while at the worker cap")
	}
}

// A task with no agent recorded cannot be judged finished — which is the practical cost of
// delegating without `cone set <id> agent <name>`.
func TestAClaimWithNoWorkerRecordedIsNotMistakenForFinished(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "done", "/Users/x/Developer/be"}))
	b := boardWith(t)
	nt, _ := b.New(board.Task{Title: "claimed and never delegated"})
	b.Promote(nt.ID)
	b.Claim(nt.ID, "lead")

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("treated an unattributed claim as finished work")
	}
}

// Claim stamps the herdr identity of whoever claimed it, so a lead working a task inline is
// recorded as its own agent — and a lead sits at `done` between the turns of that very work.
// Reporting it back as "finished and waiting on you" interrupts the work it is describing.
func TestALeadIsNotToldAboutItsOwnClaim(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "done", "/Users/x/Developer/be"}))
	b := boardWith(t)
	claimedBy(t, b, "lead") // the lead is its own agent

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("woke a lead about the task it is itself in the middle of")
	}
}
