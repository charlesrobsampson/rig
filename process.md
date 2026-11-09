# Process: how to complete a task

Read this fully before touching code. Tasks live in `checklist.md`. Design lives in `manifest.md` (what and why). Exact shapes (files, API, events, enums, naming) live in `docs/formats.md`. If the two conflict, `manifest.md` wins on intent and `docs/formats.md` wins on exact shape; tell the user about the conflict. Section references like "manifest §4" or "formats §5" point to numbered headings.

## 0. Orientation (read once)
- **What rig is:** a local daemon (`rigd`) plus CLI/TUI that runs several AI coding agents in parallel, each isolated by a git worktree, a Docker sandbox, and markdown lifecycle files. Every client (CLI, TUI, MCP) talks to the daemon over one local socket API.
- **Key invariants:** (1) `state.json` in the ticket dir is the truth; the store is a rebuildable cache. (2) Agents never edit `state.json`, `ticket.yaml`, or the agreed section of `contract.md`. (3) Git push credentials never enter a container. (4) Gates are human-only; no code path lets an agent approve one. (5) Tests never launch a real agent, Docker, or network.
- **When blocked or unsure:** stop and ask the user. Do not invent a shape, skip a failing test, or widen scope. A short question beats a wrong guess.

## Commands: use Taskfile
All project scripts run through [Task](https://taskfile.dev) via `Taskfile.yml` at the repo root (`brew install go-task`). Use `task <name>` instead of raw `go`/`docker` commands for anything it covers, and put any new repeatable script there (not in ad-hoc shell snippets or README prose). `task --list` shows everything.

| Task | Does |
|---|---|
| `task build` | build `./rig` |
| `task build:fakeagent` | build `bin/fakeagent`, the scripted stand-in agent for integration tests |
| `task test` | default tests (offline, no tags) |
| `task test:race` | default tests with `-race` |
| `task test:integration -- <pkgs>` | tests tagged `integration` (real tmux/Herdr); default `./...` |
| `task test:docker -- <pkgs>` | tests tagged `docker` (real Docker) |
| `task test:llm -- <pkgs>` | tests tagged `llm` (local Ollama) |
| `task vet`, `task fmt`, `task fmt:check` | vet; format; verify formatting without changing files |
| `task check` | `fmt:check` + `vet` + `test` (the pre-commit gate) |
| `task tidy` | `go mod tidy` |
| `task clean` | remove build output |

Taskfile is development tooling only. The shipped `rig` is a single normal Go binary (`go build ./cmd/rig` must always work on its own); no rig code, test, or runtime path may require Task to be installed.

Task files must stay simple: each task is a short, readable list of commands, with no logic that belongs in Go. Anything non-trivial goes in a Go program under `tools/` and the task just runs it.

## 1. Start
1. Pick the first unchecked task in `checklist.md` whose dependencies are all checked **and merged to `master`**. If a dependency is not merged, stop and tell the user.
2. On `master`, write your session into the task's `Session:` line (`claude --resume <session-id>`) and commit `start task NN`. The session id is the directory name of the current session under `~/.claude/projects/-Users-charley-go-src-github-com-charlesrobsampson-rig/`.
3. Create the branch named in the task (`task/NN-slug`) from `master`.
4. Read the task's `Read:` files and the manifest/formats sections it names, plus any `Notes:` left by the tasks it depends on. Do not read everything else.
5. If the task is ambiguous or the manifest contradicts itself, stop and ask the user. Do not guess.

## 2. Work
- **Stay in scope.** Do only what the task lists. If you find extra work, append a new task to `checklist.md` (in order, with dependencies) instead of doing it. If a decision in `manifest.md` changes, update `manifest.md` in the same branch.
- **TDD where applicable.** Write the failing tests first, then the code. Some tasks (spikes, wiring, UI polish) have no sensible tests; the task says so.
- **Don't break anything.** `task check` (format check, vet, tests) must be green before every commit. Never delete or weaken an existing test to make it pass. **Formats:** additive changes (a new field, API method, event, or enum value) are fine; add them to `docs/formats.md` in the same branch. Renaming, removing, or changing the meaning of anything in `docs/formats.md` needs the user's approval. Code must match `docs/formats.md` exactly; never invent a shape that isn't there.
- **Every user-visible command supports `--json`.** The CLI is a thin client of the daemon API (manifest §6). Logic lives behind the API, never in `cmd/`.

## 3. Code style
- Human readable and self-documenting: clear names, small packages, obvious flow.
- Minimal comments. Comment only *why* something is done instead of the obvious alternative. Never narrate what the code does.
- Reuse where it makes sense. No one-off helpers, and no functions that wrap a few lines used once; inline them. Extract only when logic is shared or the abstraction is a real boundary.
- Follow the package layout in manifest §6. External systems (Docker, git hosting, terminal backends, ticket sources, harnesses, summarizers, store) go behind the interfaces in manifest §6. Packages depend on interfaces, never on a concrete backend.
- Errors: wrap with context (`fmt.Errorf("start ticket %s: %w", id, err)`), return, handle once at the edge. No panics outside `main`.
- Concurrency: every goroutine has an owner and a stop path (context cancellation). The daemon must shut down cleanly with no leaks.
- Pure Go where sensible. No CGo without the task saying so.
- Never commit secrets, real tokens, real ticket data, or personal paths. Fixtures must be scrubbed or synthetic.

## 4. Tests
- **Table-driven, user-style, integration-leaning.** Drive public contracts (CLI entrypoint with args, daemon API calls, TUI key presses, store via its public API) and assert observable results. Avoid testing internals, so refactors don't break tests.
- **Fully offline and self-contained.** Default tests touch nothing outside the test process: no network, no real Docker, no real Herdr/tmux, no real `gh`/Jira/GitHub, no real `HOME` or config. Use temp dirs. They must pass with the network off and Docker stopped.
- **Real git is allowed.** Worktree and diff logic is tested against real `git` on throwaway repos created under a temp dir (helper in `internal/testkit`). Set `GIT_CONFIG_GLOBAL=/dev/null` and a fixed author so results are deterministic.
- **Fakes over mocks.** For each external system a test needs, build a small in-memory fake with real behavior in `internal/testkit`: if the real thing can create, change and delete state, the fake can too and tests observe that state. Examples: fake Docker (networks, containers, ports, logs, health), fake terminal backend (panes, state transitions, received input), fake clock (advance time), fake `gh` (records PRs), fake ticket source, `httptest` servers for Ollama/Claude/Jira/GitHub. Build only what the current tasks need. Do not mock individual functions.
- **Contract suites.** An interface with several implementations (e.g. `TerminalBackend`) has one shared test suite in its package that every implementation, including the fake, must pass.
- **Real-backend tests are opt-in.** Tests that need real Docker, Herdr, tmux, or network live behind build tags (`docker`, `integration`, `llm`) and are run only by the task's **Manual check** (via `task test:integration`, `task test:docker`, `task test:llm`). Never in the default `task test`.
- **No agent or LLM calls in tests.** Never launch Claude Code or any real agent, and never call a hosted model API. Where a test needs an agent, use the scripted fake agent (`internal/testkit/fakeagent`): a small program driven by a script (write file, commit, append a contract proposal, go idle, echo injected input to a file). Prompt/harness tests assert the prompt text and command only, not model behavior. Whether a real agent follows the prompt is a manual check.
- **Local LLMs only, and only opt-in.** The one place a model is useful in tests is the summarizer. Default tests use an `httptest` fake. A test against real Ollama is allowed behind the `llm` build tag (free, local), never in default `task test` or CI.
- **Time from the injected clock** (`internal/clock`), never `time.Now()` or `time.Sleep` in logic.
- **Sockets:** unix socket paths have a ~104-char limit on macOS. In tests create the socket dir with `os.MkdirTemp("", "rig")`, not `t.TempDir()`.
- Each task lists the cases its tests must cover. Add more if you find gaps.

## 5. Finish
1. Run the task's **Manual check** yourself when possible (spikes, real Docker, and UI tasks often need the user; say so).
2. `task check` green, and `task test:race` green for any task touching goroutines. No new default test depends on the network, Docker, or a real agent. `git status` clean apart from the commits.
3. Tick the task's checkbox in the final commit. Fill in anything the next task needs to know under the task's `Notes:`.
4. Commit in small, meaningful commits. End each commit message with the attribution lines the harness specifies.
5. Do not merge. Tell the user the branch is ready, summarize what changed, and list the manual check.

## 6. Spike tasks
Spikes answer a question. Write findings and the decision to `docs/spikes/NN-name.md` (what you tried, what worked, what didn't, the chosen approach, and any numbers). Keep throwaway code under `spikes/` or delete it. If the answer contradicts `manifest.md`, update `manifest.md` and tell the user before continuing. Later tasks that depend on a spike read its doc, not the spike code.
