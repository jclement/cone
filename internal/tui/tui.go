// Package tui is the interactive board.
//
// House rule from `go-cli`: gorgeous, but scriptable. Every action here has a CLI equivalent,
// and nothing is only reachable through the TUI — an agent must never need a terminal UI to
// participate.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jclement/cone/internal/board"
	"github.com/jclement/cone/internal/selfupdate"
)

var (
	cDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	cHead   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	cSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("42"))
	cState  = lipgloss.NewStyle().Foreground(lipgloss.Color("111"))
	cBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	cTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	cErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

type row struct {
	task  *board.Task
	state board.State
}

type model struct {
	b        *board.Board
	upd      *selfupdate.Check
	rows     []row
	cur      int
	status   string
	update   string
	isErr    bool
	detail   bool
	pending  string // a destructive key awaiting its second press: "d:<id>"
	w, h     int
	quitting bool

	// The watching half: what the heartbeat has been doing, and what herdr says about the
	// workers behind the claims. Both are read on a timer — the board never waits on them.
	activity []string
	agents   map[string]string // agent name -> herdr status
	beat     time.Time         // when the heartbeat last wrote
	ticks    int
	noWatch  bool // `a` hides the activity pane
}

// tickMsg drives the live refresh. The board is directory reads — the same thing the heartbeat
// does for free every interval — so redrawing once a second costs nothing and means the screen
// is never a snapshot of whenever you happened to open it.
type tickMsg time.Time

// agentsMsg is what herdr says about every agent it knows. It arrives on its own schedule
// because it costs subprocesses, and a board that stalled a second per redraw to ask about
// workers would be worse than one that answers about them fifteen seconds late.
type agentsMsg map[string]string

// updateMsg carries the answer from the background update check. It arrives whenever it
// arrives — the board is already on screen and usable before then.
type updateMsg struct{ tag string }

// Run shows the board. upd is the update check started at launch and may be nil.
func Run(b *board.Board, upd *selfupdate.Check) error {
	m := &model{b: b, upd: upd}
	m.reload()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *model) reload() {
	// Remember the task, not the row. Every mutation reorders the list — completing the row
	// above you slides a different task under the cursor, and the next keypress acts on it.
	var was string
	if r := m.sel(); r != nil {
		was = r.task.ID
	}
	m.rows = nil
	for _, s := range board.States {
		if s == board.Done {
			continue // done is history; `cone ls done` still shows it
		}
		list, err := m.b.List(s)
		if err != nil {
			continue
		}
		for _, t := range list {
			m.rows = append(m.rows, row{task: t, state: s})
		}
	}
	if was != "" {
		for i, r := range m.rows {
			if r.task.ID == was {
				m.cur = i
				break
			}
		}
	}
	if m.cur >= len(m.rows) {
		m.cur = max(0, len(m.rows)-1)
	}
}

func (m *model) Init() tea.Cmd {
	m.refreshWatch()
	return tea.Batch(awaitUpdate(m.upd), tickCmd(), agentsCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// agentsCmd asks herdr who is running. A failure yields an empty map rather than an error: the
// board is still worth looking at when herdr is unreachable, and the claims simply go
// un-annotated instead of the screen filling with a problem you already know about.
func agentsCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		list, err := board.Agents(ctx, os.Getenv("CONE_HERDR"))
		if err != nil {
			return agentsMsg(nil)
		}
		out := agentsMsg{}
		for _, a := range list {
			out[a.Name] = a.Status
		}
		return out
	}
}

// refreshWatch re-reads the heartbeat's own narration. The watcher already logs every poke,
// hold, capture and reap with a timestamp — this surfaces what was previously only reachable
// by tailing a dotfile nobody knew was there.
func (m *model) refreshWatch() {
	path := filepath.Join(m.b.Root, ".watch.log")
	if st, err := os.Stat(path); err == nil {
		m.beat = st.ModTime()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		m.activity = nil
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n := len(lines); n > activityLines {
		lines = lines[n-activityLines:]
	}
	m.activity = lines
}

const activityLines = 8

// awaitUpdate blocks in Bubble Tea's command goroutine, so an unreachable GitHub costs the
// board nothing. No update, or a check that was skipped, yields no message at all.
func awaitUpdate(upd *selfupdate.Check) tea.Cmd {
	if upd == nil {
		return nil
	}
	return func() tea.Msg {
		rel := upd.Result()
		if rel == nil {
			return nil
		}
		return updateMsg{tag: rel.Tag}
	}
}

func (m *model) sel() *row {
	if len(m.rows) == 0 || m.cur >= len(m.rows) {
		return nil
	}
	return &m.rows[m.cur]
}

// confirmed gates a destructive key behind a second press of the same key on the same task.
// d and b sit next to j and k; a fat finger completed someone else's task with no undo, and
// there is no undo to add — done/ is a directory, not a transaction log.
func (m *model) confirmed(key, id, what string) bool {
	want := key + ":" + id
	if m.pending == want {
		m.pending = ""
		return true
	}
	m.pending = want
	m.say("press %s again to %s", key, what)
	return false
}

func (m *model) say(format string, a ...any) { m.status, m.isErr = fmt.Sprintf(format, a...), false }
func (m *model) oops(err error)              { m.status, m.isErr = err.Error(), true }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		// Deliberately does not touch status or pending: a live redraw must never eat the
		// message you are reading or the confirmation you are halfway through giving.
		m.reload()
		m.refreshWatch()
		m.ticks++
		if m.ticks%15 == 0 {
			return m, tea.Batch(tickCmd(), agentsCmd())
		}
		return m, tickCmd()
	case agentsMsg:
		m.agents = msg
	case updateMsg:
		m.update = msg.tag + " available — run: cone update"
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cur > 0 {
				m.cur--
			}
		case "down", "j":
			if m.cur < len(m.rows)-1 {
				m.cur++
			}
		case "enter", " ":
			m.detail = !m.detail
		case "r":
			m.reload()
			m.refreshWatch()
			m.say("reloaded")
		case "a":
			m.noWatch = !m.noWatch
		case "p": // promote inbox -> ready
			if r := m.sel(); r != nil {
				if _, err := m.b.Promote(r.task.ID); err != nil {
					m.oops(err)
				} else {
					m.say("%s is ready", r.task.ID)
					m.reload()
				}
			}
		case "c": // claim
			if r := m.sel(); r != nil {
				if _, err := m.b.Claim(r.task.ID, board.Whoami()); err != nil {
					m.oops(err)
				} else {
					m.say("claimed %s", r.task.ID)
					m.reload()
				}
			}
		case "d": // done
			r := m.sel()
			if r == nil {
				break
			}
			// An investigation must record what it found, and there is nowhere to type it
			// here. Sending people to the CLI is better than a TUI that quietly files an
			// empty result — which is exactly the hole this refusal exists to close.
			if r.task.Kind == "investigate" {
				m.oops(fmt.Errorf("%s is an investigation — finish it with: cone done %s --result \"…\"", r.task.ID, r.task.ID))
				break
			}
			if !m.confirmed("d", r.task.ID, "complete "+r.task.ID) {
				break
			}
			if _, err := m.b.CompleteWith(r.task.ID, ""); err != nil {
				m.oops(err)
			} else {
				m.say("%s done", r.task.ID)
				m.reload()
			}
		case "b": // back / release
			r := m.sel()
			if r == nil {
				break
			}
			if !m.confirmed("b", r.task.ID, "release "+r.task.ID) {
				break
			}
			if _, err := m.b.Release(r.task.ID); err != nil {
				m.oops(err)
			} else {
				m.say("released %s", r.task.ID)
				m.reload()
			}
		default:
			m.pending = "" // any other key cancels a pending confirmation
		}
	}
	return m, nil
}

func (m *model) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder

	b.WriteString(cTitle.Render("cone") + cDim.Render("  ·  the Cone of Silence never worked either") + "\n")
	b.WriteString(" " + m.summary() + "\n")
	b.WriteString(cBorder.Render(strings.Repeat("─", maxi(20, m.w-2))) + "\n")

	if len(m.rows) == 0 {
		b.WriteString("\n  " + cDim.Render("nothing on the board. `cone new \"...\"` to file something.") + "\n")
	}

	// The activity pane is the ambient view and the detail pane is a focused read; showing both
	// at once means neither fits. Detail wins while it is open.
	showActivity := !m.noWatch && !m.detail && len(m.activity) > 0

	var lines []string
	curLine := 0
	last := board.State("")
	for i, r := range m.rows {
		if r.state != last {
			lines = append(lines, "", " "+cHead.Render(strings.ToUpper(string(r.state))))
			last = r.state
		}
		line := fmt.Sprintf(" %-42s %-7s %-10s %s",
			trunc(r.task.ID, 42), r.task.Priority, trunc(r.task.Repo, 10), r.task.ClaimedBy)
		if !r.task.ClaimedAt.IsZero() {
			line += cDim.Render("  " + shortDur(time.Since(r.task.ClaimedAt)))
		}
		line += m.worker(r)
		if i == m.cur {
			curLine = len(lines)
			lines = append(lines, cSel.Render("▸"+line))
		} else {
			lines = append(lines, " "+line)
		}
	}

	// Reserve room for everything below the list, so a long board scrolls instead of pushing
	// the activity pane — the thing you opened this to watch — off the bottom of the terminal.
	if m.h > 0 {
		avail := m.h - 3 - 5
		if showActivity {
			avail -= len(m.activity) + 3
		}
		lines = window(lines, curLine, avail)
	}
	for _, l := range lines {
		b.WriteString(l + "\n")
	}

	if m.detail {
		if r := m.sel(); r != nil {
			b.WriteString("\n" + cBorder.Render(strings.Repeat("─", maxi(20, m.w-2))) + "\n")
			b.WriteString(cHead.Render(r.task.Title) + "\n")
			if r.task.Auto {
				b.WriteString(cAccent.Render("auto: pre-authorised to start without asking") + "\n")
			} else {
				b.WriteString(cWarn.Render("auto: false — ask before starting this") + "\n")
			}
			b.WriteString("\n" + cDim.Render(trunc(strings.TrimSpace(r.task.Body), 1200)) + "\n")
		}
	}

	if showActivity {
		b.WriteString("\n" + cBorder.Render(strings.Repeat("─", maxi(20, m.w-2))) + "\n")
		b.WriteString(" " + cHead.Render("ACTIVITY") + cDim.Render("  the heartbeat's own log") + "\n")
		for _, line := range m.activity {
			b.WriteString(" " + activityLine(line, m.w-2) + "\n")
		}
	}

	b.WriteString("\n" + cBorder.Render(strings.Repeat("─", maxi(20, m.w-2))) + "\n")
	if m.status != "" {
		st := cAccent
		if m.isErr {
			st = cErr
		}
		b.WriteString(" " + st.Render(m.status) + "\n")
	}
	if m.update != "" {
		b.WriteString(" " + cWarn.Render(m.update) + "\n")
	}
	b.WriteString(cDim.Render(" ↑↓ move · enter detail · p promote · c claim · d done · b release · a activity · q quit") + "\n")
	b.WriteString(cDim.Render(" d and b ask for a second press") + "\n")
	return b.String()
}

// summary is the one line that answers "is this thing working" without reading anything else:
// what is on the board, and how long ago the heartbeat last said something. A heartbeat that
// has gone quiet is the failure this whole project is organised around, so it is on screen
// permanently rather than behind a command.
func (m *model) summary() string {
	counts := map[board.State]int{}
	for _, r := range m.rows {
		counts[r.state]++
	}
	parts := []string{}
	for _, st := range []board.State{board.Inbox, board.Ready, board.Doing, board.Blocked} {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, n))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "empty")
	}
	out := cDim.Render(strings.Join(parts, " · "))

	if m.beat.IsZero() {
		return out + cDim.Render("   ·   ") + cErr.Render("heartbeat: never run")
	}
	age := time.Since(m.beat)
	beat := fmt.Sprintf("heartbeat %s ago", shortDur(age))
	switch {
	case age > 6*time.Hour:
		return out + cDim.Render("   ·   ") + cErr.Render(beat)
	case age > 30*time.Minute:
		return out + cDim.Render("   ·   ") + cWarn.Render(beat)
	}
	return out + cDim.Render("   ·   ") + cAccent.Render(beat)
}

// worker annotates a claim with what herdr says about the agent behind it. This is the
// difference between a row that looks busy and one that is: a claim whose worker has finished
// is waiting on YOU, and a claim whose worker herdr no longer knows about is a lost slot.
func (m *model) worker(r row) string {
	if r.state != board.Doing || r.task.Agent == "" {
		return ""
	}
	if m.agents == nil {
		return "" // herdr has not answered yet; say nothing rather than guess
	}
	switch m.agents[r.task.Agent] {
	case "working":
		return cAccent.Render("  ● working")
	case "idle", "done":
		return cWarn.Render("  ✓ finished — land it")
	case "":
		return cErr.Render("  ✗ worker gone")
	default:
		return cDim.Render("  ? " + m.agents[r.task.Agent])
	}
}

// activityLine renders one line of the watcher log: the time it happened, and what it was.
// Colour carries the kind, because the whole point of having this on screen is to see a poke
// land — or fail to — without reading every word.
func activityLine(line string, width int) string {
	when, rest := "", line
	if f := strings.Fields(line); len(f) > 1 {
		if t, err := time.Parse(time.RFC3339, f[0]); err == nil {
			when, rest = t.Local().Format("15:04:05"), strings.TrimSpace(line[len(f[0]):])
		}
	}
	st := cDim
	switch {
	case strings.Contains(rest, "cannot") || strings.Contains(rest, "could not") || strings.Contains(rest, "did not"):
		st = cErr
	case strings.HasPrefix(rest, "poked"):
		st = cAccent
	case strings.Contains(rest, "captured") || strings.Contains(rest, "is gone") || strings.Contains(rest, "watching"):
		st = cWarn
	}
	return cDim.Render(when) + " " + st.Render(trunc(rest, maxi(20, width-10)))
}

// window keeps the cursor on screen when the board is taller than the terminal, and says how
// much it is not showing — a list that silently ends at the fold is how you miss the task you
// came to find.
func window(lines []string, cur, avail int) []string {
	if avail < 3 || len(lines) <= avail {
		return lines
	}
	start := cur - avail/2
	if start < 0 {
		start = 0
	}
	if start+avail > len(lines) {
		start = len(lines) - avail
	}
	out := append([]string{}, lines[start:start+avail]...)
	if start > 0 {
		out[0] = cDim.Render(fmt.Sprintf("  ↑ %d more above", start))
	}
	if end := start + avail; end < len(lines) {
		out[len(out)-1] = cDim.Render(fmt.Sprintf("  ↓ %d more below", len(lines)-end))
	}
	return out
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max(a, b int) int { return maxi(a, b) }

func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

var _ = cState
