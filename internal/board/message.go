package board

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Message is a note left for other agents. Append-only, one file each — the same
// one-file-per-thing rule as tasks, so the same "any agent can participate with mv and cat"
// property holds.
//
// This is deliberately not a chat. There are no threads, no replies and no read receipts,
// because the useful content is "here is something you would otherwise have to rediscover",
// not conversation. Anything needing a reply is a task or a question for the human.
type Message struct {
	From   string
	Topic  string
	Posted time.Time
	Body   string
	Path   string
}

func (b *Board) boardDir() string { return filepath.Join(b.Root, "board") }

func (b *Board) Post(from, topic, body string) (*Message, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("a message needs a body")
	}
	if from == "" {
		from = "unknown"
	}
	m := &Message{From: from, Topic: topic, Posted: time.Now().UTC(), Body: body}
	name := fmt.Sprintf("%s-%s.md", m.Posted.Format("20060102T150405Z"), slugRe.ReplaceAllString(strings.ToLower(topic), "-"))
	m.Path = filepath.Join(b.boardDir(), strings.Trim(name, "-"))

	content := fmt.Sprintf("%s\nfrom: %s\ntopic: %s\nposted: %s\n%s\n\n%s\n",
		fence, m.From, m.Topic, fmtTime(m.Posted), fence, strings.TrimSpace(m.Body))
	return m, os.WriteFile(m.Path, []byte(content), 0o644)
}

// Read returns the n most recent messages, newest first.
func (b *Board) Read(n int) ([]*Message, error) {
	entries, err := os.ReadDir(b.boardDir())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	// Filenames lead with a UTC timestamp, so lexical order is chronological.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if n > 0 && len(names) > n {
		names = names[:n]
	}

	var out []*Message
	for _, name := range names {
		p := filepath.Join(b.boardDir(), name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, parseMessage(string(data), p))
	}
	return out, nil
}

func parseMessage(raw, path string) *Message {
	m := &Message{Path: path}
	parts := strings.SplitN(raw, fence, 3)
	if len(parts) < 3 {
		m.Body = strings.TrimSpace(raw)
		return m
	}
	for _, line := range strings.Split(parts[1], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "from":
			m.From = strings.TrimSpace(v)
		case "topic":
			m.Topic = strings.TrimSpace(v)
		case "posted":
			m.Posted = parseTime(strings.TrimSpace(v))
		}
	}
	m.Body = strings.TrimSpace(parts[2])
	return m
}
