# Spike 03: tmux state detection and message injection

**Questions**
1. tmux has no native `working | blocked | idle`. How does rig learn an agent's state?
2. How does rig push a message into a running agent (contract-change notices, manifest §3) without corrupting it?

Tested with tmux **3.8** (private socket `-L`), Claude Code **2.1.319**, macOS arm64. Probe: `spikes/tmux/probe.sh` (offline, no tokens). Claude experiments ran in the (trusted) repo directory.

## Answers
1. **State comes from Claude Code hooks, not from scraping the screen.** Rig launches Claude with `--settings <file>` that adds hooks which report state to rig. This works identically under tmux and Herdr, needs no change to the user's own config, and is far less fragile than parsing the TUI. A screen heuristic stays as a fallback.
2. **Injection is `send-keys -l <text>` (or a bracketed paste) followed by a separate `Enter`, but only when the agent is not `blocked`.** Claude Code queues text typed while it is working and delivers it intact. Injecting while a permission prompt is showing is **dangerous**: the Enter approved the tool call in my test.

## 1. tmux building blocks (all verified)
| Need | Command | Result |
|---|---|---|
| Isolated server | `tmux -L <sock> new-session -d -s <ticket> -c <dir> -x 200 -y 50` | Own socket, never touches the user's tmux. |
| Window with cwd, env, command | `new-window -d -t <ticket> -n <service> -c <dir> -e K=V '<cmd>'` | Works; `-d` avoids switching focus. |
| Read screen | `capture-pane -p -t <ticket>:<service>` | Plain text. |
| Liveness | `list-panes -F '#{pane_dead} #{pane_dead_status} #{pane_pid} #{pane_current_command}'` | |
| Keep dead panes | `set-option -g remain-on-exit on` | Without it a finished pane's window vanishes; with it `pane_dead=1` and the exit status is readable (`exit 3` → `3`). **Global option, not session-scoped.** |
| Attach | `tmux -L <sock> attach -t <ticket>` | |

## 2. State detection

### Option A: Claude Code hooks via `--settings` (recommended)
`claude --settings <file-or-json>` loads extra settings for that process only. Hooks fired and observed, with timings:

| Hook event | Fires | Maps to |
|---|---|---|
| `SessionStart` | when the TUI is up | ready (safe to inject from here) |
| `UserPromptSubmit` | when a prompt is submitted (typed, injected, or queued text delivered) | `working` |
| `PreToolUse` / `PostToolUse` | around each tool call | `working` |
| `PermissionRequest` | **immediately** (about 0.1 s) when a permission dialog appears | `blocked` |
| `Notification` (`permission_prompt`) | **about 6 s late**, and an `idle_prompt` notification also fires ~1 s after a permission dialog | secondary signal only |
| `Stop` | when the agent finishes a turn | `idle` |
| `SessionEnd` | on exit | `exited` |

- Use `PermissionRequest` for `blocked`, not `Notification` (the latter is slow and ambiguous).
- Hook commands receive the event as JSON on stdin (`hook_event_name`, `tool_name`, ...).
- Proposed channel: the hook command is `rig agent-event <state>`, a tiny CLI subcommand that reads `RIG_TICKET` and `RIG_SERVICE` from the environment (set by the harness at launch) and sends an `agent.event` request to the daemon. If the daemon is unreachable it must exit 0 quickly and never block or fail the agent.
- Config sketch: `spikes/tmux/hooks-settings.json`.
- Works for any terminal backend, so the supervisor can use it for Herdr too (cross-check against Herdr's own detection).

### Option B: screen heuristic (fallback)
Against Claude Code 2.1.319's TUI:
- `esc to interrupt` in the status line → working.
- A permission dialog (`Do you want to proceed?` with a numbered list) → blocked.
- Neither → idle (the last turn prints `✻ <verb> for Ns · done <time>`).

Works without any hook setup and for other agents with their own markers, but the strings are UI text and will change between versions. Treat as best-effort.

### Recommended split
- `TerminalBackend.State(pane)` returns backend-level facts only: pane alive/dead with exit status, and an optional heuristic state.
- The harness owns the **agent-state channel** (hooks for claude-code); the supervisor (task 30) merges: fresh hook state wins, backend/heuristic state fills gaps, a dead pane always means `exited`.

## 3. Message injection

| Method | Result |
|---|---|
| `send-keys -l "<text>"` | Literal text. **Newlines inside the text are inserted as newlines in Claude's input box and do not submit.** Quotes, `$vars`, backticks, tabs and a literal `\n` all arrived byte-exact (verified into `cat` and into Claude). |
| `send-keys Enter` | Submits. Send it as a **separate** call. |
| `load-buffer` + `paste-buffer -p -d` (bracketed paste) | Also works; Claude shows `[Pasted text #1 +3 lines]` and submits the full content on Enter. Preferred for large or arbitrary text and for harnesses that treat raw newlines as Enter. |

**While the agent is working:** Claude Code queues the message. The screen shows the text and `Press up to edit queued messages`; it is delivered to the model at the next boundary (observed: injected during a 15 s `sleep` tool call, the model's reply covered both prompts: "finished queued"). No corruption, no lost input. Hooks fired `UserPromptSubmit` for the queued message at submit time.

**While blocked on a permission prompt: never inject.** I typed text and pressed Enter into a `Do you want to proceed?` dialog. The text was ignored but Enter picked the default "1. Yes", and the tool ran (`PostToolUse` fired, the file was created, mtime equal to the Enter). The same hazard exists with Herdr `send-text` + Enter.

### Injection rules for `agent.message` (task 32)
1. Refuse (or queue in rig) when state is `blocked` or the pane is dead. Never send digits, Enter, or Escape to a blocked agent.
2. Wait for `ready` (the `SessionStart` hook) before the first injection. Text typed before the TUI is up is lost (Claude took ~6 s to start).
3. Send the text, then Enter, as separate calls, with a short pause (0.3–0.5 s) so the TUI registers the text first.
4. When `working`, injecting is safe: Claude queues it. Prefer injecting at `idle` for messages that must be seen next.
5. Use `send-keys -l` for single-line text; use bracketed paste for multi-line or large text.
6. Known limitation: if a human is attached and mid-typing, injected text interleaves with theirs. Mitigation: prefix notices with a clear marker (`[rig] ...`), and keep them short.

### Other channels considered (not tested)
- **Hook-delivered messages** (`PostToolUse` / `UserPromptSubmit` can return extra context; `Stop` can block with a reason to make the agent continue). This would deliver a notice without keystrokes and without any blocked-state hazard, but only at those moments. Worth revisiting if keystroke injection proves flaky; it uses the same `--settings` mechanism.
- Claude's non-interactive `-p`/stream-json mode has no way to reach a running TUI session.

## 4. Container note (task 40)
Hooks run where the agent runs. If the agent is in a container, `rig agent-event` must reach the daemon from inside: either mount the daemon socket, or the hook writes events to a file under a mounted directory that the daemon watches. Decide in task 40; the host-mode path is unaffected.

## 5. Decision
- tmux backend: private socket, one session per ticket, one window per service, `remain-on-exit on`, liveness and exit status from `list-panes`.
- Agent state: hooks channel (`--settings` + `rig agent-event`) as primary, screen heuristic as fallback, merged in the supervisor.
- Injection: rules in §3, shared by tmux and Herdr backends.
- Formats additions made (additive): `agent.event` API method. Manifest open question on injection resolved.

## 6. Impact on later tasks
| Task | Change |
|---|---|
| 26 | `TerminalBackend.State` returns liveness + optional heuristic; contract suite covers dead-pane exit status. |
| 28 | tmux backend per §1; heuristic per §2B; `remain-on-exit on`. |
| 29 | Harness adds `--settings` hooks and sets `RIG_TICKET`/`RIG_SERVICE`; needs the `rig agent-event` subcommand (add to task 30 or 31). |
| 30 | Supervisor merges hook events and backend facts. |
| 32 | Implements the injection rules in §3. |
| 40 | Decide how hook events cross the container boundary (§4). |
