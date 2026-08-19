// Package board is the agent coordination board.
//
// The design rule that everything else follows from: one file per thing, and the directory
// is the state. Moving a task between directories IS the state transition, and because
// same-filesystem rename is atomic, it is also the lock — two agents racing to claim the
// same task, exactly one wins, with no daemon, no database and no coordinator.
//
// Keep that property. Never replace a rename with copy+remove, and never introduce a
// separate lock file: the whole value of this layout is that any agent can participate
// with nothing but `mv`, even when this binary is not installed.
package board

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// State is a task's lifecycle position, which is literally the directory it sits in.
type State string

const (
	Inbox   State = "inbox"   // filed, not yet triaged
	Ready   State = "ready"   // triaged and claimable — the heartbeat watches this
	Doing   State = "doing"   // claimed; Frontmatter records who and when
	Blocked State = "blocked" // waiting on something outside the agent's control
	Done    State = "done"
)

// States is the canonical order for listing: the order work flows.
var States = []State{Inbox, Ready, Doing, Blocked, Done}

// ErrLostRace is returned when another agent claimed a task first. It is an ordinary
// outcome in a concurrent system, not a failure — callers should pick something else
// rather than retrying.
var ErrLostRace = errors.New("another agent claimed it first")

var ErrNotFound = errors.New("no such task")

// Task is one unit of work. Fields map to YAML frontmatter; Body is the markdown beneath.
type Task struct {
	ID        string    `yaml:"id"`
	Title     string    `yaml:"title"`
	Repo      string    `yaml:"repo"`
	Kind      string    `yaml:"kind"`     // investigate | implement | review | chore
	Priority  string    `yaml:"priority"` // low | normal | high
	Auto      bool      `yaml:"auto"`     // pre-authorised to start without asking
	Source    string    `yaml:"source"`   // which inbox it arrived from
	SourceRef string    `yaml:"source_ref"`
	Created   time.Time `yaml:"created"`
	ClaimedBy string    `yaml:"claimed_by"`
	ClaimedAt time.Time `yaml:"claimed_at"`
	Worktree  string    `yaml:"worktree"`
	Completed time.Time `yaml:"completed"`

	Body  string `yaml:"-"`
	State State  `yaml:"-"`
	Path  string `yaml:"-"`
}

// Board is a rooted directory. Everything is relative to it.
type Board struct{ Root string }

func Open(root string) (*Board, error) {
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, "agent")
	}
	b := &Board{Root: root}
	return b, b.ensure()
}

func (b *Board) ensure() error {
	dirs := []string{"board", "claims"}
	for _, s := range States {
		dirs = append(dirs, filepath.Join("tasks", string(s)))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(b.Root, d), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (b *Board) dir(s State) string             { return filepath.Join(b.Root, "tasks", string(s)) }
func (b *Board) path(s State, id string) string { return filepath.Join(b.dir(s), id+".md") }

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug builds a stable, sortable id: date prefix plus a bounded title slug.
func Slug(title string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	return time.Now().UTC().Format("20060102") + "-" + s
}

// New files a task into inbox. It refuses to overwrite: an id collision means the caller
// is about to lose someone's task.
func (b *Board) New(t Task) (*Task, error) {
	if t.Title == "" {
		return nil, errors.New("a task needs a title")
	}
	if t.ID == "" {
		t.ID = Slug(t.Title)
	}
	if t.Kind == "" {
		t.Kind = "investigate"
	}
	if t.Priority == "" {
		t.Priority = "normal"
	}
	if t.Created.IsZero() {
		t.Created = time.Now().UTC()
	}
	if t.Body == "" {
		t.Body = t.Title + "\n\n## Done when\n\n(describe the observable outcome — not \"the code is written\")\n"
	}
	// An id must be unique across EVERY state, not just the directory being written to.
	// Checking only inbox lets a new task collide with one already claimed or completed,
	// and then Find returns whichever state it happens to look in first — the board
	// silently disagrees with itself about what a task id means.
	if existing, err := b.Find(t.ID); err == nil {
		return nil, fmt.Errorf("task %s already exists in %s", t.ID, existing.State)
	}
	t.State, t.Path = Inbox, b.path(Inbox, t.ID)

	f, err := os.OpenFile(t.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("task %s already exists", t.ID)
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(t.Marshal()); err != nil {
		return nil, err
	}
	return &t, nil
}

// Find locates a task in whichever state holds it.
func (b *Board) Find(id string) (*Task, error) {
	for _, s := range States {
		if t, err := b.load(s, id); err == nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

func (b *Board) load(s State, id string) (*Task, error) {
	p := b.path(s, id)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	t, err := Unmarshal(string(data))
	if err != nil {
		return nil, err
	}
	t.State, t.Path, t.ID = s, p, id
	return t, nil
}

// List returns tasks in a state, ordered by priority then id.
func (b *Board) List(s State) ([]*Task, error) {
	entries, err := os.ReadDir(b.dir(s))
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		t, err := b.load(s, strings.TrimSuffix(e.Name(), ".md"))
		if err != nil {
			continue // a malformed file should not hide the rest of the board
		}
		out = append(out, t)
	}
	rank := map[string]int{"high": 0, "normal": 1, "low": 2}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].Priority], rank[out[j].Priority]
		if ri != rj {
			return ri < rj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// move is the single state transition. os.Rename is atomic within a filesystem, and it is
// the only mechanism this package uses to change state — see the package comment.
func (b *Board) move(t *Task, to State) error {
	dst := b.path(to, t.ID)
	if err := os.Rename(t.Path, dst); err != nil {
		return err
	}
	t.State, t.Path = to, dst
	return nil
}

// Claim moves ready -> doing and stamps the claimant. Exactly one caller can succeed for a
// given task; the loser gets ErrLostRace.
//
// The race is resolved by the rename itself, not by checking first: a check-then-move would
// leave a window in which both callers see the file and both proceed.
func (b *Board) Claim(id, who string) (*Task, error) {
	if who == "" {
		who = "unknown"
	}
	src := b.path(Ready, id)
	dst := b.path(Doing, id)

	if err := os.Link(src, dst); err != nil {
		if os.IsExist(err) || os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrLostRace, id)
		}
		return nil, err
	}
	// The hard link won the race; drop the ready/ name. A crash between these two lines
	// leaves the task visible in both, which List tolerates and `board sweep` reports —
	// strictly better than a window where it is visible in neither.
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	t, err := b.load(Doing, id)
	if err != nil {
		return nil, err
	}
	t.ClaimedBy, t.ClaimedAt = who, time.Now().UTC()
	return t, b.Save(t)
}

// Release returns a claimed or blocked task to ready and clears the claim.
func (b *Board) Release(id string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	if t.State != Doing && t.State != Blocked {
		return nil, fmt.Errorf("only doing or blocked tasks can be released; %s is %s", id, t.State)
	}
	t.ClaimedBy, t.ClaimedAt = "", time.Time{}
	if err := b.Save(t); err != nil {
		return nil, err
	}
	return t, b.move(t, Ready)
}

func (b *Board) Complete(id string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	if t.State != Doing {
		return nil, fmt.Errorf("only a claimed task can be completed; %s is %s", id, t.State)
	}
	t.Completed = time.Now().UTC()
	if err := b.Save(t); err != nil {
		return nil, err
	}
	return t, b.move(t, Done)
}

func (b *Board) Block(id, why string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	if t.State != Doing {
		return nil, fmt.Errorf("only a claimed task can be blocked; %s is %s", id, t.State)
	}
	if why != "" {
		t.Body = strings.TrimRight(t.Body, "\n") +
			fmt.Sprintf("\n\n## Blocked %s\n\n%s\n", time.Now().UTC().Format(time.RFC3339), why)
	}
	if err := b.Save(t); err != nil {
		return nil, err
	}
	return t, b.move(t, Blocked)
}

// Promote moves inbox -> ready. Triage is a deliberate step: an untriaged task should not
// be claimable, because nobody has decided it is worth doing or what done means.
func (b *Board) Promote(id string) (*Task, error) {
	t, err := b.Find(id)
	if err != nil {
		return nil, err
	}
	if t.State != Inbox {
		return nil, fmt.Errorf("only an inbox task can be promoted; %s is %s", id, t.State)
	}
	return t, b.move(t, Ready)
}

// Save rewrites a task in place, preserving its state.
func (b *Board) Save(t *Task) error {
	tmp := t.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(t.Marshal()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.Path)
}

// Stale reports tasks claimed longer ago than d. It reports only: whether a claim is
// abandoned or merely slow is a judgement an agent should not make unilaterally.
func (b *Board) Stale(d time.Duration) ([]*Task, error) {
	all, err := b.List(Doing)
	if err != nil {
		return nil, err
	}
	var out []*Task
	cut := time.Now().UTC().Add(-d)
	for _, t := range all {
		if !t.ClaimedAt.IsZero() && t.ClaimedAt.Before(cut) {
			out = append(out, t)
		}
	}
	return out, nil
}

// Whoami identifies the acting agent for a claim. Herdr exports HERDR_AGENT into every pane
// it owns, so claims label themselves with no configuration.
func Whoami() string {
	for _, k := range []string{"CONE_AGENT", "HERDR_AGENT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	h, _ := os.Hostname()
	if h == "" {
		return "unknown"
	}
	return h
}
