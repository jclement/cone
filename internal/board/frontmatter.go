package board

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A task file is YAML frontmatter followed by markdown. Both halves matter: the frontmatter
// is what tooling reads, the body is what a human (and the claiming agent) reads.
//
// The parser is hand-rolled rather than using a YAML library, for one reason: these files are
// edited by hand and by agents with sed, and a strict parser turns a trivial formatting slip
// into "the board is broken". Unknown keys are preserved, malformed values are skipped, and a
// file that cannot be parsed at all is skipped by List rather than hiding every other task.

const fence = "---"

func Unmarshal(raw string) (*Task, error) {
	t := &Task{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	if !sc.Scan() || strings.TrimSpace(sc.Text()) != fence {
		return nil, fmt.Errorf("missing frontmatter fence")
	}

	extra := map[string]string{}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == fence {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "id":
			t.ID = v
		case "title":
			t.Title = unquote(v)
		case "repo":
			t.Repo = v
		case "kind":
			t.Kind = v
		case "priority":
			t.Priority = v
		case "auto":
			t.Auto, _ = strconv.ParseBool(v)
		case "source":
			t.Source = v
		case "source_ref":
			t.SourceRef = v
		case "created":
			t.Created = parseTime(v)
		case "claimed_by":
			t.ClaimedBy = v
		case "claimed_at":
			t.ClaimedAt = parseTime(v)
		case "worktree":
			t.Worktree = v
		case "completed":
			t.Completed = parseTime(v)
		default:
			if k != "" {
				extra[k] = v
			}
		}
	}

	var body strings.Builder
	for sc.Scan() {
		body.WriteString(sc.Text())
		body.WriteByte('\n')
	}
	t.Body = strings.TrimLeft(body.String(), "\n")
	return t, sc.Err()
}

func (t *Task) Marshal() string {
	var b strings.Builder
	b.WriteString(fence + "\n")
	kv := func(k, v string) { fmt.Fprintf(&b, "%s: %s\n", k, v) }

	kv("id", t.ID)
	kv("title", quote(t.Title))
	kv("repo", t.Repo)
	kv("kind", t.Kind)
	kv("priority", t.Priority)
	kv("auto", strconv.FormatBool(t.Auto))
	if t.Source != "" {
		kv("source", t.Source)
		kv("source_ref", t.SourceRef)
	}
	kv("created", fmtTime(t.Created))
	kv("claimed_by", t.ClaimedBy)
	kv("claimed_at", fmtTime(t.ClaimedAt))
	kv("worktree", t.Worktree)
	if !t.Completed.IsZero() {
		kv("completed", fmtTime(t.Completed))
	}
	b.WriteString(fence + "\n\n")
	b.WriteString(strings.TrimLeft(t.Body, "\n"))
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// A title containing a colon would break naive `key: value` readers — including the sed one
// an agent will inevitably reach for — so quote defensively rather than cleverly.
func quote(s string) string {
	if strings.ContainsAny(s, `:#"'`) {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
