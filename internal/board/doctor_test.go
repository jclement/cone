package board

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func findings(f []Finding, sev Severity) []string {
	var out []string
	for _, x := range f {
		if x.Severity == sev {
			out = append(out, x.Message)
		}
	}
	return out
}

func hasFinding(f []Finding, sev Severity, substr string) bool {
	for _, m := range findings(f, sev) {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// List skips a file it cannot parse, so a broken task does not merely look wrong — it does not
// exist. That is invisible exactly when someone is asking why their task never came up.
func TestDoctorSeesAFileThatDoesNotParse(t *testing.T) {
	b := tmpBoard(t)
	if err := os.WriteFile(filepath.Join(b.dir(Ready), "20260819-broken.md"),
		[]byte("no frontmatter here, just prose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if list, _ := b.List(Ready); len(list) != 0 {
		t.Fatal("precondition: List should have skipped it")
	}
	if !hasFinding(b.Doctor(""), Broken, "does not parse") {
		t.Fatal("doctor did not report an unparseable task")
	}
}

// The same id in two states makes Find return whichever directory it looks in first, so the
// board silently disagrees with itself about what that id means.
func TestDoctorSeesTheSameIDInTwoStates(t *testing.T) {
	b := tmpBoard(t)
	task, err := b.New(Task{Title: "one thing"})
	if err != nil {
		t.Fatal(err)
	}
	// Hand-place a second copy, the way an interrupted move or a stray cp would.
	data, _ := os.ReadFile(task.Path)
	if err := os.WriteFile(filepath.Join(b.dir(Done), task.ID+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasFinding(b.Doctor(""), Broken, "exists in both") {
		t.Fatalf("doctor did not report the duplicate:\n%v", findings(b.Doctor(""), Broken))
	}
}

// A claim with no claimant occupies a worker slot that nothing can ever release: the reaper
// matches on the claimant's name, and there isn't one.
func TestDoctorSeesAClaimWithNoClaimant(t *testing.T) {
	b := tmpBoard(t)
	task, err := b.New(Task{Title: "one thing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(task.ID, "someone"); err != nil {
		t.Fatal(err)
	}
	held, _ := b.Find(task.ID)
	held.ClaimedBy = ""
	if err := b.Save(held); err != nil {
		t.Fatal(err)
	}
	if !hasFinding(b.Doctor(""), Broken, "no claimed_by") {
		t.Fatal("doctor did not report a claim nothing can release")
	}
}

func TestDoctorIsQuietOnAHealthyBoard(t *testing.T) {
	b := tmpBoard(t)
	if _, err := b.New(Task{Title: "one thing"}); err != nil {
		t.Fatal(err)
	}
	if got := findings(b.Doctor(""), Broken); len(got) != 0 {
		t.Fatalf("a healthy board reported problems: %v", got)
	}
}
