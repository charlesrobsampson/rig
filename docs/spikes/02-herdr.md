# Spike 02: Herdr integration

**Question:** can Herdr be the primary `TerminalBackend` (manifest §7)? It must spawn panes with a cwd and command, report `working | blocked | idle`, accept injected text, and close panes, all scriptable from a daemon.

**Answer: yes.** Herdr covers every required operation, has a clean local socket API, and answers in well under a millisecond. There are a few quirks the backend has to handle (listed below). Tested with Herdr **0.9.5**, protocol 22, macOS arm64.

Probe script: `spikes/herdr/probe.sh` (self-contained, uses a private session, cleans up). Raw schema: `herdr api schema --output <path>`.

## 1. How rig should talk to Herdr

| Option | Verdict |
|---|---|
| Shell out to `herdr` CLI | Works, ~10–25 ms per call, JSON on stdout. Fine for a fallback and for debugging. |
| **Unix socket API (JSON lines)** | **Recommended.** Sub-millisecond, supports streaming events, no process spawn. |

- Socket protocol: one JSON object per line. Request `{"id","method","params"}`; response `{"id","result":{...}}` or `{"id","error":{"code","message"}}`.
- The socket path comes from `herdr session list` (default session: `~/.config/herdr/herdr.sock`; named session: `~/.config/herdr/sessions/<name>/herdr.sock`).
- 102 methods exist (`herdr api schema --json`). Rig needs about 12 (table in §4).
- Measured on the socket: `ping` 0.1 ms, `pane.list` 0.4 ms, `pane.send_text` 0.1 ms, `agent.get` 0.2 ms. Through the CLI: ~20 ms. Spawning `agent start` for Claude Code takes ~3.7 s (it waits for the agent to become ready).

## 2. Use a dedicated named session
- `herdr --session rig` / `herdr --session rig server` creates an isolated server with its own socket. Rig must use its own session (default name `rig`) so it never mixes with the user's default workspaces.
- The user attaches with `herdr session attach rig` and sees every ticket's agents as workspaces/tabs/panes. That is the "jump into an agent" experience, for free.
- `herdr --session <name> server` runs headless, no TUI needed, and survives the process that started it. Verified: panes keep running with nobody attached.
- Operating on a session that isn't running does **not** start it: error `server_not_running` ("run `herdr session attach <name>` to start or attach it"). Rig must start the headless server itself (`herdr --session rig server`) when it's absent, then wait for the socket.
- Stop with `herdr session stop <name>` or `server.stop`.

## 3. Findings per required operation

| Operation | CLI | Result |
|---|---|---|
| Spawn with cwd | `workspace create --cwd D --label L --no-focus`, `tab create --workspace W --cwd D` | Works. Returns workspace/tab/root pane ids (`w1`, `w1:t1`, `w1:p1`). `--no-focus` avoids stealing focus. |
| Run a command | `pane run <pane> "<cmd>"` | Types the command and presses Enter in the pane's shell; silent on success. |
| Start a known agent | `agent start <name> --kind claude --pane <pane> [-- args]` | Launches Claude Code, waits until interactive-ready. |
| List | `pane list`, `agent list` | `agent list` returns only panes that have a detected/reported agent. |
| Read state | `agent get <name-or-pane>` | `agent_status`: `idle | working | blocked | done | unknown`. |
| Read output | `pane read <pane> --source visible|recent|recent_unwrapped|detection` | Plain text (ANSI stripped by default). |
| Send text | `pane send-text <pane> "<text>"` | Literal text, **no Enter appended**. Multiline, quotes, `$vars`, backticks, tabs and a literal `\n` all arrived byte-exact (verified with `od -c`). |
| Send keys | `pane send-keys <pane> Enter ctrl+d ...` | Key names are lowercase with `+` (`ctrl+d` works; `Ctrl-d`, `C-d` are `invalid_key`). |
| Wait | `agent wait <t> --until idle --until done --timeout ms`; `pane wait-output <pane> --match RE --timeout ms` | Works. Multiple states need repeated `--until` (comma form is rejected). Timeout returns error code `timeout`. |
| Events | socket `events.subscribe` | Streams `pane.agent_status_changed` etc. Needs a `pane_id` per subscription. Delivered ~immediately after the change. |
| Close | `pane close <pane>`, `workspace close <ws>` | Works. Later calls on the pane return `pane_not_found`. |

## 4. Recommended `TerminalBackend` surface (Herdr mapping)

| Interface method | Herdr call |
|---|---|
| `EnsureReady()` | `ping` on the session socket; if `server_not_running`, run `herdr --session rig server` and retry |
| `Spawn(ticket, service, cwd, cmd, env)` | `workspace create` (once per ticket, label = ticket id) then `tab create` (label = service) then `pane run`; for a known agent, `agent start` |
| `List()` | `pane list` + `agent list` |
| `State(pane)` | `agent get` → map `done` → idle-ish (see quirks) |
| `Send(pane, text)` | `pane send_text` then `send_keys Enter` (separate, so rig controls submission) |
| `Close(pane)` | `pane close` |
| `AttachCommand(ticket, service)` | `herdr session attach rig` (plus `workspace focus`/`tab focus` first) |
| `Subscribe(panes)` | `events.subscribe` per pane, with polling as the fallback |

## 5. Quirks the backend must handle
1. **`done` vs `idle`.** `done` means "finished and not yet seen by a human"; it becomes `idle` once seen. Treat `done` as idle for rig's `AgentState`, and wait on both (`--until idle --until done`).
2. **External state reports need a known agent kind.** `pane report-agent <pane> --source rig --agent claude --state working` is retained; with a made-up label such as `fakeagent`, the agent record **vanishes within ~1 s** (`agent_not_found`). So the fake agent (task 26b) cannot report under a custom label to real Herdr. Options: the fake agent reports as `claude`, or integration tests drive a real shell process and assert on pane output. Default tests use the fake backend and are unaffected.
3. **`wait-output` matches the command echo.** `pane run "echo MARK"` makes `MARK` appear in the echoed command line immediately, so the wait matches too early. Match on text that does not appear in the typed command.
4. **`send-text` does not submit.** Always follow with `send-keys Enter` when the target expects a line.
5. **Claude Code trust dialog.** A fresh directory shows "Is this a project you trust?" and Herdr correctly reports `blocked`. Rig-created worktrees are new directories, so every first launch will block until trusted. This needs a decision in the harness task (29): pre-trust the worktree path, or surface it as a "needs me" state for the human. I did not accept the dialog (it writes to the user's Claude config).
6. **Errors go to stderr** in the CLI, results to stdout. Error codes seen: `pane_not_found`, `agent_not_found`, `timeout`, `invalid_key`, `invalid_request`, `server_not_running`, `not_git_worktree`.
7. **Agent detection manifests are fetched remotely** (`herdr server agent-manifests` shows a "remote" source and a last-check time). Detection quality can change when Herdr updates its manifests; pin nothing, but treat state as best-effort and keep the supervisor tolerant (task 30).
8. **`herdr integration install claude`** installs hooks into Claude Code for higher-fidelity state. This edits the user's agent config, so rig should not do it silently; document it as an optional step.
9. Herdr has its own `worktree create/list/open/remove`. Rig does **not** use it: rig owns worktrees (task 17–22) so behavior is identical across terminal backends.

## 6. Failure modes
| Situation | Behavior | Rig response |
|---|---|---|
| Herdr not installed | `herdr: command not found` | `unavailable`, with an install hint |
| Session server not running | `server_not_running` | start it, retry once |
| Pane closed or died | `pane_not_found` | supervisor marks service `exited` |
| Agent not detected | `agent_not_found` | state `unknown`, not an error |
| Wait timeout | `timeout` | caller decides |
| Protocol drift after Herdr update | `invalid_request` | `unavailable` with the Herdr version in the message |

## 7. Decision
- Herdr is the primary backend, accessed over the **socket API** in a **dedicated `rig` session**, with the CLI as a debugging aid.
- Backend interface per §4; quirks per §5 become backend behavior and contract-suite cases.
- Not blocked on anything: no contradiction with `manifest.md`. Small manifest additions made (named session, socket API).

## 8. Real Claude Code check (done)
Run in a directory Claude already trusts (the repo), via `agent start rigclaude --kind claude --pane <pane>`:

| Step | Observed |
|---|---|
| after `agent start` (about 3.7 s) | `idle`, `interactive_ready: true` |
| `pane send-text` of the prompt, before Enter | still `idle`; text sits in the input box |
| `pane send-keys Enter` | `working` within 1 s |
| Claude replies (about 1 s) | `done`, and it **stays** `done` while nobody is attached (see quirk 1) |
| Screen | prompt arrived intact and Claude answered it |

Conclusion: state detection, injection, and the send-text-then-Enter split all work against real Claude Code. The trust dialog (quirk 5) only appears in untrusted directories.
