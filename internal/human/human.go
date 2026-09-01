// Package human posts an agent's question to a human-in-the-loop service and brings the
// answer back to the board.
//
// cone defines the contract — a POST that files a question, a GET that reports its status —
// and which service satisfies it is configuration, exactly as inboxes work. No service name
// appears in a code path and there is no default URL: a change to somebody else's API is a
// config edit, not a cone release, and a host with no ~/.config/cone/human.json simply has no
// human service, which every feature here degrades to a clear message about rather than an
// error loop.
//
// The loop the package closes: `cone ask` posts the question, stamps its id onto the task and
// parks the task in blocked/; the watch tick polls blocked questions and, when one is
// answered, notes the answer verbatim and returns the task to ready/ — where the ordinary wake
// machinery offers it like any other work. There is deliberately no separate notification
// path, no ack round-trip, and no per-question state outside the board.
package human

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
	"github.com/jclement/cone/internal/inbox"
)

// The contract's enumerations. The server enforces them too; validating locally exists so an
// agent gets "choice needs --recommend" instead of a 422 body it has to guess at.
const (
	KindConfirm = "confirm"
	KindChoice  = "choice"
	KindAck     = "ack"
	KindManual  = "manual"

	StatusOpen      = "open"
	StatusAnswered  = "answered"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

// Config is ~/.config/cone/human.json: one object naming the service that answers questions on
// this host. Token semantics are identical to an inbox entry — same TokenSource, same
// enrollment file layout.
type Config struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	AskPath      string `json:"ask_path"`
	QuestionPath string `json:"question_path"`
	inbox.TokenSource
}

// The default paths are part of the contract, so a minimal config is just a name, a url and a
// token source.
const (
	defaultAskPath      = "/api/v1/questions"
	defaultQuestionPath = "/api/v1/questions/{id}"
)

// ConfigPath is where the service is declared. $CONE_HUMAN overrides it.
func ConfigPath() string {
	if p := os.Getenv("CONE_HUMAN"); p != "" {
		return p
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "human.json"
		}
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "cone", "human.json")
}

// Configured reads the service declaration. A missing file is not an error — it means this
// host has no human service, which is a normal state every caller must handle. A file that
// exists and cannot be used IS an error: a typo that silently drops questions is
// indistinguishable from a human who never answers.
func Configured() (*Service, error) {
	data, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	if c.Name == "" || c.URL == "" {
		return nil, fmt.Errorf("%s: the human service needs a name and a url", ConfigPath())
	}
	if c.AskPath == "" {
		c.AskPath = defaultAskPath
	}
	if c.QuestionPath == "" {
		c.QuestionPath = defaultQuestionPath
	}
	if !strings.Contains(c.QuestionPath, "{id}") {
		return nil, fmt.Errorf("%s: question_path must contain {id}", ConfigPath())
	}
	token, err := c.Resolve()
	if err != nil {
		return nil, fmt.Errorf("human service %q: %w", c.Name, err)
	}
	if token == "" {
		return nil, fmt.Errorf("human service %q: no token (set token, token_env or token_file)", c.Name)
	}
	return &Service{
		Name: c.Name, baseURL: strings.TrimRight(c.URL, "/"),
		askPath: c.AskPath, questionPath: c.QuestionPath, token: token,
	}, nil
}

// Service is a configured human-in-the-loop endpoint.
type Service struct {
	Name    string
	baseURL string

	askPath      string
	questionPath string
	token        string
	Client       *http.Client // tests inject one; nil gets a sane default per call
}

// Option is one choice offered to the human.
type Option struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// Question is what gets posted. Field names mirror the wire contract.
type Question struct {
	Title          string            `json:"title"`
	BodyMD         string            `json:"body_md"`
	Kind           string            `json:"kind"`
	Options        []Option          `json:"options,omitempty"`
	Recommend      string            `json:"recommend,omitempty"`
	Context        map[string]string `json:"context"`
	Urgency        string            `json:"urgency"`
	ExpiresInHours int               `json:"expires_in_hours,omitempty"`
}

// Validate is the local half of the contract's rules — only the ones where a local check gives
// a materially better error than the server's. What makes a question answerable from a phone
// (a verbatim diff, options with consequences, the recommendation and its strongest counter)
// is guidance for the asker, deliberately not enforced here.
func (q Question) Validate() error {
	if strings.TrimSpace(q.Title) == "" {
		return fmt.Errorf("a question needs a title")
	}
	if strings.TrimSpace(q.BodyMD) == "" {
		return fmt.Errorf("a question needs a body — the human answers from a phone and sees nothing else")
	}
	switch q.Kind {
	case KindConfirm, KindChoice, KindAck, KindManual:
	default:
		return fmt.Errorf("kind must be confirm, choice, ack or manual (got %q)", q.Kind)
	}
	switch q.Urgency {
	case "low", "normal", "blocking":
	default:
		return fmt.Errorf("urgency must be low, normal or blocking (got %q)", q.Urgency)
	}
	// The contract requires a recommendation on the kinds that present a decision: a human
	// choosing blind between options the agent understands better is the worst of both worlds.
	if (q.Kind == KindConfirm || q.Kind == KindChoice) && q.Recommend == "" {
		return fmt.Errorf("kind %q requires --recommend — say which option you would pick, and why in the body", q.Kind)
	}
	if q.Recommend != "" && len(q.Options) > 0 {
		found := false
		for _, o := range q.Options {
			if o.Key == q.Recommend {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("--recommend %q names no --option key", q.Recommend)
		}
	}
	return nil
}

// Asked is the service's receipt for a filed question.
type Asked struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// Answer is a question's current status, and — once settled — what the human said.
type Answer struct {
	Status string `json:"status"` // open | answered | expired | cancelled
	Value  string `json:"answer_value"`
	Note   string `json:"answer_note"`
}

// Settled reports whether the question will never change again.
func (a Answer) Settled() bool { return a.Status != StatusOpen && a.Status != "" }

func (s *Service) client(timeout time.Duration) *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: timeout}
}

// Ask files a question and returns the service's receipt.
func (s *Service) Ask(ctx context.Context, q Question) (*Asked, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+s.askPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s returned %s: %s", s.Name, resp.Status, snippet(resp.Body))
	}
	var env struct {
		Data Asked `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if env.Data.ID == "" {
		return nil, fmt.Errorf("%s accepted the question but returned no id", s.Name)
	}
	return &env.Data, nil
}

// maxWait is the contract's long-poll ceiling.
const maxWait = 90 * time.Second

// Question fetches a question's status. wait > 0 long-polls: the server holds the request up
// to that long (capped at the contract's 90s) waiting for the question to settle.
func (s *Service) Question(ctx context.Context, id string, wait time.Duration) (*Answer, error) {
	if wait > maxWait {
		wait = maxWait
	}
	url := s.baseURL + strings.ReplaceAll(s.questionPath, "{id}", id)
	timeout := 20 * time.Second
	if wait > 0 {
		url += fmt.Sprintf("?wait=%d", int(wait.Seconds()))
		// The client timeout must outlive the server's hold, or every long-poll "fails".
		timeout = wait + 30*time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s returned %s: %s", s.Name, resp.Status, snippet(resp.Body))
	}
	var a Answer
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, err
	}
	if a.Status == "" {
		return nil, fmt.Errorf("%s returned no status for question %s", s.Name, id)
	}
	return &a, nil
}

func snippet(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 200))
	return strings.TrimSpace(string(data))
}

// ── Applying an outcome to the board ─────────────────────────────────────────────────────
//
// `cone ask --wait` and the watch sweep both end at the same place: the answer noted on the
// task, verbatim, and the task back in ready/ where the heartbeat offers it like any other
// work. One function produces that end state so the two paths cannot drift.

// Answered records the human's answer on the task and returns it to ready. The answer text is
// kept verbatim; the note heading carries the question id, which is also what marks the
// question as applied so the sweep never delivers the same answer twice. The question key on
// the frontmatter is deliberately kept — it is the task's history.
//
// The RELEASE HAPPENS FIRST, and the order is load-bearing. Note-then-release left a two-call
// window where a crash produced a task in blocked/ whose body already carried the applied
// marker — which the sweep and doctor both skip, so it was stranded silently and permanently.
// Release-then-note fails the other way: a task in ready/ with the answer not yet noted, which
// is merely degraded — a lead is offered it, the Asked note carries the question URL, and if it
// is ever blocked again the sweep re-fetches and finally notes the still-unapplied answer.
func Answered(b *board.Board, taskID, questionID string, ans *Answer) (*board.Task, error) {
	text := strings.TrimSpace(ans.Value)
	if strings.TrimSpace(ans.Note) != "" {
		if text != "" {
			text += "\n\n"
		}
		text += strings.TrimSpace(ans.Note)
	}
	if text == "" {
		text = "(answered with no text)"
	}
	return settle(b, taskID, appliedHeading(StatusAnswered, questionID), text)
}

// Unanswered records that a question settled without an answer and returns the task to ready
// so a lead re-triages it with eyes open. Expiry is not consent: the task comes back for a
// decision, never as authorisation to proceed.
func Unanswered(b *board.Board, taskID, questionID, status string) (*board.Task, error) {
	return settle(b, taskID, appliedHeading(status, questionID), UnansweredNote(questionID, status))
}

// settle is the shared release-then-note ordering — see Answered for why that order.
func settle(b *board.Board, taskID, heading, text string) (*board.Task, error) {
	t, err := b.Find(taskID)
	if err != nil {
		return nil, err
	}
	if t.State == board.Blocked {
		if _, err := b.Release(taskID); err != nil {
			return nil, err
		}
	}
	return b.Note(taskID, heading, text)
}

// UnansweredNote is the text recorded when a question settles with nothing.
func UnansweredNote(questionID, status string) string {
	verb := "expired"
	if status == StatusCancelled {
		verb = "was cancelled"
	}
	return fmt.Sprintf("question %s %s unanswered — expiry is not consent. Nothing was approved; re-triage before acting.", questionID, verb)
}

// appliedHeading is the note heading that marks a question's outcome as delivered to the task.
func appliedHeading(status, questionID string) string {
	if status == StatusAnswered {
		return "Answer " + questionID
	}
	return "Unanswered " + questionID
}

// Applied reports whether the task's body already records an outcome for its question. The
// question key is kept on the frontmatter for history, so without this check a task blocked
// again later — for an unrelated reason — would have its old, already-delivered answer applied
// once more and be yanked back to ready on every tick.
func Applied(t *board.Task) bool {
	if t.Question == "" {
		return false
	}
	return strings.Contains(t.Body, "## "+appliedHeading(StatusAnswered, t.Question)) ||
		strings.Contains(t.Body, "## "+appliedHeading(StatusExpired, t.Question))
}
