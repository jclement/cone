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
// looking. "Loaded" is not "working", so this checks the paths and the log.
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
	return f
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
