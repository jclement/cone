// Package inbox pulls tasks from outside sources into the board.
//
// The board is the system of record. An inbox is *a* way tasks arrive — not the only one,
// and never the place work lives. Meat Prompt is one source; herdrer on another host is a
// second; the CLI and the MCP server file directly. Adding a third source should mean
// implementing one interface, not touching the board.
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
				errs = append(errs, fmt.Sprintf("%s: %v", s.Name(), err))
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

// ── Meat Prompt / herdrer ────────────────────────────────────────────────────────────
//
// Both expose the same shape for the reverse channel: the human queues something, an agent
// claims it. They differ in path (/api/v1/tasks/claim vs /api/v1/agent/tasks/claim), so the
// path is configuration rather than two near-identical implementations.

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

// FromEnv builds whichever sources this host is actually configured for. A host with neither
// service configured gets an empty list and syncs nothing, which is correct — cone is useful
// with no inboxes at all.
func FromEnv() []Source {
	var out []Source
	if u, t := readCreds("MEATPROMPT"); u != "" && t != "" {
		out = append(out, &HTTPSource{SourceName: "meatprompt", BaseURL: u, Token: t, ClaimPath: "/api/v1/tasks/claim"})
	}
	if u, t := readCreds("HERDRER"); u != "" && t != "" {
		out = append(out, &HTTPSource{SourceName: "herdrer", BaseURL: u, Token: t, ClaimPath: "/api/v1/agent/tasks/claim"})
	}
	return out
}

// readCreds checks the environment, then the conventional credentials file, so a source works
// whether it was enrolled by a script or exported by a shell.
func readCreds(prefix string) (url, token string) {
	url, token = os.Getenv(prefix+"_URL"), os.Getenv(prefix+"_TOKEN")
	if url != "" && token != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = home + "/.config"
	}
	data, err := os.ReadFile(cfg + "/" + strings.ToLower(prefix) + "/env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case prefix + "_URL":
			url = v
		case prefix + "_TOKEN":
			token = v
		}
	}
	return
}
