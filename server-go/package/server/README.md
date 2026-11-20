# RamiCore headless server

The same RamiCore host the desktop app runs, packaged for a Linux server without
a GUI or a Tauri window. The web UI is served by the host, and the desktop app
or a phone browser can connect to it as a remote server.

```text
ramicore               Go host (HTTP/WS + agents + tools)
web/                   web UI served by the host
node/agent-host/       official Claude Agent SDK sidecar
node/plugin-host/      TypeScript plugin bridge
ramicore.service       systemd unit template
install.sh             installer (copies files, creates the service)
```

## Requirements

- Ubuntu 22.04+ / Debian 12+ (other systemd distributions work too).
- Node.js 22.x for the user the service runs as, on the service `PATH`
  (`/usr/local/bin:/usr/bin:/bin`). NodeSource packages work well; nvm
  installations are not visible to systemd unless linked into `/usr/local/bin`.
- Claude Code (`claude`) on that same `PATH`, or installed later from
  **Settings → Runtimes** in the UI. Codex, OpenCode and PI are optional and can
  also be installed from the UI.
- No display server, X11 or desktop environment is needed.

## Install

Copy the `RamiCore-Server-Linux.x64.tar.gz` release asset to the server, then:

```bash
tar -xzf RamiCore-Server-Linux.x64.tar.gz
cd RamiCore
sudo ./install.sh
```

The installer:

- copies the host to `/opt/ramicore`;
- runs the service as the `sudo` user (override with `SERVICE_USER=name`);
- generates an access token in `/etc/ramicore/ramicore.env`;
- listens on `0.0.0.0:17420` (override with `RAMICORE_ADDR=host:port`);
- enables and starts `ramicore.service`.

It prints the UI address and token at the end. Open `http://<server-ip>:17420/`
and sign in with the token, or choose **Connect to server** in the desktop app
and save the address there.

## Operate

```bash
systemctl status ramicore
journalctl -u ramicore -f
sudo systemctl restart ramicore
```

Sessions, settings, gateways, agents and network configuration live in the
service user's `~/.prometheus` directory. Keep that account stable across
upgrades.

Change the address or token in `/etc/ramicore/ramicore.env`, then restart the
service. Changing the token signs out existing browser sessions.

## Upgrade

Extract a newer tarball and run `sudo ./install.sh` again. The existing token and
address are preserved; the service is restarted with the new host and web UI.

## Uninstall

```bash
sudo systemctl disable --now ramicore
sudo rm -rf /opt/ramicore /etc/ramicore /etc/systemd/system/ramicore.service
sudo systemctl daemon-reload
```

This keeps the user's `~/.prometheus` data.

## Network and security

The service listens on every interface because it is meant to be reached
remotely, and requires the token for non-loopback access. For anything beyond a
trusted LAN:

- put it behind an HTTPS reverse proxy (Caddy, Nginx) or use the built-in
  Cloudflare tunnel under **Settings → Network access**;
- open only the port you need in the firewall;
- treat the token like a password.

The host serves the UI itself, so no separate web server is required. There is
no graphical installer and no tray icon; `systemctl` is the lifecycle.
