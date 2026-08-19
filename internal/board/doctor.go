package board

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Doctor answers "why did nothing happen?".
//
// Every failure this checks for is one that was, at some point, invisible: a task file that
// does not parse is skipped by List and simply does not exist as far as the board is
// concerned; a claim with no claimant occupies a worker slot that nothing can ever release;
// the heartbeat spent weeks unable to reach herdr and said so only under a flag nobody passed.
// A coordination tool that fails quietly is worse than no coordination tool, because everyone
// believes the queue is being served.
//
// Reports only. Nothing here changes state — each finding names the command that would.

type Severity int

const (
	OK Severity = iota
	Warn
	Broken
)

type Finding struct {
	Severity Severity
	Area     string
	Message  string
	Fix      string // the command that resolves it, if there is one
}

func (s Severity) Mark() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	default:
		return "✗"
	}
}

// Doctor runs every check and returns what it found, worst first within each area.
func (b *Board) Doctor(herdrBin string) []Finding {
	var out []Finding
	out = append(out, b.checkFiles()...)
	out = append(out, b.checkStates()...)
	out = append(out, b.checkClaims(herdrBin)...)
	out = append(out, b.checkIndex()...)
	out = append(out, checkOrchestrators(herdrBin)...)
	return out
}

// checkFiles reads every file on disk directly rather than through List, which is the whole
// point: List skips what it cannot parse, so a broken task is invisible exactly when someone
// is asking why their task never came up.
func (b *Board) checkFiles() []Finding {
	var out []Finding
	seen := map[string]string{} // id -> "state/file"
	parsed, broken, stray := 0, 0, 0

	for _, s := range States {
		entries, err := os.ReadDir(b.dir(s))
		if err != nil {
			out = append(out, Finding{Broken, "files", fmt.Sprintf("cannot read %s: %v", b.dir(s), err), "cone install"})
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".md") {
				if strings.HasPrefix(name, ".") {
					continue
				}
				stray++
				out = append(out, Finding{Warn, "files",
					fmt.Sprintf("%s/%s is not a .md file — nothing will ever read it", s, name), ""})
				continue
			}
			path := filepath.Join(b.dir(s), name)
			data, err := os.ReadFile(path)
			if err != nil {
				broken++
				out = append(out, Finding{Broken, "files", fmt.Sprintf("%s: %v", path, err), ""})
				continue
			}
			t, err := Unmarshal(string(data))
			if err != nil {
				broken++
				out = append(out, Finding{Broken, "files",
					fmt.Sprintf("%s does not parse (%v) — it is invisible to every command", path, err), ""})
				continue
			}
			parsed++

			id := strings.TrimSuffix(name, ".md")
			if t.ID != "" && t.ID != id {
				out = append(out, Finding{Warn, "files",
					fmt.Sprintf("%s says id: %s — the filename wins, so the frontmatter is a lie", path, t.ID), ""})
			}
			if where, dup := seen[id]; dup {
				out = append(out, Finding{Broken, "files",
					fmt.Sprintf("%s exists in both %s and %s/ — Find returns whichever it looks in first", id, where, s),
					"move or delete one of them by hand; there is no safe automatic answer"})
			}
			seen[id] = string(s)
		}
	}
	if broken == 0 && stray == 0 {
		out = append(out, Finding{OK, "files", fmt.Sprintf("%d task files, all parse, no duplicate ids", parsed), ""})
	}
	return out
}

// checkStates finds tasks whose frontmatter disagrees with the directory they are in. The
// directory is the state; anything else is a leftover from a hand-edit or an interrupted move.
func (b *Board) checkStates() []Finding {
	var out []Finding
	for _, s := range States {
		tasks, err := b.List(s)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			switch s {
			case Doing:
				if t.ClaimedBy == "" {
					out = append(out, Finding{Broken, "claims",
						fmt.Sprintf("%s is in doing/ with no claimed_by — it holds a worker slot nothing can release", t.ID),
						"cone back " + t.ID})
				}
				if t.ClaimedAt.IsZero() {
					out = append(out, Finding{Warn, "claims",
						fmt.Sprintf("%s has no claimed_at, so it can never look stale", t.ID), ""})
				}
			case Ready, Inbox:
				if t.ClaimedBy != "" {
					out = append(out, Finding{Warn, "claims",
						fmt.Sprintf("%s is in %s/ but still says claimed_by: %s", t.ID, s, t.ClaimedBy), ""})
				}
			case Done:
				if t.Completed.IsZero() {
					out = append(out, Finding{Warn, "states",
						fmt.Sprintf("%s is done with no completed timestamp", t.ID), ""})
				}
			}
		}
	}
	return out
}

// checkClaims asks herdr who is actually alive. A claim held by an agent that no longer exists
// is not work in progress — it is a permanently lost slot, and four of them stop the heartbeat
// with no visible symptom.
func (b *Board) checkClaims(herdrBin string) []Finding {
	doing, err := b.List(Doing)
	if err != nil || len(doing) == 0 {
		return nil
	}
	live, err := LiveAgents(herdrBin)
	if err != nil {
		return []Finding{{Warn, "claims",
			fmt.Sprintf("cannot ask herdr who is alive (%v), so %d claim(s) cannot be checked", err, len(doing)), ""}}
	}

	var dead, unowned []string
	for _, t := range doing {
		switch {
		case t.Agent == "":
			// The reaper will not touch these, deliberately — nothing can say a worker
			// stopped when nobody said which worker started. So they are reported here
			// instead, because otherwise they hold a slot and no command anywhere mentions
			// them until the cap wedges.
			unowned = append(unowned, t.ID)
		case !live[t.Agent]:
			dead = append(dead, t.ID)
		}
	}
	sort.Strings(dead)
	sort.Strings(unowned)

	var out []Finding
	if len(dead) > 0 {
		out = append(out, Finding{Broken, "claims",
			fmt.Sprintf("%d claim(s) whose worker herdr no longer knows about: %s", len(dead), strings.Join(dead, ", ")),
			"cone reap"})
	}
	if len(unowned) > 0 {
		out = append(out, Finding{Warn, "claims",
			fmt.Sprintf("%d claim(s) with no agent recorded, so nothing can ever release them: %s",
				len(unowned), strings.Join(unowned, ", ")),
			"cone set <id> agent <name>, or cone back <id> if nobody is working it"})
	}
	if len(out) == 0 {
		out = append(out, Finding{OK, "claims", fmt.Sprintf("%d claim(s) in flight, all held by live workers", len(doing)), ""})
	}
	return out
}

// checkOrchestrators answers the question that has no other answer: **is there anything the
// heartbeat could actually wake?**
//
// The failure is silent and easy to walk into. `cone watch` finds leads through
// `herdr agent list`, so a Claude you started yourself in a plain terminal is invisible to it —
// the board fills up, the watcher runs perfectly, and nothing is ever poked, which looks
// exactly like having nothing to do. Nothing else in the system reports that, because from
// every other angle both halves are healthy.
func checkOrchestrators(herdrBin string) []Finding {
	agents, err := Agents(context.Background(), herdrBin)
	if err != nil {
		return []Finding{{Warn, "orchestrators",
			fmt.Sprintf("cannot ask herdr who is running (%v), so nothing can be woken", err), ""}}
	}

	var leads, idle []string
	for _, a := range agents {
		if !a.IsLead() {
			continue
		}
		leads = append(leads, fmt.Sprintf("%s (%s, %s)", a.Name, a.Status, a.CWD))
		if a.Status == "idle" {
			idle = append(idle, a.Name)
		}
	}
	sort.Strings(leads)

	if len(leads) == 0 {
		return []Finding{{Warn, "orchestrators",
			"herdr knows of no agent in a main checkout, so the heartbeat has nobody to wake — " +
				"a `claude` started in a plain terminal is invisible to it",
			"start leads through herdr (`herdr agent start <name> --kind claude --pane <id>`)"}}
	}
	out := []Finding{{OK, "orchestrators",
		fmt.Sprintf("%d lead(s) visible to herdr: %s", len(leads), strings.Join(leads, ", ")), ""}}
	if len(idle) == 0 {
		out = append(out, Finding{OK, "orchestrators",
			"none idle right now — work waiting will be offered when one frees up", ""})
	}
	return out
}

func (b *Board) checkIndex() []Finding {
	st, err := os.Stat(b.IndexPath())
	if err != nil {
		return []Finding{{Warn, "index", "no search index — cone search will find nothing", "cone reindex"}}
	}
	newest := time.Time{}
	for _, s := range States {
		entries, err := os.ReadDir(b.dir(s))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	if newest.After(st.ModTime().Add(time.Minute)) {
		return []Finding{{Warn, "index",
			fmt.Sprintf("the index is older than the board (%s behind) — search will miss recent tasks",
				newest.Sub(st.ModTime()).Round(time.Minute)), "cone reindex"}}
	}
	return []Finding{{OK, "index", "current", ""}}
}
