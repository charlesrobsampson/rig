# Spike 04: agent inside a container

**Question:** is it practical to run the agent in a container by default (manifest §4)? What must the image contain, how do files, git, TTY, and credentials work, and how fast is it?

**Answer: yes, with four things rig must handle:** a corporate TLS-inspection CA, git metadata for worktrees, a per-service image overlay, and credentials passed as env. Tested with Docker Desktop 29.9.2 (VirtioFS, arm64), Claude Code 2.1.319, macOS. Probe: `spikes/container/probe.sh` (needs the image from `spikes/container/Dockerfile`). Overlay example: `spikes/container/overlay.Dockerfile`.

**Not verified (needs a login):** an authenticated Claude turn inside a container. Everything up to "Not logged in" was verified (§7). See "Open items".

## 1. Image strategy
The agent needs the service's toolchain (to run its tests) plus Claude Code and git. Two options were built:

| Image | Build | Size | Notes |
|---|---|---|---|
| Generic: `node:22-bookworm-slim` + git + `npm i -g @anthropic-ai/claude-code` | 21 s cold, 0.7 s warm | 1 GB | Fine for repos that only need Node. |
| **Overlay: `FROM <service toolchain image>` + git + native Claude installer** | 53 s cold | 2.27 GB (golang:1.24 + about 340 MB Claude) | **Recommended.** No Node needed in the service image; Go 1.24 and Claude both ran as an arbitrary non-root uid. |

- Rig builds `rig-agent-<ticket>-<service>` as an overlay on the service's base image (the service Dockerfile's final or build stage, or a configured `agent_base` image) and caches it by base-image digest + Claude version. Warm start is the cached image: container creation to shell is about 0.19 s.
- Image must contain: git, ca-certificates, the corporate CA (below), ripgrep/procps (Claude uses them), a writable `HOME` for any uid (`mkdir /home/agent && chmod 1777`), `WORKDIR /work`.
- Run as the host user's uid (`--user $(id -u):$(id -g)`); the missing `/etc/passwd` entry is cosmetic (`I have no name!` prompt). Git needs identity via `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env.

## 2. Corporate TLS inspection (Zscaler on this machine)
The first image build failed: `npm error UNABLE_TO_GET_ISSUER_CERT_LOCALLY`. HTTPS from containers is re-signed by Zscaler, whose root CA is in the macOS keychain but not in the image.
- Fix verified: export the (public) root certificate and bake it in: `security find-certificate -a -c "Zscaler Root CA" -p /Library/Keychains/System.keychain > corp-ca.pem`, then `COPY` + `update-ca-certificates`, and set `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `CURL_CA_BUNDLE`. After that npm, the native installer, and `curl https://api.anthropic.com` all work.
- Rig design: global config `docker.extra_ca_certs` (list of PEM paths) used at image build, and mounted read-only at run time for any service that reaches the network. Never committed (`spikes/**/certs/` is git-ignored). A machine without inspection simply has none.
- Interacts with the egress proxy (spike 05): the proxy must not break these CA settings.

## 3. Git inside the container (decision needed)
A git worktree's `.git` is a **file** pointing at a host path (`gitdir: /abs/host/path/repo/.git/worktrees/wt`). Results, all verified:

| Setup | Result |
|---|---|
| Mount the worktree only | **Broken**: `fatal: not a git repository: /abs/host/path/...` |
| Worktree + main `.git` mounted rw **at the same real host path** | Works; commits land on the branch visible on the host. Path must be the `realpath` (macOS temp/symlinked dirs: `/var` vs `/private/var` broke my first attempt). Worktree mounted at `/work` or at its host path both worked; the host path is safer for `git worktree` metadata. |
| …plus `hooks/` and `config` mounted **read-only** over the rw `.git` | Commits still work; writing a hook gives `Read-only file system`, writing config gives `Device or resource busy`. This blocks the host-code-execution vectors (a hook the user later triggers on the host). |
| **Isolated shared clone**: `git clone --shared main clone`, mount the clone at `/work` and main's `.git/objects` read-only at its host path | Works. Agent commits into the clone; main repo's refs are not even visible; `git fetch` from inside fails (no reachable remote). Clone `.git` is 120 KB (objects shared via alternates). Host imports with `git fetch <clone> branch:branch`. |

**The trade-off.** The worktree options give the container read-write access to the **whole main repository's refs and objects**: an agent could rewrite or delete any branch of the user's repo, not only its own. The isolated clone confines the blast radius to one throwaway directory, which matches the "completely sandboxed" goal in the manifest, and rig's host-side push/PR step is unchanged.

**Recommendation:** for `sandbox: container`, use the **isolated shared clone**; keep real worktrees for `sandbox: host`. Both sit behind one `internal/git` "checkout" abstraction (`Provision`, `IsDirty`, `Remove`, `Import`), so everything above it (phases, review diff, push) is identical. Caveats to handle: `--shared` clones depend on the main repo's objects, so rig must fetch the clone's branch into the main repo before the main repo prunes (also done at `rig push`/`review`), and `git gc` in the main repo should not be run while tickets are active.

**This changes tasks 17, 18, 21, 22 (worktree-only today) and the `worktree` field in formats §5.** I have **not** changed them. Needs the user's approval; see "Open items".

## 4. Files, performance, startup
- **Ownership:** files created in the container appear on the host as the host user (`502:20`) whatever uid the container uses (Docker Desktop VirtioFS). No chown problems on macOS. (On Linux the container uid must equal the host uid; `--user` already does that.)
- **Bind-mount speed:** creating 3000 small files took 1.02 s through the mount vs 0.34 s on the host (container start included, about 0.2 s): roughly 2–3x slower on metadata-heavy work. Acceptable; builds with huge `node_modules` should use a named volume for dependency dirs (later optimization).
- **Container start:** about 0.19 s for an idle container.

## 5. Attaching a terminal
`docker exec -it <container> bash` inside a tmux pane (and so inside Herdr) gives a real PTY: correct initial size, `tput cols` follows window resizes, and `docker exec`'s exit ends the pane. So the pane command for a containerized agent is simply `docker exec -it <agent-container> claude --settings <file> ...`.
- Hook events (spike 03 §4): a directory mounted into the container (verified: `/rig-events/events.log` written inside, visible on the host) is the simplest channel. The hook command appends one line per event; the daemon tails the file. No daemon socket mount needed, and nothing outside that directory is exposed. `RIG_TICKET`/`RIG_SERVICE` pass as container env.

## 6. What stays out of the container
- Git push credentials, SSH keys, `gh` auth: never mounted (host-side `rig push`).
- The Docker socket: never mounted.
- Anything outside: the checkout (or worktree), the events dir, the read-only objects/CA mounts, and the ticket network.

## 7. Authentication
**Verified without a login:** Claude Code starts in the container; it reaches `api.anthropic.com` (HTTP 404 on `/` means the server answered); `claude -p hi` fails with a clean `Not logged in · Please run /login` rather than a TLS error.

**What Claude Code accepts** (found in the binary and `claude --help`): `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`, requires a Claude subscription), `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` (bearer, for gateways), `ANTHROPIC_BASE_URL`, `apiKeyHelper` (a settings command that prints a key; can be supplied per launch via `--settings`), and `CLAUDE_CODE_USE_BEDROCK` / `CLAUDE_CODE_USE_VERTEX` / `CLAUDE_CODE_USE_FOUNDRY` with the matching cloud credentials.

**Why the host login can't just be mounted:** on macOS it lives in the Keychain, which a container cannot read; mounting `~/.claude` carries settings, not credentials. The user's current auth (a Claude Enterprise login) can reach a container only via a `setup-token` OAuth token, if the org allows it.

**Feasibility of "all auth types, even different per ticket": high.** Each launch already has its own environment, so per-ticket and per-service credentials need no special mechanism. Proposed design (additive, nothing implemented):
- Global config `auth.profiles`, each with a `type` and the secret *names* it needs, plus `auth.default`:
  `host_login` (host mode only: use whatever login the machine has), `oauth_token`, `api_key`, `auth_token`, `bedrock`, `vertex`, `foundry`.
- `ticket.yaml`: `auth: <profile>` at ticket level, overridable per service.
- The harness maps a profile to env vars (`Harness.AuthEnv(profile) -> env`), so other harnesses (Cursor, custom) define their own mapping; values come from the secrets resolver (task 41: Keychain or `.env`) and are never written into the ticket dir, logs, or API output.
- `container` mode + `host_login` is rejected with a clear error (it can't work).

**Limits worth knowing:**
1. Parallel agents on one subscription share its rate limits; per-ticket profiles are the lever.
2. An env credential is readable by the agent itself (and by `docker inspect`). That is inherent to running an agent with credentials; use scoped or short-lived tokens, and rely on the egress allowlist (spike 05) to limit exfiltration.
3. Bedrock/Vertex need cloud credentials (AWS env or a read-only credentials mount): more setup than a token.
4. Enterprise policy may forbid `setup-token` or API keys; the profile model lets the user pick whatever their org permits.

## 8. Open items
1. **Approval for the git decision (§3):** use isolated shared clones for container mode?
2. **Authenticated check:** to prove an authenticated Claude turn inside a container, the user stores a token in Keychain (`security add-generic-password -a $USER -s rig-spike-claude-token -w`, which prompts, so the token never enters chat), after `claude setup-token`; then re-run with `-e CLAUDE_CODE_OAUTH_TOKEN`. Optional now; task 40's manual check covers it.

## 9. Impact on later tasks
| Task | Change |
|---|---|
| 09, 08 | Add `auth.profiles`/`auth.default` (global) and `auth` (ticket/service) plus `docker.extra_ca_certs`. |
| 17, 18, 21, 22 | Pending the git decision: checkout abstraction with worktree (host) and shared clone (container) implementations. |
| 29 | `Harness.AuthEnv(profile)`; claude-code maps profile types to env. |
| 34, 37, 40 | Build the overlay agent image (cached by base digest + Claude version); bake/mount extra CAs; run as host uid; `docker exec -it` pane command; events dir mount; set `RIG_TICKET`/`RIG_SERVICE`. |
| 41 | Resolve profile secrets (Keychain / `.env`); reject `host_login` in container mode. |
