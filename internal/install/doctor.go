package install

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/jclement/cone/internal/board"
)

// Doctor checks the half of the system that lives outside the board: the scheduler unit, the
// binaries it names, and whether the watcher has ever actually said anything.
//
// This exists because of one bug. The unit was installed, launchctl listed it as loaded and
// exit code 0, and it had never once done its job: launchd's PATH does not include Homebrew,
// so `herdr` did not resolve, and the failure was logged only under a flag the unit did not
// pass. .watch.log was zero bytes for weeks and nobody looked, because nothing suggested
// looking. "Loaded" is not "working", so this checks the paths and the log — and, since an
// upgrade leaves the old process running, whether the code executing is the code installed.
func Doctor(root string) []board.Finding {
	var f []board.Finding

	// Each stage records what it found and moves on. An early return here meant that on a
	// machine with no scheduler installed — a CI runner, a fresh checkout — doctor never got
	// as far as looking at the board's own evidence, which is the more useful half.
	unit, kind := unitPath()
	data, err := readUnit(unit)
	switch {
	case unit == "":
		f = append(f, board.Finding{Severity: board.Warn, Area: "scheduler",
			Message: fmt.Sprintf("no scheduler support on %s — run `cone watch` yourself", runtime.GOOS)})
	case err != nil:
		f = append(f, board.Finding{Severity: board.Warn, Area: "scheduler",
			Message: "the heartbeat is not installed — nothing wakes an orchestrator when work arrives",
			Fix:     "cone install"})
	default:
		f = append(f, board.Finding{Severity: board.OK, Area: "scheduler", Message: kind + ": " + unit})
	}

	// Every absolute path the unit names must exist. A path baked in at install time and
	// deleted by an upgrade is the exact failure mode here: brew upgrade removes the Cellar
	// directory a resolved symlink pointed into, and the scheduler retries it forever.
	for _, p := range pathsIn(data) {
		if _, err := os.Stat(p); err != nil {
			f = append(f, board.Finding{Severity: board.Broken, Area: "scheduler",
				Message: fmt.Sprintf("the unit runs %s, which does not exist — the heartbeat cannot work", p),
				Fix:     "cone install"})
		}
	}
	if data != "" && !strings.Contains(data, "herdr") {
		f = append(f, board.Finding{Severity: board.Broken, Area: "scheduler",
			Message: "the unit names no herdr binary, so a bare `herdr` is resolved against launchd's PATH — which has no Homebrew in it",
			Fix:     "cone install"})
	}

	// Tasks claimed from a human's queue that could not be filed exist nowhere else.
	if entries, err := os.ReadDir(filepath.Join(root, "inbox-quarantine")); err == nil && len(entries) > 0 {
		f = append(f, board.Finding{Severity: board.Broken, Area: "inbox",
			Message: fmt.Sprintf("%d task(s) in inbox-quarantine/ — claimed from an inbox, never filed, and that queue no longer has them", len(entries)),
			Fix:     "read them and move each into tasks/inbox/"})
	}

	// The log is the only evidence the thing runs. Zero bytes is the signature of the bug
	// above: loaded, retried every 30 seconds, silent the whole time.
	logPath := filepath.Join(root, ".watch.log")
	st, err := os.Stat(logPath)
	switch {
	case err != nil:
		f = append(f, board.Finding{Severity: board.Warn, Area: "heartbeat",
			Message: "no " + logPath + " — the watcher has not started since it was installed", Fix: "cone install"})
	case st.Size() == 0:
		f = append(f, board.Finding{Severity: board.Broken, Area: "heartbeat",
			Message: logPath + " is empty: the watcher has never logged anything, which means it has never run",
			Fix:     "cone install"})
	default:
		age := time.Since(st.ModTime()).Round(time.Minute)
		sev := board.OK
		msg := fmt.Sprintf("last wrote %s ago: %s", age, lastLine(logPath))
		if age > 6*time.Hour {
			sev, msg = board.Warn, fmt.Sprintf("has not written for %s: %s", age, lastLine(logPath))
		}
		f = append(f, board.Finding{Severity: sev, Area: "heartbeat", Message: msg})
	}
	if stale := staleWatcher(data, logPath); stale != nil {
		f = append(f, *stale)
	}
	return f
}

// staleWatcher catches the heartbeat that is running code you have already replaced.
//
// launchd's KeepAlive respawns a process that DIES; it does not notice one whose binary changed
// underneath it. So `brew upgrade cone` swaps the binary on disk and the running watcher goes on
// executing whatever it was started with, for as long as the machine stays up. Seen for real:
// a watcher two days old serving the version a fix had just been shipped to replace, logging the
// symptom of that exact bug every interval while doctor called the board healthy.
//
// It is the same lesson a third time. "Loaded" is not "working", and now **running is not
// current** — the one an upgrade silently creates, which makes it doctor's business.
//
// The process start time comes from the log rather than from ps: the watcher stamps a line when
// it starts, that line is already there on every platform, and it needs no pid hunting and no
// per-OS process table. No startup line means no comparison, and no comparison means say
// nothing — a guess here would be worse than silence.
func staleWatcher(unit, logPath string) *board.Finding {
	exe := watcherBinary(unit)
	if exe == "" {
		return nil
	}
	bin, err := os.Stat(exe)
	if err != nil {
		return nil // the missing-path check above already reports this, and better
	}
	started, ok := lastStart(logPath)
	if !ok || !bin.ModTime().After(started) {
		return nil
	}
	return &board.Finding{
		Severity: board.Broken, Area: "heartbeat",
		Message: fmt.Sprintf(
			"running since %s, but %s was replaced %s later — the heartbeat is still executing the version you upgraded from",
			started.Format(time.RFC3339), exe, bin.ModTime().Sub(started).Round(time.Minute)),
		Fix: restartHint(),
	}
}

// watcherBinary is the program the unit runs. It is the first absolute path in the file by
// construction: launchd's ProgramArguments and systemd's ExecStart both name the executable
// before any of its arguments.
func watcherBinary(unit string) string {
	paths := pathsIn(unit)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// lastStart is when the running watcher started, read from the line it stamps on startup.
func lastStart(logPath string) (time.Time, bool) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return time.Time{}, false
	}
	var out time.Time
	var ok bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "watching" {
			continue
		}
		t, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		out, ok = t, true // the LAST one: the watcher may have been restarted since
	}
	return out, ok
}

func restartHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "launchctl kickstart -k gui/$(id -u)/" + label
	case "linux":
		return "systemctl --user restart cone"
	}
	return "restart the watcher"
}

type Finding = board.Finding

// readUnit returns the unit file's contents, or "" when there is no unit to read.
func readUnit(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func unitPath() (path, kind string) {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), "launchd"
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "cone.service"), "systemd"
	}
	return "", ""
}

var absPathRe = regexp.MustCompile(`(/[\w.@-]+)+`)

// pathsIn pulls the absolute paths a unit executes out of it. Only paths under a bin
// directory are checked — the board root and the log path are created on demand.
func pathsIn(unit string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range absPathRe.FindAllString(unit, -1) {
		if !strings.Contains(m, "/bin/") || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func lastLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines[len(lines)-1]
}
