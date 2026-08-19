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
