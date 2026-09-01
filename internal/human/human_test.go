package human

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// configure points the package at a config file naming srv as the human service.
func configure(t *testing.T, srv *httptest.Server, extra string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "human.json")
	body := `{"name":"meatprompt","url":"` + srv.URL + `","token":"sk_test"` + extra + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONE_HUMAN", cfg)
}

// The wire contract is fixed — meatprompt already satisfies it verbatim — so what goes over
// the wire is pinned here: paths, bearer token, and the exact field names.
func TestAskSpeaksTheContractVerbatim(t *testing.T) {
	var got map[string]any
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
			"id": "q_9", "url": "http://" + r.Host + "/q/q_9", "expires_at": "2026-09-02T00:00:00Z",
		}})
	}))
	defer srv.Close()
	configure(t, srv, "")

	svc, err := Configured()
	if err != nil || svc == nil {
		t.Fatal(err)
	}
	asked, err := svc.Ask(context.Background(), Question{
		Title: "which way?", BodyMD: "the diff", Kind: KindChoice,
		Options:   []Option{{Key: "a", Label: "plan A", Detail: "slower"}, {Key: "b", Label: "plan B"}},
		Recommend: "b", Urgency: "normal",
		Context: map[string]string{"task": "t-1", "board": "/tmp/board"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked.ID != "q_9" {
		t.Fatalf("asked = %+v", asked)
	}
	if path != "/api/v1/questions" {
		t.Fatalf("posted to %q, want the default ask_path", path)
	}
	if auth != "Bearer sk_test" {
		t.Fatalf("auth = %q", auth)
	}
	for _, k := range []string{"title", "body_md", "kind", "options", "recommend", "context", "urgency"} {
		if _, ok := got[k]; !ok {
			t.Errorf("the posted body is missing %q: %v", k, got)
		}
	}
	opts := got["options"].([]any)[0].(map[string]any)
	if opts["key"] != "a" || opts["label"] != "plan A" || opts["detail"] != "slower" {
		t.Fatalf("option shape is off the contract: %v", opts)
	}
}

func TestQuestionSubstitutesTheIDAndLongPolls(t *testing.T) {
	var path, wait string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, wait = r.URL.Path, r.URL.Query().Get("wait")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "answered", "answer_value": "b", "answer_note": "but carefully",
		})
	}))
	defer srv.Close()
	configure(t, srv, "")

	svc, _ := Configured()
	ans, err := svc.Question(context.Background(), "q_9", maxWait)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/questions/q_9" || wait != "90" {
		t.Fatalf("GET %s?wait=%s — the {id} substitution or the long-poll is off", path, wait)
	}
	if ans.Status != StatusAnswered || ans.Value != "b" || ans.Note != "but carefully" {
		t.Fatalf("answer = %+v", ans)
	}
}

// Most hosts have no human service, and that must cost nothing and say nothing.
func TestNoConfigFileMeansNoServiceNotAnError(t *testing.T) {
	t.Setenv("CONE_HUMAN", filepath.Join(t.TempDir(), "absent.json"))
	svc, err := Configured()
	if err != nil || svc != nil {
		t.Fatalf("a host with no human service got (%v, %v)", svc, err)
	}
}

// A file that exists and is wrong must be an error: an agent whose questions silently go
// nowhere is indistinguishable from a human who never answers.
func TestABrokenConfigIsReportedNotIgnored(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"not json", `{`},
		{"no url", `{"name":"x","token":"t"}`},
		{"no name", `{"url":"https://e.example","token":"t"}`},
		{"no token", `{"name":"x","url":"https://e.example"}`},
		{"question_path without id", `{"name":"x","url":"https://e.example","token":"t","question_path":"/api/q"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "human.json")
			os.WriteFile(p, []byte(c.body), 0o644)
			t.Setenv("CONE_HUMAN", p)
			if _, err := Configured(); err == nil {
				t.Fatal("a broken human config was accepted; questions on this host now go nowhere in silence")
			}
		})
	}
}

// The contract requires a recommendation wherever the human is being handed a decision; the
// local check exists so the error names the flag rather than arriving as a server 4xx.
func TestDecisionKindsRequireARecommendation(t *testing.T) {
	base := Question{Title: "t", BodyMD: "b", Urgency: "normal"}
	for _, kind := range []string{KindConfirm, KindChoice} {
		q := base
		q.Kind = kind
		if err := q.Validate(); err == nil || !strings.Contains(err.Error(), "--recommend") {
			t.Fatalf("kind %s without a recommendation validated (err=%v)", kind, err)
		}
	}
	q := base
	q.Kind = KindChoice
	q.Options = []Option{{Key: "a", Label: "A"}}
	q.Recommend = "z"
	if err := q.Validate(); err == nil {
		t.Fatal("a recommendation naming no option validated")
	}
	q.Recommend = "a"
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	q = base
	q.Kind = KindManual
	if err := q.Validate(); err != nil {
		t.Fatalf("manual needs no recommendation, got %v", err)
	}
}

// The convergent end state both `cone ask --wait` and the watch sweep produce: answer noted
// verbatim, task back in ready, question key kept.
func TestAnsweredReturnsTheTaskToReadyWithTheAnswerNoted(t *testing.T) {
	b, err := board.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task, _ := b.New(board.Task{Title: "needs a decision"})
	b.Promote(task.ID)
	b.Block(task.ID, "asked the human")
	b.Set(task.ID, "question", "q_5")

	got, err := Answered(b, task.ID, "q_5", &Answer{Status: StatusAnswered, Value: "option b", Note: "watch the index"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != board.Ready {
		t.Fatalf("state = %s, want ready", got.State)
	}
	fresh, _ := b.Find(task.ID)
	if !strings.Contains(fresh.Body, "option b") || !strings.Contains(fresh.Body, "watch the index") {
		t.Fatalf("answer not noted verbatim:\n%s", fresh.Body)
	}
	if fresh.Question != "q_5" {
		t.Fatalf("question key lost: %q", fresh.Question)
	}
	if !Applied(fresh) {
		t.Fatal("a delivered answer must read as applied, or the sweep delivers it forever")
	}
}
