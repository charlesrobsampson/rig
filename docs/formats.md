# RIG Formats and Contracts

The exact shapes that every part of rig shares. `manifest.md` says *what* and *why*; this file says *exactly how*. If a task needs a shape not defined here, stop and ask the user, then add it here in the same branch.

**Change rules:** adding a field, method, event, or enum value is fine (additive). Renaming, removing, or changing the meaning of anything needs the user's approval. Readers must ignore unknown fields; writers must keep `schema_version`.

---

## 1. Conventions
- Timestamps: RFC 3339 UTC (`2026-11-05T14:03:00Z`). Durations in Go-style strings (`30s`, `5m`).
- JSON keys: `snake_case`. YAML keys: `snake_case`.
- Ticket id: `^[a-z0-9][a-z0-9-]{0,62}$`. Service/dependency name: `^[a-z][a-z0-9-]{0,30}$`. Names are unique within a ticket (services and dependencies share one namespace).
- All paths stored in files are absolute, except `ticket.yaml` service `path` (relative to the ticket dir).
- Every persisted JSON file has `"schema_version": 1` at the top level.

## 2. Filesystem locations
| What | Path | Override |
|---|---|---|
| rig home | `~/.rig` | `RIG_HOME` |
| global config | `~/.config/rig/config.json` | `RIG_CONFIG` |
| tickets | `<rig home>/tickets/<id>/` | |
| daemon socket | `<rig home>/rigd.sock` | |
| daemon pid | `<rig home>/rigd.pid` | |
| daemon log | `<rig home>/rigd.log` | |
| runtime cache store | `<rig home>/store.json` | |

## 3. Ticket directory
```text
<rig home>/tickets/<id>/
├── ticket.yaml        # human-owned config
├── state.json         # rig-owned truth (phase, approvals, ports, ids)
├── plan.md            # human + agent
├── tasks.md           # agent; GitHub checkboxes `- [ ]` / `- [x]`, progress = counted boxes
├── manual-tests.md    # agent-written; see §10
├── contract.md        # multi-service tickets only; see §9
├── .rig-events/<svc>/ # events.log: state words appended by a container agent's hooks (§7); read by the daemon
├── messages.jsonl     # rig-written log of messages typed into agents (`rig msg`): {at, service, from, text, delivered, reason?}
└── <service path>/    # checkout (default `./<service name>`): isolated shared clone in container mode, git worktree in host mode
```
`state.json` is the only file rig rewrites; it also appends to `messages.jsonl` and creates the stubs of `rig new`. Agents must never edit `state.json` or `ticket.yaml`. `store.json` is a rebuildable cache (see manifest §1, principle 4).

## 4. `ticket.yaml`
Shape per manifest §5, plus:
- `services[].base`: base branch for the checkout and diffs. Default: the repo's default branch (`origin/HEAD`, else `main`, else `master`).
- `services[].path`: default `./<name>`.
- `services[].policy.gates`: absent = inherit; `[]` = no gates.
- `policy.push.mode` (`none | push-branch | merge | pr`, per service, ticket or global) and `policy.push.merge_commit` (bool, same levels, an explicit `false` overrides a `true` above it; default false): merge mode only fast-forwards the base unless it is true, in which case a merge commit is pushed when the base moved on.
- `secrets`: names only. Values that look like secrets in `env` (high-entropy or known token prefixes) are rejected.
- Known gate names: `plan_approval`, `review_approval`, `manual_test_signoff`.
- `auth`: name of an auth profile from the global config (`auth.profiles`); ticket-level, overridable per service (`services[].auth`). Profile types: `host_login` (host mode only), `oauth_token`, `api_key`, `auth_token`, `bedrock`, `vertex`, `foundry`. A profile lists secret *names* (and non-secret env such as region); values never appear in any rig file or API payload. `sandbox: container` with a `host_login` profile is a validation error.
- Global config also has `auth.default` (profile name) `docker.extra_ca_certs` (list of PEM file paths) and `docker.port_range` (`{min, max}`, default 41000-41999, within 1024-65535: where host ports for service containers are allocated), `docker.agent_base` (default `debian:bookworm-slim`: the image an agent runs on when its service has no Dockerfile; otherwise the service's own image) `secrets.keychain_service` (default `rig`: macOS Keychain generic passwords of this service, account = the secret's name), `secrets.env_file` (an absolute `.env` path, `~` allowed; consulted after the Keychain) and `docker.claude_version` (default `latest`: what the agent image's Claude Code installer is asked for).
- `egress.allow`: extra hostnames reachable through the egress proxy when `docker.network_mode: internal` (ticket-wide, and per service as `services[].egress.allow`; the ticket's proxy enforces the union). Global config: `egress.default_allow` (default `["api.anthropic.com"]`) and `egress.default_ports` (default `[443]`). Hostnames are exact or `*.suffix`.
- Unknown keys are errors naming the key and line.

## 5. `state.json`
```json
{
  "schema_version": 1,
  "ticket_id": "ticket-412",
  "phase": "plan",
  "approvals": [
    {"gate": "plan_approval", "service": "", "by": "human", "at": "2026-11-05T14:03:00Z"}
  ],
  "transitions": [
    {"from": "plan", "to": "implement", "by": "human", "reason": "", "at": "2026-11-05T14:05:00Z"}
  ],
  "services": {
    "backend": {
      "status": "pending",
      "checkout": {"kind": "clone", "path": "/abs/path"},
      "branch": "feat/x",
      "base": "main",
      "host_port": 0,
      "container_id": "",
      "agent": {"backend": "herdr", "pane_id": "", "state": "unknown", "container_id": ""},
      "push": {"status": "pending", "pr_url": "", "error": ""},
      "error": ""
    }
  },
  "dependencies": {"db": {"container_id": "", "status": "pending"}},
  "network": {"name": "", "id": ""},
  "egress_network": {"name": "", "id": ""},
  "contract": {"pending": false, "agreed_hash": "", "seen": [], "violation": ""},
  "manual_tests": {"steps": {}, "signed_off": false},
  "created_at": "2026-11-05T14:00:00Z",
  "updated_at": "2026-11-05T14:05:00Z"
}
```
- `approvals[].service` empty = applies to every service that requires the gate.
- `transitions[].by`: `human` or `agent:<service>`. `reason` required for backward moves.
- `services[].status`: `pending | provisioned | running | stopped | error`.
- `agent.state` / `AgentState`: `working | blocked | idle | unknown | exited`.
- `push.status`: `pending | done | failed`.
- `contract.pending` is true while some proposal is open; `agreed_hash` is the hash of the agreed section as rig last knew it to be right (set from the template when the ticket is created; follows edits freely during the `plan` phase); `seen` lists the proposal ids already announced; `resolved` lists proposals resolved through rig (their `contract.resolved` event was already published, so the file watcher skips them); `violation` is non-empty after the agreed section was edited directly once the plan phase is over, and stays until it is resolved.
- `egress_network` is present only in `network_mode: internal` (the proxy's outward network); absent otherwise.
- Writes are atomic (temp file + rename) and serialized per ticket.

## 6. Lifecycle rules
- Phases: `plan → implement → test → review → push → done`.
- Forward: exactly one step at a time. Backward: to any earlier phase, with a reason. `done` is terminal.
- Gate checks (a gate applies per service; a ticket moves only when every service that requires it has approved, or an approval with empty `service` exists):
  | Transition | Gate |
  |---|---|
  | plan → implement | `plan_approval` |
  | test → review | `manual_test_signoff` |
  | review → push | `review_approval` |
- Default policy gates: `[plan_approval, review_approval]`.
- For a ticket with more than one service, `plan_approval` is refused (`blocked`, with a detail saying what to write) while `contract.md` has nothing under `## Agreed` (only whitespace and guidance comments count as nothing); the plan → implement move is refused the same way if the contract was emptied after approval. Single-service tickets and tickets whose services do not require the plan gate are unaffected.
- Agents may *request* forward transitions; rig grants only if gates pass. Agents may not approve gates.

## 7. Naming and container topology
- Network: `rig-<ticket>` (created `--internal` when `network_mode: internal`); `rig-<ticket>-egress` (normal bridge, joined only by the egress proxy). Service container: `rig-<ticket>-<service>`. Agent container: `rig-<ticket>-<service>-agent`. Dependency: `rig-<ticket>-dep-<name>`. Egress proxy: `rig-<ticket>-proxy` (in `internal` mode it also publishes the services' host ports, which Docker cannot do for a container that is only on an internal network, and forwards them inward; env `ALLOW_HOSTS`, `ALLOW_PORTS`, `LISTEN`, `FORWARD=<port>=<service>:<port>,...`).
- Labels on every Docker object: `rig.ticket=<id>`, `rig.service=<name>` (when applicable), `rig.role=service|agent|dependency|proxy|network`.
- **Working topology (spike 04 confirms or amends):** per service, a *service container* runs the `start` command, and an *agent container* (toolchain image with the agent installed) runs the agent. Both bind-mount the same worktree and join the ticket network, so the agent can run tests and reach the service by name. Dependencies are siblings on the same network.
- In-network URL of a service: `http://<service>:<internal_port>`. Host URL: `http://localhost:<host_port>`.
- Git push credentials are never mounted into any container.
- **Repository access (decided in spike 04):** the checkout kind follows the sandbox. `sandbox: container` provisions an **isolated shared clone** (`git clone --shared <repo> <path>`, branch created from `base`): the container mounts only that clone (rw, at `/work`) and the main repo's `.git/objects` **read-only at its real host path** (the clone's alternates point there). The main repo's refs, hooks, and config are never mounted, so an agent cannot touch other branches. `sandbox: host` provisions a real `git worktree`. `services[].checkout.kind` in `state.json` records which (`clone` or `worktree`).
- Work leaves a clone only through rig: `Import` fetches the clone's branch into the main repo (`git fetch <clone> branch:branch`); `rig stop` and `rig push` import first. Commits never exist solely in a clone that rig deleted.
- Host-side git against a clone is **untrusted-input safe**: rig runs those commands through a hardened runner that disables hooks, fsmonitor, external diff and textconv, pagers, and credential helpers, so a hook or config planted by the agent cannot run on the host.

## 8. Daemon API
Transport: unix socket, JSON lines (one JSON object per line, UTF-8, max 1 MiB per line).

Request: `{"v":1,"id":"<client-chosen>","method":"ticket.list","params":{}}`
Response: `{"v":1,"id":"<same>","ok":true,"result":{}}` or `{"v":1,"id":"<same>","ok":false,"error":{"code":"not_found","message":"..."}}`.
Streaming methods (`events.subscribe`, `logs.stream`) send many lines with the same `id`: `{"v":1,"id":"..","stream":{...}}`, ending with `{"v":1,"id":"..","ok":true,"end":true}`.
The daemon handles each request on a connection concurrently, so responses may arrive in a different order than the requests: clients match them by `id`. A client that wants a stream (`events.subscribe`, `logs.stream`) should open a dedicated connection for it; the stream ends when the client disconnects.
The first message on a connection must be `hello`: `{"method":"hello","params":{"client":"rig-cli","version":"x"}}`; the server replies with its protocol version. Mismatched major version is rejected.

Error codes: `invalid`, `not_found`, `conflict`, `blocked` (gate/dirty/limit; `error.details` lists reasons), `unavailable` (Docker/terminal backend down), `not_running`, `internal`.

| Method | Purpose |
|---|---|
| `hello`, `status` | handshake; daemon version, pid, uptime |
| `config.get` | effective global config |
| `ticket.list`, `ticket.get`, `ticket.rescan` | read tickets |
| `ticket.new`, `ticket.start`, `ticket.stop` | create / provision / teardown (`start` params: `sandbox`, `dry_run`; the result also carries `network`, `containers[]`, `dependencies[]`, `warnings[]`; the result also carries `agents[]` of `{service, pane_id, launched, note, error}` (one agent per host-sandbox service); a dry run provisions nothing and returns `launches[]` of `{service, harness, dir, command, env}` with secret values as `<secret NAME>`; `stop` params: `force`, `keep_checkouts`; its result lists `services[]` (checkout imported/removed) and `resources[]`, the Docker containers and networks removed; stopping an already stopped ticket succeeds with nothing in either; a stop that cannot finish fails with the cause's code and `details[]` naming what failed and what remains, and keeps the checkouts) |
| `gate.list`, `gate.approve` | pending gates / approve |
| `phase.set`, `transition.request` | human move / agent request |
| `agent.attach`, `agent.message` | returns attach command / injects text (`agent.message` params `{ticket, service, text}`, result `{service, sent}`; errors `blocked` when the agent waits on a permission prompt (nothing typed), `not_running`, `unavailable` if the agent is not ready in time; `agent.attach` params `{ticket, service?}` (service optional when the ticket has one), result `{service, command[]}`; errors `not_found`, `not_running` when no agent is running) |
| `push.run` | `{id, service?}`: runs the push step of a ticket in the push phase; result `{summary, services[{service, mode, status, skipped?, pushed, ref?, sha?, pr_url?, note?, error?}], done}` (a failed service is in the result with `error`, not an API error; `done` when every service is pushed and the ticket moved to `done`) |
| `agent.event` | agent hook reports a state (`ready\|working\|blocked\|idle\|exited`) for a ticket/service |
| `contract.list` | `{ticket}` -> `{ticket, proposals[{id, from, status, what, why, rejected?}], problems?[], violation?}`; `not_found` without a contract.md |
| `contract.accept` | `{ticket, proposal}` -> `{proposal, outcome: "accepted"}`: human action. Merges the proposal into the agreed section (as a `####` entry), marks it accepted, appends a changelog line, publishes `contract.resolved` and types a notice into every agent. `not_found` for an unknown id, `conflict` when it is not open |
| `contract.reject` | `{ticket, proposal, reason}` -> `{proposal, outcome: "rejected"}`: human action; `reason` required (`invalid` without). Records `**Rejected:** <reason>` under the proposal and in the changelog; the proposer is told the reason |
| `contract.ack` | `{ticket}` -> `{}`: clears `state.contract.violation` once a human has reviewed a direct edit; `conflict` when there is none |
| `review.diff` | per-service diff and stat |
| `push.run` | runs the push step of a ticket in the push phase. Params `{id, service?}`; result `{summary, services[{service, mode, status, skipped?, pushed, ref?, sha?, pr_url?, note?, error?}], done}`. A service that fails is in the result with `error` (not an API error); services already pushed are skipped on a rerun; `done` is true when every service is pushed and the ticket moved to `done` |
| `tests.render` | `{ticket}` -> `{preamble?, steps[{id, title, body, done}], signed_off}`: manual-tests.md with placeholders filled from live state. `invalid` with `details[]` listing every unfillable placeholder (or bad step heading); `not_found` when the file does not exist |
| `logs.stream` | container logs |
| `events.subscribe` | event stream |
| `reconcile.run` | drift report, optional fix |
| `daemon.stop` | graceful shutdown |

New methods may be added by later tasks following the same envelope. Method payload types (`ticket.list`, `ticket.get`, `status`, and later ones) are defined in `internal/api`, whose JSON field names are the reference; this file fixes the envelope, error codes, method names, and events.

## 9. Events
Envelope: `{"type":"phase.changed","ticket":"ticket-412","service":"","at":"...","data":{}}`.

| Type | `data` |
|---|---|
| `ticket.created`, `ticket.started`, `ticket.stopped` | `{}` |
| `service.status` | `{"status":"running"}` |
| `phase.changed` | `{"from","to","by","reason"}` |
| `gate.approved` | `{"gate","service"}` |
| `agent.state` | `{"state","previous"}` |
| `contract.pending` | `{"proposal":"P-backend-1"}` |
| `contract.violation` | `{"detail"}` (agreed section edited directly) |
| `contract.resolved` | `{"proposal","outcome":"accepted|rejected"}` |
| `egress.denied` | `{"host","port","reason"}` (proxy refused a connection) |
| `error` | `{"message"}` |
| `subscribed` | `{}` (first item of every `events.subscribe` stream: the subscription is live) |

## 10. CLI conventions
- Every command that prints data supports `--json` (a global flag), printing exactly the API `result` payload.
- Human output to stdout, diagnostics and errors to stderr.
- Exit codes: `0` ok, `1` generic failure, `2` usage/invalid input, `3` blocked (gate, dirty checkout, limit), `4` daemon/backend unavailable, `5` not found.
- Destructive commands (`stop --force`, `reconcile --fix`) prompt unless `--yes`.

## 11. `contract.md`
````markdown
# Contract: ticket-412

## Agreed
Free markdown. Machine-readable parts use fenced blocks tagged `endpoint` or `schema`:

```endpoint
POST /api/v1/dashboard
request: {"user_id": "string"}
response: {"widgets": ["string"]}
errors: 400 invalid_user
```

## Proposed changes
### P-backend-1 · from backend · open
**What:** add `locale` to the request.
**Why:** frontend needs localized widget names.

## Changelog
- 2026-11-05 P-backend-1 accepted by human: added `locale`.
````
- Proposal heading regex: `^### (P-[a-z][a-z0-9-]*-\d+) · from ([a-z][a-z0-9-]*) · (open|accepted|rejected)$`. The id embeds the proposing service, so agents never collide. The number is that service's next integer.
- Rejected proposals carry a `**Rejected:** <reason>` line.
- Agents append proposals only. Rig edits `## Agreed` and `## Changelog` on accept.
- Malformed proposals are reported, never silently dropped, and never hide valid ones. A proposal is malformed when its heading does not match, its id names a different service than its `from`, its id repeats, it lacks `**What:**` or `**Why:**`, or it is rejected without a `**Rejected:**` reason. The other proposals are still read. HTML comments (the template's guidance) are ignored everywhere, `###` lines inside code fences are text, and a field's value continues over following lines until the next marker.
- `rig new` writes the template (guidance comments only, nothing agreed) for a ticket with two or more services; single-service tickets have no `contract.md`. "Nothing agreed" means the `## Agreed` section is empty apart from comments.

## 12. `manual-tests.md`
- Each step is a level-3 heading `### <step-id>: <title>` (id: `^[a-z0-9-]+$`, unique) followed by free markdown, usually with fenced shell blocks.
- Placeholders (rendered at read time): `{{<service>.url}}` (host URL), `{{<service>.port}}` (host port), `{{<service>.internal_url}}`, `{{<dependency>.host}}`, `{{ticket.id}}`. Unknown placeholders are errors listing every offender.
- Step completion is stored in `state.json` `manual_tests.steps[<step-id>]`, not in the file.
- A `### ` heading inside a fenced code block is text, not a step. Placeholders may appear anywhere (preamble, titles, bodies, code blocks), with optional spaces inside the braces (`{{ api.url }}`). `{{<service>.url}}` and `.port` need the service to be running (they are its host port); `.internal_url` needs `internal_port`. Anything else is reported, all offenders at once, including `{{...}}` text that is not a placeholder; the file is never rendered partially.

## 13. Harness prompt contract
- The launch prompt is assembled from: ticket title, current phase, `plan.md`, `tasks.md`, `contract.md` (multi-service only), and the rules for that phase. It never includes secret values.
- Rules every agent receives: edit only your service's checkout; never edit `state.json`, `ticket.yaml`, or `## Agreed`; propose contract changes under `## Proposed changes`; request phase transitions, never assume them; write `manual-tests.md` with placeholders during/after `implement`.
