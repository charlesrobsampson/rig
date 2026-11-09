# RIG: Multi-Agent Parallel Orchestration Engine
*One place to manage AI coding agents across many tasks, services, and instances of the same service.*

---

## 1. Summary
**RIG** is a local daemon (`rigd`) + CLI/TUI (`rig`) that runs several AI coding agents in parallel, each fully isolated. Isolation comes from **isolated git checkouts** (code: a shared clone per service in container mode, a worktree in host mode), **Docker** (runtime + sandbox), and **markdown lifecycle artifacts** (state). Terminal sessions are managed through a pluggable **terminal backend** (Herdr first, tmux fallback).

**Exact file, API, event, and naming shapes live in [`docs/formats.md`](docs/formats.md).** This manifest covers intent and design; that file is the precise contract.

**Goal:** one dashboard answers: what is running, what is each agent doing, what needs me, and where do I click to test it.

**Non-goals (v1):** cloud/remote execution, multi-user, Windows, auto-deploy.

### Design principles
1. **Sandbox by default.** Agents run in containers. Host access is opt-in.
2. **Everything is an interface.** Ticket source, terminal backend, harness, store, summarizer. Each has one default, more later.
3. **Daemon API is the product.** CLI, TUI, and other agents are all clients of the same API (see §6).
4. **Files are truth.** Ticket state lives in `~/.rig/tickets/<id>/`; the daemon store is a cache that can be rebuilt.
5. **Policy per ticket/service.** Gates, push behavior, and network mode are configurable, never hardcoded.

---

## 2. Ticket Layout

```text
~/.rig/tickets/ticket-412/
├── ticket.yaml        # config (services, env, policy)
├── state.json         # phase, per-service status, allocated ports (rig-owned)
├── plan.md            # shared human/AI design (the "why" and approach)
├── contract.md        # cross-service interfaces (endpoints, schemas, events); only for multi-service tickets
├── tasks.md           # micro-steps + TDD checklist
├── manual-tests.md    # agent-written test steps; rig substitutes live ports
├── backend/           # checkout of repo A (isolated shared clone, or worktree in host mode)
└── frontend/          # checkout of repo B
```

---

## 3. Lifecycle State Machine

`plan → implement → test → review → push → done`

| Phase | Who drives | Exit condition |
|---|---|---|
| plan | agent + human | `plan.md` approved (gate) |
| implement | agent (TDD loop) | all tests in `tasks.md` pass |
| test | rig (automated) then human (manual) | manual-tests signed off |
| review | human, via `delta` diff | approved (gate) |
| push | rig | per `push.mode` policy |

* Rig **records and enforces** phase in `state.json`. Agents request transitions; rig checks gates.
* Backward moves allowed (review → implement). Each move is logged.
* **Gates are configurable** (see policy). Default: strict (`plan_approval`, `review_approval` both required).

### Cross-service contract (`contract.md`)
Multi-service tickets need agents in different repos to agree on interfaces. The ticket dir sits outside every repo, so it is the natural shared space.

* `plan.md` = intent and approach. `contract.md` = concrete, testable interface: endpoints, request/response shapes, events, error codes, version/date.
* Written during the plan phase; approval of the plan includes approval of the contract.
* **Single source of truth.** Every service agent reads it at start and before finishing each task.
* **Change protocol:** an agent never edits agreed sections directly. It appends to a `## Proposed changes` section (who, what, why). Rig detects the change, flags the ticket as "contract change pending", and notifies the other service's agent (injected message) and the human. Once accepted, the change is merged into the agreed section and logged in `## Changelog`.
* Contract tests (optional): each service's tests reference the contract so drift fails a build.

---

## 4. Sandbox Model (key decision)

**Default: agent runs inside a container.** Host stays clean even if the agent does something bad.

| Concern | Decision |
|---|---|
| Agent location | Per-service container with the service's isolated checkout bind-mounted rw. `sandbox: container` (default) or `host` (opt-in, per ticket). |
| Repo access | Container mode uses an **isolated shared clone**: only the clone is mounted rw, plus the main repo's object store read-only. The main repo's branches, hooks, and config are unreachable. Work returns via `git fetch <clone> branch:branch` done by rig on the host (`docs/spikes/04-agent-container.md` §3). Host mode uses a normal worktree. |
| Dependent services (db, cache, other repos) | Sibling containers on a **per-ticket network** `rig-<ticket-id>`. |
| Multiple instances of same service | Per-ticket networks mean identical service names/DNS never collide across tickets. |
| Host ports | Dynamic allocation, persisted in `state.json`, checked against host-in-use. Container ports stay fixed. |
| Git credentials | **Never in the container.** Agent commits locally; rig pushes from host. |
| Agent auth | Named **auth profiles** (`oauth_token`, `api_key`, `auth_token`, `bedrock`, `vertex`, `foundry`, `host_login` for host mode only) chosen per ticket or per service; values resolved from Keychain/`.env` and injected as env. The macOS login can't be mounted into a container; a `claude setup-token` OAuth token or an API key is used instead (`docs/spikes/04-agent-container.md` §7). |
| Corporate TLS inspection | `docker.extra_ca_certs` PEM paths are baked into agent/service images and mounted read-only at run time. |
| API keys / secrets | Injected as env at start from keychain or a `.env` reference. Never written into `ticket.yaml`. |
| Network egress | `network_mode`: `bridge` (open) or `internal`. In `internal` mode all of a ticket's containers sit on an `--internal` network (no route out, no external DNS) and reach the outside only through a per-ticket allowlist proxy (HTTPS CONNECT tunnel, no TLS interception, refuses non-public addresses) that alone joins an egress network. Default allowlist: the model API; extend per ticket/service (`docs/spikes/05-egress.md`). |
| Host-needed cases | `sandbox: host` runs the agent on the host, services still in Docker. Rig warns loudly. |

**Tradeoff accepted:** container-agent costs image-setup effort per service (toolchain, deps) but gives real safety and reproducible parallel instances. Host mode is the escape hatch.

**Service definition:** each service points to a `Dockerfile` or `compose` fragment in its repo (or a `rig` override), plus `start`, `test`, and optional `healthcheck` commands. These are plain commands that rig runs as written; rig has no knowledge of Taskfile or any other task runner. Taskfile is only used for developing rig itself (see `process.md`): rig ships and runs as a single normal Go binary with no runtime dependency on it. `base_image` alone is only a fallback for the agent toolchain.

---

## 5. Configuration

### Global (`~/.config/rig/config.json`)
```json
{
  "summarizer": { "provider": "ollama", "model": "llama3:8b", "endpoint": "http://localhost:11434" },
  "defaults": {
    "harness": "claude-code",
    "sandbox": "container",
    "terminal_backend": "herdr",
    "ticket_source": "manual",
    "policy": { "gates": ["plan_approval", "review_approval"], "push": { "mode": "pr" } }
  },
  "limits": { "max_active_tickets": 8 }
}
```
Precedence: CLI flags > `ticket.yaml` > service defaults > global.

### Ticket (`ticket.yaml`)
```yaml
id: "ticket-412"
title: "Full-stack feature for user dashboard"
source: { type: "jira", key: "PROJ-412" }   # or manual | github | linear

services:
  - name: backend
    repo: /path/to/backendRepo
    branch: feat/auth-backend-412
    path: ./backend
    build: { dockerfile: Dockerfile }
    start: "go run ./cmd/server"
    test: "go test ./..."
    internal_port: 8080
    healthcheck: "/healthz"
    policy:                       # overrides ticket/global policy
      gates: [plan_approval, review_approval]
      push: { mode: pr }          # pr | push-branch | merge | none
  - name: frontend
    repo: /path/to/frontendRepo
    branch: feat/auth-frontend-412
    path: ./frontend
    start: "bun dev"
    test: "bun test"
    internal_port: 3000
    policy:
      gates: []                   # greenfield: no gates
      push: { mode: merge }

dependencies:                      # shared per-ticket sidecars
  - name: db
    image: postgres:16
    env: { POSTGRES_PASSWORD: dev }

docker: { network_mode: bridge }   # bridge | internal
sandbox: container                 # container | host
env: { DEBUG: "true" }
secrets: [ANTHROPIC_API_KEY]       # names only; values from keychain/.env
harness: claude-code
```

**Push modes:** `merge` for greenfield; `pr` or `push-branch` for production services where a merge could trigger deploy; `none` for manual handling.

---

## 6. Architecture

### Daemon + clients
`rigd` owns state, containers, ports, and terminal sessions. `rig` (CLI) and the TUI are thin clients over a **local Unix-socket API** (JSON). Every CLI command supports `--json`.

> **Agent-as-orchestrator:** because the API is the product, a supervising agent can drive rig through the same API (later exposed as an MCP server): list tickets, read agent status, nudge, request review. No extra work beyond keeping the API clean and complete.

### Interfaces (each with one v1 implementation)
| Interface | v1 | Later |
|---|---|---|
| `TicketSource` | manual (yaml) | Jira, GitHub, Linear |
| `TerminalBackend` | Herdr + tmux (both in v1, validates the interface) | zellij etc. |
| `Harness` | claude-code | cursor, custom |
| `Store` | JSON file | SQLite |
| `Summarizer` | Ollama | Claude API |

### Layout
```text
rig/
├── cmd/rig/        # main + cobra commands (new, start, list, logs, attach, approve, review, push, stop, daemon, ...); thin clients of the daemon API
├── internal/
│   ├── api/        # socket protocol shared by daemon + clients
│   ├── clock/      # injectable clock (real implementation)
│   ├── config/     # global + ticket parsing
│   ├── contract/   # contract.md template + parser
│   ├── daemon/     # server, router, supervisor, recovery
│   ├── docker/     # networks, containers, port allocator
│   ├── events/     # in-process event bus feeding events.subscribe streams
│   ├── domain/     # shared types: Ticket, Service, Policy, enums, id validation
│   ├── fsutil/     # atomic file writes and cross-process locks
│   ├── lifecycle/  # start/stop of a ticket: provisions checkouts, records state, emits events
│   ├── gates/      # which approvals a ticket needs before changing phase (pure rules)
│   ├── git/        # checkout provisioning (clone/worktree), dirty checks, import, push/PR
│   ├── harness/    # agent launchers
│   ├── registry/   # reads tickets (ticket.yaml + state.json) for list/get
│   ├── policy/     # precedence resolution (flags > service > ticket > global)
│   ├── state/      # state.json reader/writer + phase machine
│   ├── store/      # runtime cache store (JSON, SQLite later)
│   ├── summary/    # LLM summaries
│   ├── terminal/   # TerminalBackend interface + herdr and tmux impls
│   ├── testkit/    # fakes (clock, docker, terminal, gh, ticket source) and fake agent
│   ├── ticket/     # TicketSource interface + impls
│   └── tui/        # Bubble Tea app, views, components
├── docs/           # formats.md, spikes/
├── spikes/         # throwaway spike prototypes
├── tools/          # Go programs for non-trivial scripts, run by Taskfile
├── Taskfile.yml    # dev scripts (development only; not a runtime dependency)
├── go.mod
└── go.sum
```

### CLI
* `rig` open TUI (auto-starts daemon)
* `rig new` create ticket (manual or `--from jira:PROJ-412`)
* `rig start <id> [--sandbox host|container]` provision checkouts
* `rig stop <id> [--force] [--keep-checkouts] [--yes]` import work, then remove checkouts (never loses work silently)
* `rig gates <id>` show the approvals a ticket needs; `rig approve <id> <gate> [--service name]` grant one (humans only)
* `rig list` tickets, phase, status, ports
* `rig attach <id> [service]` jump into agent terminal
* `rig approve <id> <gate>` pass a gate
* `rig review <id>` open `delta` diff
* `rig push <id>` run push policy
* `rig logs <id>` stream logs
* `rig events` print daemon events as they happen
* `rig daemon start|status|stop|run` manage the background daemon (`run` is the foreground form the others re-exec)
* `rig config show` print the effective global config
* Global flags: `--config <file>`, `--json`

---

## 7. Engineering Subsystems

* **Checkouts:** container mode = isolated `git clone --shared` per service (agent can only touch its own clone); host mode = `git worktree`. Both share the main repo's object store. `stop` imports the branch into the main repo first, then refuses to delete a dirty checkout without `--force`; branches are always kept. All host-side git on a clone uses a hardened runner (no hooks, fsmonitor, external diff, pager, credential helpers).
* **Networking:** per-ticket bridge/internal network; dynamic host port mapping; port map persisted and verified on daemon restart.
* **Terminal state:** agent state comes from Claude Code hooks (`--settings`, reported via `rig agent-event`) with backend detection/screen heuristics as fallback; rig overlays lifecycle phase. Message injection is keystrokes (text, then Enter) and is refused while an agent is `blocked` (see `docs/spikes/03-tmux.md`). Herdr is accessed over its local socket API in a dedicated named session (`rig`), so rig never touches the user's default Herdr workspaces; attach with `herdr session attach rig` (see `docs/spikes/02-herdr.md`).
* **Recovery:** on daemon restart, reconcile `state.json` against actual containers, checkouts, and terminals; mark drift instead of guessing.
* **Summaries:** background LLM compresses diffs + `plan.md`/`tasks.md`. Always shown beside raw status and diff stats; summary is advisory.
* **Manual testing:** agent writes `manual-tests.md` with placeholders like `{{backend.url}}`; rig substitutes live ports and shows it in the TUI.
* **Limits & cost:** cap on concurrent active tickets; optional per-agent token/cost readout when the harness exposes it.
* **Neovim + delta:** open a ticket's checkouts in Neovim; reviews use `delta`.

---

## 8. Open Questions
* Egress proxy for `internal` mode: build a minimal one vs. reuse an existing proxy (spike in Phase 5).
* Image build caching for fast ticket startup.
