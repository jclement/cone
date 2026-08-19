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
  claims/      file-level claims per repo
```

**One file per thing; the directory is the state.** Claiming a task is `mv ready/x.md
doing/x.md`, and because same-filesystem rename is atomic, that *is* the lock — 32 agents can
race for the same task and exactly one wins, with no daemon, no database and no coordinator.
That property is tested, not assumed.

The consequence that matters: **any agent can participate with nothing but `mv` and `cat`.**
This binary is a convenience, never a gatekeeper.

## The heartbeat

An agent session is turn-based. It cannot watch a folder, and an agent that polls costs a turn
every tick to usually learn nothing. So `cone watch` does the watching in a process that is
free to be idle, and wakes a model only when **all** of these hold:

1. `tasks/ready/` is non-empty
2. an orchestrator is idle
3. we are under the concurrent-worker cap
4. we have not already poked about this exact set of tasks

**Idle costs zero tokens. Work costs one turn.** It hashes the ready set, so it will not nag
about an unchanged queue.

```
launchd / systemd
      │
      ├─ ready/ non-empty?          free
      ├─ an idle orchestrator?      free
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
| `cone done <id>` · `block` · `back` | the rest of the lifecycle |
| `cone stale [-h 8]` | claims older than N hours (reports only) |
| `cone post <topic> <text>` · `read` | the message board |
| `cone search <query>` | full-text across tasks and messages |
| `cone tui` | interactive board (also the default with no args) |
| `cone watch` | the heartbeat |
| `cone sync` | pull tasks from configured inboxes |
| `cone mcp` · `serve` | MCP over stdio · over HTTP |
| `cone install` | board, scheduler, index |

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
`cone_post`, `cone_read`. Deliberately few — MCP tool descriptions sit in an agent's context
for a whole session, so each one is a permanent tax.

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

## License

MIT © 2026 Jeff Clement
