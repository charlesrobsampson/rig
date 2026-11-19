# Field test 01: two dummy tickets, host sandbox, real Claude Code

2026-11-18, branch `task/72-agent-journal`. First run of rig against a real agent.

## Setup

- A separate rig home (`paths.home` under `$TMPDIR`, to keep the socket path short) and config: `sandbox: host`, `terminal_backend: tmux`, gates `plan_approval` and `review_approval`, push mode `none`, auth profile `host_login`.
- Two throwaway repositories, each a shell script with a `test.sh`, and a `.claude/settings.json` that allows the few commands the work needs (`sh test.sh`, `git add/commit/diff/status/log`, `rig agent-event`).
- Tickets: `greet-shout` ("add a --shout flag to greet.sh") and `calc-sub` ("add a sub command to calc.sh"), with `ANTHROPIC_MODEL: claude-sonnet-5-5` in `env`.
- Claude Code 2.1.327. Docker was not running, so container mode was not tried.
- The human side was played from the CLI: `rig gates/approve/phase/msg/tests/review/push/stop`, with permission prompts answered in the tmux pane.

## Result

Both tickets went from `plan` to `done`. Cost: $0.22 and $0.15. That needed one fix during the run (finding 3) and several nudges by hand (findings 7, 9, 10, 11).

## Findings

Severity: **blocker** stops the work, **high** needs a human to notice something rig should tell them, **medium** friction, **low** polish.

| # | Severity | Finding | Suggested fix |
|---|---|---|---|
| 1 | blocker (fixed) | The agent never got its prompt: Claude Code reads `--add-dir` as taking several values, so the prompt after `--add-dir <ticket dir>` became a second directory ("was read as a value of --add-dir, not as a prompt"). Every ticket was affected, host and container. | Fixed in this branch: the prompt follows `--`. |
| 2 | high | Every new host ticket stops at two Claude Code trust prompts before any hook runs: one for the worktree, one for `--add-dir <ticket dir>`. `rig list` shows `idle`, not `blocked`, so nothing tells the human. | Treat a fresh agent that has not reported `ready` within a few seconds as needing attention, or recognise the trust dialog in the screen heuristic as blocked. Document trusting the tickets directory once. |
| 3 | high | The prompt says "request the implement phase" but gives no way to do it. Both agents looked for a command (`rig --help`), found none, and asked in chat or journaled a note. A container agent has no `rig` binary at all. | Give agents a documented request path that works in both sandboxes. A journal record of kind `transition.request` (`data: {"to": "implement"}`) that the daemon turns into `transition.request` would reuse the channel from task 72. Name it in the prompt. |
| 4 | high | A transition request is invisible. After planning, the ticket showed `plan` and `idle`; nothing said a plan was waiting for approval. | Show a pending request as APPROVE on `rig list` and in the TUI. Follows from 3. |
| 5 | high | Moving the phase does not tell the agent. After `rig phase … implement` it sat idle with its plan-phase instructions until messaged. It also never received the implement-phase rules (manual tests, committing), because the prompt is built once, at launch. | On `phase.changed`, type a notice with the new phase's rules into the agent, the way contract notices are typed in today. |
| 6 | medium | The common rule "Edit only files inside your own service's checkout" contradicts the plan phase ("Refine plan.md and tasks.md"), which live in the ticket directory. Both agents reported this independently through the journal. | Reword: "your checkout, and the ticket's plan.md, tasks.md, manual-tests.md and contract proposals". |
| 7 | medium | The agent implemented and tested but did not commit ("I did not commit"). Claude Code's own default, commit only when asked, beat one line of the launch prompt. | Make committing explicit and give the reason ("rig imports your branch; uncommitted work is not reviewed or pushed"). Warn at `phase test` or `review` when the checkout is dirty. |
| 8 | medium | The starter `manual-tests.md` assumes an HTTP service. Its introduction and example use `{{svc.url}}`/`{{svc.port}}`, so for a service with no port `rig tests` refuses to render anything, introduction included. | No live placeholders in the starter's prose. Pick the example by whether the service has a port. |
| 9 | medium | `rig stop` on a host-only ticket fails when Docker is not running ("stop the egress proxy: docker unavailable", "list its containers"). It stops the agent, then keeps the checkouts and reports failure, although the ticket never had a container or network. | Skip Docker cleanup when `state.json` records no containers, network or proxy. Or treat "Docker down, nothing recorded" as nothing to remove. |
| 10 | medium | Host agents inherit the user's personal Claude Code configuration (`~/.claude/CLAUDE.md`, skills, settings). Here the agents answered in the user's personal writing style. A ticket agent should not pick up personal instructions by accident. | Say so in the host-mode warning, and offer a clean configuration (a per-ticket `CLAUDE_CONFIG_DIR`) as an option. Relates to task 73. |
| 11 | low | `rig review` pipes the diff through delta with colours even when stdout is not a terminal. | Plain patch output when stdout is not a TTY. |
| 12 | low | A `done` ticket keeps its agent running (`provisioned/idle`), and nothing suggests stopping it. | After `push` finishes, say "run rig stop <id> to stop the agent and remove the checkout", or offer to stop it. |
| 13 | low | A daemon left running from an older binary answered "unknown method journal.list", which reads like a typo. | Fixed in this branch: the client names the older daemon and `rig daemon stop`. |

## What worked

- After the fix, every phase worked from the CLI: plan, implement, test, review, push (`none`), done.
- `blocked` was accurate for permission prompts. `idle` and `working` matched the screen. The cost readout updated live.
- The gates refused nothing they shouldn't have, and approvals behaved as documented.
- `rig tests` was clear for ticking steps and signing off, and its errors list every problem at once.
- `rig review` opens with a useful stat header. Push mode `none` says exactly where the branch is.
- **The journal (task 72) worked on first contact.** Unprompted, both agents journaled a `note` (plan ready, work done) and a `feedback` record, and the feedback was correct (finding 6). `rig journal --kind feedback` collected both across tickets.

## Not covered

Container sandbox (Docker was off), `internal` network mode and the egress proxy, multi-service tickets and `contract.md`, push modes `pr` and `merge`, the TUI, the MCP server, a daemon restart while agents run, and Herdr.
