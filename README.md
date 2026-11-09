# rig

Run several AI coding agents at once, each fully isolated, and keep track of all of them
from one place.

rig is a small local daemon (`rigd`) with a CLI and a terminal UI. You describe a piece of
work as a **ticket** (one or more services, each a git repository). rig gives every
service its own isolated checkout, runs the coding agent in a Docker container with
nothing but that checkout, and keeps the ticket's plan, tasks, tests and status in
ordinary markdown files. One dashboard answers: what is running, what is each agent doing,
what needs me, and where do I click to test it.

- **Isolated by default.** The agent works in a container on its own clone of your
  repository. Your repository's branches, hooks and config are out of its reach, and your
  git credentials never enter the container.
- **Parallel.** Several tickets, several services per ticket, several instances of the
  same service: each ticket has its own Docker network and its own ports.
- **You stay in charge.** Work moves through phases (plan, implement, test, review, push)
  with approval gates only a human can pass. Agents *request*; you *grant*.
- **Files are the truth.** A ticket is a directory (`~/.rig/tickets/<id>/`) of readable
  files. The daemon is a cache and a coordinator, and can be restarted at any time.
- **Multi-service work stays consistent.** Services that must agree on an interface share a
  `contract.md`. Agents propose changes, you accept or reject them, and the other agents
  are told.

Status: it works end to end and is thoroughly tested against fakes; the parts that need a
real login (Claude Code, GitHub, Jira) are exercised by hand. See the
[checklist](checklist.md) for what has been verified and what is still a manual check.

## Requirements

- macOS or Linux ([Linux notes](docs/linux.md)). Windows is not supported.
- Docker (for the default container sandbox and for service containers).
- `git`.
- A terminal multiplexer for the agents' panes: `tmux`, or [Herdr](docs/spikes/02-herdr.md).
- A coding agent. v1 drives [Claude Code](https://claude.com/claude-code) (`claude`).
- Optional: `gh` for `pr` pushes, `delta` for side-by-side diffs, Ollama for summaries.

## Install

From source (needs Go and [go-task](https://taskfile.dev); nothing else):

```sh
git clone https://github.com/charlesrobsampson/rig && cd rig
task build          # builds ./rig with the egress proxy embedded
install -m 755 rig /usr/local/bin/rig
```

Release builds for macOS and Linux (arm64 and amd64) come from `task release`, which
writes archives and a checksum file to `dist/`.

## Quickstart

```sh
# 1. Tell rig how you like things (all optional; see "Configuration").
mkdir -p ~/.config/rig && cp examples/config.json ~/.config/rig/config.json

# 2. Give agents their credentials: a Claude Code token (or API key), kept out of every
#    file. The example config's "claude" auth profile reads the secret CLAUDE_CODE_OAUTH_TOKEN.
claude setup-token                                          # prints a token
security add-generic-password -s rig -a CLAUDE_CODE_OAUTH_TOKEN -w   # macOS: paste it
                                                            # (Linux and servers: docs/linux.md)

# 3. Describe the work.
rig new --id add-locale --title "Return a Content-Language header" \
        --service api=/path/to/your/api-repo

# 4. Start it: checkouts, containers, an agent in its own terminal pane.
rig start add-locale

# 5. See and talk to what is running.
rig                       # the TUI (or `rig list` for a table)
rig attach add-locale     # jump into the agent's terminal
rig msg add-locale api "also cover the 404 case"

# 6. Move the work through its phases; gates need you.
rig gates add-locale
rig approve add-locale plan_approval
rig phase add-locale implement
rig review add-locale     # what the agent changed, through delta if installed
rig phase add-locale test
rig tests add-locale      # manual tests written by the agent, with live ports filled in
rig approve add-locale review_approval
rig phase add-locale push && rig push add-locale

# 7. Tear down (imports the work first, never loses it silently).
rig stop add-locale
```

An auth profile maps stored secrets to what the agent reads: `oauth_token`, `api_key`,
`auth_token`, `bedrock`, `vertex`, `foundry`, or `host_login` (host mode only: the agent
uses your own login). A container cannot use your machine's login, which is why container
agents need a profile. A ticket with two services looks like
[`examples/tickets/two-services.yaml`](examples/tickets/two-services.yaml).

## The terminal UI

Bare `rig` on a terminal opens the TUI (anywhere else it prints `rig list`). It is a client
of the same daemon API as the CLI, and updates live from the daemon's event stream.

- **Dashboard.** One row per ticket: phase, an agent light per service (● working,
  ○ idle, ! blocked, ✕ exited), ports, cost, and what needs you (BLOCKED agent, CONTRACT
  CHANGE, ERROR, DRIFT, APPROVE gate). Tickets that need you sort to the top.
- **Ticket view** (`enter`). Tabs for the plan, tasks (with progress), contract, diff,
  logs and manual tests, which you tick off and sign off.
- **Actions.** `a` approve, `v` move to review, `p` push, `x` stop, `t` attach (the TUI
  suspends and comes back), `m` message an agent, `c`/`C` accept or reject a contract
  change. Anything destructive asks first. `?` shows every key.

## How a ticket works

```text
~/.rig/tickets/add-locale/
├── ticket.yaml        what to run (services, env, policy)
├── state.json         rig's record: phase, approvals, ports, container and pane ids
├── plan.md            the shared plan; the agent refines it, you approve it
├── tasks.md           small steps, checked off; rig counts the boxes
├── manual-tests.md    steps for you to try by hand, with {{svc.url}} placeholders
├── contract.md        (multi-service) what the services agreed on
└── api/               the service's isolated checkout
```

Phases are `plan → implement → test → review → push → done`. Moving forward needs the
phase's gate approved by a human (which gates apply is policy: default `plan_approval` and
`review_approval`); moving back needs a reason. Agents can ask for a move
(`transition.request`) but never grant one.

`rig push` publishes each service the way its policy says: `none`, `push-branch`, `merge`
(fast-forward, optionally a merge commit) or `pr` (through `gh`). It runs on the host with
your credentials, one service at a time, and can be re-run after a failure.

## Architecture

```text
 rig (CLI)   rig (TUI)   rig mcp (supervising agent)
      \          |          /
       unix socket, JSON lines  ── docs/formats.md §8
                 |
               rigd ── supervisor (agent state from hooks and the screen)
                 │   ── lifecycle (checkouts, networks, containers, push)
                 │   ── watchers (contract, egress, reconcile, summaries)
        ┌────────┼─────────────┐
      git     Docker      terminal backend (tmux / Herdr)
```

Everything a client can do is a daemon API call, so the CLI, the TUI and
[an orchestrating agent over MCP](docs/mcp.md) are interchangeable. The exact file,
API, event and naming shapes are in [`docs/formats.md`](docs/formats.md); the design and
reasoning are in [`manifest.md`](manifest.md).

## The sandbox model, and where it stops

What the container sandbox gives you:

| | |
|---|---|
| Code access | The agent gets **only its clone** of the repository (read-write) and the original's object store read-only. Your repository's branches, hooks and config are unreachable; its work comes back when rig fetches the branch on the host. |
| Git credentials | Never in the container. The agent commits locally; rig pushes from the host. |
| Secrets | Only the names a ticket lists reach the agent's environment, from the Keychain / Secret Service / `.env`; never written to ticket files, logs or API payloads. |
| Isolation between tickets | A Docker network per ticket; identical service names never collide; ports are allocated per ticket. |
| Network | `network_mode: bridge` (open) or `internal`: no route out except a per-ticket allowlist proxy (HTTPS tunnelling, no TLS interception, refuses private addresses) with `api.anthropic.com` allowed by default. |
| Host | `sandbox: host` runs the agent on your machine with your access to everything; rig warns loudly. It exists for work that needs the host. |

What it does **not** give you, so you can decide with open eyes:

- A container is not a virtual machine. Containers share your kernel; a kernel or Docker
  vulnerability is outside what rig can contain.
- The agent can see and use its own environment: whatever secret you give it (an API key,
  a token) it holds, and can send anywhere the network lets it. Use `internal` mode and a
  tight allowlist for anything sensitive, and prefer narrowly scoped tokens.
- `bridge` mode is open: the agent can reach the internet and your local network.
- Service containers run what the repository's Dockerfile and `start` command say; rig
  runs them as written.
- rig mounts no Docker socket into containers and never runs agents privileged, but a
  `Dockerfile` you point it at is your code and runs with its normal power.
- You are the review gate: agents write code, you read the diff before it is pushed.

## Configuration

`~/.config/rig/config.json` (or `$RIG_CONFIG`, or `--config`). Everything has a default;
unknown keys are errors. [`examples/config.json`](examples/config.json):

```json
{
  "defaults": {
    "harness": "claude-code", "sandbox": "container", "terminal_backend": "tmux",
    "policy": { "gates": ["plan_approval", "review_approval"], "push": { "mode": "pr" } }
  },
  "limits": { "max_active_tickets": 4 },
  "auth": { "default": "claude", "profiles": { "claude": { "type": "oauth_token", "secrets": ["CLAUDE_CODE_OAUTH_TOKEN"] } } },
  "summarizer": { "provider": "none" },
  "jira": { "base_url": "https://example.atlassian.net", "email": "me@example.com" },
  "secrets": { "keychain_service": "rig", "env_file": "~/.config/rig/secrets.env" }
}
```

Precedence is flags, then `ticket.yaml`, then the service, then the global config. Examples
of tickets: [one service](examples/tickets/single-service.yaml),
[two services with a database](examples/tickets/two-services.yaml), and a
[no-gates experiment on a locked-down network](examples/tickets/greenfield.yaml).

Optional pieces, each off until you configure it:

- **Summaries** (`summarizer.provider`: `ollama` for a local model, `claude` for the API):
  a short advisory note on each ticket, always shown next to the real diff stats and
  marked REMOTE when the content left your machine.
- **Ticket sources:** `rig new --from jira:PROJ-412` or `--from github:owner/repo#123`.
- **Contract tests:** `rig contract export` turns a contract's endpoint and schema blocks
  into an OpenAPI document your services' tests can check themselves against
  ([docs/contract-tests.md](docs/contract-tests.md)).
- **MCP:** `claude mcp add rig -- rig mcp` lets a supervising agent list tickets, read
  files, message agents and propose contract changes; it can approve nothing
  ([docs/mcp.md](docs/mcp.md)).

## Documentation

| | |
|---|---|
| [`manifest.md`](manifest.md) | what rig is for and why it is built this way |
| [`docs/formats.md`](docs/formats.md) | the exact file, API, event and naming shapes |
| [`docs/linux.md`](docs/linux.md) | running on Linux |
| [`docs/mcp.md`](docs/mcp.md) | the MCP server |
| [`docs/contract-tests.md`](docs/contract-tests.md) | checking services against `contract.md` |
| [`docs/spikes/`](docs/spikes) | the experiments behind the design decisions |
| [`process.md`](process.md) / [`checklist.md`](checklist.md) | how rig is developed, and the task list with notes |

## Development

You need Go and [go-task](https://taskfile.dev). Nothing else is needed to build or run the
default tests; they use fakes for Docker, the terminal, GitHub, Jira and the models, and
pass with the network off.

```sh
task check          # gofmt, go vet, the default tests: run before every commit
task test:race      # the same tests with the race detector
task test:linux     # the default tests on Linux, in a container (needs Docker)
task test:docker    # tests against real Docker
task test:integration   # tests against real tmux / Herdr
task test:llm       # tests against a local Ollama (skips if none)
task build          # ./rig;  task release  # archives for macOS and Linux in dist/
```

## License

No license has been chosen yet, so all rights are reserved by default. Choosing one is
the maintainer's decision.
