// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../packages/subagents/src/agents.ts
import { randomUUID } from "node:crypto";
import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

// ../../../packages/subagents/src/bundled.ts
var BUNDLED_AGENTS = {
  scout: `---
name: scout
description: Fast read-only codebase recon; returns compressed context another agent can act on
tools: read, grep, find, ls, bash
thinking: low
systemPromptMode: replace
inheritProjectContext: true
---

You are scout, a read-only reconnaissance subagent.

Find what the brief asks for and report it compactly: exact file paths with line numbers, the relevant symbols, how the pieces connect, and anything surprising. Do not edit files. Prefer search over reading whole files. End with a short list of the facts the parent needs to proceed.
`,
  researcher: `---
name: researcher
description: Investigates a question across the code and the web and returns a sourced answer
tools: read, grep, find, ls, bash
thinking: medium
systemPromptMode: replace
inheritProjectContext: true
---

You are researcher, a subagent that answers one question thoroughly.

Gather evidence from the repository (and any documentation on disk) before concluding. Separate what you verified from what you infer. Cite file paths and line numbers. Do not edit files. Finish with the answer first, then the evidence.
`,
  reviewer: `---
name: reviewer
description: Reviews a diff or a set of files for bugs, regressions and missing tests
tools: read, grep, find, ls, bash
thinking: high
systemPromptMode: replace
inheritProjectContext: true
---

You are reviewer, a read-only code review subagent.

Look for correctness bugs, broken edge cases, regressions against existing behavior, security problems and missing tests. Run read-only checks (tests, type checks) when they help. Report findings ordered by severity, each with path:line, why it is wrong and a concrete fix. If nothing is wrong, say so plainly. Do not edit files.
`,
  oracle: `---
name: oracle
description: Second opinion on a design or a hard decision; weighs options and recommends one
tools: read, grep, find, ls, bash
thinking: high
systemPromptMode: replace
inheritProjectContext: true
---

You are oracle, an advisory subagent.

Read the relevant code, then weigh the options the brief describes (and any it missed). Recommend one, explain the trade-offs that decide it, and name the risks and the cheapest way to check them. Be direct. Do not edit files.
`,
  worker: `---
name: worker
description: Implements a bounded change end to end: edit, run checks, report
tools: read, grep, find, ls, bash, edit, write
thinking: high
systemPromptMode: replace
inheritProjectContext: true
---

You are worker, the implementation subagent and the only writer for the files in your brief.

Read the named files first, then make narrow, coherent edits that follow the surrounding style. Run the checks the brief names (or the nearest tests and type check) and fix what you broke. Finish with: what changed (paths), what you ran and its result, and anything you could not verify.
`,
  "claude-code": `---
name: claude-code
description: Claude Code CLI, read-only analysis. Needs the local claude CLI signed in
runner: claude-code
inheritProjectContext: true
---

Analyze the task below. Do not edit files. Return a concise final answer with evidence.
`,
  "claude-code-writer": `---
name: claude-code-writer
description: Claude Code CLI that may edit files in the workspace. Needs the local claude CLI signed in
runner: claude-code
access: write
inheritProjectContext: true
---

Carry out the task below with focused edits, run the relevant checks, and finish with a short summary of what changed.
`,
  codex: `---
name: codex
description: Codex CLI, read-only analysis. Needs the local codex CLI signed in
runner: codex
inheritProjectContext: true
---

Analyze the task below. Do not edit files. Return a concise final answer with evidence.
`,
  "codex-writer": `---
name: codex-writer
description: Codex CLI that may edit files in the workspace. Needs the local codex CLI signed in
runner: codex
access: write
inheritProjectContext: true
---

Carry out the task below with focused edits, run the relevant checks, and finish with a short summary of what changed.
`
};

// ../../../packages/subagents/src/agents.ts
var RUNNERS = ["pi", "claude-code", "codex"];
var THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh"];
var KEY_LINE = /^([A-Za-z][\w-]*):[ \t]*(.*)$/;
function parseAgentFile(text) {
  const normalized = text.replace(/\r\n/g, "\n");
  if (!normalized.startsWith("---")) return { entries: [], body: normalized.trim() };
  const end = normalized.indexOf("\n---", 3);
  if (end === -1) return { entries: [], body: normalized.trim() };
  const entries = [];
  for (const line of normalized.slice(4, end).split("\n")) {
    const m = KEY_LINE.exec(line);
    if (m) entries.push({ key: m[1], lines: [line] });
    else if (entries.length && line.trim()) entries[entries.length - 1]?.lines.push(line);
  }
  return { entries, body: normalized.slice(end + 4).trim() };
}
var unquote = (s) => {
  const t = s.trim();
  return t.length >= 2 && (t.startsWith('"') && t.endsWith('"') || t.startsWith("'") && t.endsWith("'")) ? t.slice(1, -1) : t;
};
function valueOf(entries, key) {
  const e = entries.find((x) => x.key === key);
  if (!e) return void 0;
  const first = (KEY_LINE.exec(e.lines[0] ?? "")?.[2] ?? "").trim();
  if (first && first !== "|" && first !== ">") return unquote(first);
  return e.lines.slice(1).map((l) => l.trim().replace(/^-\s+/, "")).filter(Boolean).join(", ");
}
var list = (v) => (v ?? "").split(",").map((x) => x.trim()).filter(Boolean);
var flag = (v) => v === "true" ? true : v === "false" ? false : void 0;
function defFromFile(name, parsed) {
  const v = (k) => valueOf(parsed.entries, k);
  const ctx = v("defaultContext");
  return {
    name,
    description: v("description") ?? "",
    mode: v("systemPromptMode") === "append" ? "append" : "replace",
    prompt: parsed.body,
    model: v("model") ?? "",
    thinking: v("thinking") ?? "",
    tools: list(v("tools")),
    context: ctx === "fresh" || ctx === "fork" ? ctx : "",
    inheritProject: flag(v("inheritProjectContext")) ?? false,
    inheritGlobal: flag(v("inheritGlobalContext")) ?? false,
    inheritSkills: flag(v("inheritSkills")) ?? false,
    timeoutMin: Math.round((Number(v("timeoutMs")) || 0) / 6e4),
    advertise: flag(v("advertise")) ?? false,
    runner: parseRunner(v("runner")),
    writable: v("access") === "write"
  };
}
function parseRunner(v) {
  return RUNNERS.includes(v) ? v : "pi";
}
var MANAGED = ["name", "description", "tools", "model", "thinking", "systemPromptMode", "inheritProjectContext", "inheritGlobalContext", "inheritSkills", "defaultContext", "timeoutMs", "advertise", "runner", "access"];
var oneLine = (s) => s.replace(/\s+/g, " ").trim();
function serializeAgent(def, prior) {
  const set = /* @__PURE__ */ new Map([
    ["name", def.name],
    ["description", oneLine(def.description) || void 0],
    ["tools", def.tools.length ? def.tools.join(", ") : void 0],
    ["model", def.model || void 0],
    ["thinking", def.thinking || void 0],
    ["systemPromptMode", def.mode],
    ["inheritProjectContext", String(def.inheritProject)],
    ["inheritGlobalContext", def.inheritGlobal ? "true" : void 0],
    ["inheritSkills", String(def.inheritSkills)],
    ["defaultContext", def.context || void 0],
    ["timeoutMs", def.timeoutMin > 0 ? String(Math.round(def.timeoutMin * 6e4)) : void 0],
    ["advertise", def.advertise ? "true" : void 0],
    ["runner", def.runner === "pi" ? void 0 : def.runner],
    ["access", def.runner !== "pi" && def.writable ? "write" : void 0]
  ]);
  const lines = [];
  const written = /* @__PURE__ */ new Set();
  for (const e of prior?.entries ?? []) {
    if (!MANAGED.includes(e.key)) {
      lines.push(...e.lines);
      continue;
    }
    const next = set.get(e.key);
    if (next !== void 0) lines.push(`${e.key}: ${next}`);
    written.add(e.key);
  }
  const fresh = [];
  for (const key of MANAGED) {
    const next = set.get(key);
    if (written.has(key) || next === void 0) continue;
    fresh.push(`${key}: ${next}`);
  }
  return `---
${[...fresh, ...lines].join("\n")}
---

${def.prompt.trim()}
`;
}
var NAME_RE = /^[a-z0-9][a-z0-9_.-]{0,63}$/;
var MAX_PROMPT = 2e5;
function sanitizeDef(input, name) {
  if (!input || typeof input !== "object") throw new Error("\u5B50\u4EE3\u7406\u6570\u636E\u4E0D\u5408\u6CD5");
  const r = input;
  const s = (v) => typeof v === "string" ? v : "";
  const thinking = s(r.thinking).trim();
  if (thinking && !THINKING_LEVELS.includes(thinking)) throw new Error("\u601D\u8003\u5F3A\u5EA6\u4E0D\u5408\u6CD5");
  const context = s(r.context);
  const tools = Array.isArray(r.tools) ? [...new Set(r.tools.filter((t) => typeof t === "string").map((t) => t.trim()).filter((t) => t && t.length < 160 && !/[,\n]/.test(t)))].slice(0, 200) : [];
  const timeout = Number(r.timeoutMin);
  return {
    name,
    description: oneLine(s(r.description)).slice(0, 500),
    mode: r.mode === "append" ? "append" : "replace",
    prompt: s(r.prompt).slice(0, MAX_PROMPT),
    model: s(r.model).trim().slice(0, 200),
    thinking,
    tools,
    context: context === "fresh" || context === "fork" ? context : "",
    inheritProject: r.inheritProject === true,
    inheritGlobal: r.inheritGlobal === true,
    inheritSkills: r.inheritSkills === true,
    timeoutMin: Number.isFinite(timeout) && timeout > 0 ? Math.min(Math.round(timeout), 24 * 60) : 0,
    advertise: r.advertise === true,
    runner: parseRunner(r.runner),
    writable: r.writable === true
  };
}
var isObj = (v) => !!v && typeof v === "object" && !Array.isArray(v);
async function readJson(file) {
  try {
    const parsed = JSON.parse(await readFile(file, "utf8"));
    return isObj(parsed) ? parsed : {};
  } catch (err) {
    if (err.code === "ENOENT") return {};
    throw new Error(`\u65E0\u6CD5\u89E3\u6790 ${file}\uFF0C\u4E3A\u907F\u514D\u8986\u76D6\u5DF2\u6709\u8BBE\u7F6E\uFF0C\u5DF2\u505C\u6B62\u5199\u5165`);
  }
}
async function writeJson(file, value) {
  await mkdir(dirname(file), { recursive: true });
  const tmp = `${file}.${randomUUID().slice(0, 8)}.tmp`;
  await writeFile(tmp, `${JSON.stringify(value, null, 2)}
`);
  await rename(tmp, file);
}
var OVERRIDE_KEYS = ["description", "model", "thinking", "systemPromptMode", "inheritProjectContext", "inheritGlobalContext", "inheritSkills", "defaultContext", "tools", "systemPrompt", "advertise", "runner", "access"];
function diffOverride(base, draft) {
  const out = {};
  if (draft.description !== base.description) out.description = draft.description;
  if (draft.model !== base.model) out.model = draft.model || "inherit";
  if (draft.thinking !== base.thinking) out.thinking = draft.thinking || "medium";
  if (draft.mode !== base.mode) out.systemPromptMode = draft.mode;
  if (draft.inheritProject !== base.inheritProject) out.inheritProjectContext = draft.inheritProject;
  if (draft.inheritGlobal !== base.inheritGlobal) out.inheritGlobalContext = draft.inheritGlobal;
  if (draft.inheritSkills !== base.inheritSkills) out.inheritSkills = draft.inheritSkills;
  if (draft.context !== base.context) out.defaultContext = draft.context || false;
  if (draft.tools.join(",") !== base.tools.join(",")) out.tools = draft.tools.length ? draft.tools : "inherit";
  if (draft.prompt.trim() !== base.prompt.trim()) out.systemPrompt = draft.prompt;
  if (draft.advertise !== base.advertise) out.advertise = draft.advertise;
  if (draft.runner !== base.runner) out.runner = draft.runner;
  if (draft.writable !== base.writable) out.access = draft.writable ? "write" : "read";
  return out;
}
function applyOverride(base, o) {
  if (!o) return base;
  const next = { ...base, tools: [...base.tools] };
  if (typeof o.description === "string") next.description = o.description;
  if (typeof o.model === "string") next.model = o.model === "inherit" ? "" : o.model;
  if (typeof o.thinking === "string") next.thinking = o.thinking;
  if (o.systemPromptMode === "append" || o.systemPromptMode === "replace") next.mode = o.systemPromptMode;
  if (typeof o.inheritProjectContext === "boolean") next.inheritProject = o.inheritProjectContext;
  if (typeof o.inheritGlobalContext === "boolean") next.inheritGlobal = o.inheritGlobalContext;
  if (typeof o.inheritSkills === "boolean") next.inheritSkills = o.inheritSkills;
  if (o.defaultContext === false) next.context = "";
  else if (o.defaultContext === "fresh" || o.defaultContext === "fork") next.context = o.defaultContext;
  if (Array.isArray(o.tools)) next.tools = o.tools.filter((t) => typeof t === "string");
  else if (o.tools === "inherit" || o.tools === false) next.tools = [];
  if (typeof o.systemPrompt === "string") next.prompt = o.systemPrompt;
  if (typeof o.advertise === "boolean") next.advertise = o.advertise;
  if (typeof o.runner === "string") next.runner = parseRunner(o.runner);
  if (o.access === "write" || o.access === "read") next.writable = o.access === "write";
  return next;
}
var AgentStore = class {
  agentDir;
  constructor(agentDir) {
    this.agentDir = agentDir;
  }
  get userDir() {
    return join(this.agentDir, "agents");
  }
  get settingsFile() {
    return join(this.agentDir, "settings.json");
  }
  /** The agents that ship with the app. */
  bundled() {
    return Object.entries(BUNDLED_AGENTS).map(([name, text]) => ({ name, def: defFromFile(name, parseAgentFile(text)) }));
  }
  async readDir(dir, depth = 0) {
    let items;
    try {
      items = await readdir(dir, { withFileTypes: true });
    } catch {
      return [];
    }
    const out = [];
    for (const it of items) {
      const file = join(dir, it.name);
      if (it.isDirectory()) {
        if (depth < 3 && !it.name.startsWith(".")) out.push(...await this.readDir(file, depth + 1));
        continue;
      }
      if (!it.name.endsWith(".md") || it.name.endsWith(".chain.md")) continue;
      try {
        const parsed = parseAgentFile(await readFile(file, "utf8"));
        const name = valueOf(parsed.entries, "name") || it.name.replace(/\.md$/, "");
        out.push({ name, file, parsed });
      } catch {
      }
    }
    return out;
  }
  async overrides() {
    const settings = await readJson(this.settingsFile);
    const sub = isObj(settings.subagents) ? settings.subagents : {};
    const raw = isObj(sub.agentOverrides) ? sub.agentOverrides : {};
    const all = {};
    for (const [k, v] of Object.entries(raw)) if (isObj(v)) all[k] = v;
    return { settings, all };
  }
  async writeOverride(name, next) {
    const { settings, all } = await this.overrides();
    if (next && Object.keys(next).length) all[name] = next;
    else delete all[name];
    const sub = isObj(settings.subagents) ? { ...settings.subagents } : {};
    if (Object.keys(all).length) sub.agentOverrides = all;
    else delete sub.agentOverrides;
    const out = { ...settings };
    if (Object.keys(sub).length) out.subagents = sub;
    else delete out.subagents;
    await writeJson(this.settingsFile, out);
  }
  /** Every agent the user can see: bundled ones, custom files, with edits and disabled flags applied. */
  async list() {
    const bundled = new Map(this.bundled().map((b) => [b.name, b.def]));
    const custom = /* @__PURE__ */ new Map();
    for (const e of await this.readDir(this.userDir)) custom.set(e.name, { def: defFromFile(e.name, e.parsed), file: e.file });
    const { all } = await this.overrides();
    const views = [];
    for (const name of /* @__PURE__ */ new Set([...bundled.keys(), ...custom.keys()])) {
      const o = all[name];
      const mine = custom.get(name);
      const base = mine?.def ?? bundled.get(name);
      const def = applyOverride(base, o);
      const diffs = !mine && o ? Object.keys(diffOverride(base, def)).length > 0 : false;
      views.push({
        ...def,
        source: mine ? "custom" : "builtin",
        shadows: Boolean(mine && bundled.has(name)),
        modified: diffs,
        disabled: o?.disabled === true,
        ...mine ? { file: mine.file } : {}
      });
    }
    return views.sort((a, b) => a.source === b.source ? a.name.localeCompare(b.name) : a.source === "custom" ? -1 : 1);
  }
  async get(name) {
    const hit = (await this.list()).find((a) => a.name === name);
    if (!hit) throw new Error("\u5B50\u4EE3\u7406\u4E0D\u5B58\u5728");
    return hit;
  }
  async create(name, draft) {
    if (!NAME_RE.test(name)) throw new Error("\u540D\u79F0\u53EA\u80FD\u542B\u5C0F\u5199\u5B57\u6BCD\u3001\u6570\u5B57\u3001- _ .\uFF0C\u4E14\u4EE5\u5B57\u6BCD\u6216\u6570\u5B57\u5F00\u5934");
    if ((await this.list()).some((a) => a.name === name)) throw new Error("\u5DF2\u5B58\u5728\u540C\u540D\u5B50\u4EE3\u7406");
    const def = sanitizeDef(draft, name);
    await mkdir(this.userDir, { recursive: true });
    await writeFile(join(this.userDir, `${name}.md`), serializeAgent(def));
  }
  async save(name, draft) {
    const current = await this.get(name);
    const def = sanitizeDef(draft, name);
    if (current.source === "custom" && current.file) {
      const prior = parseAgentFile(await readFile(current.file, "utf8"));
      await writeFile(current.file, serializeAgent(def, prior));
      const { all: all2 } = await this.overrides();
      const o = all2[name];
      if (o && OVERRIDE_KEYS.some((k) => k in o)) {
        const keep = Object.fromEntries(Object.entries(o).filter(([k]) => !OVERRIDE_KEYS.includes(k)));
        await this.writeOverride(name, keep);
      }
      return;
    }
    const bundledBase = this.bundledBase(name);
    const { all } = await this.overrides();
    const kept = Object.fromEntries(Object.entries(all[name] ?? {}).filter(([k]) => !OVERRIDE_KEYS.includes(k)));
    await this.writeOverride(name, { ...kept, ...diffOverride(bundledBase, def) });
  }
  bundledBase(name) {
    const hit = this.bundled().find((b) => b.name === name);
    if (!hit) throw new Error("\u627E\u4E0D\u5230\u5185\u7F6E\u5B50\u4EE3\u7406\u7684\u539F\u59CB\u5B9A\u4E49");
    return hit.def;
  }
  async setDisabled(name, disabled) {
    await this.get(name);
    const { all } = await this.overrides();
    const next = { ...all[name] ?? {} };
    if (disabled) next.disabled = true;
    else delete next.disabled;
    await this.writeOverride(name, next);
  }
  /** Bundled: drop the edits. Custom that shadows a bundled agent: delete the file so the original returns. */
  async reset(name) {
    const current = await this.get(name);
    if (current.source === "custom" && current.file) {
      if (!current.shadows) throw new Error("\u81EA\u5B9A\u4E49\u5B50\u4EE3\u7406\u6CA1\u6709\u53EF\u6062\u590D\u7684\u5185\u7F6E\u7248\u672C\uFF0C\u8BF7\u4F7F\u7528\u5220\u9664");
      await rm(current.file, { force: true });
    }
    const { all } = await this.overrides();
    const keep = Object.fromEntries(Object.entries(all[name] ?? {}).filter(([k]) => k === "disabled"));
    await this.writeOverride(name, keep);
  }
  async remove(name) {
    const current = await this.get(name);
    if (current.source !== "custom" || !current.file) throw new Error("\u5185\u7F6E\u5B50\u4EE3\u7406\u4E0D\u80FD\u5220\u9664\uFF0C\u53EA\u80FD\u505C\u7528");
    await rm(current.file, { force: true });
    await this.writeOverride(name, void 0);
  }
};

// ../../../packages/subagents/src/cli-runner.ts
import { existsSync, readFileSync } from "node:fs";
import { delimiter, dirname as dirname2, extname, join as join2, resolve } from "node:path";
function resolveExecutable(name, env = process.env, platform = process.platform) {
  const win = platform === "win32";
  const exts = win ? ["", ...(env.PATHEXT ?? ".COM;.EXE;.BAT;.CMD").split(";").map((e) => e.toLowerCase())] : [""];
  const dirs = (env.PATH ?? env.Path ?? "").split(delimiter).filter(Boolean);
  for (const dir of dirs) {
    for (const ext of exts) {
      const file = join2(dir, name + ext);
      if (!existsSync(file)) continue;
      const e = extname(file).toLowerCase();
      if (win && (e === ".cmd" || e === ".bat")) {
        const target = shimTarget(file);
        if (target) return target;
        continue;
      }
      if (win && e !== ".exe" && e !== ".com") continue;
      return { command: file, prefix: [] };
    }
  }
  return void 0;
}
function shimTarget(shim) {
  let text;
  try {
    text = readFileSync(shim, "utf8");
  } catch {
    return void 0;
  }
  const hits = [...text.matchAll(/"%dp0%[\\/]+([^"]+)"/gi)].map((m) => m[1]);
  for (const rel of hits.reverse()) {
    const file = resolve(dirname2(shim), rel.replace(/\\/g, "/"));
    if (!existsSync(file)) continue;
    return /\.(c|m)?js$/i.test(file) ? { command: process.execPath, prefix: [file] } : { command: file, prefix: [] };
  }
  return void 0;
}
var CLI_BINARY = { "claude-code": "claude", codex: "codex" };
function cliAvailable(kind) {
  return resolveExecutable(CLI_BINARY[kind]) !== void 0;
}

// ../../../packages/subagents/src/pi-runner.ts
var PI_CHILD_TOOLS = ["read", "grep", "find", "ls", "bash", "edit", "write"];

// node_modules/typebox/build/system/hashing/hash.mjs
var ByteMarker;
(function(ByteMarker2) {
  ByteMarker2[ByteMarker2["Array"] = 0] = "Array";
  ByteMarker2[ByteMarker2["BigInt"] = 1] = "BigInt";
  ByteMarker2[ByteMarker2["Boolean"] = 2] = "Boolean";
  ByteMarker2[ByteMarker2["Date"] = 3] = "Date";
  ByteMarker2[ByteMarker2["Constructor"] = 4] = "Constructor";
  ByteMarker2[ByteMarker2["Function"] = 5] = "Function";
  ByteMarker2[ByteMarker2["Null"] = 6] = "Null";
  ByteMarker2[ByteMarker2["Number"] = 7] = "Number";
  ByteMarker2[ByteMarker2["Object"] = 8] = "Object";
  ByteMarker2[ByteMarker2["RegExp"] = 9] = "RegExp";
  ByteMarker2[ByteMarker2["String"] = 10] = "String";
  ByteMarker2[ByteMarker2["Symbol"] = 11] = "Symbol";
  ByteMarker2[ByteMarker2["TypeArray"] = 12] = "TypeArray";
  ByteMarker2[ByteMarker2["Undefined"] = 13] = "Undefined";
})(ByteMarker || (ByteMarker = {}));
var Accumulator = BigInt("14695981039346656037");
var [Prime, Size] = [BigInt("1099511628211"), BigInt(
  "18446744073709551616"
  /* 2 ^ 64 */
)];
var Bytes = Array.from({ length: 256 }).map((_, i) => BigInt(i));
var F64 = new Float64Array(1);
var F64In = new DataView(F64.buffer);
var F64Out = new Uint8Array(F64.buffer);
var encoder = new TextEncoder();

// node_modules/typebox/build/type/types/integer.mjs
var IntegerPattern = "-?(?:0|[1-9][0-9]*)";

// node_modules/typebox/build/type/types/number.mjs
var NumberPattern = "-?(?:0|[1-9][0-9]*)(?:\\.[0-9]+)?";

// node_modules/typebox/build/type/types/string.mjs
var StringPattern = ".*";

// node_modules/typebox/build/type/types/record.mjs
var IntegerKey = `^${IntegerPattern}$`;
var NumberKey = `^${NumberPattern}$`;
var StringKey = `^${StringPattern}$`;

// node_modules/typebox/build/type/script/token/internal/char.mjs
function Range(start, end) {
  return Array.from({ length: end - start + 1 }, (_, i) => String.fromCharCode(start + i));
}
var Alpha = [
  ...Range(97, 122),
  // Lowercase
  ...Range(65, 90)
  // Uppercase
];
var Zero = "0";
var NonZero = Range(49, 57);
var Digit = [Zero, ...NonZero];
var UnderScore = "_";
var DollarSign = "$";

// node_modules/typebox/build/type/script/token/unsigned_integer.mjs
var AllowedDigits = [...Digit, UnderScore];

// node_modules/typebox/build/type/script/token/ident.mjs
var Initial = [...Alpha, UnderScore, DollarSign];
var Remaining = [...Initial, ...Digit];

// node_modules/typebox/build/type/script/token/unsigned_number.mjs
var AllowedDigits2 = [...Digit, UnderScore];

// node_modules/typebox/build/type/engine/helpers/keys.mjs
var integerKeyPattern = new RegExp("^(?:0|[1-9][0-9]*)$");

// node_modules/typebox/build/type/engine/indexed/from_object.mjs
var NumericKeyPattern = new RegExp(IntegerKey);

// ../../../packages/subagents/src/runtime.ts
var DEFAULT_TIMEOUT_MS = 30 * 6e4;

// ../../../plugins/subagents/src/index.ts
var str = (v) => typeof v === "string" ? v.trim() : "";
var index_default = definePlugin({
  name: "subagents",
  version: "0.1.0",
  optional: true,
  setup(ctx) {
    const store = new AgentStore(ctx.agentDir);
    const changed = async () => ctx.notifyChanged();
    ctx.rpc("list", async () => ({
      clis: { "claude-code": cliAvailable("claude-code"), codex: cliAvailable("codex") },
      agents: (await store.list()).map(({ prompt, ...rest }) => ({ ...rest, promptLength: prompt.length }))
    }));
    ctx.rpc("get", async (p) => store.get(str(p.name)));
    ctx.rpc("tools", async () => ({ known: PI_CHILD_TOOLS }));
    ctx.rpc("create", async (p) => {
      const name = str(p.name);
      await store.create(name, p.agent ?? { description: "", prompt: "" });
      await changed();
      return { name };
    });
    ctx.rpc("save", async (p) => {
      const name = str(p.name);
      await store.save(name, p.agent);
      await changed();
      return { name };
    });
    ctx.rpc("disable", async (p) => {
      await store.setDisabled(str(p.name), p.disabled === true);
      await changed();
      return null;
    });
    ctx.rpc("reset", async (p) => {
      await store.reset(str(p.name));
      await changed();
      return null;
    });
    ctx.rpc("delete", async (p) => {
      await store.remove(str(p.name));
      await changed();
      return null;
    });
  }
});
export {
  index_default as default
};
