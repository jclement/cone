// Package inbox pulls tasks from outside sources into the board.
//
// The board is the system of record. An inbox is *a* way tasks arrive — not the only one,
// and never the place work lives. The CLI and the MCP server file directly; an inbox is for
// the case where a human queues something somewhere else — a phone, a web form — and wants an
// agent to pick it up. cone knows about no particular service: sources are declared in
// ~/.config/cone/inboxes.json, so a change to somebody else's API is a config edit, not a
// cone release.
//
// Every source is pull-based and idempotent: it is asked "what is new since X" and returns
// tasks carrying a stable SourceRef, so re-running never duplicates. That matters more than
// it sounds — an inbox that double-files on a retry is worse than one that misses, because
// duplicated work looks like two people wanting the same thing.
package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
)

// Source is anything that can hand the board new tasks.
type Source interface {
	Name() string
	// Fetch returns tasks not previously seen. Implementations must set SourceRef to a
	// stable id from the upstream system so Sync can deduplicate.
	Fetch(ctx context.Context) ([]board.Task, error)
	// Ack tells the source a task was filed, so it stops offering it. Sources that cannot
	// acknowledge (a read-only feed) should return nil and rely on SourceRef dedup.
	Ack(ctx context.Context, ref string) error
}

// Sync pulls from every source and files what is new. It returns what it filed.
//
// Deduplication is by SourceRef against every state, not just inbox: a task already claimed
// or completed must not reappear because the upstream still lists it.
func Sync(ctx context.Context, b *board.Board, sources []Source) ([]*board.Task, error) {
	seen, err := seenRefs(b)
	if err != nil {
		return nil, err
	}

	var filed []*board.Task
	var errs []string
	for _, s := range sources {
		tasks, err := s.Fetch(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.Name(), err))
			continue
		}
		for _, t := range tasks {
			key := s.Name() + "/" + t.SourceRef
			if t.SourceRef == "" || seen[key] {
				continue
			}
			t.Source = s.Name()
			nt, err := b.New(t)
			if err != nil {
				// The upstream claim already removed this from the human's queue and there
				// is no release endpoint here, so a failure to file means the request exists
				// nowhere: not on their phone, not on the board. Quarantine it instead —
				// `cone doctor` reports the directory, and the file is a task ready to be
				// moved into inbox/ by hand.
				if qerr := quarantine(b, t, err); qerr != nil {
					errs = append(errs, fmt.Sprintf("%s: %v (AND could not quarantine it: %v)", s.Name(), err, qerr))
				} else {
					errs = append(errs, fmt.Sprintf("%s: %v — quarantined, not lost", s.Name(), err))
				}
				continue
			}
			seen[key] = true
			filed = append(filed, nt)
			if err := s.Ack(ctx, t.SourceRef); err != nil {
				// The task is filed; failing to ack means we might see it again, which
				// dedup already handles. Not worth failing the sync over.
				errs = append(errs, fmt.Sprintf("%s: ack %s: %v", s.Name(), t.SourceRef, err))
			}
		}
	}
	if len(errs) > 0 {
		return filed, fmt.Errorf("sync had problems: %s", strings.Join(errs, "; "))
	}
	return filed, nil
}

// QuarantineDir holds tasks that were claimed upstream but could not be filed. Nothing else
// remembers them: claiming on these endpoints is destructive and there is no release call.
func QuarantineDir(b *board.Board) string { return filepath.Join(b.Root, "inbox-quarantine") }

func quarantine(b *board.Board, t board.Task, cause error) error {
	dir := QuarantineDir(b)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	t.Body = strings.TrimRight(t.Body, "\n") +
		fmt.Sprintf("\n\n## Could not be filed\n\n%v\n\nClaimed from %s (%s) and removed from that queue, "+
			"so this file is the only copy. Fix the cause and move it into tasks/inbox/.\n", cause, t.Source, t.SourceRef)
	name := t.Source + "-" + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, t.SourceRef) + ".md"
	return os.WriteFile(filepath.Join(dir, name), []byte(t.Marshal()), 0o644)
}

func seenRefs(b *board.Board) (map[string]bool, error) {
	seen := map[string]bool{}
	for _, st := range board.States {
		tasks, err := b.List(st)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.Source != "" && t.SourceRef != "" {
				seen[t.Source+"/"+t.SourceRef] = true
			}
		}
	}
	return seen, nil
}

// ── Configured sources ───────────────────────────────────────────────────────────────
//
// cone knows about no particular service, deliberately. Two named integrations used to be
// compiled in — their URLs, their claim paths, the layout of their credential files — which
// made this package a place other people's APIs go to rot: a path change upstream became a
// cone release. A source is configuration now.
//
//	~/.config/cone/inboxes.json
//	[
//	  {
//	    "name":       "phone-queue",
//	    "url":        "https://queue.example.com",
//	    "claim_path": "/api/v1/tasks/claim",
//	    "token_file": "~/.config/phone-queue/env",
//	    "token_key":  "PHONE_QUEUE_TOKEN"
//	  }
//	]
//
// A source needs one endpoint: a GET that hands the caller one queued task and returns 204
// when there are none. `token` and `token_env` work in place of `token_file`. No file, or an
// empty list, means this host syncs nothing — which is the common case and costs nothing.

// Config is one entry in inboxes.json.
type Config struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	ClaimPath string `json:"claim_path"`
	Token     string `json:"token"`
	TokenEnv  string `json:"token_env"`
	TokenFile string `json:"token_file"`
	TokenKey  string `json:"token_key"`
}

// ConfigPath is where sources are declared. $CONE_INBOXES overrides it.
func ConfigPath() string {
	if p := os.Getenv("CONE_INBOXES"); p != "" {
		return p
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "inboxes.json"
		}
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "cone", "inboxes.json")
}

// Configured reads the source list. A missing file is not an error: most hosts have none.
// A file that exists and cannot be read IS an error — a typo that silently syncs nothing is
// indistinguishable from a queue that happens to be empty.
func Configured() ([]Source, error) {
	data, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(), err)
	}

	var out []Source
	for _, c := range cfgs {
		if c.Name == "" || c.URL == "" {
			return nil, fmt.Errorf("%s: every inbox needs a name and a url", ConfigPath())
		}
		if c.ClaimPath == "" {
			return nil, fmt.Errorf("%s: inbox %q needs a claim_path", ConfigPath(), c.Name)
		}
		token, err := c.token()
		if err != nil {
			return nil, fmt.Errorf("inbox %q: %w", c.Name, err)
		}
		if token == "" {
			return nil, fmt.Errorf("inbox %q: no token (set token, token_env or token_file)", c.Name)
		}
		out = append(out, &HTTPSource{
			SourceName: c.Name, BaseURL: c.URL, Token: token, ClaimPath: c.ClaimPath,
		})
	}
	return out, nil
}

func (c Config) token() (string, error) {
	if c.Token != "" {
		return c.Token, nil
	}
	if c.TokenEnv != "" {
		return os.Getenv(c.TokenEnv), nil
	}
	if c.TokenFile == "" {
		return "", nil
	}
	path := c.TokenFile
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// A KEY=VALUE file, which is what every service's enrollment writes. Without a token_key
	// the first value wins, so a single-line file needs no extra configuration.
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if c.TokenKey == "" || strings.TrimSpace(k) == c.TokenKey {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s has no %s", path, orDefault(c.TokenKey, "usable KEY=VALUE line"))
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type HTTPSource struct {
	SourceName string
	BaseURL    string
	Token      string
	ClaimPath  string // e.g. /api/v1/tasks/claim
	Client     *http.Client
}

func (h *HTTPSource) Name() string { return h.SourceName }

func (h *HTTPSource) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Fetch drains the upstream queue by claiming repeatedly until it reports empty.
//
// Claiming on fetch is deliberate: these endpoints hand a task to exactly one caller, so
// leaving it upstream would let a second agent claim it directly and work it outside the
// board. Pulling it in makes the board the single place work is tracked.
func (h *HTTPSource) Fetch(ctx context.Context) ([]board.Task, error) {
	if h.BaseURL == "" || h.Token == "" {
		return nil, nil // not configured on this host; silently contribute nothing
	}
	var out []board.Task
	for i := 0; i < 25; i++ { // bounded: a broken upstream must not spin forever
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.BaseURL, "/")+h.ClaimPath, nil)
		if err != nil {
			return out, err
		}
		req.Header.Set("Authorization", "Bearer "+h.Token)
		resp, err := h.client().Do(req)
		if err != nil {
			return out, err
		}
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			return out, nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return out, fmt.Errorf("%s returned %s", h.SourceName, resp.Status)
		}
		var env struct {
			Data struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Body  string `json:"body"`
				Repo  string `json:"repo"`
			} `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		if err != nil {
			return out, err
		}
		if env.Data.ID == "" {
			return out, nil
		}
		title := env.Data.Title
		if title == "" {
			title = firstLine(env.Data.Body)
		}
		out = append(out, board.Task{
			Title:     title,
			Body:      env.Data.Body,
			Repo:      env.Data.Repo,
			SourceRef: env.Data.ID,
			Kind:      "investigate",
		})
	}
	return out, nil
}

// Ack is a no-op: claiming already removed it from the upstream queue.
func (h *HTTPSource) Ack(context.Context, string) error { return nil }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.TrimLeft(s, "# "))
	if len(s) > 72 {
		s = s[:72]
	}
	if s == "" {
		return "untitled task"
	}
	return s
}
