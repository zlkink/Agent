# HelixAgent

Go host for the Prometheus agent UI. This package contains:

```text
HelixAgent.exe          Go host (HTTP/WS + agents + tools)
web/                    the React UI built from Prometheus
node/agent-host/        official Claude Agent SDK sidecar + sdk.mjs
node/plugin-host/       TypeScript plugin bridge (bundled plugins)
```

No Node.js or PI runtime is shipped inside the package; like the original
desktop app they are installed on first run (see below).

## First run

Node.js 22.23.4 is required by the Claude sidecar and plugin host. If the machine
has no suitable `node` on `PATH`, startup shows the runtime install screen instead
of downloading silently; install Node under **Settings → Runtimes**, then reopen
the app. Common macOS locations (Homebrew, `/usr/local/bin`, nvm, `~/.local/bin`)
are searched automatically.

A gateway is optional: without one, HelixAgent uses the Claude Code CLI's own
login. Add a gateway in Settings at any time.

PI is optional. Claude Code, Codex and the normal tools work without it; only the
PI runtime, the plugin market and a few PI-based plugins need it. Install it from
**Settings → Runtimes** when needed (about 145 MB), then restart the app. The
installer puts it in the app's runtime folder and passes `PROMETHEUS_PI_DIR` to
the host, so nothing is bundled in this package.

## Run

Double-click `HelixAgent Desktop.exe`. It starts the native window and runs the
Go host as its sidecar; no scripts are needed.

Browser mode is optional, for debugging or embedding:

```bat
HelixAgent.exe -serve -addr 127.0.0.1:17420 -web web -allow-write
```

Then open <http://127.0.0.1:17420/>.

The host reads the Prometheus config from `%USERPROFILE%\.prometheus\config.json`
(gateways, API keys). Use the UI's Settings page to add a gateway if the file is
missing. Data and sessions stay under `%USERPROFILE%\.prometheus\go-poc\`.

## Exit

Closing the window only hides it to the system tray. Use the tray menu's Quit to
stop the host and every helper process before moving or deleting this folder.
If a helper is force-killed, it exits on its own within a few seconds once its
parent is gone.

## macOS

The release build is ad-hoc signed, not notarized with a paid Developer ID.
If Gatekeeper still refuses the downloaded app, run once:

```bash
xattr -cr "/Applications/HelixAgent.app"
open "/Applications/HelixAgent.app"
```

## Requirements

- Claude Code CLI (`claude`) on `PATH` or in `%USERPROFILE%\.prometheus\agents`
  for the Claude Code runtime.
- Node.js 22.19+ on `PATH`; otherwise the first launch downloads it as described
  above.
- The Claude Agent SDK, LanceDB and the TypeScript plugin host are included.
- Optional: Codex CLI or `opencode` on `PATH` (or in the Prometheus agents
  directory) for those runtimes. They only light up in the agent menu when the
  CLI is actually present and answers `--version`.

## Network access

Settings → Network access can expose the same UI to other devices:

- Set a login password (at least 8 characters), then turn on LAN access. The
  host listens on the chosen port for every LAN interface. Other devices open
  `http://<this-computer-LAN-IP>:<port>/` and sign in once; the session cookie
  lasts 30 days. The local window keeps using loopback and never asks.
- A domain allow-list accepts `host.example.com` and `*.example.com` entries
  for reverse proxies. IP addresses and the temporary Cloudflare hostname are
  always accepted.
- The Cloudflare switch downloads `cloudflared` on first use, exactly like the
  original server: into `%USERPROFILE%\.prometheus\bin`, through `curl` and the
  environment or Windows system proxy, then opens a temporary public
  `https://….trycloudflare.com` address. It points at the password-protected LAN
  listener, so a password is required first.

Password hashes, the port and the allow-list are stored in
`%USERPROFILE%\.prometheus\go-poc\network.json`. Changing the password signs out
every device.

## SSH servers

The SSH settings page starts empty: hosts are never imported automatically.
Use **导入服务器** (Import servers) to read `~/.ssh/config` and tick the hosts to
add, several at a time. Existing names are skipped.

## Flags

Browser mode accepts arguments on HelixAgent.exe directly, e.g.:

```bat
HelixAgent.exe -serve -web web -model group/deepseek
HelixAgent.exe -serve -web web -token my-secret-token
HelixAgent.exe -serve -web web -native
```

Useful flags: `-addr`, `-web`, `-model`, `-token`, `-native`, `-allow-write`,
`-plugin-host`, `-agents`.

## Notes

- `assistant_ask` works in any chat, not only delegated runs.
- The plugin host is started automatically from `node/plugin-host`.
- Agent update detection is available with `HelixAgent.exe -agents`.
