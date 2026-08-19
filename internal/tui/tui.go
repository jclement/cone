// Package tui is the interactive board.
//
// House rule from `go-cli`: gorgeous, but scriptable. Every action here has a CLI equivalent,
// and nothing is only reachable through the TUI — an agent must never need a terminal UI to
// participate.
package tui

import (
	"fmt"
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
	w, h     int
	quitting bool
}

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
	if m.cur >= len(m.rows) {
		m.cur = max(0, len(m.rows)-1)
	}
}

func (m *model) Init() tea.Cmd { return awaitUpdate(m.upd) }

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

func (m *model) say(format string, a ...any) { m.status, m.isErr = fmt.Sprintf(format, a...), false }
func (m *model) oops(err error)              { m.status, m.isErr = err.Error(), true }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
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
			m.say("reloaded")
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
			if r := m.sel(); r != nil {
				if _, err := m.b.Complete(r.task.ID); err != nil {
					m.oops(err)
				} else {
					m.say("%s done", r.task.ID)
					m.reload()
				}
			}
		case "b": // back / release
			if r := m.sel(); r != nil {
				if _, err := m.b.Release(r.task.ID); err != nil {
					m.oops(err)
				} else {
					m.say("released %s", r.task.ID)
					m.reload()
				}
			}
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
	b.WriteString(cBorder.Render(strings.Repeat("─", maxi(20, m.w-2))) + "\n")

	if len(m.rows) == 0 {
		b.WriteString("\n  " + cDim.Render("nothing on the board. `cone new \"...\"` to file something.") + "\n")
	}

	last := board.State("")
	for i, r := range m.rows {
		if r.state != last {
			b.WriteString("\n " + cHead.Render(strings.ToUpper(string(r.state))) + "\n")
			last = r.state
		}
		line := fmt.Sprintf(" %-42s %-7s %-10s %s",
			trunc(r.task.ID, 42), r.task.Priority, trunc(r.task.Repo, 10), r.task.ClaimedBy)
		if !r.task.ClaimedAt.IsZero() {
			line += cDim.Render("  " + shortDur(time.Since(r.task.ClaimedAt)))
		}
		if i == m.cur {
			b.WriteString(cSel.Render("▸"+line) + "\n")
		} else {
			b.WriteString(" " + line + "\n")
		}
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
	b.WriteString(cDim.Render(" ↑↓ move · enter detail · p promote · c claim · d done · b release · r reload · q quit") + "\n")
	return b.String()
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
