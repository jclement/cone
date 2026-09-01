package human

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// A broken human.json makes `cone ask` fail and the sweep silently skip — which from the
// outside looks exactly like a human who never answers. Doctor is where that becomes a line
// of output instead of a mystery.
func TestDoctorReportsABrokenHumanConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "human.json")
	os.WriteFile(p, []byte(`{`), 0o644)
	t.Setenv("CONE_HUMAN", p)
	b, _ := board.Open(t.TempDir())

	findings := Doctor(b)
	if len(findings) != 1 || findings[0].Severity != board.Broken {
		t.Fatalf("a broken config produced %+v", findings)
	}
}

// No service is the normal state on most hosts and must say nothing — unless the board holds
// questions that nothing can ever check, which is a task waiting on an answer that cannot come.
func TestDoctorIsSilentWithNoServiceUnlessQuestionsAreStranded(t *testing.T) {
	t.Setenv("CONE_HUMAN", filepath.Join(t.TempDir(), "absent.json"))
	b, _ := board.Open(t.TempDir())

	if findings := Doctor(b); len(findings) != 0 {
		t.Fatalf("an unconfigured healthy host produced %+v", findings)
	}

	task, _ := b.New(board.Task{Title: "asked, then the config was deleted"})
	b.Promote(task.ID)
	b.Block(task.ID, "")
	b.Set(task.ID, "question", "q_1")

	findings := Doctor(b)
	if len(findings) != 1 || findings[0].Severity != board.Warn ||
		!strings.Contains(findings[0].Message, task.ID) {
		t.Fatalf("a stranded question produced %+v", findings)
	}
}

// A question the sweep cannot fetch is a task that can never come back; doctor runs the same
// no-wait GET the sweep does, so it reports precisely what the sweep is experiencing.
func TestDoctorSurfacesAQuestionThatCannotBeChecked(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "human.json")
	// A port nothing listens on: the same shape as a dead or moved service.
	os.WriteFile(cfg, []byte(`{"name":"svc","url":"http://127.0.0.1:1","token":"t"}`), 0o644)
	t.Setenv("CONE_HUMAN", cfg)
	b, _ := board.Open(t.TempDir())
	task, _ := b.New(board.Task{Title: "waiting on a dead service"})
	b.Promote(task.ID)
	b.Block(task.ID, "")
	b.Set(task.ID, "question", "q_2")

	var warned bool
	for _, f := range Doctor(b) {
		if f.Severity == board.Warn && strings.Contains(f.Message, task.ID) {
			warned = true
		}
	}
	if !warned {
		t.Fatal("an uncheckable question produced no finding — the sweep is failing on it in silence")
	}
}
