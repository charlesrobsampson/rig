# Running rig on Linux

rig is developed on macOS and the default test suite passes on Linux too (arm64 verified;
`task test:linux` runs it in a container, see below). The differences are small:

| | macOS | Linux |
|---|---|---|
| Secret store | Keychain (`security`) | the Secret Service through `secret-tool` (libsecret) |
| `.env` fallback | yes | yes |
| Terminal backends | Herdr, tmux | tmux; Herdr where it is available for your platform |
| Docker | Docker Desktop / Colima | native Docker Engine |

## Secrets

Secrets named in `ticket.yaml` (`secrets:`), auth profiles, and the Jira / GitHub / Claude
summarizer credentials are looked up in the platform's own store first and then in the
`.env` file (`secrets.env_file`, an absolute path, `~` allowed). Names only ever appear in
config; values never do.

On Linux the store is whatever implements the freedesktop Secret Service (GNOME Keyring,
KWallet, KeePassXC), read with `secret-tool` from `libsecret-tools`:

```sh
sudo apt install libsecret-tools        # Debian/Ubuntu; dnf install libsecret on Fedora
secret-tool store --label="rig ANTHROPIC_API_KEY" service rig account ANTHROPIC_API_KEY
```

(it prompts for the value, so it never lands in shell history). `service` is
`secrets.keychain_service` from the config (default `rig`); `account` is the secret's name.

A machine without a Secret Service, which means a headless server, an SSH session with no
D-Bus, a container, or no `secret-tool` installed, is not an error: rig finds nothing there
and consults the `.env` file. This is the usual setup on servers:

```sh
install -m 600 /dev/null ~/.config/rig/secrets.env
echo 'ANTHROPIC_API_KEY=sk-ant-...' >> ~/.config/rig/secrets.env
# config.json: "secrets": {"env_file": "~/.config/rig/secrets.env"}
```

A real failure of the store (a locked keyring that cannot prompt, say) is reported by name
and does not fall through silently.

## Docker

Install Docker Engine and make sure your user can run `docker` without sudo. Agents in
containers run as your own uid and gid, so the checkouts they write stay yours and
`rig stop` can remove them; no user namespace setup is needed. Rootless Docker and Podman's
Docker-compatible socket have not been tried.

## Terminal backend

Set `defaults.terminal_backend` to `tmux` (always available) or `herdr`. `rig attach` hands
your terminal to the backend's own attach command either way.

## Paths and sockets

State lives under `~/.rig` (override with `RIG_HOME`) and the config under
`~/.config/rig/config.json` on both systems. The daemon's unix socket is in the rig home;
Linux allows a socket path of 108 bytes (macOS 104), so a very deep `RIG_HOME` is the only
way to hit the limit.

## Running the test suite on Linux

```sh
task test:linux                          # every package
task test:linux -- ./internal/lifecycle  # some
```

It cross-compiles each package's tests for Linux and runs them in a small Debian container
(git and a shell, nothing else) as a non-root user with the repository mounted read-only,
so a test that writes where it should not, or leans on root's permissions, fails. It needs
Docker and nothing else, and works from macOS. The suite that runs is the default one:
nothing in it needs Docker, a terminal multiplexer or a network.

## Not covered

A real two-service ticket, started with real agents on a Linux machine, has not been run
by the author; that is a manual check (see the checklist).
