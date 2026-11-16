// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/git/src/git.ts
import { spawn } from "node:child_process";
import { existsSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { isAbsolute, join, relative, resolve } from "node:path";
var MAX_OUTPUT = 4 * 1024 * 1024;
var MAX_DIFF = 2e5;
var TIMEOUT_MS = 6e4;
var GitError = class extends Error {
};
function runGit(cwd, args, options = {}) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn("git", args, {
      cwd,
      windowsHide: true,
      // Never block on a credential or editor prompt: there is no terminal to answer it.
      env: { ...process.env, GIT_TERMINAL_PROMPT: "0", GIT_EDITOR: "true", GIT_PAGER: "cat", LC_ALL: "C" }
    });
    let stdout = "";
    let stderr = "";
    let truncated = false;
    const timer = setTimeout(() => child.kill(), options.timeoutMs ?? TIMEOUT_MS);
    child.stdout.on("data", (d) => {
      if (stdout.length < MAX_OUTPUT) stdout += d.toString("utf8");
      else truncated = true;
    });
    child.stderr.on("data", (d) => {
      if (stderr.length < 64e3) stderr += d.toString("utf8");
    });
    child.on("error", (err) => {
      clearTimeout(timer);
      reject(new GitError(err.code === "ENOENT" ? "\u672A\u627E\u5230 git\uFF0C\u8BF7\u5148\u5B89\u88C5 Git \u5E76\u786E\u4FDD\u5B83\u5728 PATH \u4E2D" : err.message));
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      resolvePromise({ code: code ?? 1, stdout: truncated ? `${stdout}
\u2026(\u8F93\u51FA\u8FC7\u957F\uFF0C\u5DF2\u622A\u65AD)` : stdout, stderr });
    });
    child.stdin.on("error", () => {
    });
    child.stdin.end(options.input ?? "");
  });
}
async function git(cwd, args, input) {
  const r = await runGit(cwd, args, { input });
  if (r.code !== 0) throw new GitError((r.stderr || r.stdout).trim().split("\n").slice(0, 6).join("\n") || `git ${args[0]} \u5931\u8D25\uFF08\u9000\u51FA\u7801 ${r.code}\uFF09`);
  return r.stdout;
}
function checkDir(dir) {
  if (typeof dir !== "string" || !dir.trim() || !isAbsolute(dir)) throw new GitError("\u9700\u8981\u63D0\u4F9B\u5DE5\u4F5C\u533A\u7684\u7EDD\u5BF9\u8DEF\u5F84");
  const abs = resolve(dir);
  if (!existsSync(abs) || !statSync(abs).isDirectory()) throw new GitError(`\u76EE\u5F55\u4E0D\u5B58\u5728\uFF1A${abs}`);
  return abs;
}
function checkPathspec(p) {
  if (typeof p !== "string" || !p || p.includes("\0")) throw new GitError("\u6587\u4EF6\u8DEF\u5F84\u4E0D\u5408\u6CD5");
  if (isAbsolute(p) || p.split(/[\\/]/).includes("..") || p.startsWith("-")) throw new GitError(`\u6587\u4EF6\u8DEF\u5F84\u4E0D\u5408\u6CD5\uFF1A${p}`);
  return p;
}
function checkRef(name) {
  if (typeof name !== "string" || !name || name.startsWith("-") || /[\s~^:?*[\\\0]|\.\.|@\{|\/\/|\.lock$|\/$/.test(name)) throw new GitError(`\u5206\u652F\u6216\u5F15\u7528\u540D\u4E0D\u5408\u6CD5\uFF1A${String(name)}`);
  return name;
}
function parseStatus(out) {
  const status2 = { ahead: 0, behind: 0, files: [] };
  const parts = out.split("\0");
  for (let i = 0; i < parts.length; i++) {
    const line = parts[i] ?? "";
    if (!line) continue;
    if (line.startsWith("# branch.head ")) {
      const head = line.slice(14);
      status2.detached = head === "(detached)";
      status2.branch = status2.detached ? void 0 : head;
    } else if (line.startsWith("# branch.upstream ")) {
      status2.upstream = line.slice(18);
    } else if (line.startsWith("# branch.ab ")) {
      const m = /\+(\d+) -(\d+)/.exec(line);
      if (m) {
        status2.ahead = Number(m[1]);
        status2.behind = Number(m[2]);
      }
    } else if (line.startsWith("1 ")) {
      const f = line.split(" ");
      status2.files.push({ path: f.slice(8).join(" "), index: f[1]?.[0] ?? ".", worktree: f[1]?.[1] ?? "." });
    } else if (line.startsWith("2 ")) {
      const f = line.split(" ");
      status2.files.push({ path: f.slice(9).join(" "), origPath: parts[++i], index: f[1]?.[0] ?? ".", worktree: f[1]?.[1] ?? "." });
    } else if (line.startsWith("u ")) {
      const f = line.split(" ");
      status2.files.push({ path: f.slice(10).join(" "), index: "U", worktree: "U" });
    } else if (line.startsWith("? ")) {
      status2.files.push({ path: line.slice(2), index: "?", worktree: "?" });
    }
  }
  for (const f of status2.files) {
    if (f.index === ".") f.index = " ";
    if (f.worktree === ".") f.worktree = " ";
  }
  return status2;
}
async function status(dir) {
  const probe = await runGit(dir, ["rev-parse", "--show-toplevel"]);
  if (probe.code !== 0) return { isRepo: false, ahead: 0, behind: 0, files: [] };
  const out = await git(dir, ["status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all"]);
  const root = probe.stdout.trim();
  const status2 = parseStatus(out);
  const rel = relative(root, resolve(dir)).replaceAll("\\", "/");
  if (rel && rel !== ".") {
    const strip = (p) => p.startsWith(`${rel}/`) ? p.slice(rel.length + 1) : p === rel ? "" : void 0;
    status2.files = status2.files.flatMap((f) => {
      const path = strip(f.path);
      if (!path) return [];
      const origPath = f.origPath ? strip(f.origPath) : void 0;
      return [{ ...f, path, ...origPath ? { origPath } : { origPath: void 0 } }];
    });
  }
  return { isRepo: true, root, ...status2 };
}
var SEP = "";
var REC = "";
function parseLog(out) {
  return out.split(REC).map((r) => r.replace(/^\n/, "")).filter(Boolean).map((r) => {
    const [hash = "", author = "", date = "", refs = "", subject = ""] = r.split(SEP);
    return { hash, short: hash.slice(0, 7), author, date, refs, subject };
  });
}
async function log(dir, limit) {
  const n = Math.min(Math.max(Math.floor(limit) || 50, 1), 200);
  const r = await runGit(dir, ["log", `-n${n}`, `--format=%H${SEP}%an${SEP}%aI${SEP}%D${SEP}%s${REC}`]);
  if (r.code !== 0) return [];
  return parseLog(r.stdout);
}
async function branches(dir) {
  const out = await git(dir, ["branch", "-a", "--format=%(HEAD)	%(refname)	%(refname:short)	%(upstream:short)"]);
  const list = [];
  for (const line of out.split("\n")) {
    const [head, ref = "", short = "", upstream] = line.split("	");
    if (!ref || ref.endsWith("/HEAD") || ref.includes("(")) continue;
    const remote = ref.startsWith("refs/remotes/");
    list.push({ name: short, current: head === "*", remote, ...upstream ? { upstream } : {} });
  }
  return list;
}
function clip(text) {
  return text.length > MAX_DIFF ? { text: text.slice(0, MAX_DIFF), truncated: true } : { text, truncated: false };
}
function emptyFile() {
  const file = join(tmpdir(), "prometheus-empty-file");
  if (!existsSync(file)) writeFileSync(file, "");
  return file;
}
function untrackedHeaders(text, path) {
  const shown = path.replaceAll("\\", "/");
  return text.split("\n").map((line) => {
    if (line.startsWith("diff --git ")) return `diff --git a/${shown} b/${shown}`;
    if (line.startsWith("--- ")) return "--- /dev/null";
    if (line.startsWith("+++ ")) return `+++ b/${shown}`;
    return line;
  }).join("\n");
}
async function diff(dir, path, staged, untracked) {
  checkPathspec(path);
  if (untracked) {
    const target = isAbsolute(path) ? path : join(dir, path);
    const r = await runGit(dir, ["diff", "--no-index", "--no-color", "--", emptyFile(), target]);
    return clip(untrackedHeaders(r.stdout, path));
  }
  return clip(await git(dir, ["diff", "--no-color", "--relative", ...staged ? ["--cached"] : [], "--", path]));
}
async function show(dir, hash) {
  if (!/^[0-9a-f]{7,40}$/i.test(hash)) throw new GitError("\u63D0\u4EA4\u53F7\u4E0D\u5408\u6CD5");
  return clip(await git(dir, ["show", "--no-color", "--stat", "--patch", "--format=fuller", hash, "--"]));
}
var EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904";
var MAX_UNTRACKED = 1e6;
async function changedFiles(dir, rev) {
  const [names, nums] = await Promise.all([
    git(dir, ["diff", "--no-color", "--no-renames", "--relative", "--name-status", "-z", ...rev, "--"]),
    git(dir, ["diff", "--no-color", "--no-renames", "--relative", "--numstat", "-z", ...rev, "--"])
  ]);
  const status2 = /* @__PURE__ */ new Map();
  const parts = names.split("\0");
  for (let i = 0; i + 1 < parts.length; i += 2) if (parts[i]) status2.set(parts[i + 1], parts[i].charAt(0));
  const files = [];
  for (const rec of nums.split("\0")) {
    const m = /^(\d+|-)\t(\d+|-)\t(.+)$/s.exec(rec);
    if (!m) continue;
    const path = m[3];
    const binary = m[1] === "-";
    files.push({ path, status: status2.get(path) ?? "M", added: binary ? 0 : Number(m[1]), deleted: binary ? 0 : Number(m[2]), binary });
  }
  return files.sort((a, b) => a.path.localeCompare(b.path));
}
async function hasCommit(dir, rev) {
  return (await runGit(dir, ["rev-parse", "--verify", "--quiet", `${rev}^{commit}`])).code === 0;
}
function checkHash(hash) {
  if (!/^[0-9a-f]{7,40}$/i.test(hash)) throw new GitError("\u63D0\u4EA4\u53F7\u4E0D\u5408\u6CD5");
  return hash;
}
async function commitRev(dir, hash) {
  checkHash(hash);
  return [await hasCommit(dir, `${hash}^`) ? `${hash}^` : EMPTY_TREE, hash];
}
async function commitFiles(dir, hash) {
  return changedFiles(dir, await commitRev(dir, hash));
}
async function commitDiff(dir, hash, path) {
  checkPathspec(path);
  return clip(await git(dir, ["diff", "--no-color", "--no-renames", "--relative", ...await commitRev(dir, hash), "--", path]));
}
async function defaultBase(dir) {
  const current = (await runGit(dir, ["branch", "--show-current"])).stdout.trim();
  const head = (await runGit(dir, ["symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"])).stdout.trim();
  for (const c of [head, "main", "master", "develop", "origin/main", "origin/master"]) {
    if (c && c !== current && await hasCommit(dir, c)) return c;
  }
  const up = (await runGit(dir, ["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"])).stdout.trim();
  return up && await hasCommit(dir, up) ? up : null;
}
async function rangeFiles(dir, base) {
  const b = base ? checkRef(base) : await defaultBase(dir);
  if (!b) throw new GitError("\u627E\u4E0D\u5230\u53EF\u5BF9\u6BD4\u7684\u57FA\u7EBF\u5206\u652F\uFF0C\u8BF7\u624B\u52A8\u9009\u62E9");
  return { base: b, files: await changedFiles(dir, [`${b}...HEAD`]) };
}
async function rangeDiff(dir, base, path) {
  checkPathspec(path);
  return clip(await git(dir, ["diff", "--no-color", "--no-renames", "--relative", `${checkRef(base)}...HEAD`, "--", path]));
}
var norm = (p) => p.split("\\").join("/");
async function untrackedFiles(dir) {
  const r = await runGit(dir, ["ls-files", "--others", "--exclude-standard", "-z"]);
  return r.code === 0 ? r.stdout.split("\0").filter(Boolean) : [];
}
async function changes(dir, only, cwd) {
  const base = await hasCommit(dir, "HEAD") ? "HEAD" : EMPTY_TREE;
  const [tracked, fresh] = await Promise.all([changedFiles(dir, [base]), untrackedFiles(dir)]);
  const all = [...tracked];
  const seen = new Set(tracked.map((f) => f.path));
  for (const rel of fresh) {
    if (seen.has(rel)) continue;
    let added = 0;
    let binary = false;
    try {
      const abs = join(dir, rel);
      if (statSync(abs).size > MAX_UNTRACKED) binary = true;
      else {
        const buf = readFileSync(abs);
        binary = buf.includes(0);
        if (!binary) added = buf.length === 0 ? 0 : buf.toString("utf8").split("\n").length - (buf[buf.length - 1] === 10 ? 1 : 0);
      }
    } catch {
      continue;
    }
    all.push({ path: norm(rel), status: "A", added, deleted: 0, binary });
  }
  if (!only) return all.sort((a, b) => a.path.localeCompare(b.path));
  const fold = (p) => process.platform === "win32" ? p.toLowerCase() : p;
  const want = /* @__PURE__ */ new Set();
  for (const p of only) {
    if (typeof p !== "string" || !p) continue;
    const rel = norm(relative(dir, isAbsolute(p) ? p : resolve(cwd && isAbsolute(cwd) ? cwd : dir, p)));
    if (rel && !rel.startsWith("..") && !isAbsolute(rel)) want.add(fold(rel));
  }
  return all.filter((f) => want.has(fold(f.path))).sort((a, b) => a.path.localeCompare(b.path));
}
async function fileDiff(dir, path) {
  checkPathspec(path);
  const tracked = (await runGit(dir, ["ls-files", "--error-unmatch", "--", path])).code === 0;
  if (!tracked && existsSync(join(dir, path))) return diff(dir, path, false, true);
  const base = await hasCommit(dir, "HEAD") ? "HEAD" : EMPTY_TREE;
  return clip(await git(dir, ["diff", "--no-color", "--no-renames", "--relative", base, "--", path]));
}
async function stage(dir, paths) {
  await git(dir, ["add", "--", ...paths.map(checkPathspec)]);
}
async function unstage(dir, paths) {
  const specs = paths.map(checkPathspec);
  const r = await runGit(dir, ["restore", "--staged", "--", ...specs]);
  if (r.code !== 0) await git(dir, ["rm", "--cached", "-r", "-q", "--", ...specs]);
}
async function discard(dir, files) {
  const tracked = files.filter((f) => !f.untracked).map((f) => checkPathspec(f.path));
  const fresh = files.filter((f) => f.untracked).map((f) => checkPathspec(f.path));
  if (tracked.length) await git(dir, ["restore", "--worktree", "--", ...tracked]);
  if (fresh.length) await git(dir, ["clean", "-f", "-q", "--", ...fresh]);
}
async function commit(dir, message) {
  const msg = typeof message === "string" ? message.trim() : "";
  if (!msg) throw new GitError("\u63D0\u4EA4\u8BF4\u660E\u4E0D\u80FD\u4E3A\u7A7A");
  await git(dir, ["commit", "-F", "-"], msg);
  return (await git(dir, ["rev-parse", "--short", "HEAD"])).trim();
}
async function checkout(dir, name, create) {
  const ref = checkRef(name);
  await git(dir, create ? ["switch", "-c", ref] : ["switch", ref]);
}
async function fetch(dir) {
  return (await git(dir, ["fetch", "--prune"])).trim();
}
async function pull(dir) {
  return (await git(dir, ["pull", "--ff-only"])).trim() || "\u5DF2\u662F\u6700\u65B0";
}
async function push(dir) {
  const st = await status(dir);
  if (!st.branch) throw new GitError("\u5F53\u524D\u4E0D\u5728\u4EFB\u4F55\u5206\u652F\u4E0A\uFF0C\u65E0\u6CD5\u63A8\u9001");
  const args = st.upstream ? ["push"] : ["push", "-u", "origin", st.branch];
  const r = await runGit(dir, args);
  if (r.code !== 0) throw new GitError((r.stderr || r.stdout).trim().split("\n").slice(0, 6).join("\n"));
  return (r.stderr + r.stdout).trim() || "\u5DF2\u63A8\u9001";
}
async function init(dir) {
  await git(dir, ["init"]);
}

// ../../../plugins/git/src/index.ts
var str = (v) => typeof v === "string" ? v : "";
var strs = (v) => Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
var index_default = definePlugin({
  name: "git",
  version: "0.1.0",
  optional: true,
  setup(ctx) {
    const dirOf = async (params) => {
      const dir = checkDir(params.workspace);
      const known = await ctx.workspaces();
      const norm2 = (p) => (process.platform === "win32" ? p.toLowerCase() : p).replace(/[\\/]+$/, "");
      if (!known.some((w) => norm2(w.path) === norm2(dir))) throw new GitError("\u8BE5\u76EE\u5F55\u4E0D\u662F\u5DF2\u6DFB\u52A0\u7684\u5DE5\u4F5C\u533A");
      return dir;
    };
    ctx.rpc("status", async (p) => status(await dirOf(p)));
    ctx.rpc("log", async (p) => log(await dirOf(p), Number(p.limit) || 50));
    ctx.rpc("branches", async (p) => branches(await dirOf(p)));
    ctx.rpc("diff", async (p) => diff(await dirOf(p), str(p.path), p.staged === true, p.untracked === true));
    ctx.rpc("show", async (p) => show(await dirOf(p), str(p.hash)));
    ctx.rpc("commitFiles", async (p) => commitFiles(await dirOf(p), str(p.hash)));
    ctx.rpc("commitDiff", async (p) => commitDiff(await dirOf(p), str(p.hash), str(p.path)));
    ctx.rpc("rangeFiles", async (p) => rangeFiles(await dirOf(p), str(p.base)));
    ctx.rpc("rangeDiff", async (p) => rangeDiff(await dirOf(p), str(p.base), str(p.path)));
    ctx.rpc("changes", async (p) => changes(await dirOf(p), Array.isArray(p.paths) ? strs(p.paths) : void 0, str(p.cwd) || void 0));
    ctx.rpc("fileDiff", async (p) => fileDiff(await dirOf(p), str(p.path)));
    ctx.rpc("stage", async (p) => void await stage(await dirOf(p), strs(p.paths)));
    ctx.rpc("unstage", async (p) => void await unstage(await dirOf(p), strs(p.paths)));
    ctx.rpc("discard", async (p) => {
      const files = Array.isArray(p.files) ? p.files.map((f) => ({ path: str(f.path), untracked: f.untracked === true })) : [];
      await discard(await dirOf(p), files);
    });
    ctx.rpc("commit", async (p) => commit(await dirOf(p), str(p.message)));
    ctx.rpc("checkout", async (p) => void await checkout(await dirOf(p), str(p.name), p.create === true));
    ctx.rpc("fetch", async (p) => fetch(await dirOf(p)));
    ctx.rpc("pull", async (p) => pull(await dirOf(p)));
    ctx.rpc("push", async (p) => push(await dirOf(p)));
    ctx.rpc("init", async (p) => void await init(await dirOf(p)));
  }
});
export {
  index_default as default
};
