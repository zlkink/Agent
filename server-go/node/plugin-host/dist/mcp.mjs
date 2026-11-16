// ../../../plugins/mcp/src/index.ts
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

// ../../../plugins/mcp/src/client.ts
import { spawn } from "node:child_process";
var PROTOCOL = "2025-06-18";
var fail = (e) => e instanceof Error ? e : new Error(String(e));
var StdioTransport = class {
  child;
  next = 1;
  buffer = "";
  pending = /* @__PURE__ */ new Map();
  stderr = "";
  dead;
  constructor(opts) {
    const shell = process.platform === "win32";
    const quote = (v) => shell && /[\s&()^|<>]/.test(v) && !v.startsWith('"') ? `"${v}"` : v;
    this.child = spawn(quote(opts.command), (opts.args ?? []).map(quote), {
      cwd: opts.cwd,
      env: { ...process.env, ...opts.env },
      windowsHide: true,
      shell,
      stdio: ["pipe", "pipe", "pipe"]
    });
    this.child.stdout.setEncoding("utf8");
    this.child.stdout.on("data", (chunk) => this.onData(chunk));
    this.child.stderr.setEncoding("utf8");
    this.child.stderr.on("data", (chunk) => {
      this.stderr = (this.stderr + chunk).slice(-1500);
    });
    const die = (e) => {
      this.dead ??= e;
      for (const p of this.pending.values()) p.reject(e);
      this.pending.clear();
    };
    this.child.on("error", (e) => die(new Error(`\u65E0\u6CD5\u542F\u52A8 ${opts.command}\uFF1A${e.message}`)));
    this.child.on("exit", (code) => die(new Error(`\u670D\u52A1\u5668\u5DF2\u9000\u51FA\uFF08${code}\uFF09${this.stderr.trim() ? `\uFF1A${this.stderr.trim().split(/\r?\n/).slice(-8).join(" | ")}` : ""}`)));
    this.child.stdin.on("error", () => void 0);
  }
  onData(chunk) {
    this.buffer += chunk;
    let at;
    while ((at = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, at).trim();
      this.buffer = this.buffer.slice(at + 1);
      if (!line) continue;
      let msg;
      try {
        msg = JSON.parse(line);
      } catch {
        continue;
      }
      if (msg.id === void 0) continue;
      const p = this.pending.get(msg.id);
      if (!p) continue;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new Error(msg.error.message ?? "MCP error"));
      else p.resolve(msg.result);
    }
  }
  request(method, params, signal) {
    if (this.dead) return Promise.reject(this.dead);
    const id = this.next++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      signal.addEventListener("abort", () => {
        this.pending.delete(id);
        reject(new Error("\u8FDE\u63A5\u8D85\u65F6"));
      });
      this.child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}
`);
    });
  }
  async notify(method, params) {
    this.child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method, params })}
`);
  }
  async close() {
    this.child.stdin.end();
    this.child.kill();
  }
};
var HttpTransport = class {
  next = 1;
  session;
  opts;
  constructor(opts) {
    this.opts = opts;
  }
  async post(body, signal) {
    return fetch(this.opts.url, {
      method: "POST",
      signal,
      headers: {
        "content-type": "application/json",
        accept: "application/json, text/event-stream",
        "mcp-protocol-version": PROTOCOL,
        ...this.session ? { "mcp-session-id": this.session } : {},
        ...this.opts.headers
      },
      body: JSON.stringify(body)
    });
  }
  async request(method, params, signal) {
    const id = this.next++;
    const res = await this.post({ jsonrpc: "2.0", id, method, params }, signal);
    this.session ??= res.headers.get("mcp-session-id") ?? void 0;
    if (!res.ok) throw new Error(`HTTP ${res.status}${res.status === 401 ? "\uFF08\u9700\u8981\u8BA4\u8BC1\uFF1A\u8BF7\u5728\u8BF7\u6C42\u5934\u91CC\u586B\u5199\u4EE4\u724C\uFF09" : ""}`);
    const text = await res.text();
    const kind = res.headers.get("content-type") ?? "";
    const messages = kind.includes("text/event-stream") ? text.split(/\r?\n\r?\n/).flatMap((ev) => {
      const data = ev.split(/\r?\n/).filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trimStart()).join("\n");
      try {
        return data ? [JSON.parse(data)] : [];
      } catch {
        return [];
      }
    }) : [JSON.parse(text)];
    const mine = messages.find((m) => m.id === id);
    if (!mine) throw new Error("\u670D\u52A1\u5668\u6CA1\u6709\u8FD4\u56DE\u7ED3\u679C");
    if (mine.error) throw new Error(mine.error.message ?? "MCP error");
    return mine.result;
  }
  async notify(method, params) {
    await this.post({ jsonrpc: "2.0", method, params }, AbortSignal.timeout(1e4)).catch(() => void 0);
  }
  async close() {
    if (!this.session) return;
    await fetch(this.opts.url, { method: "DELETE", headers: { "mcp-session-id": this.session, ...this.opts.headers }, signal: AbortSignal.timeout(3e3) }).catch(() => void 0);
  }
};
var McpClient = class {
  transport;
  constructor(transport) {
    this.transport = transport;
  }
  async connect(signal) {
    const result = await this.transport.request("initialize", { protocolVersion: PROTOCOL, capabilities: {}, clientInfo: { name: "prometheus", version: "0.1.0" } }, signal).catch((e) => {
      throw fail(e);
    });
    await this.transport.notify("notifications/initialized");
    return {
      serverInfo: { name: String(result?.serverInfo?.name ?? "?"), version: String(result?.serverInfo?.version ?? "") },
      hasTools: Boolean(result?.capabilities?.tools)
    };
  }
  async listTools(signal) {
    const out = [];
    let cursor;
    do {
      const page = await this.transport.request("tools/list", cursor ? { cursor } : {}, signal);
      for (const t of Array.isArray(page?.tools) ? page.tools : []) out.push({ name: String(t.name), description: typeof t.description === "string" ? t.description : void 0 });
      cursor = typeof page?.nextCursor === "string" ? page.nextCursor : void 0;
    } while (cursor && out.length < 500);
    return out;
  }
  close() {
    return this.transport.close();
  }
};

// ../../../packages/plugin-sdk/src/json-store.ts
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
function createJsonStore(path, fallback) {
  let lock = Promise.resolve();
  const read = async () => {
    try {
      return JSON.parse(await readFile(path, "utf8"));
    } catch (err) {
      if (err.code !== "ENOENT") throw err;
      return fallback();
    }
  };
  const update = (fn) => {
    const run = async () => {
      const next = await fn(await read());
      await mkdir(dirname(path), { recursive: true });
      const tmp = `${path}.${process.pid}.tmp`;
      await writeFile(tmp, JSON.stringify(next, null, 2), { mode: 384 });
      await rename(tmp, path);
      return next;
    };
    const result = lock.then(run, run);
    lock = result.catch(() => {
    });
    return result;
  };
  return { read, update };
}

// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/mcp/src/spec.ts
var NAME_PATTERN = /^[a-zA-Z0-9][\w-]{0,47}$/;
var EXPOSURES = ["codemode", "deferred", "direct", "hidden"];
var HEADER_PATTERN = /^[A-Za-z0-9-]{1,64}$/;
var ENV_PATTERN = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
var MASK = "\u2022\u2022\u2022\u2022\u2022\u2022";
function stringRecord(value, keyPattern, label) {
  if (value === void 0) return void 0;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} \u5FC5\u987B\u662F\u952E\u503C\u5BF9\u8C61`);
  const out = {};
  for (const [k, v] of Object.entries(value)) {
    if (!keyPattern.test(k)) throw new Error(`${label} \u952E\u540D\u4E0D\u5408\u6CD5\uFF1A${k}`);
    if (typeof v !== "string") throw new Error(`${label}.${k} \u5FC5\u987B\u662F\u5B57\u7B26\u4E32`);
    out[k] = v;
  }
  return out;
}
function parseSpec(raw) {
  if (!raw || typeof raw !== "object") throw new Error("\u7F3A\u5C11\u670D\u52A1\u5668\u914D\u7F6E");
  const r = raw;
  const type = r.type === "http" ? "http" : r.type === "stdio" ? "stdio" : void 0;
  if (!type) throw new Error("type \u5FC5\u987B\u662F stdio \u6216 http");
  const exposure = r.exposure === void 0 ? void 0 : r.exposure;
  if (exposure !== void 0 && !EXPOSURES.includes(exposure)) throw new Error("exposure \u53D6\u503C\u4E0D\u5408\u6CD5");
  const timeout = r.timeout === void 0 ? void 0 : Number(r.timeout);
  if (timeout !== void 0 && !(timeout > 0 && timeout <= 3600)) throw new Error("timeout \u9700\u5728 1\u20133600 \u79D2\u4E4B\u95F4");
  const spec = {
    type,
    description: typeof r.description === "string" && r.description.trim() ? r.description.trim().slice(0, 500) : void 0,
    exposure,
    enabled: r.enabled !== false,
    timeout
  };
  if (type === "stdio") {
    if (typeof r.command !== "string" || !r.command.trim()) throw new Error("stdio \u670D\u52A1\u5668\u9700\u8981 command");
    spec.command = r.command.trim();
    if (r.args !== void 0) {
      if (!Array.isArray(r.args) || r.args.some((a) => typeof a !== "string")) throw new Error("args \u5FC5\u987B\u662F\u5B57\u7B26\u4E32\u6570\u7EC4");
      spec.args = r.args;
    }
    spec.env = stringRecord(r.env, ENV_PATTERN, "env");
    if (typeof r.cwd === "string" && r.cwd.trim()) spec.cwd = r.cwd.trim();
  } else {
    if (typeof r.url !== "string") throw new Error("http \u670D\u52A1\u5668\u9700\u8981 url");
    let url;
    try {
      url = new URL(r.url);
    } catch {
      throw new Error("url \u4E0D\u662F\u5408\u6CD5\u5730\u5740");
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("url \u5FC5\u987B\u662F http(s) \u5730\u5740");
    spec.url = url.toString();
    spec.headers = stringRecord(r.headers, HEADER_PATTERN, "headers");
  }
  return spec;
}
function maskRecord(record) {
  if (!record) return void 0;
  return Object.fromEntries(Object.entries(record).map(([k, v]) => [k, /^\$\{[A-Za-z_]\w*\}$/.test(v) ? v : MASK]));
}
function redactSpec(spec) {
  return { ...spec, env: maskRecord(spec.env), headers: maskRecord(spec.headers) };
}
function unmaskRecord(next, prev) {
  if (!next) return void 0;
  return Object.fromEntries(Object.entries(next).map(([k, v]) => [k, v === MASK ? prev?.[k] ?? "" : v]));
}
function mergeSpec(next, prev) {
  return { ...next, env: unmaskRecord(next.env, prev?.env), headers: unmaskRecord(next.headers, prev?.headers) };
}
function toPiConfig(spec) {
  const base = { description: spec.description, exposure: spec.exposure, enabled: spec.enabled, timeout: spec.timeout };
  return spec.type === "stdio" ? { ...base, type: "stdio", command: spec.command, args: spec.args, env: spec.env, cwd: spec.cwd } : { ...base, type: "http", url: spec.url, headers: spec.headers };
}
function fromPiConfig(raw) {
  if (!raw || typeof raw !== "object") return void 0;
  const r = raw;
  const isHttp = typeof r.url === "string";
  if (!isHttp && typeof r.command !== "string") return void 0;
  return {
    type: isHttp ? "http" : "stdio",
    command: isHttp ? void 0 : r.command,
    args: Array.isArray(r.args) ? r.args : void 0,
    url: isHttp ? r.url : void 0,
    env: r.env,
    headers: r.headers,
    description: typeof r.description === "string" ? r.description : void 0,
    exposure: r.exposure,
    enabled: r.enabled !== false
  };
}

// ../../../plugins/mcp/src/index.ts
var PROBE_TIMEOUT_MS = 2e4;
function expandEnv(value) {
  return value.replace(/\$\{([A-Za-z_]\w*)\}/g, (_, name) => process.env[name] ?? "");
}
function expandRecord(record) {
  return record && Object.fromEntries(Object.entries(record).map(([k, v]) => [k, expandEnv(v)]));
}
function transportFor(spec, cwd) {
  if (spec.type === "http") return new HttpTransport({ url: spec.url ?? "", headers: expandRecord(spec.headers) });
  return new StdioTransport({ command: spec.command ?? "", args: spec.args, env: expandRecord(spec.env), cwd: spec.cwd ?? cwd });
}
async function probe(spec, cwd) {
  const started = performance.now();
  const client = new McpClient(transportFor(spec, cwd));
  const signal = AbortSignal.timeout(PROBE_TIMEOUT_MS);
  try {
    const init = await client.connect(signal);
    const tools = init.hasTools ? await client.listTools(signal) : [];
    return {
      ok: true,
      latencyMs: Math.round(performance.now() - started),
      server: `${init.serverInfo.name} ${init.serverInfo.version}`,
      tools: tools.map((t) => ({ name: t.name, description: t.description?.slice(0, 300) }))
    };
  } catch (err) {
    return { ok: false, latencyMs: Math.round(performance.now() - started), tools: [], error: err instanceof Error ? err.message : String(err) };
  } finally {
    await client.close().catch(() => {
    });
  }
}
function readPiServers(agentDir) {
  const path = join(agentDir, "mcp.json");
  if (!existsSync(path)) return {};
  try {
    const raw = JSON.parse(readFileSync(path, "utf8"));
    const out = {};
    for (const [name, entry] of Object.entries(raw.mcpServers ?? {})) {
      const spec = fromPiConfig(entry);
      if (spec) out[name] = spec;
    }
    return out;
  } catch {
    return {};
  }
}
function requireName(value) {
  if (typeof value !== "string" || !NAME_PATTERN.test(value)) throw new Error("\u670D\u52A1\u5668\u540D\u53EA\u80FD\u5305\u542B\u5B57\u6BCD\u3001\u6570\u5B57\u3001- \u548C _\uFF0C\u4E14\u4E0D\u8D85\u8FC7 48 \u4E2A\u5B57\u7B26");
  return value;
}
var index_default = definePlugin({
  name: "mcp",
  version: "0.1.0",
  async setup(ctx) {
    const store = createJsonStore(join(ctx.dataDir, "servers.json"), () => ({ servers: {} }));
    await store.read();
    ctx.extension(async (pi) => {
      const { servers } = await store.read();
      for (const [name, spec] of Object.entries(servers)) {
        if (spec.enabled === false) continue;
        pi.registerMcpServer(name, toPiConfig(spec));
      }
    });
    ctx.rpc("list", async () => {
      const { servers } = await store.read();
      const own = Object.entries(servers).map(([name, spec]) => ({ name, spec: redactSpec(spec), source: "prometheus" }));
      const pi = Object.entries(readPiServers(ctx.agentDir)).filter(([name]) => !servers[name]).map(([name, spec]) => ({ name, spec: redactSpec(spec), source: "pi" }));
      return [...own, ...pi].sort((a, b) => a.name.localeCompare(b.name));
    });
    ctx.rpc("save", async (params) => {
      const name = requireName(params.name);
      const previous = params.previousName === void 0 ? void 0 : requireName(params.previousName);
      const spec = parseSpec(params.spec);
      await store.update((state) => {
        if (name !== previous && state.servers[name]) throw new Error(`\u670D\u52A1\u5668\u5DF2\u5B58\u5728\uFF1A${name}`);
        const merged = mergeSpec(spec, state.servers[previous ?? name]);
        if (previous && previous !== name) delete state.servers[previous];
        state.servers[name] = merged;
        return state;
      });
      await ctx.reloadSessions();
      return null;
    });
    ctx.rpc("toggle", async (params) => {
      const name = requireName(params.name);
      await store.update((state) => {
        const spec = state.servers[name];
        if (!spec) throw new Error(`\u670D\u52A1\u5668\u4E0D\u5B58\u5728\uFF1A${name}`);
        spec.enabled = params.enabled !== false;
        return state;
      });
      await ctx.reloadSessions();
      return null;
    });
    ctx.rpc("delete", async (params) => {
      const name = requireName(params.name);
      await store.update((state) => {
        delete state.servers[name];
        return state;
      });
      await ctx.reloadSessions();
      return null;
    });
    ctx.rpc("probe", async (params) => {
      const { servers } = await store.read();
      if (params.spec !== void 0) {
        const prev = typeof params.name === "string" ? servers[params.name] : void 0;
        return probe(mergeSpec(parseSpec(params.spec), prev), ctx.cwd);
      }
      const name = requireName(params.name);
      const spec = servers[name] ?? readPiServers(ctx.agentDir)[name];
      if (!spec) throw new Error(`\u670D\u52A1\u5668\u4E0D\u5B58\u5728\uFF1A${name}`);
      return probe(spec, ctx.cwd);
    });
  }
});
export {
  index_default as default,
  probe
};
