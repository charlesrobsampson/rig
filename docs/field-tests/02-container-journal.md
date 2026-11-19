# Field test 02: one container ticket, real Claude Code

2026-11-19, branch `task/72-agent-journal`. This was the manual check for task 72 (the journal from a container) and the first run of rig's container sandbox against a real agent.

## Setup

- The user's own rig home (`~/.rig`) with the default configuration: container sandbox, the Herdr terminal backend, and the gates `plan_approval` and `review_approval`.
- Auth: the `claude setup-token` token in the macOS Keychain.
- A throwaway repository at `/tmp/jtest`, with one file and one commit.
- Ticket `jt` ("journal test"), one service `app` with a clone checkout, on the branch build.
- Claude Code 2.1.328 in the agent container (the `debian:bookworm-slim` agent base).

## Result

The journal works from a container.

- The agent wrote a `feedback` record with the hook command from its prompt. It came in with source `container`.
- The agent's state still moved working → idle, and the cost line still arrived ($0.09). So the hook change from `echo` to `printf` and the records sharing the events file broke neither.
- **Restart:** after `rig daemon stop` and a fresh start, the journal still held exactly one record, and the offset was kept.
- **Older daemon:** with a daemon built from master running, `rig journal` named the older daemon and `rig daemon stop`, and exited 4.

The agent could not start on its own (finding 1), and git did not work inside its container (finding 2).

## Findings

Severity is as in field test 01.

| # | Severity | Finding | Suggested fix |
|---|---|---|---|
| 1 | blocker | Every new container agent stops at Claude Code's first-run screens: the theme picker, then notes, then trust for `/work` and for `--add-dir /rig/ticket`. The container's Claude configuration is new, so onboarding runs before any hook fires. rig reported "has not reported ready within 30s", and `rig list` showed agent state `unknown`. `rig msg` correctly refused to type into the half-started agent. | Task 81: the agent's Claude configuration comes with onboarding completed, a theme set, and trust for exactly `/work` and `/rig/ticket`. |
| 2 | high | git breaks inside the container when the repository's path goes through a symlink. On macOS `/tmp` is a link to `/private/tmp`. rig mounted the object store at its resolved path, `/private/tmp/jtest/.git/objects`. But the clone's `objects/info/alternates` names `/tmp/jtest/.git/objects`, which does not exist in the container. The agent got "bad object HEAD", so status, log and commit failed. | Task 84. |
| 3 | high | The agent was told to "request implement phase" and has no way to (again). | Task 74. |
| 4 | medium | The agent reported the contradiction between "edit only your checkout" and "refine plan.md" again, unprompted, through the journal. | Task 76. |
| 5 | low | With two dev builds, the older-daemon message reads "the running daemon (version rigd dev) … older than this rig (version dev)". The version strings are not the same shape, and two "dev" builds cannot be told apart. | Task 80: print the versions the same way, and for dev builds say the daemon was started from a different build. |

## What worked

- The prompt arrived whole inside the container (the `--` fix from field test 01).
- The journal: the record format, the container hook, exactly-once delivery across a restart, and `rig journal` filtering.
- The agent used the journal unprompted, and its feedback named a real bug (finding 2) clearly enough to find the cause in a few minutes.
- `rig msg` will not type into an agent that has not reported ready.

## Not covered

Implement, test, review and push in a container (git was broken); the `internal` network mode and the egress proxy; a service whose agent runs in the service's own image; multi-service tickets.
