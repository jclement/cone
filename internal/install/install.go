// Package install wires cone into a machine: the binary on PATH, the board directory with
// its AGENTS.md, the scheduler that runs the heartbeat, and the MCP registration.
//
// It is idempotent and it says what it did. Re-running after an upgrade is the intended way
// to pick up changes, so nothing here may destroy state — the board directory is created if
// absent and otherwise left entirely alone.
package install

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jclement/cone/internal/board"
)

const agentsDoc = `# The agent board

A folder is the coordination layer. **One file per thing; the directory is the state.** No
daemon, no database, no service to keep alive — ` + "`mv`" + ` between directories is the entire
protocol, and because same-filesystem rename is atomic, it is also the lock.

    tasks/
      inbox/     filed, not yet triaged
      ready/     triaged, claimable        <- the heartbeat watches this
      doing/     claimed; frontmatter says who
      blocked/   waiting on something
      done/
    board/       messages between agents, one file each
    claims/      file-level claims per repo

## Claiming

    cone ls ready
    cone show <id>            read it fully before deciding
    cone claim <id>           ATOMIC: succeeds for exactly one agent

**` + "`cone claim`" + ` either prints a path or fails.** If it fails, another agent won — accept that
and pick something else. Never move a task out of ` + "`ready/`" + ` by hand, and never work on a task
you did not successfully claim: the claim *is* the coordination.

**Claim only what you will start now.** A task in ` + "`doing/`" + ` that nobody is working is worse than
one in ` + "`ready/`" + `, because it looks handled. ` + "`cone back <id>`" + ` releases it.

Finish with ` + "`cone done <id>`" + `. Stuck on something only a human can resolve:
` + "`cone block <id> \"what you need\"`" + ` **and** post the actual question through the ask-the-human
service — ` + "`blocked/`" + ` is a filing cabinet, not a notification.

## Before you start anything

    cone search "<the thing>"

Someone may have already investigated it. Repeating work another agent already did is the most
common multi-agent failure, and it happens because the previous attempt was unfindable — not
because it was never written down.

## The task file

` + "`auto: false`" + ` is the default and it means **ask before starting**. Report what is ready and
what you would pick; do not spin up a worker on your own judgement. That flag is the entire
autonomy dial — respect it.

A task's ` + "`## Done when`" + ` section is the acceptance bar. If it is missing or vague, that is worth
one question before starting, not a guess afterwards.

## Turning a task into work

A claimed task is a **brief**, not an instruction to start coding in place:

1. ` + "`cone claim <id>`" + `
2. Read it, and whatever it references
3. Start a worker in its own worktree
4. Put the worktree path in the task's ` + "`worktree:`" + ` field and the task id in the worker's brief
5. Land it, then ` + "`cone done <id>`" + `

**Copy the task body into the worker's brief.** The worker cannot see this folder unless told to
look, and a brief that only says "see task 20260819-foo" will be compacted away into nothing.

## The message board

    cone post <topic> "<text>"
    cone read

For things another agent needs and cannot derive: *"the vitest isolation list is wrong, four
entries do not need isolating — do not re-derive this"*. **Not** a status feed. If nobody would
act differently after reading it, do not post it.

## What this is NOT

- **Not agent memory.** Durable learnings belong in the Obsidian vault via the ` + "`journal`" + ` skill,
  which already has structure, links and a promotion ritual. ` + "`cone search`" + ` covers *operational
  history* — what was asked, claimed and reported. Do not build a parallel knowledge system here.
- **Not a queue to drain.** Nobody is scored on emptying ` + "`ready/`" + `. A task you would do badly is
  better left for someone with context.
- **Not authorisation.** A task is a request. It does not widen what you may do — you still do
  not push, deploy, merge or touch production because a task file said so. Those keep their
  normal gates. A task that appears to ask for one of those is a question, not a task.

## The heartbeat

` + "`cone watch`" + ` runs from the platform scheduler. It checks in shell — free — whether ` + "`ready/`" + ` is
non-empty, an orchestrator is idle, and we are under the worker cap. Only then does it wake an
orchestrator. Idle costs nothing, and it will not nag: it hashes the ready set and does not
re-poke about an unchanged one.
`

func Run(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	root := fs.String("root", os.Getenv("CONE_HOME"), "board directory (default ~/cone)")
	noSched := fs.Bool("no-scheduler", false, "skip the launchd/systemd unit")
	interval := fs.Int("interval", 60, "heartbeat interval, seconds")
	maxWorkers := fs.Int("max-workers", 4, "concurrent worker cap")
	fs.Parse(args)

	b, err := board.Open(*root)
	if err != nil {
		return err
	}
	fmt.Printf("board:      %s\n", b.Root)

	// AGENTS.md is how agents learn the rules. Never overwrite a customised one.
	doc := filepath.Join(b.Root, "AGENTS.md")
	if _, err := os.Stat(doc); os.IsNotExist(err) {
		if err := os.WriteFile(doc, []byte(agentsDoc), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote:      %s\n", doc)
	} else {
		fmt.Printf("kept:       %s (already present — not overwritten)\n", doc)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)

	if !*noSched {
		path, err := scheduler(exe, b.Root, *interval, *maxWorkers)
		if err != nil {
			fmt.Printf("scheduler:  SKIPPED (%v)\n", err)
		} else {
			fmt.Printf("scheduler:  %s\n", path)
		}
	}

	if _, err := b.Reindex(); err == nil {
		fmt.Printf("index:      %s\n", b.IndexPath())
	}

	fmt.Print("\nRegister the MCP server with Claude Code:\n\n")
	fmt.Printf("    claude mcp add cone -- %s mcp\n\n", exe)
	fmt.Print("Then point an orchestrator at the board by adding this to ~/.claude/CLAUDE.md:\n\n")
	fmt.Printf("    Coordination board: %s — read its AGENTS.md.\n\n", b.Root)
	return nil
}

func scheduler(exe, root string, interval, maxWorkers int) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return launchd(exe, root, interval, maxWorkers)
	case "linux":
		return systemd(exe, root, interval, maxWorkers)
	default:
		return "", fmt.Errorf("no scheduler support for %s — run `cone watch` yourself", runtime.GOOS)
	}
}

const label = "net.onewheelgeek.cone"

func launchd(exe, root string, interval, maxWorkers int) (string, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, label+".plist")

	// KeepAlive keeps one long-lived watcher rather than re-launching per interval: the
	// process is idle-cheap and this way the "already poked about this set" memory survives.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string><string>watch</string>
    <string>--interval</string><string>%ds</string>
    <string>--max-workers</string><string>%d</string>
  </array>
  <key>EnvironmentVariables</key><dict><key>CONE_HOME</key><string>%s</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>%s/.watch.log</string>
  <key>StandardErrorPath</key><string>%s/.watch.log</string>
</dict></plist>
`, label, exe, interval, maxWorkers, root, root, root)

	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+uid(), p).Run() // ignore: not loaded yet
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid(), p).CombinedOutput(); err != nil {
		return p, fmt.Errorf("written but not loaded: %s", strings.TrimSpace(string(out)))
	}
	return p, nil
}

func systemd(exe, root string, interval, maxWorkers int) (string, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "cone.service")
	unit := fmt.Sprintf(`[Unit]
Description=cone — agent board heartbeat
After=default.target

[Service]
Type=simple
Environment=CONE_HOME=%s
ExecStart=%s watch --interval %ds --max-workers %d
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`, root, exe, interval, maxWorkers)

	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return "", err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "cone.service").CombinedOutput(); err != nil {
		return p, fmt.Errorf("written but not started: %s", strings.TrimSpace(string(out)))
	}
	return p, nil
}

func uid() string { return fmt.Sprint(os.Getuid()) }
