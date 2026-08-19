package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/cone/internal/board"
)

// The unit that shipped named no herdr at all, so launchd resolved a bare "herdr" against
// PATH=/usr/bin:/bin:/usr/sbin:/sbin and never found it. Doctor has to catch that, because
// launchctl reports the unit as loaded and exits 0 either way.
func TestPathsInFindsTheBinariesAUnitRuns(t *testing.T) {
	unit := `<key>ProgramArguments</key><array>
    <string>/opt/homebrew/bin/cone</string><string>watch</string>
    <string>--herdr</string><string>/opt/homebrew/bin/herdr</string>
  </array>
  <key>StandardOutPath</key><string>/Users/x/cone/.watch.log</string>`

	got := pathsIn(unit)
	want := map[string]bool{"/opt/homebrew/bin/cone": true, "/opt/homebrew/bin/herdr": true}
	if len(got) != len(want) {
		t.Fatalf("pathsIn = %v, want exactly the two bin paths", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("pathsIn returned %q; the log path and board root are created on demand and must not be checked", p)
		}
	}
}

// Zero bytes is the signature of the bug: loaded, retried every 30 seconds, silent throughout.
// "Loaded" is not "working".
func TestAnEmptyWatchLogIsReportedAsNeverHavingRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // this machine's real unit must not decide the outcome
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".watch.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range Doctor(root) {
		if f.Severity == board.Broken && strings.Contains(f.Message, "never run") {
			found = true
		}
	}
	if !found {
		t.Fatal("an empty watch log was not reported as a heartbeat that has never run")
	}
}

// With no scheduler installed at all — a CI runner, a fresh checkout — doctor must still get
// as far as the board's own evidence. An early return here made it silent on the more useful
// half of the check.
func TestDoctorStillChecksTheLogWithNoSchedulerInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no unit anywhere: exactly the CI case that caught this
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".watch.log"), []byte("2026-08-19T16:00:00Z watching\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var areas []string
	for _, f := range Doctor(root) {
		areas = append(areas, f.Area)
	}
	if !strings.Contains(strings.Join(areas, " "), "heartbeat") {
		t.Fatalf("doctor stopped at the scheduler and never looked at the log (areas: %v)", areas)
	}
}
