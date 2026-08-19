# cone

**A coordination board for coding agents. A folder is the protocol.**

> *The Cone of Silence never actually worked either.*

Agents lose track of what you asked for, redo work another agent already did, and have no way
to leave each other a note. `cone` fixes that with a directory:

```
~/cone/
  tasks/
    inbox/     filed, not yet triaged
    ready/     triaged, claimable          ← the heartbeat watches this
    doing/     claimed; frontmatter says who
    blocked/   waiting on something
    done/
  board/       messages between agents, one file each
```

**One file per thing; the directory is the state.** Claiming a task moves it from `ready/` to
`doing/` — a hard link plus an unlink, which is atomic *and* refuses to overwrite. 32 agents can
race for the same task and exactly one wins, with no daemon, no database and no coordinator; the
losers get an error, never a second copy and never a clobbered file. That property is tested,
not assumed.

The consequence that matters: **any agent can participate with nothing but `mv` and `cat`.**
This binary is a convenience, never a gatekeeper.

## The heartbeat

An agent session is turn-based. It cannot watch a folder, and an agent that polls costs a turn
every tick to usually learn nothing. So `cone watch` does the watching in a process that is
free to be idle, and wakes a model only when **all** of these hold:

1. `tasks/ready/` is non-empty
2. an orchestrator is idle **in a checkout matching the task's `repo`**
3. we are under the concurrent-worker cap, counting only claims held by agents still alive
4. we have not already poked that orchestrator about this exact set of tasks

**Idle costs zero tokens. Work costs one turn.** It hashes the ready set, so it will not nag
about an unchanged queue — and it records that hash only once the prompt demonstrably landed,
because a signature burnt on a prompt that never arrived means that set is never mentioned
again. Anything that stops a poke is logged unconditionally: a heartbeat that fails quietly is
indistinguishable from one with nothing to do.

Claims held by agents Herdr no longer knows about are released automatically, so a few crashed
workers cannot hold every slot forever.

```
launchd / systemd
      │
      ├─ ready/ non-empty?          free
      ├─ an idle lead in that repo? free
      ├─ under the cap?             free
      └─ herdr agent prompt … /tasks   ← the only expensive step
```

## Quick start

```sh
brew install jclement/tap/cone     # or: mise run install
cone install                       # board dir, AGENTS.md, scheduler, index
claude mcp add cone -- $(which cone) mcp

cone new "why did the solver get slow"
cone ready 20260819-why-did-the-solver-get-slow
cone                               # the TUI
```

## Commands

| | |
|---|---|
| `cone new <title>` | file a task into inbox |
| `cone ls [state\|all]` | list tasks (default: ready) |
| `cone show <id>` | print a task |
| `cone ready <id>` | inbox → ready (triaged, claimable) |
| `cone claim <id>` | ready → doing. **Atomic**: exactly one agent wins |
| `cone done <id> [--result …]` | finish it. An **investigation must record what it found** |
| `cone note <id> <text>` | record a finding without changing state |
| `cone set <id> <key> <value>` | `worktree` · `agent` · `branch` · `repo` · `priority` |
| `cone block <id>` · `back` | the rest of the lifecycle |
| `cone reap [--dry-run]` | release claims held by agents herdr has lost |
| `cone stale [-h 8]` | claims older than N hours (reports only) |
| `cone post <topic> <text>` · `read` | the message board |
| `cone search <query>` | full-text across tasks and messages |
| `cone tui` | interactive board (also the default with no args) |
| `cone watch` | the heartbeat |
| `cone sync` | pull tasks from configured inboxes |
| `cone mcp` · `serve` | MCP over stdio · over HTTP |
| `cone doctor` | why did nothing happen? Checks the board *and* the heartbeat |
| `cone install` | board, scheduler, index |
| `cone update [--check]` | verified upgrade to the latest release |

## Inboxes

The board is the system of record; an inbox is *a* way tasks arrive. Meat Prompt and herdrer
are supported out of the box and detected from the environment or
`~/.config/<service>/env` — a host with neither configured simply syncs nothing.

Deduplication is by upstream id across **every** state, so re-running never double-files.
Adding a source means implementing one interface.

## MCP

```sh
claude mcp add cone -- $(which cone) mcp        # local: no auth, no port
CONE_TOKEN=… cone serve -addr 127.0.0.1:7788    # remote agents
```

Eight tools: `cone_ls`, `cone_show`, `cone_claim`, `cone_new`, `cone_update`, `cone_search`,
`cone_post`, `cone_read`. `cone_update` carries the whole lifecycle — `ready`, `done` (with a
`result`), `block`, `back`, `note`, `worktree`. Deliberately few — MCP tool descriptions sit in an agent's context
for a whole session, so each one is a permanent tax.

## Staying current

`cone version` and the TUI say when a newer release exists, from a background check that
never delays a command and never runs for a dev build, in a container, or with
`CONE_NO_UPDATE_CHECK` set.

```sh
cone update --check     # is there a newer release?
cone update             # verify it, then replace this binary
```

**An unverified download is never installed.** `cone update` pulls the release archive,
`checksums.txt` and its keyless cosign bundle, checks that the bundle was signed by this
repository's release workflow (sigstore: TUF trust root → Fulcio certificate → Rekor), then
checks the archive's SHA-256 against that signed list. Either check failing aborts the update
and says which one — there is deliberately no `--force`. The new binary lands with an atomic
rename, so an interrupted update leaves the old cone intact.

Inside a container it refuses and tells you to pull a newer image; a Homebrew install is left
to `brew upgrade cone`.

## When it looks like nothing is happening

```sh
cone doctor
```

It reads every task file directly rather than through the board, because `List` skips what it
cannot parse — a broken task is invisible exactly when you are asking why it never came up. It
also checks the half that lives outside the board: that the scheduler unit exists, that every
absolute path it names still does (a `brew upgrade` used to delete one), and that the watcher
has actually written to its log. **"Loaded" is not "working."** Exit code 1 when something is
broken, so it can be scripted.

## What this is not

- **Not agent memory.** Durable learnings belong in a real knowledge system. `cone search`
  covers *operational history* — what was asked, claimed and reported.
- **Not a queue to drain.** Nobody is scored on emptying `ready/`.
- **Not authorisation.** A task is a request. It does not widen what an agent may do; pushing,
  deploying and touching production keep their normal gates.

## Configuration

| Variable | Default | |
|---|---|---|
| `CONE_HOME` | `~/cone` | board root |
| `CONE_AGENT` | `$HERDR_AGENT`, else hostname | who claims |
| `CONE_TOKEN` | — | bearer token for `cone serve` |
| `CONE_HERDR` | `herdr` on `PATH` | which herdr the heartbeat wakes |
| `CONE_NO_UPDATE_CHECK` | — | set to anything to silence the update check |

## License

MIT © 2026 Jeff Clement
