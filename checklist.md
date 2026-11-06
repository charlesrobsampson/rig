# RIG Checklist

**Before any task: read `process.md` (how to work), the sections of `manifest.md` the task names (design intent), and the sections of `docs/formats.md` it names (exact shapes). Also read `Notes:` on the tasks you depend on.**

Tasks are in dependency order. Each task = one branch (`task/NN-slug`). Tick the box only in the task's final commit. Task fields: **Session** (resume command, filled when started) · **Depends** · **Read** · **Build** · **Tests** · **Manual check** · **Notes** (filled at finish for the next task).

Conventions: module `github.com/charlesrobsampson/rig`; packages per manifest §6 (task 01 reconciles the layout, adding `clock`, `store`, `state`, `contract`, `policy`, `testkit`); fakes live in `internal/testkit`.

**MVP cut:** tasks 01–32 give tickets, isolated checkouts, gated lifecycle, agents in Herdr/tmux panes, message injection, and `rig list`. Docker (33–44) is next.

---

## Phase 0: Foundations and spikes

### 01 · Repo bootstrap
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/01-bootstrap` · Depends: none
- Note: the directory is not yet a git repo. `git init -b master`, create `.gitignore`, and commit the existing `manifest.md`, `checklist.md`, `process.md`, `docs/formats.md` as the initial commit before recording the session.
- Read: `process.md`, `manifest.md` §6, `docs/formats.md` §1–2
- Build: `.gitignore` (build output, `.DS_Store`, `*.log`, `spikes/**/bin`); `go mod init github.com/charlesrobsampson/rig`; empty package dirs with a `doc.go` each; `cmd/rig/main.go` using cobra, printing a version with `--version`; `Taskfile.yml` (go-task) with the tasks in `process.md` "Commands: use Taskfile" (`build`, `test`, `test:race`, `test:integration`, `test:docker`, `test:llm`, `vet`, `fmt`, `fmt:check`, `check`, `tidy`, `clean`; the tagged test tasks accept package paths after `--`, default `./...`); update the manifest §6 layout to include the extra packages and `Taskfile.yml`.
- Tests: none (scaffolding). `task test` must run (zero tests is fine).
- Manual check: `task --list` shows every task; `task build && ./rig --version` prints a version; `task check` passes; `git log` shows the initial commit; `git status` clean.
- Notes: git identity is repo-local (`charlesrobsampson` + GitHub noreply email); no global identity is set. Module is `github.com/charlesrobsampson/rig`, Go 1.27.3, cobra v1.10.2. `cmd/rig/main.go` has `newRootCmd()` returning the root cobra command with `--version` (value injected via `-ldflags -X main.version`, from `git describe`). `docs/spikes/`, `spikes/`, `tools/` exist locally but are untracked while empty; create files in them as needed. Taskfile tagged tasks use `{{.CLI_ARGS | default "./..."}}`.

### 02 · Spike: Herdr integration
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/02-spike-herdr` · Depends: 01
- Context: manifest §7 relies on Herdr reporting `working | blocked | idle` and being scriptable. Stability is a known risk.
- Read: `manifest.md` §6, §7, §8; Herdr docs/CLI help on this machine
- Build: `docs/spikes/02-herdr.md`. Install/locate Herdr; document its CLI and socket API; prove: spawn a pane with a given cwd and command, list panes, read agent state for a running Claude Code, send text to a pane, close a pane. Note versions, latency, and failure modes (Herdr not running, pane died). Prototype under `spikes/herdr/`. Recommend the minimal `TerminalBackend` method set it can support.
- Tests: none (manual spike).
- Manual check: user watches a pane spawn, change state, receive injected text, and close.
- Notes: full findings in `docs/spikes/02-herdr.md`. Decision: Herdr is the primary backend, driven over its unix-socket JSON-lines API in a dedicated `rig` named session (headless via `herdr --session rig server`; start it if `server_not_running`). Quirks for tasks 26–32: `done` means idle-but-unseen (treat as idle); external `report-agent` is dropped within ~1 s unless `--agent` is a known kind (so the fake agent cannot use a custom label with real Herdr); `send-text` does not press Enter; `wait-output` matches the typed command echo; key names are `ctrl+d` style; fresh worktrees hit Claude Code's trust dialog (reported `blocked`), so task 29 must decide pre-trust vs surfacing it. The real Claude Code check (idle → working → done, injection intact) was run and passed; see the doc §8.

### 03 · Spike: tmux state detection and message injection
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/03-spike-tmux` · Depends: 01
- Context: tmux is the fallback backend and has no native `working/blocked/idle`. Contract-change notices (manifest §3) need a way to push a message into a running agent.
- Read: `manifest.md` §3 (contract), §7, §8
- Build: `docs/spikes/03-tmux.md`. Try: spawn a window with cwd+command on a private socket (`tmux -L`), read output (`capture-pane`), infer state for Claude Code (output heuristics, hooks, idle timers), inject a message (`send-keys` vs paste-buffer, escaping, behavior while the agent is mid-response). Also check Claude Code hooks/other harness-native channels as an alternative injection path. Decide the injection mechanism and state-detection approach for both backends. Prototype under `spikes/tmux/`.
- Tests: none.
- Manual check: user sees an injected message land in a running agent without corrupting its input.
- Notes: full findings in `docs/spikes/03-tmux.md`. Decisions: agent state comes from Claude Code hooks passed per launch with `claude --settings` (`PermissionRequest`→blocked is immediate; `Notification` is ~6 s late), reported by a new `rig agent-event <state>` subcommand using `RIG_TICKET`/`RIG_SERVICE` env, with a screen heuristic as fallback; `TerminalBackend.State` returns only liveness (+ optional heuristic) and the supervisor merges. Injection = `send-keys -l` (or bracketed paste) then a separate `Enter`; Claude queues text sent while working; **never inject while `blocked`** (an injected Enter approved a permission prompt in testing); wait for `SessionStart` before the first injection. tmux needs `set-option -g remain-on-exit on` to read exit status. Added `agent.event` to formats §8. The manual check was run by the assistant against real Claude Code and passed.

### 04 · Spike: agent inside a container
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/04-spike-agent-container` · Depends: 01
- Context: manifest §4 makes container the default agent location. Need to know it is practical.
- Read: `manifest.md` §4, `docs/formats.md` §7
- Build: `docs/spikes/04-agent-container.md`. Run Claude Code inside a container with a bind-mounted worktree: confirm or amend the working topology in formats §7 (service container + agent container sharing the worktree and network); **authentication** (does the user use an API key or a subscription login? find a safe way to give the container access to either, e.g. env key, or a mounted read-only credentials dir, and state the risks); toolchain image strategy, API key injection via env, interactive TTY attach from a Herdr/tmux pane (`docker exec -it`), file ownership/uid mapping on macOS, git commits from inside without push credentials, performance of bind mounts, how to run the service and tests from inside. State what the image must contain and how long a cold/warm start takes. Prototype under `spikes/container/`.
- Tests: none.
- Manual check: user attaches to the containerized agent, makes a commit in the worktree, and sees it on the host.
- Notes: findings in `docs/spikes/04-agent-container.md`. Verified: overlay image (`FROM <service image>` + native Claude installer) runs Go + Claude as any uid; corporate TLS inspection (Zscaler) breaks image builds unless the root CA is baked in (`docker.extra_ca_certs`); host ownership is a non-issue on macOS; `docker exec -it` in tmux gives a proper PTY; hook events cross the boundary via a mounted events dir; auth is env-based with per-ticket/per-service **auth profiles** (design in §7, added to manifest §4 and formats §4). **Decision (approved by the user, applied to 17, 17b, 18, 21, 22, 40, 44, 45, 46):** a worktree mounted into a container would need the main repo's `.git` read-write (agent could rewrite any branch), so container mode uses an isolated `git clone --shared` checkout (only the clone rw, main objects ro) and host mode keeps real worktrees, behind one `Checkout` interface; host-side git on a clone uses a hardened runner; work leaves a clone only via `Import`. **Not verified:** an authenticated Claude turn inside a container (needs a setup-token or API key; optional, covered by task 40's manual check). Probe: `spikes/container/probe.sh`; images: `spikes/container/Dockerfile`, `overlay.Dockerfile`.

### 05 · Spike: egress proxy for `internal` network mode
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/05-spike-egress` · Depends: 01
- Read: `manifest.md` §4, §8
- Build: `docs/spikes/05-egress.md`. Compare: minimal Go allowlist HTTP(S) CONNECT proxy vs. reusing an existing proxy image (e.g. tinyproxy/squid). Prove: container on a Docker `--internal` network reaches only the proxy; proxy allows the model API host and a configurable allowlist; everything else is refused. Check TLS handling (CONNECT tunneling only, no interception). Recommend one with config shape and image size.
- Tests: none.
- Manual check: from the sandboxed container, the model API call succeeds and `curl example.com` fails.
- Notes: findings in `docs/spikes/05-egress.md`. Decision: a minimal Go allowlist HTTPS-CONNECT proxy (7.6 MB `scratch` image, config via `ALLOW_HOSTS`/`ALLOW_PORTS`, JSON decision logs, refuses allowlisted names that resolve to private addresses; tinyproxy tunnels to a private container and is 136 MB), run per ticket on an `--internal` network plus a separate egress network only the proxy joins. Verified with real Claude Code: it works through the proxy (only `api.anthropic.com` is required; `downloads.claude.ai` and telemetry hosts are denied harmlessly, silence with `DISABLE_AUTOUPDATER`/`DISABLE_TELEMETRY`/`DISABLE_ERROR_REPORTING`). Set proxy env in both cases and `no_proxy` for in-network service names. Unverified: whether OAuth-token auth needs `claude.ai`/`platform.claude.com`. Ship by embedding prebuilt static Linux binaries and building a `FROM scratch` image locally (§7). Formats/manifest updated (network name `rig-<t>-egress`, `egress.allow`, event `egress.denied`). The manual check was run by the assistant and passed. Probe: `spikes/egress/probe.sh`.

### 06 · Clock package and fake
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/06-clock` · Depends: 01
- Read: `process.md` §4
- Build: `internal/clock` (interface with `Now()`, `After`, timer and ticker creation) + real implementation; `internal/testkit` fake clock whose time advances only via `Advance(d)` and fires timers deterministically.
- Tests: table: timer fires only after enough `Advance`; multiple timers fire in order; stopped timer never fires; ticker fires N times for N intervals.
- Manual check: none (library).
- Notes: `clock.Clock` has `Now`, `After`, `NewTimer`, `AfterFunc`, `NewTicker`; `clock.Real()` is the production clock. `testkit.NewFakeClock(start)` moves only with `Advance(d)`, firing due timers in deadline order (ties by creation order), delivering the deadline as the timer's value, and running `AfterFunc` callbacks synchronously inside `Advance` (callbacks may schedule more work due inside the same call). Ticker/timer channels have capacity 1, so a receiver that doesn't drain misses ticks, like the real ticker. `internal/clock/doc.go` was replaced by `clock.go`.

### 07 · Domain types
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/07-domain` · Depends: 01
- Read: `manifest.md` §2, §3, §5, `docs/formats.md` §1, §4–6
- Build: Go types for `Ticket`, `Service`, `Dependency`, `Policy` (`Gates`, `Push{Mode}`), `Phase` enum, `ServiceStatus`, `AgentState` (`working|blocked|idle|unknown|exited`), `PushStatus`, `PushMode` enum, `Sandbox` enum, `NetworkMode` enum, `Gate` enum (`plan_approval|review_approval|manual_test_signoff`). Exact string forms from formats §4–6 for YAML/JSON. No behavior beyond validation of enums and ids (ticket id and service-name regexes from formats §1, name uniqueness across services and dependencies).
- Tests: table: enum round trip to/from string; invalid enum errors; ticket id validation (path traversal, empty, spaces, uppercase, 64+ chars rejected); service name validation; duplicate names across services and dependencies rejected.
- Manual check: none.
- Notes: lives in `internal/domain` (added to the manifest layout). Enums are string types with `Valid()` plus `MarshalText`/`UnmarshalText`, so they work with `encoding/json` and YAML libraries that honor `TextUnmarshaler`; marshaling an invalid or zero value is an error, so use `omitempty` on optional enum fields (done). `Policy.Gates == nil` means inherit, `[]Gate{}` means no gates (so the tags do not omit it). `Service` also carries optional `Sandbox` and `Harness` overrides for task 10; `Ticket.Policy` is the ticket-level policy. Validation helpers: `ValidateTicketID`, `ValidateName`, `CheckUniqueNames` (services and dependencies share one namespace). The auth-profile enum is left for task 09. No parsing yet (task 08). Tag names match docs/formats.md.

### 08 · Ticket config loader
- [x] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/08-ticket-config` · Depends: 07
- Read: `manifest.md` §5 (ticket), `docs/formats.md` §4
- Build: `internal/config` parses `ticket.yaml` into domain types with validation: required fields, repo path exists (injectable check), unique names (container ports are not compared: each service has its own container, and host ports are allocated at run time), `secrets` are names only (reject values that look like secrets inline), unknown keys are errors naming the key and line.
- Tests: table: the manifest example parses; missing required field names the field; unknown key errors; duplicate service name; two services same `path`; inline secret-looking env value rejected; per-service policy override parses.
- Manual check: none.
- Notes: API is `config.ParseTicket(data, config.TicketOptions{RepoCheck})` and `config.LoadTicket(path, opts)`; `RepoCheck` nil means "must be an existing directory". Parsing is strict YAML (`gopkg.in/yaml.v3`, unknown keys fail with the key and line). Validation collects every problem and returns one error listing them all (`invalid ticket.yaml:\n  - ...`). Defaults applied: `services[].path` = `./<name>`, `source.type` = `manual`; `services[].base` is left empty (resolved from the repo's default branch at provisioning, task 17). Extra checks beyond the task: repo must be an absolute path; service `path` must be relative, stay inside the ticket dir, and not collide or nest with another service's path; two services may not use the same repo + branch. `secrets` entries must be bare env-var names; errors never echo anything after `=` or a secret-looking value. Secret detection (`secretlike.go`): known token patterns, credentials inside URLs, or 32+ chars with no whitespace and entropy at least 4.2 bits (git SHAs pass). Auth-profile and egress fields parse but are only validated by later tasks (09, 42). The invalid-enum error names the enum kind and valid values but has no line number.

### 09 · Global config loader
- [ ] Done
- Session: `claude --resume b7893019-4d34-49ee-8e94-32c09cddc9c4`
- Branch: `task/09-global-config` · Depends: 07
- Read: `manifest.md` §5 (global)
- Build: load `~/.config/rig/config.json` (path overridable) with defaults: harness, sandbox, terminal backend, ticket source, default policy, `limits.max_active_tickets`, summarizer, paths (`~/.rig`), `auth.profiles` / `auth.default`, `docker.extra_ca_certs` (formats §4). Unknown keys are errors; profiles hold secret names only.
- Tests: defaults when file missing; partial file merges over defaults; unknown key names the key; invalid enum value names the key.
- Manual check: `rig config show` (add the command) prints effective config as JSON.

### 10 · Policy and config precedence
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/10-policy` · Depends: 08, 09
- Read: `manifest.md` §5 (precedence, push modes), §3 (gates)
- Build: `internal/policy` resolves effective settings per service with precedence CLI flags > service > ticket > global. Covers gates, push mode, sandbox, network mode, harness, terminal backend.
- Tests: table: each level overrides the one below; empty `gates: []` at service level means no gates (not "inherit"); missing level falls through; flag override wins.
- Manual check: none.

### 11 · Store interface and JSON implementation
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/11-store` · Depends: 07
- Read: `manifest.md` §1 (files are truth), §6 (Store), §7 (recovery), `docs/formats.md` §2, §3, §5
- Build: (a) `internal/store` interface (get/put/list/delete ticket runtime records, atomic multi-record update) + JSON file impl with atomic write (temp + rename), file lock, `0600` permissions, schema version field. Runtime records hold cache only. (b) `internal/state` reader/writer for `state.json` per formats §5: typed struct, atomic write, per-ticket write serialization, `Load`/`Update(fn)` helper so concurrent updates never lose writes. Later tasks (20, 21, 23, 24, …) use this and never touch the file directly.
- Tests: store: put/get/list/delete; concurrent writers don't corrupt; crash simulation (partial temp file left behind) is ignored on load; permissions `0600`; old schema version errors clearly. state: round trip of the formats §5 example; unknown fields preserved on rewrite; two concurrent `Update` calls both apply; missing file errors clearly; schema version mismatch errors.
- Manual check: none.

---

## Phase 1: Daemon and API skeleton

### 12 · API protocol
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/12-api-protocol` · Depends: 07
- Read: `manifest.md` §6, `docs/formats.md` §8–9
- Build: `internal/api`: the envelope, streaming form, `hello` handshake, error codes, and the method-name constants from formats §8; typed payloads for `hello`, `status`, `ticket.list`, `ticket.get`, and the event envelope + types from formats §9. Payload types for the other methods are added by the tasks that implement them (additively, per formats). Pure types + codec, no sockets yet.
- Tests: table: encode/decode round trip per message; unknown method returns structured error; version mismatch rejected; oversized (>1 MiB) line rejected; streaming messages end with the `end` marker; error codes serialize exactly as in formats §8.
- Manual check: none.

### 13 · Daemon server lifecycle
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/13-daemon-server` · Depends: 11, 12
- Read: `manifest.md` §6, `process.md` §4 (sockets)
- Build: `internal/daemon` server: unix socket (path configurable), PID file, single-instance lock, context-driven graceful shutdown, stale socket/PID cleanup, request router with handler registry.
- Tests: second server on the same socket fails with a clear error; stale PID/socket recovered; shutdown closes connections and removes socket; unknown method handled; concurrent clients.
- Manual check: none.

### 14 · Client and daemon auto-start
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/14-client` · Depends: 13
- Read: `manifest.md` §6, `docs/formats.md` §8, §10
- Build: client library: connect, hello, call, typed helpers; auto-start via an injectable spawner (detached re-exec of `rig daemon run`); `rig daemon status|stop|run`. Shared CLI helpers implementing formats §10: `--json` output, stderr for errors, exit-code mapping from API error codes (`blocked`→3, `unavailable`/`not_running`→4, `not_found`→5, `invalid`→2).
- Tests: client against an in-process server; spawner invoked exactly once when daemon is absent; "not running" error when auto-start disabled; `daemon stop` shuts down cleanly.
- Manual check: `rig daemon status` while stopped, run any command to auto-start, then `rig daemon status` and `rig daemon stop`.

### 15 · Ticket registry and `rig list`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/15-registry-list` · Depends: 08, 11, 14
- Read: `manifest.md` §2, §6 (CLI), `docs/formats.md` §3, §5, §8
- Build: daemon scans `~/.rig/tickets/*/ticket.yaml` + `state.json`, exposes `ticket.list` / `ticket.get`; `rig list` (table + `--json`) showing id, title, phase, per-service status, ports. Broken tickets are listed with an error flag, not hidden.
- Tests (temp rig home, in-process daemon): empty list; two tickets listed; malformed `ticket.yaml` shows as errored; `--json` output matches API payload; ticket added on disk appears after a rescan.
- Manual check: hand-create a ticket dir and see it in `rig list`.

### 16 · Event bus and subscribe
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/16-events` · Depends: 13
- Read: `manifest.md` §6, `docs/formats.md` §9
- Build: in-daemon event bus emitting exactly the event types in formats §9; `events.subscribe` API streaming events to clients; slow subscribers dropped without blocking publishers.
- Tests: subscriber receives published events in order; two subscribers; slow subscriber is dropped, others unaffected; unsubscribe on disconnect.
- Manual check: `rig events` (add the command) prints events as they happen.

---

## Phase 2: Tickets and checkouts (no Docker yet)

### 17 · Git: hardened runner, checkout interface, worktree
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/17-git-checkout` · Depends: 07
- Read: `manifest.md` §4 (repo access), §7 (checkouts), `docs/formats.md` §5, §7, `docs/spikes/04-agent-container.md` §3
- Build: `internal/testkit` helper creating real temp git repos with commits (fixed author, `GIT_CONFIG_GLOBAL=/dev/null`). `internal/git`: (a) a runner that shells out to `git` with an explicit env, in two flavors: *trusted* (the user's main repo) and **hardened** (any directory an agent can write to: `-c core.hooksPath=<empty dir> -c core.fsmonitor=false -c core.pager=cat -c credential.helper=`, `--no-ext-diff --no-textconv` on diff/log/show, `GIT_TERMINAL_PROMPT=0`, no system/global config that could be overridden by repo config); (b) the `Checkout` interface (`Kind()`, `Path()`, `Branch()`, `IsDirty`, `Remove`, `Import`, filled in by tasks 17b and 18) with a `Provision(spec{repo, branch, base, path, kind})` factory; (c) the `worktree` implementation of `Provision` (creates the branch if missing, reuses if it exists and is free), plus `Branches`, `CurrentCommit`, `DefaultBranch`.
- Tests (real git on temp repos): new branch created from base; existing branch reused; branch already checked out elsewhere errors clearly; path exists and non-empty errors; default-branch detection (`origin/HEAD`, `main`, `master`); **hardening**: a planted `core.fsmonitor` command, `diff.external`, a `textconv` driver via `.gitattributes`, a `post-checkout`/`pre-commit` hook, and a `credential.helper`, each writing a marker file, do **not** run through the hardened runner but **do** run through the trusted one (proves the test is real).
- Manual check: none.

### 17b · Git: isolated shared clone
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/17b-git-clone` · Depends: 17
- Read: `docs/formats.md` §7 (repository access), `docs/spikes/04-agent-container.md` §3
- Build: the `clone` implementation of `Checkout`: `git clone --shared <repo> <path>`, branch created from `base` (or reused), `origin` left pointing at the main repo; `ObjectsMounts()` returns the real (symlink-resolved) host paths of the main repo's object store plus any chained alternates, for read-only mounting at the same path in a container (git records real paths; spike 04). Runs only through the hardened runner after provisioning.
- Tests (real git): clone is small (`.git` under 1 MB for a repo with history) and has working alternates; a commit made in the clone is absent from the main repo until imported; branch exists in the clone with the right base; `ObjectsMounts` returns resolved real paths (including a path under a symlinked temp dir) and chained alternates; the clone has no write path into the main repo (main refs/hooks/config untouched after clone operations).
- Manual check: none.

### 18 · Git: dirty check, import, and remove (both checkout kinds)
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/18-git-dirty` · Depends: 17, 17b
- Read: `manifest.md` §7, `docs/formats.md` §7
- Build: for both kinds, `IsDirty` (uncommitted, untracked, and commits ahead of base reported separately; clone checks use the hardened runner), `Import(force)` (clone: `git fetch <clone> branch:branch` into the main repo, refusing a non-fast-forward unless `force`; worktree: no-op since the branch already lives in the main repo), and `Remove(force)` (clone: delete the directory; worktree: `git worktree remove` + prune). Remove never deletes a clone whose branch has not been imported unless `force`. Branches are always kept.
- Tests: clean checkout removes; modified tracked file → dirty; untracked file → dirty; committed-but-not-imported clone reports "ahead" and `Remove` refuses without force; after `Import`, the main repo has the branch with the clone's commits and `Remove` succeeds; non-fast-forward import refused with a clear error; force removes anyway; both kinds behave identically through the interface (shared contract suite).
- Manual check: none.

### 19 · TicketSource interface and manual source
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/19-ticket-source` · Depends: 07
- Read: `manifest.md` §5, §6 (interfaces)
- Build: `internal/ticket` `Source` interface (`Fetch(ref) -> TicketSeed{id,title,description,url}`) + `manual` impl (no remote) + registry by `source.type`. Fake source in `testkit`.
- Tests: manual source returns a seed from flags; registry unknown type errors; fake source returns seeded data; ids are sanitized to the ticket id charset.
- Manual check: none.

### 20 · Ticket templates and `rig new`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/20-rig-new` · Depends: 08, 11, 15, 19
- Read: `manifest.md` §2, §3, `docs/formats.md` §3–5, §10
- Build: `rig new` (flags: `--id`, `--title`, `--service name=repo[@branch]`, `--from <type>:<ref>`) creates the ticket dir with `ticket.yaml`, `plan.md`, `tasks.md`, `manual-tests.md` stubs, and `state.json` at phase `plan`. Fails if the id exists. Seed description goes into `plan.md`.
- Tests: created files exist with valid content; the generated `ticket.yaml` passes the loader; existing id errors without touching files; `--from` fake source seeds the plan; multi-service flag creates all services.
- Manual check: `rig new --id demo --title Demo --service api=/path/to/repo` then `rig list`.

### 21 · Provisioning: `rig start` (checkouts)
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/21-start-checkouts` · Depends: 10, 11, 17, 17b, 20
- Read: `manifest.md` §2, §7, `docs/formats.md` §3, §5, §7
- Build: `rig start <id>` provisions one checkout per service at `<ticket>/<service>` on the configured branch, choosing the kind from the resolved sandbox (`container` → shared clone, `host` → worktree), records `checkout: {kind, path}` and provisioning status in `state.json`, idempotent on re-run, rolls back checkouts created in this call if a later service fails.
- Tests: two-service ticket with `sandbox: container` yields two clones on the right branches; with `sandbox: host` yields worktrees; mixed per-service sandboxes yield mixed kinds; re-run is a no-op; failure on service 2 removes service 1's new checkout; state records kind, path, and per-service status.
- Manual check: `rig start demo` then `git -C ~/.rig/tickets/demo/api status`.

### 22 · `rig stop` (checkouts)
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/22-stop-checkouts` · Depends: 18, 21
- Read: `manifest.md` §7, `docs/formats.md` §7
- Build: `rig stop <id> [--force] [--keep-checkouts]` first **imports** each clone's branch into the main repo, then removes checkouts, refusing on dirty (or on a failed import) with a per-service report unless `--force`; branches always kept; state updated. Stop is the extension point for later teardown (agents, containers).
- Tests: clean ticket stops; a clone with commits is imported before removal (main repo then has the commits); dirty service blocks the whole stop and lists which; failed import blocks the stop; `--force` proceeds; `--keep-checkouts` leaves dirs; branch survives; stopping a non-started ticket errors clearly.
- Manual check: edit a file in a checkout, `rig stop demo` refuses, `rig stop demo --force` works; commit in a clone, `rig stop demo`, and see the commit on the branch in the main repo.

---

## Phase 3: Lifecycle state machine

### 23 · Phase machine
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/23-phase-machine` · Depends: 07, 11
- Read: `manifest.md` §3, `docs/formats.md` §5–6
- Build: in `internal/state`, the transition rules from formats §6 (one step forward, backward to any earlier phase, `done` terminal), pure `CanTransition(from, to)`, persistence of phase via the state writer from task 11.
- Tests: table of every phase pair; backward moves allowed to any earlier phase except from `done`; skipping forward refused; same-phase transition is an error; state survives reload.
- Manual check: none.

### 24 · Gates and `rig approve`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/24-gates` · Depends: 10, 23
- Read: `manifest.md` §3 (gates), §5 (policy), `docs/formats.md` §5–6
- Build: gate model per formats §6 (`plan_approval`: plan→implement; `manual_test_signoff`: test→review; `review_approval`: review→push); approvals stored in `state.json` per formats §5; resolver uses per-service policy (a ticket transitions only when every service's required gates are satisfied); `rig approve <id> <gate> [--service]`, `rig gates <id>` shows pending gates.
- Tests: strict default blocks plan→implement until approved; `gates: []` service passes freely; mixed services (one gated, one not) block until the gated one approves; approving a gate not required errors; approval persists across restart.
- Manual check: `rig approve demo plan_approval` unblocks the transition.

### 25 · Transitions, log, and agent requests
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/25-transitions` · Depends: 16, 24
- Read: `manifest.md` §3
- Build: `rig phase <id> <phase>` (human) and a `transition.request` API method (agent); both go through the gate checks. Append-only transition log (who, when, reason, from, to) in `state.json`; backward moves require a reason; emits a phase-changed event.
- Tests: human move and agent request both honored or refused identically; blocked request returns which gate is missing; backward move without reason refused; log order and fields; event emitted on success only.
- Manual check: `rig phase demo implement` refused until approved, then succeeds; `rig phase demo plan --reason "scope change"` works.

---

## Phase 4: Terminal and agent launch (host mode first)

### 26 · TerminalBackend interface, fake, contract suite
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/26-terminal-iface` · Depends: 02, 03, 06
- Read: `docs/spikes/02-herdr.md`, `docs/spikes/03-tmux.md`, `manifest.md` §6, §7
- Build: `internal/terminal` interface sized from the spikes (spawn pane with cwd/cmd/env, list, state as liveness + exit status + optional heuristic (not the authoritative agent state; see spike 03 §2), send input, close, attach command); `testkit` fake backend with scriptable agent states and recorded input; a shared contract test suite that any implementation can run.
- Tests: the contract suite passes against the fake: spawn then list; state transitions observed; sent input recorded in order; closing removes the pane; spawn in missing cwd errors; operations on a closed pane error.
- Manual check: none.

### 26b · Fake agent
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/26b-fakeagent` · Depends: 26
- Read: `process.md` §4, `manifest.md` §3, §7
- Build: `internal/testkit/fakeagent`, a small Go program (buildable to a binary for real-backend tests, importable in-process for fakes) that executes a script file: `write <path> <content>`, `commit <msg>`, `append-proposal <text>`, `state working|blocked|idle` (via the mechanism the backend detects), `wait-input` (records received input to a file), `exit <code>`. Used by later tasks wherever an agent's behavior matters.
- Tests: each script command has its effect; unknown command errors with the line number; `wait-input` records multiline input exactly; script runs in order and stops on `exit`.
- Manual check: run the binary with a sample script in a tmux pane and see the file and commit appear.

### 27 · Herdr backend
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/27-herdr-backend` · Depends: 26
- Read: `docs/spikes/02-herdr.md`
- Build: Herdr implementation of `TerminalBackend` over the socket API in the dedicated `rig` session (mapping and quirks in `docs/spikes/02-herdr.md` §4–5), passing the contract suite under the `integration` tag; starts the headless session server when absent; clear errors when Herdr is absent or not running.
- Tests: contract suite (tagged); default tests cover only argument building and error mapping against a fake executable.
- Manual check: `task test:integration -- ./internal/terminal/herdr/...` passes with Herdr running (uses the fake agent from 26b, not Claude Code).

### 28 · tmux backend
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/28-tmux-backend` · Depends: 26
- Read: `docs/spikes/03-tmux.md`
- Build: tmux implementation (private socket, one session per ticket, one window per service, `remain-on-exit on`), liveness/exit status from `list-panes`, screen-heuristic fallback and input injection per `docs/spikes/03-tmux.md` §1–3, passing the contract suite under the `integration` tag; `terminal_backend: herdr|tmux` selection from config.
- Tests: contract suite (tagged); default tests cover state-heuristic parsing on captured sample output (synthetic fixtures) and config selection.
- Manual check: `task test:integration -- ./internal/terminal/tmux/...` passes; switch config to tmux and spawn a pane.

### 29 · Harness interface and claude-code launcher
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/29-harness` · Depends: 10, 26
- Read: `manifest.md` §6 (interfaces), §3 (artifacts), `docs/formats.md` §13, `docs/spikes/02-herdr.md` §5 (trust dialog)
- Build: `internal/harness` interface (`Command(ticket, service, seed) -> {cmd, env, cwd}` (claude-code adds `DISABLE_AUTOUPDATER=1`, `DISABLE_TELEMETRY=1`, `DISABLE_ERROR_REPORTING=1` when the network mode is `internal`), plus `AuthEnv(profile) -> env` mapping an auth profile type to the harness's env vars (claude-code: `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, Bedrock/Vertex/Foundry flags); for claude-code the command includes `--settings` with the hooks config from `docs/spikes/03-tmux.md` §2 and env `RIG_TICKET`/`RIG_SERVICE`); claude-code implementation seeds the agent with a prompt built from `plan.md`, `tasks.md`, current phase, and (when present) `contract.md`; harness selectable per ticket.
- Tests: generated command per phase; prompt includes the artifact files; contract included only for multi-service tickets; unknown harness errors; env excludes secrets not listed in `secrets`.
- Manual check: print the generated command for a demo ticket with `rig start --dry-run` (add flag).

### 30 · Agent supervisor
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/30-supervisor` · Depends: 16, 26
- Read: `manifest.md` §7 (terminal state)
- Build: daemon component that merges agent state from `agent.event` reports (hooks), backend facts and heuristics (fresh hook state wins; dead pane = `exited`), updating per-service `AgentState` and publishing events on change; implements the `agent.event` API method and the `rig agent-event <state>` subcommand (reads `RIG_TICKET`/`RIG_SERVICE`, must exit 0 quickly even if the daemon is unreachable); polls the backend on the injected clock; survives backend errors with backoff.
- Tests (fake backend + fake clock): hook report beats a stale heuristic; `agent.event` for an unknown ticket/service errors; the subcommand exits 0 with no daemon; state change produces one event; unchanged state produces none; pane death marks service `exited`; backend error backs off then recovers; stops on context cancel.
- Manual check: none.

### 31 · Start launches agents; `rig attach`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/31-start-agents` · Depends: 21, 29, 30
- Read: `manifest.md` §6 (CLI), §7
- Build: `rig start` also spawns one agent pane per service via the harness and backend (`sandbox: host` for now); `rig attach <id> [service]` jumps to the pane; `rig list` shows agent state; `rig stop` closes panes before removing checkouts.
- Tests (fake backend): one pane per service with right cwd and command; list shows state from the supervisor; stop closes panes first; attach calls backend with the correct pane; attaching an unknown service errors.
- Manual check: with Herdr, `rig start demo` opens agents; `rig attach demo api` lands in the pane; `rig list` shows `working/idle`.

### 32 · Message injection
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/32-inject` · Depends: 03, 31
- Read: `docs/spikes/03-tmux.md`, `manifest.md` §3 (contract change protocol)
- Build: `agent.message` API + `rig msg <id> <service> "<text>"` implementing the injection rules in `docs/spikes/03-tmux.md` §3: refuse (or hold in rig) when state is `blocked` or the pane is dead, wait for `ready` before the first injection, send text then a separate Enter after a short pause, use bracketed paste for multi-line/large text, prefix with `[rig]`; records each message in the ticket log.
- Tests (fake backend): text delivered exactly (multiline, quotes, control characters escaped); refused when `blocked` (no keys sent at all); allowed when `working`; held until `ready` on a fresh agent; unknown service errors; log entry written. Tagged `integration`: the fake agent's `wait-input` receives the exact text through real tmux.
- Manual check: inject a message into a real running Claude Code agent and watch it appear intact.

---

## Phase 5: Docker sandbox

### 33 · Docker interface and fake
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/33-docker-iface` · Depends: 06
- Read: `manifest.md` §4
- Build: `internal/docker` interface (networks create/remove/inspect; image build; container create/start/stop/remove/exec/logs/inspect/health; port bindings); `testkit` fake Docker with real in-memory behavior (name collisions, network membership and DNS by service name, port conflicts, container logs, health flips); shared contract suite.
- Tests: contract suite on the fake: duplicate network name errors; container joins network and resolves peers by name; host port conflict errors; logs readable; remove cleans up; operations on unknown ids error.
- Manual check: none.

### 34 · Real Docker client
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/34-docker-client` · Depends: 33
- Read: `manifest.md` §4
- Build: Docker SDK implementation passing the contract suite under the `docker` tag; clear error when the daemon isn't reachable; labels on every object (`rig.ticket=<id>`, `rig.service=<name>`) so cleanup and reconciliation can find them.
- Tests: contract suite (tagged); default tests cover label building and error mapping.
- Manual check: `task test:docker -- ./internal/docker/...` passes; `docker ps -a --filter label=rig.ticket` shows nothing left behind.

### 35 · Host port allocator
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/35-ports` · Depends: 11, 33
- Read: `manifest.md` §4 (ports)
- Build: `internal/docker/ports` allocates free host ports (range configurable), persists the map in the ticket's `state.json`, verifies a port is still free on reuse (injectable probe), releases on stop.
- Tests: no duplicates across tickets; busy port skipped; persisted ports reused after restart; port taken by a foreign process reallocates and records it; exhausted range errors.
- Manual check: none.

### 36 · Per-ticket network
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/36-network` · Depends: 10, 33
- Read: `manifest.md` §4
- Build: create/remove `rig-<ticket-id>` per ticket on start/stop honoring `network_mode` (`bridge` | `internal`; `internal` creates it with Docker's internal flag) and, in `internal` mode, the `rig-<ticket-id>-egress` bridge network for the proxy (formats §7); idempotent; removal waits for containers to detach.
- Tests (fake Docker): network created once on repeated start; `internal` flag set from policy; stop removes it; two tickets have separate networks; removal with attached containers is deferred or errors clearly.
- Manual check: `rig start demo` then `docker network ls` shows `rig-demo`.

### 37 · Service containers
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/37-service-containers` · Depends: 35, 36, 21
- Read: `manifest.md` §4 (service definition), §5, `docs/formats.md` §5, §7
- Build: build the service image from its Dockerfile, run the container on the ticket network with the service's checkout bind-mounted at `/work`, env injected, internal port mapped to the allocated host port, `start`/`test`/`healthcheck` honored; `state.json` records container id, ports, health; `rig list` shows URLs.
- Tests (fake Docker): container has mount, env, labels, correct port mapping; health flips reflect in status; unhealthy start surfaces an error with the last log lines; stop removes container; missing Dockerfile errors before any side effects.
- Manual check: a real service starts, `curl localhost:<port>/healthz` works, `rig list` shows the same port.

### 38 · Dependencies (sidecars)
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/38-dependencies` · Depends: 37
- Read: `manifest.md` §5 (`dependencies`)
- Build: start `dependencies:` containers on the ticket network before services (e.g. postgres), wait for their health, resolve by name inside the network, tear down with the ticket.
- Tests (fake Docker): dependency starts before services; service can resolve it by name; dependency failure aborts start and cleans up; stop removes dependencies; env of a dependency is not leaked to other containers.
- Manual check: a service connects to a postgres sidecar by hostname.

### 39 · Same-service parallel acceptance
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/39-parallel-acceptance` · Depends: 38
- Read: `manifest.md` §1, §4
- Build: acceptance test that two tickets run the same service concurrently without collisions; fix whatever it finds.
- Tests (fake Docker): two tickets, same service and dependency names → distinct networks, distinct host ports, both resolve their own sidecar, stopping one leaves the other running and untouched.
- Manual check: run two real tickets of one repo side by side, hit both URLs, stop one.

### 40 · Agent in container
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/40-agent-container` · Depends: 04, 31, 37
- Read: `docs/spikes/04-agent-container.md` (all of it: overlay image, CA certs, git decision, auth), `docs/spikes/03-tmux.md` §4 (hook events from inside a container), `manifest.md` §4, `docs/formats.md` §7
- Build: with `sandbox: container` (default), an agent container (`rig-<ticket>-<service>-agent`, per the topology in formats §7 as confirmed by spike 04) mounts the service's checkout at `/work` (clone kind: plus the main repo's object store read-only at its real host path via `ObjectsMounts()`; never the main `.git`, hooks, or config) and joins the ticket network, the harness command runs inside it, and the pane attaches via exec; auth is provided the way spike 04 decided; `sandbox: host` keeps the old path with a loud warning at start; no git push credentials enter the container.
- Tests (fake Docker + fake backend): mounts are exactly the checkout (rw) and the objects paths (ro), with no main `.git`; pane command is an exec into the right container; host mode prints the warning and uses the host path; container env lacks git credential variables; agent API key present only if listed in `secrets`.
- Manual check: attach to the in-container agent, commit, run `rig stop` (which imports) and see the commit on the branch in the main repo; confirm the agent cannot see or change other branches.

### 41 · Secrets
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/41-secrets` · Depends: 08
- Read: `manifest.md` §4, §5
- Build: resolve names listed in `secrets` and the secret names of the selected auth profile (service profile > ticket profile > `auth.default`) from macOS Keychain or a `.env` reference path (injectable resolver); reject a `host_login` profile when `sandbox: container`; missing secret fails start with the name; values never written to the ticket dir, logs, or `--json` output.
- Tests: resolved from fake keychain; from `.env`; missing secret names the secret; values absent from logs and API payloads; precedence keychain > `.env`.
- Manual check: `rig start` with a real keychain entry; grep the ticket dir for the value finds nothing.

### 42 · Egress proxy
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/42-egress-proxy` · Depends: 05, 36
- Read: `docs/spikes/05-egress.md` (all of it), `manifest.md` §4, `docs/formats.md` §7, §9
- Build: `internal/egress` (the Go allowlist CONNECT proxy from `spikes/egress/goproxy`, hardened and tested) plus a `cmd/rig-egress` main; `task release` builds static `linux/arm64` and `linux/amd64` binaries that are embedded with `go:embed` and turned into a `FROM scratch` image on first use (cached by content hash, `docs/spikes/05-egress.md` §7). One proxy container per ticket in `internal` mode, attached to both ticket networks; allowlist = `egress.default_allow` + ticket + service `egress.allow`; agent/service env gets `HTTPS_PROXY`/`https_proxy`/`HTTP_PROXY`/`http_proxy` and `NO_PROXY`/`no_proxy` (localhost, the proxy, all service and dependency names); the daemon turns the proxy's JSON log into `egress.denied` events.
- Tests: proxy unit tests in-process (allowed host tunnels bytes both ways; denied host, plain HTTP, wrong port, and an allowlisted name resolving to loopback/private are refused; wildcard matching; JSON log lines); allowlist union and env generation (table); `internal` mode attaches containers only to the internal network, with only the proxy on the egress network; `bridge` mode starts no proxy; with the fake Docker, `egress.denied` events follow log lines; the real containment and allow/deny behavior is tagged `docker` (port `spikes/egress/probe.sh` checks).
- Manual check: sandboxed container reaches the model API and cannot reach `example.com`.

### 43 · `rig logs`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/43-logs` · Depends: 37
- Read: `manifest.md` §6 (CLI)
- Build: `rig logs <id> [service] [-f] [--tail N] [--json]` multiplexing container logs with a service prefix; works after exit.
- Tests (fake Docker): interleaved output carries prefixes; `--tail` limits; follow streams new lines until cancelled; unknown service errors.
- Manual check: follow logs of a running service.

### 44 · Container-aware stop
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/44-stop-containers` · Depends: 22, 38, 40, 42
- Read: `manifest.md` §7
- Build: extend `rig stop` to stop agents, services, dependencies, proxy, then the network, free ports, then checkouts (import and dirty check first, before anything is destroyed).
- Tests (fake Docker + fake backend): order of operations; dirty checkout aborts before containers are touched; partial failure is reported and remaining resources listed; ports freed; running twice is a no-op.
- Manual check: `rig stop demo` leaves no containers, networks, or ports (`docker ps -a`, `rig list`).

---

## Phase 6: Review and push

### 45 · `rig review`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/45-review` · Depends: 18, 25
- Read: `manifest.md` §3 (review), §7
- Build: per-service diff against the base branch (clone checkouts through the hardened runner) piped through `delta` (fallback to plain `git diff` if absent, with a notice); `--stat` summary; moves the ticket into `review` if requested.
- Tests: diff includes committed and uncommitted changes; stat counts correct; missing `delta` falls back with the notice; multi-service output separated per service.
- Manual check: `rig review demo` shows a side-by-side diff in `delta`.

### 46 · Push modes: none, push-branch, merge
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/46-push-modes` · Depends: 18, 10
- Read: `manifest.md` §5 (push modes)
- Build: `internal/git` push implementations run on the host, always **after `Import`** (so the branch is in the main repo) and from the main repo with the user's credentials, never from a clone: `none` (report only), `push-branch` (push the feature branch), `merge` (merge into base and push, refusing non-fast-forward unless configured). Credentials stay on the host.
- Tests (real git against a local bare remote): a clone's commits reach the remote only via import then push; push-branch updates the remote branch; merge fast-forwards the base; diverged base is refused with a clear error; `none` pushes nothing.
- Manual check: run against a throwaway remote repo.

### 47 · Push mode: PR
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/47-push-pr` · Depends: 46
- Read: `manifest.md` §5
- Build: `pr` mode pushes the branch and opens a PR through `gh` (injectable runner) with a body generated from `plan.md` and the diff stat; records the PR URL in `state.json`; idempotent if a PR exists.
- Tests (fake `gh`): PR created with title/body; existing PR reused; `gh` missing or unauthenticated errors clearly; URL recorded; multi-service creates one PR per service.
- Manual check: open a real PR on a scratch repo.

### 48 · `rig push` wiring and gates
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/48-rig-push` · Depends: 25, 47
- Read: `manifest.md` §3 (push phase), §5
- Build: `rig push <id>` requires phase `push` (gates satisfied), runs each service's configured mode, reports per-service results, moves to `done` only if every service succeeded; partial failure leaves state resumable.
- Tests: refused before gates satisfied; mixed modes (`merge` + `pr`) each behave per service; partial failure doesn't mark `done` and a re-run only retries the failed service; `--json` result shape.
- Manual check: end-to-end push on a scratch two-service ticket.

---

## Phase 7: Manual testing support

### 49 · Manual-tests renderer
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/49-manual-tests` · Depends: 35
- Read: `manifest.md` §7 (manual testing), `docs/formats.md` §7, §12
- Build: render `manual-tests.md` per formats §12, substituting `{{<service>.url}}`, `{{<service>.port}}`, `{{<service>.internal_url}}`, `{{<dependency>.host}}`, `{{ticket.id}}` from live state; parse steps (`### <id>: <title>`); unknown placeholders are errors listing every offender.
- Tests: table: substitution for each placeholder; multiple services; unknown placeholder errors; no placeholders passes through unchanged; stopped service gives a clear message.
- Manual check: none.

### 50 · `rig tests` and agent prompt
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/50-rig-tests` · Depends: 29, 49
- Read: `manifest.md` §7
- Build: `rig tests <id>` prints the rendered instructions; harness prompt gains instructions to write `manual-tests.md` with placeholders during the `test` phase.
- Tests: command output equals the renderer's; prompt contains the instruction only from `implement` phase on; missing file prints a hint.
- Manual check: have an agent write one and run the curl lines it contains.

---

## Phase 8: Cross-service contract

### 51 · `contract.md` template and parser
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/51-contract-parser` · Depends: 20
- Read: `manifest.md` §3 (contract), `docs/formats.md` §11
- Build: `internal/contract` following formats §11 exactly (proposal heading regex, `**What:**`/`**Why:**`/`**Rejected:**` lines, fenced `endpoint`/`schema` blocks); template with `## Agreed`, `## Proposed changes`, `## Changelog`; parser for proposed-change entries (id, author service, what, why, status); created by `rig new` for tickets with 2+ services.
- Tests: template creation only for multi-service tickets; parse zero/one/many proposals; malformed entry reported without hiding valid ones; stable ids for proposals.
- Manual check: none.

### 52 · Contract watcher and pending flag
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/52-contract-watch` · Depends: 16, 51
- Read: `manifest.md` §3 (contract)
- Build: daemon watches `contract.md` (fs events with a debounce on the injected clock), detects new proposals, sets "contract change pending" in `state.json`, emits an event, and shows it in `rig list`; also flags direct edits to `## Agreed` by an agent as a violation.
- Tests (fake clock, temp dir): new proposal sets flag once; debounce collapses rapid writes; editing Agreed text emits a violation event; flag clears when no proposals remain.
- Manual check: edit `contract.md` by hand and watch `rig events`.

### 53 · Notify and `rig contract accept|reject`
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/53-contract-resolve` · Depends: 32, 52
- Read: `manifest.md` §3 (contract)
- Build: on a new proposal, inject a notice into the other services' agents (task 32) and raise the human flag; `rig contract list <id>`, `accept <id> <change>` merges into Agreed and appends to Changelog, `reject` records the reason and notifies the proposer.
- Tests (fake backend; a scripted fake agent appends the proposal): other services receive the notice, the proposer does not; accept updates both sections atomically; reject notifies with the reason; unknown change id errors; resolved proposals aren't re-notified.
- Manual check: two agents on a demo ticket negotiate one endpoint change.

### 54 · Plan gate requires contract
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/54-contract-gate` · Depends: 24, 29, 51
- Read: `manifest.md` §3
- Build: for multi-service tickets, `plan_approval` also requires a non-empty `## Agreed` section; harness prompt tells agents to read the contract at start and propose rather than edit.
- Tests: approval refused with an empty contract on a multi-service ticket; single-service tickets unaffected; prompt contains the rule only for multi-service tickets.
- Manual check: try approving a plan with an empty contract.

---

## Phase 9: TUI

### 55 · TUI skeleton
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/55-tui-skeleton` · Depends: 16, 15
- Read: `manifest.md` §1, §6
- Build: Bubble Tea app opened by bare `rig` (prints `list` output when stdout is not a terminal): root model, layout, keymap, status bar, help overlay, daemon connection with reconnect and a "daemon not running" state.
- Tests (drive the model with key messages, assert state/view text): help toggles; quit; daemon-down state renders; reconnect restores the view; narrow terminal doesn't panic.
- Manual check: run `rig` with the daemon up and down.

### 56 · Dashboard view
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/56-tui-dashboard` · Depends: 55
- Read: `manifest.md` §1 (goal: what needs me)
- Build: fleet table (ticket, phase, per-service agent state lights, ports, pending gates, "contract change pending"); "needs me" rows sorted to the top (blocked agent, awaiting gate, pending contract change, errors); navigation bounds.
- Tests: rows sort by attention priority; each need type renders its marker; selection stays on the same ticket when rows reorder; empty fleet message.
- Manual check: run with two demo tickets in different states.

### 57 · Detail view
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/57-tui-detail` · Depends: 56, 45
- Read: `manifest.md` §2, §7
- Build: per-ticket tabs: plan.md, tasks.md (checkbox progress), contract.md, diff (via `delta` output rendered), logs; scrollable; refresh on file change events.
- Tests: tab switching; markdown/task progress counts; missing optional file handled; scroll bounds.
- Manual check: open a ticket and move between tabs while an agent edits files.

### 58 · TUI actions
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/58-tui-actions` · Depends: 57, 53, 48
- Read: `manifest.md` §6 (CLI)
- Build: keybindings for approve, attach (suspends TUI and restores), review, push, stop (with dirty confirmation), inject message, accept/reject contract change; all call the same API as the CLI.
- Tests: each key sends the right API call; destructive actions require confirmation; errors from the API show in the status bar and change nothing; attach round trip.
- Manual check: drive a ticket from `plan` to `done` entirely from the TUI.

### 59 · Manual-test view
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/59-tui-manual` · Depends: 57, 50
- Read: `manifest.md` §7
- Build: rendered manual-tests tab with copyable code blocks, per-step state stored in `state.json` `manual_tests.steps` (formats §5), and a "sign off" action that sets `signed_off` and records the `manual_test_signoff` approval.
- Tests: step toggle persists across restart; sign-off requires every step checked or an explicit override; rendering uses live ports.
- Manual check: complete a manual test pass in the TUI.

### 60 · Live updates
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/60-tui-live` · Depends: 16, 56
- Read: `manifest.md` §6
- Build: TUI subscribes to the event stream and updates views without polling; reconnect with resubscribe and full refresh.
- Tests (fake event source): agent state event updates the light; phase event updates the row; dropped subscription triggers refresh; event burst doesn't block rendering.
- Manual check: change state from another terminal and see the TUI update.

---

## Phase 10: Summaries, sources, orchestration

### 61 · Summarizer interface and Ollama backend
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/61-summarizer` · Depends: 09, 18
- Read: `manifest.md` §7 (summaries)
- Build: `Summarizer` interface with `none` (default) and `ollama`; background job summarizes diff + plan/tasks, cached by content hash; always stored beside raw diff stats; failures never block anything.
- Tests (`httptest` fake Ollama): summary cached; unchanged content not re-requested; backend error leaves the previous summary and records the error; `none` returns nothing. One extra test against real Ollama behind the `llm` tag, run with `task test:llm` (skips if unreachable).
- Manual check: with Ollama running, `rig list --summaries` shows one.

### 62 · Claude summarizer and dashboard integration
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/62-summary-claude` · Depends: 56, 61
- Read: `manifest.md` §7
- Build: `claude` backend (key via secrets resolver); dashboard shows the summary next to diff stats, marked advisory, with an indicator when a remote backend is active.
- Tests (`httptest`): request body excludes files outside the ticket; summary and stats both render; remote indicator shown only for remote backends.
- Manual check: summary appears in the dashboard for a real ticket.

### 63 · Jira ticket source
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/63-jira` · Depends: 19, 41
- Read: `manifest.md` §5 (`source`)
- Build: `jira` source (`rig new --from jira:PROJ-412`) filling title and description into `plan.md`; credentials from the secrets resolver; no writes back to Jira.
- Tests (`httptest` fake Jira): seed fields mapped; 404 and 401 give clear errors; markup converted to markdown reasonably; credentials never logged.
- Manual check: create a ticket from a real Jira key.

### 64 · GitHub issues ticket source
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/64-github-source` · Depends: 19, 41
- Read: `manifest.md` §5
- Build: `github` source (`--from github:owner/repo#123`), same shape as Jira.
- Tests (`httptest`): same cases as Jira; PRs referenced as issues handled or rejected clearly.
- Manual check: create a ticket from a real issue.

### 65 · MCP server
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/65-mcp` · Depends: 32, 53, 25
- Read: `manifest.md` §6 (agent-as-orchestrator)
- Build: `rig mcp` stdio MCP server wrapping the daemon API: list tickets, get status, read artifacts, send message, request transition, propose contract change. No approval tools: gates stay human-only. Docs for pointing a supervising agent at it.
- Tests: tool list matches the documented set; each tool maps to the right API call; approval is not exposed; errors become tool errors, not crashes.
- Manual check: a supervising Claude session lists tickets and nudges an agent.

---

## Phase 11: Hardening and release

### 66 · Daemon restart reconciliation
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/66-reconcile` · Depends: 44, 30
- Read: `manifest.md` §7 (recovery)
- Build: on daemon start, reconcile `state.json` against real containers (by labels), checkouts, and panes; mark drift (missing, orphaned) and expose it in `rig list`; `rig reconcile --fix` removes orphans after confirmation.
- Tests (fakes): clean state yields no drift; killed container shows as missing; orphan container found by label; `--fix` removes only rig-labeled orphans; agents keep running across a daemon restart.
- Manual check: kill the daemon mid-ticket, restart, and see nothing lost.

### 67 · Limits and cost readout
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/67-limits` · Depends: 15, 29
- Read: `manifest.md` §7 (limits)
- Build: enforce `max_active_tickets` on `rig start` with a clear message; optional per-agent token/cost readout when the harness exposes it, shown in `rig list --json` and the dashboard.
- Tests: start refused at the cap; stopping frees a slot; cost field absent (not zero) when unsupported; cap configurable.
- Manual check: set the cap to 1 and try starting a second ticket.

### 68 · Contract test helper
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/68-contract-tests` · Depends: 54
- Read: `manifest.md` §3 (contract tests)
- Build: optional helper that extracts machine-readable endpoint/schema blocks from `contract.md` and exposes them to service test suites (e.g. emits an OpenAPI fragment); document usage.
- Tests: extraction of fenced blocks; invalid block reported with a line number; no blocks yields an empty result.
- Manual check: a service test fails when it diverges from the contract.

### 69 · Linux check
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/69-linux` · Depends: 66
- Read: `process.md` §4
- Build: run the suite on Linux (CI or container), fix path/socket/permission assumptions, document differences (Keychain → secret-tool or `.env`).
- Tests: default suite green on Linux; secrets resolver has a Linux implementation behind the same interface.
- Manual check: on a Linux box, run a demo ticket end to end.

### 70 · README, license, CI, release
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/70-release` · Depends: 69
- Read: `manifest.md` §1
- Build: README (install, quickstart, architecture, config examples, sandbox model and its limits), example `ticket.yaml` files, license, CI running `task check` and `task test:race` (plus the tagged suites where the runner supports them), and a `task release` target producing release builds. Contributors need only Go and `go-task`.
- Tests: none. CI must pass.
- Manual check: a clean machine follows the README to a running two-service demo ticket.

---

## Backlog (not scheduled)

### 71 · Image overrides for services, agents, and dependencies
- [ ] Done
- Session: _(fill on start)_
- Branch: `task/71-image-overrides` · Depends: 37, 40
- Read: `docs/spikes/04-agent-container.md` §1, `docs/formats.md` §4, §7
- Build: let `ticket.yaml` (and global config as a default) name the image a container runs in instead of rig building one: `services[].image` (service container), `services[].agent_image` (agent base; rig still layers git, certs, and the agent on top unless `agent_image_final: true`), and `dependencies[].image` (already supported). Validate that overridden agent images contain what rig needs (git, a shell, the agent binary) with a clear error at start.
- Tests (fake Docker): override used instead of a build; precedence service > ticket > global; missing required tools reported before any container starts; cache key includes the override digest.
- Manual check: point a service at a prebuilt image and run it.
