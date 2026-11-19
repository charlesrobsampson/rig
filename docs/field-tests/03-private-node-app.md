# Field test 03: a real private Node app in a container

2026-11-19, rig at master `8b2e4c4` (task 73 merged). This was the first run of a real product repository through rig, ahead of a pilot on it.

## Setup

- A throwaway copy of a private TypeScript web app (React and Vite front end, a Node API server, Jest and `node --test`), with its git remote removed so nothing could be pushed. `node_modules`, `.env.local` and `.npmrc` were left out of the copy.
- Its dependencies include private packages from GitHub Packages. Locally they install with an `.npmrc` that references `${NODE_AUTH_TOKEN}`, set from `gh auth token` with the `read:packages` scope.
- The network inspects TLS through a corporate proxy, which re-signs `registry.npmjs.org` with its own root CA. The host trusts it through `NODE_EXTRA_CA_CERTS`.
- A separate rig home and config: the `oauth_token` profile, push mode `none`, container sandbox, Herdr. Ticket `ft3`, one service on its own branch.
- The plan told the agent not to change source or commit: install, run `npm run verify` (typecheck, lint, Jest, server tests), and journal every blocker.

## Runs

| Run | Change | Result |
|---|---|---|
| 0 | `build.dockerfile: Dockerfile` (the app's own) | The service image did not build (finding 1). |
| 1 | No Dockerfile; `docker.agent_base: node:24-slim` | The agent started, found node 24 and no token, ran `npm ci`, and failed on TLS to registry.npmjs.org. It left three clear `feedback` records. $0.08. |
| 2 | `docker.extra_ca_certs` = the proxy's root CA; `secrets: [NODE_AUTH_TOKEN]` from an env file; the agent wrote `.npmrc` with the reference | TLS worked. GitHub Packages returned 401: the test's env file held the literal text `${NODE_AUTH_TOKEN}`, copied from `.npmrc` by mistake. The agent named the cause without reading the value. $0.08. |
| 3 | A real token in the env file | The agent stayed in plan, rewrote `tasks.md`, and asked for implement through a `note` record (finding 4). After approval and a `rig msg`: `npm ci` installed 501 packages including the private ones in 44s; typecheck and lint clean; Jest 280 passed and 2 todo in 23 suites; server tests 68 of 68. Nothing committed; only the untracked `.npmrc` was left. $0.09. |

## Findings

| # | Severity | Finding | Suggested fix |
|---|---|---|---|
| 1 | blocker | rig cannot build a service Dockerfile that needs a BuildKit secret (`RUN --mount=type=secret,id=...`). The app's Dockerfile installs its private packages that way, so its image cannot be built, and its final stage is a runtime image anyway, not a toolchain. When the service image failed, rig still tried to build the agent image on top of it, which failed with "pull access denied" for the missing image, and printed the TLS proxy hint, which did not apply. | Task 90. |
| 2 | blocker (on such networks) | Behind a TLS-inspecting proxy every package download fails until `docker.extra_ca_certs` names the proxy's root CA. rig does not notice that the host already trusts one (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`). With it set, everything worked. | Task 88 (`rig setup`) offers the host's extra CA file. |
| 3 | high | A checkout cannot be given an untracked, ignored file such as `.npmrc`. The agent had to write it by hand. Worse, a clone does not get the main repository's `.git/info/exclude`, so the file was not ignored and a `git add -A` would have committed it. | Task 91. |
| 4 | high | The agent asked for a phase change through a `note` record, which rig does not act on. Runs 1 and 2 ignored the plan-phase rule and installed and ran tests right away; run 3 kept to it. | Tasks 74 and 76. |
| 5 | medium | `docker.agent_base` is global. One repository needs node and another Go, so the base must be per service when a service has no Dockerfile of its own. | Task 92. |
| 6 | low | Agent images pile up: ten `rig-agent:*` images of 1 to 1.6 GB after a few tests, and nothing removes old ones. | Task 93. |
| 7 | low | Only the untracked `.npmrc` remained, but `rig stop --force` asks for confirmation on a terminal and aborts without one, so a scripted cleanup cannot use it. | Note for task 80. |

## What worked

- The secrets env file check: a token file readable by others (mode 644) was refused with the exact `chmod` to run.
- `extra_ca_certs` is built into the agent image, and node, npm and curl all trusted it.
- Secrets by reference: the token reached the agent's environment and `.npmrc` named it; the agent never printed or read the value, and refused to work around a denied check.
- The journal: every blocker came back as a precise `feedback` record, and cost $0.06 to $0.09 per run.
- A full `verify` of a real app (typecheck, lint, 280 Jest tests, 68 server tests) runs inside a rig container in about two minutes.

## Not covered

Code changes, commits, review and push; `internal` network mode with the egress proxy (GitHub Packages and npmjs would need allowing); a dev server with a published port; the app's own agent configuration through `agent_config` (it has none yet).
