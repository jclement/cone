package install

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jclement/cone/internal/board"
)

// A unit and a log describing a watcher that started `startedAgo` before now, running a binary
// last written `binaryAgo` before now.
func watcherAged(t *testing.T, startedAgo, binaryAgo time.Duration, startLine bool) (unit, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "cone")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-binaryAgo)
	if err := os.Chtimes(exe, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	logPath = filepath.Join(dir, ".watch.log")
	line := ""
	if startLine {
		line = fmt.Sprintf("%s watching /board every 1m0s (cap 4 workers, herdr /x/bin/herdr)\n",
			time.Now().Add(-startedAgo).UTC().Format(time.RFC3339))
	}
	if err := os.WriteFile(logPath, []byte(line+"2026-01-01T00:00:00Z holding: nothing to do\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "<string>" + exe + "</string><string>watch</string>", logPath
}

// The bug this exists for: `brew upgrade` replaces the binary, launchd's KeepAlive only respawns
// a process that DIES, and the watcher goes on running the version you replaced — while doctor
// reports a loaded scheduler and a log being written, both true.
func TestAWatcherOlderThanItsBinaryIsReported(t *testing.T) {
	unit, logPath := watcherAged(t, 48*time.Hour, 1*time.Hour, true)

	got := staleWatcher(unit, logPath)
	if got == nil {
		t.Fatal("a watcher running code replaced an hour ago was not reported")
	}
	if got.Severity != board.Broken {
		t.Errorf("severity = %v, want Broken so `cone doctor` exits non-zero", got.Severity)
	}
	if got.Fix == "" {
		t.Error("reported the problem without naming the command that fixes it")
	}
}

func TestACurrentWatcherIsNotReported(t *testing.T) {
	// Started an hour ago, from a binary written two days ago: exactly right.
	unit, logPath := watcherAged(t, 1*time.Hour, 48*time.Hour, true)

	if got := staleWatcher(unit, logPath); got != nil {
		t.Fatalf("a watcher running the installed binary was called stale: %s", got.Message)
	}
}

// No startup line means the log was rotated or predates the stamp. There is nothing to compare,
// and a guess would be worse than silence — doctor's whole value is that its findings are true.
func TestNoStartupLineMeansNoClaim(t *testing.T) {
	unit, logPath := watcherAged(t, 48*time.Hour, 1*time.Hour, false)

	if got := staleWatcher(unit, logPath); got != nil {
		t.Fatalf("claimed staleness with no start time to compare against: %s", got.Message)
	}
}

// A restart is what fixes this, so the LAST startup line is the one that counts — otherwise the
// warning would persist forever after being acted on.
func TestTheMostRecentRestartIsTheOneThatCounts(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "cone")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	mtime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(exe, mtime, mtime)

	logPath := filepath.Join(dir, ".watch.log")
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	os.WriteFile(logPath, []byte(
		old+" watching /board every 1m0s\n"+
			old+" holding: nothing to do\n"+
			recent+" watching /board every 1m0s\n"), 0o644)

	unit := "<string>" + exe + "</string>"
	if got := staleWatcher(unit, logPath); got != nil {
		t.Fatalf("still reported stale after a restart picked up the new binary: %s", got.Message)
	}
}
