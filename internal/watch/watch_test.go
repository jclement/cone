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

// "done" is not idle. A finished agent still lists for a while, and prompting it is a message
// into a pane whose session has ended — accepted here once, which burnt the signature so the
// real orchestrator never heard about that set.
func TestAFinishedAgentIsNotTreatedAsIdle(t *testing.T) {
	bin, dir := fakeHerdr(t, agents([3]string{"lead", "done", "/Users/x/Developer/be"}))
	b := boardWith(t, board.Task{Title: "one thing"})

	New(b, Options{HerdrBin: bin}).Tick(context.Background())

	if strings.Contains(calls(t, dir), "agent prompt") {
		t.Fatal("prompted an agent that had already finished")
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
