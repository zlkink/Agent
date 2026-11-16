# Prometheus Go host — Plan B proof of concept

A vertical slice of the proposed Plan B architecture:

```text
HTTP/NDJSON or CLI
        │
        ▼
Go host  ── relay (127.0.0.1) ──► gateway (magpie, anthropic-messages)
   │
   └── node agent-host (official Claude Agent SDK) ──► claude.exe
```

What this proves:

- Go can own the HTTP surface, the loopback gateway relay, process supervision
  and agent update detection without touching the Claude CLI protocol.
- The Node sidecar uses the official Claude Agent SDK and translates SDK events
  into a neutral event protocol; Go never sees SDK types.
- Both `claude --version` and `npm view @anthropic-ai/claude-code version`
  detection work from Go.
- **Prometheus wire protocol v1 runs in Go**: `/ws` speaks `hello`, `auth`,
  `subscribe`, `rpc`, `event`, `pong` and the session/chat RPCs the web client
  uses. The real Web UI can be served from `-web` and will talk to the Go host.
- **Sessions, settings and workspaces persist** under `~/.prometheus/go-poc/`
  (never touching the Node backend's data), with history restored after restart.
- **Interactive permissions** flow sidecar → Go → `ui.request` → `ui.respond`
  back to the sidecar. `default` asks the UI, `auto`/`bypassPermissions` allow,
  `plan` stays read-only, `acceptEdits` allows edits.
- **MCP tool bus** in Go (`todo_write`, `memory_save`, `memory_search`,
  `open_file`) is passed to Claude as `mcpServers.prometheus`; calls, permissions
  and results round-trip through the same bus.
- **Codex runtime** via `codex exec --json`, with streamed text, tool cards,
  usage and a transcript preamble for continuity.
- **PI runtime** via PI's own `--mode rpc`: PI stays the Node process, Go sends
  prompts and forwards its events. PI is pointed at the Go relay through a
  per-run `models.json`; its MCP servers come from the same Go tool bus.
- **ACP runtime** (`claude-code-acp`) and **OpenCode runtime**
  (`opencode serve` REST + `/event` SSE), with text/tool events and the same
  Prometheus tool bus. Both were exercised end to end.
- **Subagent tool**: the MCP `subagent` tool runs a bounded Claude Code or Codex
  one-shot in the session folder and returns its text.
- **TypeScript plugin bridge**: all 22 bundled Prometheus plugins (`todos`,
  `memory`, `folder`, `logs`, `git`, `terminal`, `ssh`, `workbench`, `connectors`,
  `voice`, `subagents`, `modes`, `skills`, `automations`, `flows`, `assistant`,
  `browser`, `computer-use`, `veil`, `mcp`, `packages`, `knowledge-lancedb`) run
  in a Node plugin host. Their tools are merged into the MCP bus, their RPCs are
  proxied via `plugin.call`, their `/plugins/*` HTTP routes are proxied, and their
  static/turn context providers are injected into every runtime.
- **Plugin context**: memory recall and the workbench guideline reach Claude via
  `systemPrompt.append` + `UserPromptSubmit`, and other runtimes via a
  `<prometheus_context>` prefix.
- **Semantic knowledge**: `knowledge-lancedb` is bridged for real LanceDB
  vector storage/search; the embedding calls go through the configured gateway.
- **Workspaces** are passed to the plugin host on every tool/RPC call, so git,
  terminal and folder operations work for every added project folder.
- **Bootstrap**: `prometheus -bootstrap` ensures Node (downloading the official
  build if missing), installs PI into `~/.prometheus/go-poc/pi`, and builds the
  TypeScript plugin host bundles.
- **Token auth / network mode**: `-token` / `PROMETHEUS_TOKEN` enables the WS
  `auth` handshake, `/api/login` cookie exchange, and protects `/plugins/*` and
  downloads. Non-loopback binds refuse to start without a token.
- **Git worktrees**: create, status, diff, base sync, squash/merge/apply merge,
  backup/restore of overwritten project files, and discard.
- **Real browser test**: `node node/browser-ui-test.mjs` launches headless
  Chrome, opens the served UI, clicks a session, types a prompt and checks the
  streamed answer.
- **Gateway CRUD** writes `config.json` without dropping the fields the Go host
  does not model, preserves masked API keys, and can probe/model-list a gateway.
- **Files**: real `fs.dirs`, `fs.mkdir`, `file.stat`, `file.read`, `file.link`
  + `/api/download/<ticket>`, `system.open*`.
- **Knowledge**: an in-memory `kb.*` store with ingest/search (semantic LanceDB
  remains a Node-side component).

Not implemented yet (the remaining gaps are host-API depth, not runtime plumbing):

- advanced actions of `assistant` / `automations` / `flows` that need the real
  host session/delegation APIs (their plugins load and their RPCs are reachable,
  but the host stubs return empty results)
- `veil` request interception is registered but not yet wired into the Go relay
- the Node download path in `-bootstrap` is implemented but was not exercised on
  this machine because Node is already present
- real embedding credentials: LanceDB was verified with a local fake embedding
  endpoint because the configured gateway exposes no embeddings model
- worktree `restore-backup` is implemented but only merge/discard were exercised

## Run

From this directory (Node 22+ and Go 1.25+ installed):

```bash
go build -o prometheus.exe ./cmd/prometheus

# One-shot: installs PI + plugin-host dependencies and builds the plugin bundles
./prometheus.exe -bootstrap

# 1. Agent detection + update check
./prometheus.exe -agents
./prometheus.exe -agents -offline

# 2. One prompt through the Go relay and the Node sidecar
./prometheus.exe -prompt "只回答：1+1 等于几" -model group/deepseek

# 3. HTTP host + WebSocket protocol v1
./prometheus.exe -serve -addr 127.0.0.1:17420 -model group/deepseek \
  -web "D:/Program Files/Prometheus/server/web"
# open http://127.0.0.1:17420/ in a browser
curl http://127.0.0.1:17420/api/health

# 4. Protocol smoke test without a browser (same frames as apps/web/src/lib/client.ts)
node node/test-client.mjs ws://127.0.0.1:17420/ws "只回答：11+12 等于几"
```

Useful flags:

| Flag | Meaning |
|---|---|
| `-native` | use the Claude CLI's own login instead of a Prometheus gateway |
| `-model <id>` | override the gateway model |
| `-cwd <dir>` | working directory for the agent |
| `-sidecar <path>` | agent-host script (default `node/agent-host/index.mjs`) |
| `-web <dir>` | web UI assets to serve (default: `apps/web/dist` when present) |
| `-plugin-host <url>` | use a running plugin host instead of auto-starting the bundled one |
| `-bootstrap` | ensure Node, install PI and build the TypeScript plugin host bundles |
| `-token <token>` | access token for LAN/remote clients (also `PROMETHEUS_TOKEN`) |
| `-allow-write` | let the agent use Bash/Edit/Write; the permission bridge still gates them |
| `-offline` | skip `npm view` in `-agents` |

## Layout

```text
server-go/
  cmd/prometheus/main.go          flags, setup, CLI
  internal/agents/agents.go       claude/codex detection + npm update check
  internal/config/config.go       config.json read + gateway CRUD (masked keys)
  internal/relay/relay.go         loopback reverse proxy, token auth, SSE flush
  internal/sidecar/sidecar.go     Node process + JSONL event loop + permissions
  internal/host/host.go           wire protocol v1: sessions, RPC, events, persistence
  internal/host/codex.go          codex exec --json adapter
  internal/host/pi.go             PI RPC adapter + models.json/mcp.json
  internal/host/acp.go            Agent Client Protocol adapter
  internal/host/opencode.go       opencode serve REST + SSE adapter
  internal/host/subagent.go       subagent MCP tool (Claude/Codex one-shot)
  internal/host/worktree.go       git worktree create/status/diff/merge/discard
  internal/pi/client.go           PI --mode rpc JSONL client
  internal/acp/client.go          ACP JSON-RPC client
  internal/opencode/client.go     opencode serve REST/SSE client
  internal/toolbus/toolbus.go     MCP Streamable HTTP tools + plugin proxy
  internal/store/store.go         ~/.prometheus/go-poc/state.json
  internal/server/server.go       /ws, /api/*, auth, static web UI
  node/agent-host/index.mjs       official Claude Agent SDK -> neutral events
  node/plugin-host/index.mjs      bundled TypeScript plugins -> tools/RPC/routes
  node/plugin-host/build-plugins.mjs  esbuild bundling step
  node/test-client.mjs            protocol smoke test
  node/browser-ui-test.mjs        headless Chrome click-through test
```

## Security notes (POC)

- The relay binds to `127.0.0.1` and requires a per-process token.
- The real gateway key stays in the Go process; the sidecar only gets the relay
  URL and token.
- The sidecar is read-only: `Read/Grep/Glob/WebFetch/WebSearch` are allowed,
  `Bash/Edit/Write/...` are denied. This is deliberately narrower than the real
  product and should be replaced by the shared PermissionGate.
- The HTTP host has no authentication. Keep it on loopback.
