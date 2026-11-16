// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/packages/src/capture.ts
var DEFAULT_TIMEOUT_MS = 5 * 6e4;
function captureCommandOutput(manager, sink, timeoutMs = DEFAULT_TIMEOUT_MS) {
  const spawn = Reflect.get(manager, "spawnCaptureCommand");
  if (typeof spawn !== "function") return false;
  const run = (command, args, options) => new Promise((resolve, reject) => {
    sink(`$ ${command} ${args.join(" ")}
`);
    let child;
    try {
      child = spawn.call(manager, command, args, { cwd: options?.cwd });
    } catch (err) {
      reject(err);
      return;
    }
    const timer = setTimeout(() => {
      child.kill();
      reject(new Error(`\u6267\u884C\u8D85\u65F6\uFF08${Math.round(timeoutMs / 1e3)} \u79D2\uFF09\uFF1A${command}`));
    }, timeoutMs);
    child.stdout?.on("data", (d) => sink(d.toString("utf8")));
    child.stderr?.on("data", (d) => sink(d.toString("utf8")));
    child.on("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      if (code === 0) resolve();
      else reject(new Error(`${command} \u9000\u51FA\u7801 ${code}`));
    });
  });
  Reflect.set(manager, "runCommand", run);
  return true;
}

// ../../../plugins/packages/src/explain.ts
var RULES = [
  [/spawn \S+ ENOENT|not recognized as an internal|command not found/i, "\u627E\u4E0D\u5230 npm \u6216 git \u547D\u4EE4\uFF0C\u8BF7\u5B89\u88C5 Node.js\uFF08\u542B npm\uFF09/ Git\uFF0C\u5E76\u786E\u4FDD\u5B83\u4EEC\u5728 PATH \u4E2D"],
  [/\bE404\b|404 Not Found|is not in this registry/i, "\u5728 registry \u4E2D\u627E\u4E0D\u5230\u8BE5\u5305\uFF0C\u8BF7\u68C0\u67E5\u5305\u540D\u4E0E\u7248\u672C"],
  [/\bETARGET\b|notarget|No matching version/i, "\u627E\u4E0D\u5230\u5339\u914D\u7684\u7248\u672C"],
  [/publickey|Authentication failed|could not read Username/i, "git \u4ED3\u5E93\u9700\u8981\u8BA4\u8BC1\uFF0C\u65E0\u6CD5\u533F\u540D\u62C9\u53D6"],
  [/ENOTFOUND|EAI_AGAIN|ECONNREFUSED|ECONNRESET|ETIMEDOUT|ESOCKETTIMEDOUT|getaddrinfo|Could not resolve host|unable to access|network/i, "\u65E0\u6CD5\u8BBF\u95EE registry \u6216 git \u4ED3\u5E93\uFF0C\u8BF7\u68C0\u67E5\u7F51\u7EDC\u6216\u4EE3\u7406\u540E\u91CD\u8BD5"],
  [/EACCES|EPERM|permission denied/i, "\u6CA1\u6709\u5199\u5165\u6743\u9650\uFF0C\u8BF7\u68C0\u67E5 PI \u914D\u7F6E\u76EE\u5F55\uFF08~/.pi/agent\uFF09\u7684\u6743\u9650"],
  [/超时/, "\u6267\u884C\u8D85\u65F6"]
];
var NPM_NOISE = /A complete log|Log files were|For a full report|debug-0\.log|npm notice|npm warn/i;
var MAX_REASON = 300;
function explainFailure(err, log) {
  const message = err instanceof Error ? err.message : String(err);
  const haystack = `${log.join("\n")}
${message}`;
  const detail = [...log].reverse().map((l) => /^npm (?:error|ERR!)\s*(.*)$/i.exec(l)?.[1]?.trim()).find((l) => Boolean(l) && !NPM_NOISE.test(l ?? "")) ?? [...log].reverse().find((l) => !l.startsWith("$") && l.trim()) ?? message;
  const hint = RULES.find(([re]) => re.test(haystack))?.[1];
  const text = hint ? detail && !hint.includes(detail) && detail !== message ? `${hint}\uFF08${detail}\uFF09` : hint : detail;
  return text.length > MAX_REASON ? `${text.slice(0, MAX_REASON)}\u2026` : text;
}

// ../../../plugins/packages/src/jobs.ts
import { randomUUID } from "node:crypto";
var MAX_LOG_LINES = 200;
var MAX_LINE = 400;
var MAX_FINISHED = 20;
var ANSI = /\u001b\[[0-9;?]*[ -/]*[@-~]/g;
var JobTracker = class {
  jobs = [];
  tail = Promise.resolve();
  running;
  partial = "";
  explain;
  constructor(explain) {
    this.explain = explain;
  }
  run(action, source, label, task) {
    const existing = this.jobs.find((j) => (j.status === "queued" || j.status === "running") && j.action === action && j.source === source);
    if (existing) return structuredClone(existing);
    const job = { id: randomUUID(), action, source, label, status: "queued", startedAt: Date.now(), log: [] };
    this.jobs.push(job);
    const exec = async () => {
      job.status = "running";
      this.running = job;
      this.partial = "";
      try {
        await task();
        this.flush();
        job.status = "success";
      } catch (err) {
        this.flush();
        job.status = "error";
        job.error = this.explain(err, job.log);
      } finally {
        this.running = void 0;
        job.finishedAt = Date.now();
        this.prune();
      }
    };
    this.tail = this.tail.then(exec);
    return structuredClone(job);
  }
  /** Feed raw process output to the job that is currently running. */
  append(chunk) {
    const job = this.running;
    if (!job) return;
    const lines = (this.partial + chunk.replace(ANSI, "").replace(/\r\n?/g, "\n")).split("\n");
    this.partial = lines.pop() ?? "";
    for (const line of lines) this.push(job, line);
  }
  list() {
    return this.jobs.map((j) => structuredClone(j)).reverse();
  }
  dismiss(id) {
    this.jobs = this.jobs.filter((j) => j.id !== id || j.status === "queued" || j.status === "running");
  }
  flush() {
    if (this.running && this.partial.trim()) this.push(this.running, this.partial);
    this.partial = "";
  }
  push(job, line) {
    const text = line.trimEnd().slice(0, MAX_LINE);
    if (!text.trim()) return;
    job.log.push(text);
    if (job.log.length > MAX_LOG_LINES) job.log.splice(0, job.log.length - MAX_LOG_LINES);
  }
  prune() {
    const finished = this.jobs.filter((j) => j.status === "success" || j.status === "error");
    const drop = new Set(finished.slice(0, Math.max(0, finished.length - MAX_FINISHED)));
    if (drop.size) this.jobs = this.jobs.filter((j) => !drop.has(j));
  }
};

// ../../../plugins/packages/src/manifest.ts
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
var KINDS = ["extensions", "skills", "prompts", "themes"];
function readManifest(root) {
  let pkg = {};
  try {
    pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  } catch {
  }
  const declared = pkg.pi && typeof pkg.pi === "object" ? pkg.pi : void 0;
  const kinds = KINDS.filter((k) => {
    const entry = declared?.[k];
    if (Array.isArray(entry)) return entry.length > 0;
    return existsSync(join(root, k));
  });
  const text = (v) => typeof v === "string" && v ? v : void 0;
  return { name: text(pkg.name), version: text(pkg.version), description: text(pkg.description), kinds };
}

// ../../../plugins/packages/src/registry.ts
var DEFAULT_REGISTRY = "https://registry.npmjs.org";
var PACKAGE_KEYWORD = "pi-package";
var MAX_QUERY = 100;
function resolveRegistry(value) {
  if (typeof value !== "string" || !value.trim()) return DEFAULT_REGISTRY;
  let url;
  try {
    url = new URL(value.trim());
  } catch {
    throw new Error("pluginSettings.packages.registry \u4E0D\u662F\u5408\u6CD5\u5730\u5740");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") throw new Error("registry \u5FC5\u987B\u662F http(s) \u5730\u5740");
  return url.toString().replace(/\/+$/, "");
}
function searchUrl(registry, query, page, size) {
  const text = `keywords:${PACKAGE_KEYWORD} ${query.trim().slice(0, MAX_QUERY)}`.trim();
  const params = new URLSearchParams({ text, size: String(size), from: String(page * size) });
  return `${registry}/-/v1/search?${params}`;
}
var str = (v) => typeof v === "string" && v ? v : void 0;
function parseSearch(json, page, size) {
  const root = json && typeof json === "object" ? json : {};
  const objects = Array.isArray(root.objects) ? root.objects : [];
  const items = [];
  for (const entry of objects) {
    const pkg = entry?.package;
    const name = str(pkg?.name);
    const version = str(pkg?.version);
    if (!pkg || !name || !version) continue;
    const keywords = Array.isArray(pkg.keywords) ? pkg.keywords.filter((k) => typeof k === "string") : [];
    if (!keywords.includes(PACKAGE_KEYWORD)) continue;
    const links = pkg.links && typeof pkg.links === "object" ? pkg.links : {};
    const author = pkg.author?.name ?? pkg.publisher?.username;
    items.push({
      name,
      version,
      description: str(pkg.description)?.slice(0, 500),
      author: str(author),
      date: str(pkg.date),
      keywords: keywords.filter((k) => k !== PACKAGE_KEYWORD).slice(0, 8),
      source: `npm:${name}`,
      links: { npm: str(links.npm), homepage: str(links.homepage), repository: str(links.repository) }
    });
  }
  return { total: typeof root.total === "number" ? root.total : items.length, items, page, size };
}

// ../../../plugins/packages/src/source.ts
var NPM_SPEC = /^(?:@[a-z0-9][a-z0-9._~-]*\/)?[a-z0-9][a-z0-9._~-]*(?:@[a-z0-9._~^<>=|*+-]+)?$/i;
var GIT_SPEC = /^(?:git:|https:\/\/)[a-z0-9][a-z0-9.-]*\/[\w.~/-]+(?:@[\w.~/-]+)?$/i;
var MAX_SOURCE = 300;
function normalizeSource(input) {
  const raw = typeof input === "string" ? input.trim() : "";
  if (!raw) throw new Error("\u8BF7\u586B\u5199\u5305\u540D\u6216\u6765\u6E90");
  if (raw.length > MAX_SOURCE) throw new Error("\u6765\u6E90\u8FC7\u957F");
  if (raw.startsWith("npm:")) {
    if (!NPM_SPEC.test(raw.slice(4))) throw new Error("npm \u5305\u540D\u4E0D\u5408\u6CD5");
    return raw;
  }
  if (raw.startsWith("git:") || raw.startsWith("https://")) {
    if (!GIT_SPEC.test(raw) || raw.split("/").includes("..")) throw new Error("git \u6765\u6E90\u4E0D\u5408\u6CD5\uFF0C\u793A\u4F8B\uFF1Agit:github.com/owner/repo@v1");
    return raw;
  }
  if (NPM_SPEC.test(raw)) return `npm:${raw}`;
  throw new Error("\u4EC5\u652F\u6301 npm \u5305\u540D\uFF08npm:pkg@1.0.0\uFF09\u6216 git \u5730\u5740\uFF08git:github.com/owner/repo\uFF09");
}
function npmNameOf(source) {
  if (!source.startsWith("npm:")) return void 0;
  const spec = source.slice(4);
  const at = spec.lastIndexOf("@");
  return at > 0 ? spec.slice(0, at) : spec;
}

// ../../../plugins/packages/src/index.ts
var pi = await import("@earendil-works/pi-coding-agent").catch(() => void 0);
var SEARCH_TIMEOUT_MS = 15e3;
var PAGE_SIZE = 24;
function fallbackName(source) {
  return npmNameOf(source) ?? source.replace(/^git:/, "").replace(/^https:\/\//, "").replace(/@[^/@]+$/, "");
}
var index_default = definePlugin({
  name: "packages",
  version: "0.1.0",
  setup(ctx) {
    if (!pi) {
      ctx.log.warn("PI Agent \u672A\u5B89\u88C5\uFF0CPI \u6269\u5C55\u5305\u7BA1\u7406\u4E0D\u53EF\u7528");
      return;
    }
    const { SettingsManager, DefaultPackageManager } = pi;
    const registry = resolveRegistry(ctx.settings.registry);
    const settings = SettingsManager.create(ctx.cwd, ctx.agentDir);
    const manager = new DefaultPackageManager({ cwd: ctx.cwd, agentDir: ctx.agentDir, settingsManager: settings });
    const jobs = new JobTracker(explainFailure);
    if (!captureCommandOutput(manager, (chunk) => jobs.append(chunk))) ctx.log.warn("\u65E0\u6CD5\u6355\u83B7 npm \u8F93\u51FA\uFF0C\u5B89\u88C5\u5931\u8D25\u65F6\u53EA\u80FD\u663E\u793A\u9000\u51FA\u7801");
    const list = async () => {
      await settings.reload();
      return manager.listConfiguredPackages().map((p) => {
        const manifest = p.installedPath ? readManifest(p.installedPath) : void 0;
        return {
          source: p.source,
          scope: p.scope,
          name: manifest?.name ?? fallbackName(p.source),
          version: manifest?.version,
          description: manifest?.description,
          kinds: manifest?.kinds ?? [],
          installed: Boolean(p.installedPath),
          filtered: p.filtered
        };
      });
    };
    const apply = async () => {
      await settings.flush();
      await ctx.reloadSessions();
    };
    ctx.rpc("list", list);
    ctx.rpc("search", async (params) => {
      const query = typeof params.query === "string" ? params.query : "";
      const page = Math.max(0, Math.min(Number.isInteger(params.page) ? params.page : 0, 500));
      let res;
      try {
        res = await fetch(searchUrl(registry, query, page, PAGE_SIZE), { signal: AbortSignal.timeout(SEARCH_TIMEOUT_MS), headers: { accept: "application/json" } });
      } catch (err) {
        throw new Error(`\u65E0\u6CD5\u8FDE\u63A5\u63D2\u4EF6\u5E02\u573A\uFF1A${err instanceof Error ? err.message : err}`);
      }
      if (!res.ok) throw new Error(`\u63D2\u4EF6\u5E02\u573A\u8FD4\u56DE ${res.status}`);
      return parseSearch(await res.json(), page, PAGE_SIZE);
    });
    ctx.rpc("install", (params) => {
      const source = normalizeSource(params.source);
      return jobs.run("install", source, typeof params.label === "string" && params.label ? params.label.slice(0, 120) : fallbackName(source), async () => {
        await settings.reload();
        await manager.installAndPersist(source);
        await apply();
      });
    });
    ctx.rpc("remove", (params) => {
      if (typeof params.source !== "string") throw new Error("\u7F3A\u5C11 source");
      const source = params.source;
      return jobs.run("remove", source, fallbackName(source), async () => {
        await settings.reload();
        const configured = manager.listConfiguredPackages().find((p) => p.scope === "user" && p.source === source);
        if (!configured) throw new Error("\u8BE5\u5305\u4E0D\u5728\u5DF2\u5B89\u88C5\u5217\u8868\u4E2D");
        await manager.removeAndPersist(configured.source);
        await apply();
      });
    });
    ctx.rpc("update", (params) => {
      const source = typeof params.source === "string" ? params.source : void 0;
      return jobs.run("update", source ?? "*", source ? fallbackName(source) : "*", async () => {
        await settings.reload();
        if (source && !manager.listConfiguredPackages().some((p) => p.source === source)) throw new Error("\u8BE5\u5305\u4E0D\u5728\u5DF2\u5B89\u88C5\u5217\u8868\u4E2D");
        await manager.update(source);
        await apply();
      });
    });
    ctx.rpc("jobs", () => jobs.list());
    ctx.rpc("dismiss", (params) => {
      if (typeof params.id === "string") jobs.dismiss(params.id);
      return null;
    });
    ctx.rpc("checkUpdates", async () => {
      await settings.reload();
      const updates = await manager.checkForAvailableUpdates();
      return updates.map((u) => ({ source: u.source, name: u.displayName, scope: u.scope, type: u.type }));
    });
  }
});
export {
  index_default as default
};
