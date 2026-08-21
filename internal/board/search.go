package board

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Search is a full-text index over what cone already owns: tasks in every state, and board
// messages. It exists to answer one question before work starts — *has anyone looked at this
// before, and what did they find?* Step repetition is the single most common multi-agent
// failure, and it happens because the previous attempt is unfindable, not because it was
// never written down.
//
// Two rules keep this from becoming a liability:
//
//  1. THE INDEX IS A CACHE. The files are the truth. `cone reindex` rebuilds it from disk at
//     any time, and deleting the database loses nothing. This is what preserves the property
//     that an agent can participate with `mv` and `cat` alone.
//
//  2. THIS IS NOT A KNOWLEDGE BASE. cone indexes *operational history*: what was asked, what
//     was claimed, what an agent reported. Whatever knowledge system its user already keeps is
//     curated and linked and has a promotion ritual, and a durable learning belongs there. A
//     board that grew a second one would be worse at both jobs and would compete with the real
//     one for the same query.
//
// modernc.org/sqlite is used so CGO stays off and the binary stays a single static artifact.

const indexFile = ".index.db"

type Index struct{ db *sql.DB }

func (b *Board) IndexPath() string { return filepath.Join(b.Root, indexFile) }

func (b *Board) OpenIndex() (*Index, error) {
	db, err := sql.Open("sqlite", b.IndexPath()+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One writer at a time; several agents may run `cone search` concurrently and WAL
	// handles the readers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
			doc_id UNINDEXED, kind UNINDEXED, state UNINDEXED, repo,
			who UNINDEXED, path UNINDEXED, updated UNINDEXED,
			title, body,
			tokenize = 'porter unicode61'
		);`); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

func (i *Index) Close() error { return i.db.Close() }

type Hit struct {
	Kind    string // task | message
	ID      string
	Title   string
	State   string
	Repo    string
	Who     string
	Path    string
	Updated time.Time
	Snippet string
}

// Reindex rebuilds the whole index from disk. Always correct, which beats incremental
// bookkeeping that can silently drift from the files — but it is O(every task on the board),
// so it runs on READ when the board has changed, never on every write. It used to be called
// from every mutation: one `cone note` on a board with 500 completed tasks re-parsed all 500
// files and rebuilt the whole table, and the heartbeat triggered that every minute.
func (b *Board) Reindex() (int, error) {
	idx, err := b.OpenIndex()
	if err != nil {
		return 0, err
	}
	defer idx.Close()

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM docs`); err != nil {
		return 0, err
	}
	ins, err := tx.Prepare(`INSERT INTO docs (doc_id,kind,state,repo,who,path,updated,title,body) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()

	n := 0
	for _, st := range States {
		tasks, err := b.List(st)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			upd := t.Created
			if !t.ClaimedAt.IsZero() {
				upd = t.ClaimedAt
			}
			if !t.Completed.IsZero() {
				upd = t.Completed
			}
			if _, err := ins.Exec(t.ID, "task", string(t.State), t.Repo, t.ClaimedBy,
				t.Path, fmtTime(upd), t.Title, withoutWorkerOutput(t.Body)); err != nil {
				return n, err
			}
			n++
		}
	}
	msgs, err := b.Read(0)
	if err == nil {
		for _, m := range msgs {
			if _, err := ins.Exec(filepath.Base(m.Path), "message", "", "", m.From,
				m.Path, fmtTime(m.Posted), m.Topic, m.Body); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, tx.Commit()
}

// indexIsStale compares the index against the newest thing on disk. A missing index is stale
// by definition; so is one older than any task file, which is what makes deferring the rebuild
// to read time safe — including when another agent, or a human with an editor, changed a file
// without going through this binary at all.
func (b *Board) indexIsStale() bool {
	st, err := os.Stat(b.IndexPath())
	if err != nil {
		return true
	}
	dirs := []string{b.boardDir()}
	for _, s := range States {
		dirs = append(dirs, b.dir(s))
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err == nil && info.ModTime().After(st.ModTime()) {
				return true
			}
		}
	}
	return false
}

// Search runs an FTS5 query. Bare words are ANDed; FTS5 operators (OR, NEAR, "quoted
// phrase", prefix*) all work, which is why the query is passed through rather than escaped
// into uselessness.
func (b *Board) Search(query string, limit int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if b.indexIsStale() {
		if _, err := b.Reindex(); err != nil {
			return nil, err
		}
	}
	idx, err := b.OpenIndex()
	if err != nil {
		return nil, err
	}
	defer idx.Close()

	if limit <= 0 {
		limit = 20
	}
	rows, err := idx.db.Query(`
		SELECT doc_id, kind, state, repo, who, path, updated, title,
		       snippet(docs, 8, '«', '»', ' … ', 14)
		FROM docs WHERE docs MATCH ? ORDER BY rank LIMIT ?`, query, limit)
	if err != nil {
		// An FTS5 syntax error is a user mistake, not a crash — say which.
		return nil, fmt.Errorf("search: %w (check FTS5 syntax; quote phrases)", err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var updated string
		if err := rows.Scan(&h.ID, &h.Kind, &h.State, &h.Repo, &h.Who, &h.Path, &updated, &h.Title, &h.Snippet); err != nil {
			return out, err
		}
		h.Updated = parseTime(updated)
		out = append(out, h)
	}
	return out, rows.Err()
}
