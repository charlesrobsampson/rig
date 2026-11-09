# Supervising rig from an agent (MCP)

`rig mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server on
standard input and output. It lets a supervising agent (a Claude Code session, say) watch
rig's coding agents and nudge them, with the same daemon API the CLI and TUI use. It is a
thin wrapper: every tool below is exactly one daemon call (`ticket.get` also reads
`gate.list`), so what an agent can do through it is what a CLI user can do, minus every
decision.

## Pointing a supervising agent at it

Claude Code:

```sh
claude mcp add rig -- rig mcp
```

Any MCP client that launches stdio servers takes the same command:

```json
{ "mcpServers": { "rig": { "command": "rig", "args": ["mcp"] } } }
```

Use the full path to `rig` if it is not on the client's `PATH`; add `"--config", "<file>"`
to the arguments to use a different rig config. The server starts the rig daemon on its
first tool call if it is not running, and survives daemon restarts (it makes one
connection per call). Standard output carries only protocol messages; diagnostics go to
standard error.

## Tools

| Tool | Arguments | Does |
|---|---|---|
| `rig_list_tickets` | none | every ticket: phase, services (agent state, host port), pending contract change, the approval a human still owes, any summary |
| `rig_get_ticket` | `ticket` | one ticket's summary row, recorded state (phase, per-service agent state, approvals, contract and manual-test progress) and the gates it needs |
| `rig_read_artifact` | `ticket`, `name` (`plan.md`, `tasks.md`, `contract.md`, `manual-tests.md`) | the text of that file; no other file can be read |
| `rig_send_message` | `ticket`, `service`, `text` | types the text into that service's running agent, as `rig msg` does; refused while the agent waits on a permission prompt |
| `rig_request_transition` | `ticket`, `service`, `to`, `reason?` | asks for a phase move on behalf of a service, under exactly the human's rules: a forward move whose gate no human has approved is refused (`blocked`, with the missing approvals listed); a backward move needs a reason |
| `rig_propose_contract_change` | `ticket`, `service`, `what`, `why` | appends an open proposal to a multi-service ticket's `contract.md`; the other services' agents are told, and a human decides |

Arguments are checked strictly: a missing or blank required argument, a wrong type, or an
argument the tool does not define is an error and nothing is sent to the daemon.

A failed call is a tool result with `isError: true`, not a protocol error, so the model
can read it and react: daemon errors read `rig error (<code>): <message>` followed by the
reasons, one per line (for example the approvals a refused transition is waiting for).
Protocol errors are only for malformed requests and unknown tools.

## What it cannot do

There is no tool to approve a gate, set a phase around the gates, accept or reject a
contract change, tick off or sign off manual tests, push, stop or start a ticket, create
tickets, or change any configuration. Those stay with the human, in the CLI or the TUI.
A supervising agent can *ask* (request a transition, propose a change, message an agent)
and *read*; it can never grant. `rig_request_transition` in particular is judged exactly
as a human's move would be, so it cannot be used to skip a gate.

The server acts with your own authority on your own daemon socket; it is not a boundary
against someone who can already run `rig` as you. Do not expose it over a network.

## A typical supervising session

1. `rig_list_tickets`: which tickets need attention (a blocked agent, a pending gate, a
   contract change).
2. `rig_get_ticket` and `rig_read_artifact` on the interesting one: where is it, what does
   `plan.md` / `tasks.md` say.
3. `rig_send_message` to an agent that has stalled: "the test failure is in `auth_test.go`;
   rerun after fixing the fixture".
4. `rig_propose_contract_change` when two services disagree, then tell the human to run
   `rig contract accept` or `reject`.
