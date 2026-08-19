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

**The consequence that matters: the protocol is a filesystem, not an API.** Any tool, in any
language, on any host that can hard-link a file can implement a claim correctly —
`ln ready/x.md doing/x.md && rm ready/x.md`, in that order, is the whole lock. What is *not*
safe is `mv`: it succeeds unconditionally, so two agents that both `mv` both believe they won,
and a task that lands in `doing/` without a `claimed_by` stamp is invisible to `stale`, `reap`
and the worker cap alike. Agents should use `cone claim`; this binary is a convenience, but the
link-then-unlink order is not.

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

## The 2am problem

A worker finishes at 02:10 and nobody reads it. By morning the pane is gone — a restart, a
teardown, a laptop that slept — and with it the only account of what happened. The task then
looks abandoned, gets released, and the work is offered to somebody else as if it were new.

So the heartbeat **captures a finished worker's recent output onto its task** while the pane
still exists, and a task with captured output is held for review rather than re-queued. Only
work with nothing to show goes back on the queue.

It replaces rather than appends (Herdr marks an agent `done` at the end of every turn, not once
at the end of the work), it is capped and marked unverified, and **it is kept out of the search
index** — terminal tail is evidence, not a finding, and it must not compete for rank with what
an agent actually concluded. The written result is still the deliverable; this is the safety net
under it.

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
| `cone mcp` | MCP over stdio |
| `cone doctor` | why did nothing happen? Checks the board *and* the heartbeat |
| `cone install` | board, scheduler, index |
| `cone update [--check]` | verified upgrade to the latest release |

## Inboxes

The board is the system of record; an inbox is *a* way tasks arrive — for when a human queues
something somewhere else (a phone, a web form) and wants an agent to pick it up.

**cone knows about no particular service.** Sources are declared in
`~/.config/cone/inboxes.json`, so a change to somebody else's API is a config edit rather than
a cone release:

```json
[
  {
    "name":       "phone-queue",
    "url":        "https://queue.example.com",
    "claim_path": "/api/v1/tasks/claim",
    "token_file": "~/.config/phone-queue/env",
    "token_key":  "PHONE_QUEUE_TOKEN"
  }
]
```

A source needs one endpoint: a GET that hands the caller one queued task and returns `204` when
there are none. `token` and `token_env` work in place of `token_file`. No file means no inboxes,
which is the common case and costs nothing — but a file that exists and is wrong is an error,
because silently syncing nothing is indistinguishable from an empty queue.

Deduplication is by upstream id across **every** state, so re-running never double-files. These
endpoints hand a task to exactly one caller, so a fetch is destructive: anything that cannot
then be filed lands in `inbox-quarantine/` rather than disappearing, and `cone doctor` says so.

## MCP

```sh
claude mcp add cone -- $(which cone) mcp        # stdio: no auth, no port
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

`cone watch` finds leads through `herdr agent list`, so **an agent Herdr did not start is
invisible to it** — the board fills up, the watcher runs perfectly, and nothing is ever woken,
which looks exactly like having nothing to do. `cone doctor` names the leads it can see, so that
failure is a line of output rather than a silence.

```sh
cone doctor
```

It also reads every task file directly rather than through the board, because `List` skips what
it cannot parse — a broken task is invisible exactly when you are asking why it never came up. It
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
| `CONE_INBOXES` | `~/.config/cone/inboxes.json` | where sources are declared |
| `CONE_HERDR` | `herdr` on `PATH` | which herdr the heartbeat wakes |
| `CONE_NO_UPDATE_CHECK` | — | set to anything to silence the update check |

## License

MIT © 2026 Jeff Clement
