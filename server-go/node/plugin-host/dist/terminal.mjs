// ../../../plugins/terminal/src/index.ts
import { randomUUID } from "node:crypto";
import { stat } from "node:fs/promises";
import { resolve } from "node:path";

// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/terminal/src/shell.ts
import { spawn, spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
var MAX_BUFFER = 4e5;
function shellSpec(platform = process.platform, env = process.env) {
  if (platform === "win32") {
    return {
      command: "powershell.exe",
      args: ["-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "-"],
      init: "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8\n"
    };
  }
  const fallback = platform === "android" || !existsSync("/bin/bash") ? "/system/bin/sh" : "/bin/bash";
  return { command: env.SHELL || (existsSync(fallback) ? fallback : "sh"), args: [], init: "" };
}
var ShellSession = class {
  id;
  cwd;
  proc;
  buf = "";
  /** Characters dropped from the front of `buf`; offsets handed to clients are absolute. */
  base = 0;
  exitCode = null;
  closed = false;
  spec;
  lastUsed = Date.now();
  constructor(id, cwd, spec = shellSpec()) {
    this.id = id;
    this.cwd = cwd;
    this.spec = spec;
    this.start();
  }
  append(text) {
    this.buf += text;
    if (this.buf.length > MAX_BUFFER) {
      const drop = this.buf.length - MAX_BUFFER;
      this.buf = this.buf.slice(drop);
      this.base += drop;
    }
  }
  start() {
    this.exitCode = null;
    const proc = spawn(this.spec.command, this.spec.args, {
      cwd: this.cwd,
      env: { ...process.env, TERM: "dumb", NO_COLOR: "1" },
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
      detached: process.platform !== "win32"
    });
    this.proc = proc;
    proc.stdout?.setEncoding("utf8").on("data", (d) => this.append(d));
    proc.stderr?.setEncoding("utf8").on("data", (d) => this.append(d));
    proc.stdin?.on("error", () => void 0);
    proc.on("error", (e) => {
      this.append(`
[\u65E0\u6CD5\u542F\u52A8 shell: ${e.message}]
`);
      if (this.proc === proc) this.exitCode = -1;
    });
    proc.on("exit", (code) => {
      if (this.proc !== proc) return;
      this.exitCode = code ?? 0;
      if (!this.closed) this.append(`
[shell \u5DF2\u9000\u51FA\uFF0C\u4EE3\u7801 ${code ?? 0}]
`);
    });
    if (this.spec.init) proc.stdin?.write(this.spec.init);
  }
  get alive() {
    return this.exitCode === null;
  }
  read(since) {
    this.lastUsed = Date.now();
    const from = Math.max(since, this.base);
    return { data: this.buf.slice(from - this.base), next: this.base + this.buf.length, alive: this.alive, exitCode: this.exitCode };
  }
  /** Send one command line; it is echoed into the output so history survives a reload. */
  run(line) {
    this.lastUsed = Date.now();
    if (!this.alive) throw new Error("shell \u5DF2\u9000\u51FA\uFF0C\u8BF7\u91CD\u542F\u7EC8\u7AEF");
    this.append(`> ${line}
`);
    this.proc?.stdin?.write(`${line}
`);
  }
  /** Stop whatever is running by replacing the shell (pipes cannot carry Ctrl+C). Output so far is kept. */
  interrupt() {
    this.append("^C\n");
    this.killProc();
    this.start();
  }
  killProc() {
    const proc = this.proc;
    this.proc = void 0;
    if (!proc?.pid) return;
    try {
      if (process.platform === "win32") spawnSync("taskkill", ["/pid", String(proc.pid), "/T", "/F"], { windowsHide: true });
      else process.kill(-proc.pid, "SIGKILL");
    } catch {
    }
  }
  close() {
    this.closed = true;
    this.killProc();
    this.exitCode ??= 0;
  }
};

// ../../../plugins/terminal/src/index.ts
var str = (v) => typeof v === "string" ? v : "";
var MAX_SESSIONS = 8;
var IDLE_MS = 30 * 6e4;
var index_default = definePlugin({
  name: "terminal",
  version: "0.1.0",
  // Nothing else depends on it: the terminal tab only exists while the user opens one.
  optional: true,
  setup(ctx) {
    const sessions = /* @__PURE__ */ new Map();
    const need = (id) => {
      const s = sessions.get(str(id));
      if (!s) throw new Error("\u7EC8\u7AEF\u5DF2\u5173\u95ED");
      return s;
    };
    const reap = setInterval(() => {
      for (const [id, s] of sessions) {
        if (Date.now() - s.lastUsed > IDLE_MS) {
          s.close();
          sessions.delete(id);
        }
      }
    }, 6e4);
    reap.unref();
    ctx.onDispose(() => {
      clearInterval(reap);
      for (const s of sessions.values()) s.close();
      sessions.clear();
    });
    const allowedCwd = async (wanted) => {
      const norm = (p) => process.platform === "win32" ? resolve(p).toLowerCase() : resolve(p);
      const known = [ctx.cwd, ...(await ctx.workspaces()).map((w) => w.path)].map(norm);
      const dir = resolve(wanted || ctx.cwd);
      if (!known.includes(norm(dir))) throw new Error("\u8BE5\u76EE\u5F55\u4E0D\u5728\u5DF2\u6DFB\u52A0\u7684\u9879\u76EE\u4E2D");
      if (!(await stat(dir).catch(() => void 0))?.isDirectory()) throw new Error("\u76EE\u5F55\u4E0D\u5B58\u5728");
      return dir;
    };
    ctx.rpc("open", async (p) => {
      if (sessions.size >= MAX_SESSIONS) throw new Error(`\u6700\u591A\u540C\u65F6\u6253\u5F00 ${MAX_SESSIONS} \u4E2A\u7EC8\u7AEF`);
      const cwd = await allowedCwd(str(p.cwd));
      const id = randomUUID();
      sessions.set(id, new ShellSession(id, cwd));
      return { id, cwd };
    });
    ctx.rpc("read", (p) => {
      const s = need(p.id);
      const since = typeof p.since === "number" && p.since >= 0 ? p.since : 0;
      return s.read(since);
    });
    ctx.rpc("run", (p) => {
      const line = str(p.line);
      if (line.length > 2e4) throw new Error("\u547D\u4EE4\u8FC7\u957F");
      need(p.id).run(line);
      return {};
    });
    ctx.rpc("interrupt", (p) => {
      need(p.id).interrupt();
      return {};
    });
    ctx.rpc("close", (p) => {
      const s = sessions.get(str(p.id));
      s?.close();
      sessions.delete(str(p.id));
      return {};
    });
  }
});
export {
  index_default as default
};
