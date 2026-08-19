package inbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// These endpoints hand a task to exactly one caller and there is no release call, so the claim
// is destructive: once fetched, the request is gone from the human's phone. If filing then
// fails, the request exists nowhere at all — not upstream, not on the board, and the only
// trace was a string appended to an error slice.
func TestATaskClaimedUpstreamIsNeverLostWhenFilingFails(t *testing.T) {
	b, err := board.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Occupy the id the incoming task will derive, so New must fail.
	if _, err := b.New(board.Task{ID: "taken", Title: "already here"}); err != nil {
		t.Fatal(err)
	}

	served := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		served = true
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
			"id": "mp-42", "title": "check the nightly backup", "body": "it did not run",
		}})
	}))
	defer srv.Close()

	src := &HTTPSource{SourceName: "meatprompt", BaseURL: srv.URL, Token: "t", ClaimPath: "/claim"}
	// Force the collision: the task arrives with an id that already exists.
	filed, err := Sync(context.Background(), b, []Source{fixedID{src, "taken"}})
	if err == nil {
		t.Fatal("a failure to file was reported as a clean sync")
	}
	if len(filed) != 0 {
		t.Fatalf("filed %d tasks, want 0", len(filed))
	}

	entries, rerr := os.ReadDir(QuarantineDir(b))
	if rerr != nil || len(entries) != 1 {
		t.Fatalf("the request was not quarantined (%v); it is now gone from both systems", rerr)
	}
	data, _ := os.ReadFile(filepath.Join(QuarantineDir(b), entries[0].Name()))
	if !contains(string(data), "check the nightly backup") {
		t.Fatalf("the quarantined file does not contain the request:\n%s", data)
	}
}

// fixedID makes every task arrive with the same board id, which is the collision case.
type fixedID struct {
	Source
	id string
}

func (f fixedID) Fetch(ctx context.Context) ([]board.Task, error) {
	tasks, err := f.Source.Fetch(ctx)
	for i := range tasks {
		tasks[i].ID = f.id
	}
	return tasks, err
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// cone compiled in two named services — their URLs, their claim paths, the layout of their
// credential files — which made a path change upstream into a cone release. Sources are
// configuration; the code knows about none of them.
func TestSourcesComeFromConfigurationNotFromCompiledInServices(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "env")
	os.WriteFile(tokenFile, []byte("# a comment\nSOME_SERVICE_TOKEN=\"sk_live_abc\"\n"), 0o600)

	cfg := filepath.Join(dir, "inboxes.json")
	os.WriteFile(cfg, []byte(`[{
	  "name": "phone-queue",
	  "url": "https://queue.example.com",
	  "claim_path": "/api/v1/tasks/claim",
	  "token_file": "`+tokenFile+`",
	  "token_key": "SOME_SERVICE_TOKEN"
	}]`), 0o644)
	t.Setenv("CONE_INBOXES", cfg)

	got, err := Configured()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sources, want 1", len(got))
	}
	h, ok := got[0].(*HTTPSource)
	if !ok {
		t.Fatalf("got %T", got[0])
	}
	if h.SourceName != "phone-queue" || h.BaseURL != "https://queue.example.com" ||
		h.ClaimPath != "/api/v1/tasks/claim" || h.Token != "sk_live_abc" {
		t.Fatalf("source did not come through as declared: %+v", h)
	}
}

// Most hosts have no inbox at all, and that must cost nothing and say nothing.
func TestNoConfigFileIsNotAnError(t *testing.T) {
	t.Setenv("CONE_INBOXES", filepath.Join(t.TempDir(), "absent.json"))
	got, err := Configured()
	if err != nil || len(got) != 0 {
		t.Fatalf("a host with no inboxes got (%v, %v)", got, err)
	}
}

// A file that exists and is wrong must not silently sync nothing — that is indistinguishable
// from a queue that happens to be empty, which is how a broken integration goes unnoticed.
func TestABrokenConfigIsReportedNotIgnored(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"not json", `{`},
		{"no url", `[{"name":"x","claim_path":"/c","token":"t"}]`},
		{"no claim path", `[{"name":"x","url":"https://e.example","token":"t"}]`},
		{"no token", `[{"name":"x","url":"https://e.example","claim_path":"/c"}]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "inboxes.json")
			os.WriteFile(p, []byte(c.body), 0o644)
			t.Setenv("CONE_INBOXES", p)
			if _, err := Configured(); err == nil {
				t.Fatal("a broken inbox config was accepted, and this host now syncs nothing in silence")
			}
		})
	}
}
