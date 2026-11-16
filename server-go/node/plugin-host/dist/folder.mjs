// ../../../plugins/folder/src/index.ts
import { readdir, realpath, stat } from "node:fs/promises";
import { isAbsolute, join, relative, resolve, sep } from "node:path";

// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/folder/src/index.ts
var str = (v) => typeof v === "string" ? v : "";
var MAX_ENTRIES = 2e3;
var norm = (p) => process.platform === "win32" ? p.toLowerCase() : p;
var SKIP_DIRS = /* @__PURE__ */ new Set(["node_modules", ".git", "dist", "build", "out", ".next", ".turbo", ".cache", "target", "__pycache__", ".venv", "venv", ".idea", ".vscode"]);
var MAX_SCAN = 2e4;
var INDEX_TTL = 5e3;
var indexes = /* @__PURE__ */ new Map();
function score(rel, name, q) {
  const r = rel.toLowerCase();
  const n = name.toLowerCase();
  if (!q) return 0;
  if (n === q) return 1;
  if (n.startsWith(q)) return 2;
  if (n.includes(q)) return 3;
  if (r.includes(q)) return 4;
  let i = 0;
  for (const c of r) if (c === q[i] && ++i === q.length) return 5;
  return -1;
}
function inside(path, root) {
  const rel = relative(norm(root), norm(path));
  return rel === "" || !rel.startsWith("..") && !isAbsolute(rel);
}
var index_default = definePlugin({
  name: "folder",
  version: "0.1.0",
  // Only adds a title-bar button and a viewer tab; nothing depends on it.
  optional: true,
  setup(ctx) {
    ctx.rpc("search", async (p) => {
      const cwd = str(p.cwd) || ctx.cwd;
      const roots = [...(await ctx.workspaces()).map((w) => w.path), ctx.cwd, cwd];
      let root;
      try {
        root = await realpath(cwd);
      } catch {
        return { items: [] };
      }
      let allowed = false;
      for (const r of roots) {
        const real = await realpath(r).catch(() => void 0);
        if (real && inside(root, real)) {
          allowed = true;
          break;
        }
      }
      if (!allowed) return { items: [] };
      let idx = indexes.get(root);
      if (!idx || Date.now() - idx.at > INDEX_TTL) {
        const files = [];
        const queue = [root];
        while (queue.length && files.length < MAX_SCAN) {
          const dirPath = queue.shift();
          const dirents = await readdir(dirPath, { withFileTypes: true }).catch(() => []);
          for (const d of dirents) {
            const full = join(dirPath, d.name);
            const isDir = d.isDirectory();
            if (!isDir && !d.isFile()) continue;
            if (isDir && (SKIP_DIRS.has(d.name) || d.name === ".")) continue;
            files.push({ name: d.name, path: relative(root, full).split(sep).join("/"), dir: isDir });
            if (isDir) queue.push(full);
          }
        }
        idx = { at: Date.now(), files };
        indexes.set(root, idx);
      }
      const q = str(p.query).toLowerCase().replace(/\\/g, "/");
      const scored = [];
      for (const e of idx.files) {
        const s = score(e.path, e.name, q);
        if (s >= 0) scored.push({ e, s });
      }
      scored.sort((a, b) => a.s - b.s || a.e.path.split("/").length - b.e.path.split("/").length || a.e.path.localeCompare(b.e.path));
      return { items: scored.slice(0, 8).map(({ e }) => ({ path: e.path, dir: e.dir })) };
    });
    ctx.rpc("list", async (p) => {
      const cwd = str(p.cwd) || ctx.cwd;
      const roots = [...(await ctx.workspaces()).map((w) => w.path), ctx.cwd, cwd];
      const target = resolve(cwd, str(p.path) || ".");
      if (target.includes("\0")) throw new Error("\u8DEF\u5F84\u65E0\u6548");
      let real;
      try {
        real = await realpath(target);
      } catch {
        throw new Error(`\u6587\u4EF6\u5939\u4E0D\u5B58\u5728: ${target}`);
      }
      let allowed = false;
      for (const r of roots) {
        try {
          if (inside(real, await realpath(r))) {
            allowed = true;
            break;
          }
        } catch {
        }
      }
      if (!allowed) throw new Error("\u53EA\u80FD\u6D4F\u89C8\u5DF2\u6DFB\u52A0\u5DE5\u4F5C\u533A\u5185\u7684\u6587\u4EF6\u5939");
      if (!(await stat(real)).isDirectory()) throw new Error("\u4E0D\u662F\u6587\u4EF6\u5939");
      const dirents = await readdir(real, { withFileTypes: true });
      const entries = [];
      for (const d of dirents.slice(0, MAX_ENTRIES)) {
        const path = join(real, d.name);
        let dir = d.isDirectory();
        let size;
        if (d.isSymbolicLink() || d.isFile()) {
          const info = await stat(path).catch(() => void 0);
          if (info) {
            dir = info.isDirectory();
            if (info.isFile()) size = info.size;
          }
        } else if (!dir) continue;
        entries.push({ name: d.name, path, dir, size });
      }
      entries.sort((a, b) => Number(b.dir) - Number(a.dir) || a.name.localeCompare(b.name, void 0, { numeric: true, sensitivity: "base" }));
      return { path: real, entries, truncated: dirents.length > MAX_ENTRIES };
    });
  }
});
export {
  index_default as default
};
