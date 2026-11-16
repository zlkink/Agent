// ../../../plugins/logs/src/index.ts
import { join } from "node:path";

// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/logs/src/log-store.ts
import { format } from "node:util";
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
var MAX_ENTRIES = 500;
var MAX_MESSAGE = 8e3;
function scrub(text) {
  return text.replace(/\b(sk|pk|ghp|gho|xox[abp])[-_][A-Za-z0-9_-]{12,}/g, "$1-\u2022\u2022\u2022\u2022").replace(/(Bearer\s+)[A-Za-z0-9._~+/=-]{12,}/gi, "$1\u2022\u2022\u2022\u2022").replace(/((?:api[_-]?key|token|secret|password|authorization)["']?\s*[:=]\s*["']?)[^\s"',;]{6,}/gi, "$1\u2022\u2022\u2022\u2022");
}
var LogStore = class {
  entries = [];
  nextId = 1;
  writes = 0;
  file;
  max;
  constructor(file, max = MAX_ENTRIES) {
    this.file = file;
    this.max = max;
    try {
      const lines = readFileSync(file, "utf8").split("\n").filter(Boolean);
      for (const line of lines.slice(-max)) {
        try {
          const e = JSON.parse(line);
          if (typeof e.at === "number" && typeof e.message === "string") this.entries.push({ ...e, id: this.nextId++ });
        } catch {
        }
      }
    } catch {
    }
  }
  add(level, message, at = Date.now()) {
    const text = scrub(message);
    const entry = { id: this.nextId++, at, level, message: text.length > MAX_MESSAGE ? `${text.slice(0, MAX_MESSAGE)}\u2026` : text };
    this.entries.push(entry);
    if (this.entries.length > this.max) this.entries.splice(0, this.entries.length - this.max);
    try {
      mkdirSync(dirname(this.file), { recursive: true });
      appendFileSync(this.file, `${JSON.stringify({ at: entry.at, level: entry.level, message: entry.message })}
`);
      if (++this.writes % this.max === 0) this.rewrite();
    } catch {
    }
    return entry;
  }
  list(level, limit = 200) {
    const rows = level ? this.entries.filter((e) => e.level === level) : this.entries;
    return rows.slice(-limit).reverse();
  }
  counts() {
    let error = 0;
    for (const e of this.entries) if (e.level === "error") error++;
    return { error, warn: this.entries.length - error };
  }
  clear() {
    this.entries = [];
    try {
      writeFileSync(this.file, "");
    } catch {
    }
  }
  rewrite() {
    writeFileSync(this.file, this.entries.map((e) => JSON.stringify({ at: e.at, level: e.level, message: e.message })).join("\n") + "\n");
  }
};
function captureConsole(store, target = console) {
  const original = { error: target.error, warn: target.warn };
  let busy = false;
  const wrap = (level) => function(...args) {
    original[level].apply(target, args);
    if (busy) return;
    busy = true;
    try {
      store.add(level, format(...args));
    } catch {
    } finally {
      busy = false;
    }
  };
  target.error = wrap("error");
  target.warn = wrap("warn");
  return () => {
    if (target.error !== original.error) target.error = original.error;
    if (target.warn !== original.warn) target.warn = original.warn;
  };
}

// ../../../plugins/logs/src/index.ts
var index_default = definePlugin({
  name: "logs",
  version: "0.1.0",
  optional: true,
  setup(ctx) {
    const store = new LogStore(join(ctx.dataDir, "errors.jsonl"));
    const restore = captureConsole(store);
    ctx.onDispose(restore);
    const level = (v) => v === "error" || v === "warn" ? v : void 0;
    ctx.rpc("list", (params) => {
      const limit = typeof params?.limit === "number" ? Math.min(Math.max(1, params.limit), 500) : 200;
      return { entries: store.list(level(params?.level), limit), counts: store.counts() };
    });
    ctx.rpc("clear", () => {
      store.clear();
      return { entries: [], counts: store.counts() };
    });
  }
});
export {
  index_default as default
};
