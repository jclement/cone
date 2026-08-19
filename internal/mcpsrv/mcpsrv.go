// Package mcpsrv exposes the board over MCP.
//
// Two transports, one tool set:
//
//   - stdio (`cone mcp`) for local agents. No auth, no port, no hosting — the client owns
//     the process. This is the one to register with `claude mcp add`.
//   - HTTP (`cone serve`) for agents on another machine, which is the only thing the CLI
//     genuinely cannot do. Bearer token required; bind to loopback and put a tunnel in front
//     rather than exposing it.
//
// The tool surface is deliberately small. MCP tool descriptions sit in an agent's context for
// the whole session, so every tool added is a permanent tax on every conversation. Anything an
// agent can do with one Bash call to `cone` does not need to be a tool; these exist because
// they are the operations where structure actually helps, or where the agent is remote.
package mcpsrv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
)

const protocolVersion = "2025-06-18"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tools() []tool {
	return []tool{
		{"cone_ls", "List tasks on the agent board. State is one of inbox, ready, doing, blocked, done, or all. Default: ready. Use this before claiming anything to see what is available.",
			obj(map[string]any{"state": str("inbox|ready|doing|blocked|done|all")})},
		{"cone_show", "Read one task in full — frontmatter and body. Always read a task before claiming it; the body carries the acceptance bar.",
			obj(map[string]any{"id": str("task id")}, "id")},
		{"cone_claim", "Atomically claim a ready task. Exactly one agent can succeed; if another won, this returns an error and you should pick a different task rather than retrying. Claim only what you will start now.",
			obj(map[string]any{"id": str("task id"), "agent": str("your agent name")}, "id")},
		{"cone_new", "File a new task into the inbox. Untriaged tasks are not claimable until promoted, which is deliberate.",
			obj(map[string]any{"title": str("short imperative title"), "body": str("markdown; include a '## Done when' section"), "repo": str("repo it belongs to"), "priority": str("low|normal|high")}, "title")},
		{"cone_update", "Change a task's state or record what you found. Actions: ready (promote from inbox), done, block, back (release a claim), note (record a finding and keep working), worktree (record where the work is happening). Completing an investigation REQUIRES result — a finding nobody wrote down is the failure this board exists to prevent. Blocking files it away; it does not notify anyone.",
			obj(map[string]any{
				"id":     str("task id"),
				"action": str("ready|done|block|back|note|worktree"),
				"result": str("what you found — required to finish an investigation"),
				"note":   str("why, for block; the finding, for note"),
				"value":  str("the worktree path, for worktree"),
			}, "id", "action")},
		{"cone_search", "Full-text search across every task and board message. Run this BEFORE starting work to find whether someone already investigated the same thing — repeated work is the most common multi-agent failure.",
			obj(map[string]any{"query": str("FTS5 query; quote phrases"), "limit": map[string]any{"type": "integer"}}, "query")},
		{"cone_post", "Leave a message for other agents — something they would otherwise have to rediscover. Not a status feed; if nobody would act differently after reading it, do not post it.",
			obj(map[string]any{"topic": str("short topic"), "text": str("the message"), "from": str("your agent name")}, "topic", "text")},
		{"cone_read", "Read recent board messages from other agents.",
			obj(map[string]any{"limit": map[string]any{"type": "integer"}})},
	}
}

type server struct{ b *board.Board }

func (s *server) handle(req *request) *response {
	res := &response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		res.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cone", "version": "dev"},
		}
	case "notifications/initialized", "notifications/cancelled":
		return nil // notifications take no reply
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		res.Result = map[string]any{"tools": tools()}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			res.Error = &rpcError{-32602, "bad params: " + err.Error()}
			return res
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			// Tool errors are reported in-band with isError so the model can react,
			// rather than as protocol errors which read as a broken server.
			res.Result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return res
		}
		res.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	default:
		res.Error = &rpcError{-32601, "unknown method: " + req.Method}
	}
	return res
}

func argStr(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
func argInt(a map[string]any, k string, def int) int {
	if v, ok := a[k].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

func (s *server) call(name string, a map[string]any) (string, error) {
	switch name {
	case "cone_ls":
		want := board.States
		switch st := argStr(a, "state"); st {
		case "", "ready":
			want = []board.State{board.Ready}
		case "all":
		default:
			want = []board.State{board.State(st)}
		}
		var sb strings.Builder
		for _, st := range want {
			list, err := s.b.List(st)
			if err != nil || len(list) == 0 {
				continue
			}
			fmt.Fprintf(&sb, "%s (%d)\n", strings.ToUpper(string(st)), len(list))
			for _, t := range list {
				fmt.Fprintf(&sb, "  %-44s %-7s %-10s %s\n", t.ID, t.Priority, t.Repo, t.ClaimedBy)
			}
		}
		if sb.Len() == 0 {
			return "nothing here", nil
		}
		return sb.String(), nil

	case "cone_show":
		t, err := s.b.Find(argStr(a, "id"))
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(t.Path)
		return string(data), err

	case "cone_claim":
		agent := argStr(a, "agent")
		if agent == "" {
			agent = "mcp-client"
		}
		t, err := s.b.Claim(argStr(a, "id"), agent)
		if err != nil {
			return "", err
		}
		data, _ := os.ReadFile(t.Path)
		return "claimed. The task follows; copy its body into your worker's brief — a brief that " +
			"only references a task id will be compacted away into nothing.\n\n" + string(data), nil

	case "cone_new":
		t, err := s.b.New(board.Task{
			Title: argStr(a, "title"), Body: argStr(a, "body"),
			Repo: argStr(a, "repo"), Priority: argStr(a, "priority"),
		})
		if err != nil {
			return "", err
		}
		return "filed into inbox: " + t.ID + "\n(promote it with cone_update action=ready to make it claimable)", nil

	case "cone_update":
		id, action := argStr(a, "id"), argStr(a, "action")
		var t *board.Task
		var err error
		switch action {
		case "ready":
			t, err = s.b.Promote(id)
		case "done":
			t, err = s.b.CompleteWith(id, argStr(a, "result"))
		case "note":
			text := argStr(a, "note")
			if text == "" {
				text = argStr(a, "result")
			}
			t, err = s.b.Note(id, "Note", text)
		case "worktree":
			t, err = s.b.Set(id, "worktree", argStr(a, "value"))
		case "back":
			t, err = s.b.Release(id)
		case "block":
			t, err = s.b.Block(id, argStr(a, "note"))
		default:
			return "", fmt.Errorf("unknown action %q (ready|done|block|back|note|worktree)", action)
		}
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("%s is now %s", t.ID, t.State)
		if action == "note" || action == "worktree" {
			msg = fmt.Sprintf("%s updated (still %s)", t.ID, t.State)
		}
		if action == "block" {
			msg += "\nNote: blocked/ is a filing cabinet, not a notification — post the actual question to the ask-the-human service."
		}
		return msg, nil

	case "cone_search":
		hits, err := s.b.Search(argStr(a, "query"), argInt(a, "limit", 20))
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "no matches", nil
		}
		var sb strings.Builder
		for _, h := range hits {
			loc := h.State
			if h.Kind == "message" {
				loc = "message"
			}
			fmt.Fprintf(&sb, "%s  [%s]\n  %s\n  %s\n\n", h.ID, loc, h.Title, strings.ReplaceAll(h.Snippet, "\n", " "))
		}
		return sb.String(), nil

	case "cone_post":
		from := argStr(a, "from")
		if from == "" {
			from = "mcp-client"
		}
		m, err := s.b.Post(from, argStr(a, "topic"), argStr(a, "text"))
		if err != nil {
			return "", err
		}
		return "posted: " + m.Path, nil

	case "cone_read":
		msgs, err := s.b.Read(argInt(a, "limit", 10))
		if err != nil {
			return "", err
		}
		if len(msgs) == 0 {
			return "no messages", nil
		}
		var sb strings.Builder
		for _, m := range msgs {
			fmt.Fprintf(&sb, "─── %s · %s · %s\n%s\n\n",
				m.Posted.Format("2006-01-02 15:04"), m.From, m.Topic, m.Body)
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// ServeStdio speaks newline-delimited JSON-RPC on stdin/stdout, which is what an MCP client
// launching this as a subprocess expects. Nothing may be written to stdout except protocol
// frames — diagnostics go to stderr or they corrupt the stream.
func ServeStdio(b *board.Board) error {
	s := &server{b: b}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	out := json.NewEncoder(os.Stdout)

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = out.Encode(response{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if res := s.handle(&req); res != nil {
			if err := out.Encode(res); err != nil {
				return err
			}
		}
	}
	return in.Err()
}

// ServeHTTP is the remote transport. A token is required: this hands out the ability to file
// and claim work, and an unauthenticated board on a shared network is a prompt-injection
// channel into every agent that reads it.
func ServeHTTP(b *board.Board, addr, token string) error {
	if token == "" {
		return fmt.Errorf("refusing to serve without a token: set --token or CONE_TOKEN")
	}
	s := &server{b: b}
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "root": b.Root})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "post JSON-RPC to this endpoint", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "parse error", http.StatusBadRequest)
			return
		}
		res := s.handle(&req)
		w.Header().Set("Content-Type", "application/json")
		if res == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	})

	srv := &http.Server{
		Addr: addr, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "cone serving MCP on http://%s/mcp (board: %s)\n", addr, b.Root)
	return srv.ListenAndServe()
}
