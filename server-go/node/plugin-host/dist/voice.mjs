// ../../../plugins/voice/src/index.ts
import { join } from "node:path";

// ../../../packages/protocol/src/binary.ts
var encoder = new TextEncoder();
var decoder = new TextDecoder();

// ../../../packages/protocol/src/errors.ts
var AppError = class extends Error {
  code;
  params;
  constructor(code, params = {}) {
    const detail = Object.entries(params).map(([k, v]) => `${k}=${String(v)}`).join(", ");
    super(detail ? `${code} (${detail})` : code);
    this.name = "AppError";
    this.code = code;
    this.params = params;
  }
};

// ../../../packages/protocol/src/goal.ts
var GOAL_DIRECTIVE_TAG = "goal_directive";
var GOAL_STATUS_TAG = "goal_status";
var DIRECTIVE_RE = new RegExp(`^\\s*<${GOAL_DIRECTIVE_TAG} turn="(\\d+)" max_turns="(\\d+)">\\n[\\s\\S]*?\\nObjective:\\n([\\s\\S]*?)\\n\\nRules:[\\s\\S]*?</${GOAL_DIRECTIVE_TAG}>\\s*$`);
var STATUS_RE = new RegExp(`<${GOAL_STATUS_TAG}>\\s*(complete|blocked)\\s*(?::\\s*([\\s\\S]*?))?\\s*</${GOAL_STATUS_TAG}>`, "gi");

// ../../../packages/protocol/src/index.ts
var BUILTIN_AGENTS = [
  { id: "claude-code", name: "Claude Code", kind: "claude-code", npm: { pkg: "@anthropic-ai/claude-code", bin: "claude" } },
  { id: "codex", name: "Codex", kind: "codex", npm: { pkg: "@openai/codex", bin: "codex" } },
  { id: "opencode", name: "OpenCode", kind: "opencode", npm: { pkg: "opencode-ai", bin: "opencode" } },
  { id: "qwen", name: "Qwen Code", kind: "acp", command: "qwen", args: ["--acp"], npm: { pkg: "@qwen-code/qwen-code", bin: "qwen" }, homepage: "https://github.com/QwenLM/qwen-code" }
];
var ACP_PRESETS = [
  ...BUILTIN_AGENTS.filter((a) => a.kind === "acp"),
  { id: "goose", name: "Goose", kind: "acp", command: "goose", args: ["acp"], homepage: "https://block.github.io/goose" },
  { id: "kimi", name: "Kimi CLI", kind: "acp", command: "kimi", args: ["--acp"], homepage: "https://github.com/MoonshotAI/kimi-cli" },
  { id: "auggie", name: "Auggie (Augment)", kind: "acp", command: "auggie", args: ["--acp"], npm: { pkg: "@augmentcode/auggie", bin: "auggie" } },
  { id: "claude-code-acp", name: "Claude Code (ACP adapter)", kind: "acp", command: "claude-code-acp", args: [], npm: { pkg: "@zed-industries/claude-code-acp", bin: "claude-code-acp" } },
  { id: "codex-acp", name: "Codex (ACP adapter)", kind: "acp", command: "codex-acp", args: [], npm: { pkg: "@zed-industries/codex-acp", bin: "codex-acp" } }
];

// ../../../packages/gateway/src/index.ts
function authHeaders(g) {
  const h = { ...g.headers ?? {} };
  const key = g.apiKey ?? "";
  switch (g.auth.type) {
    case "none":
      break;
    case "header":
      h[g.auth.header] = `${g.auth.prefix ?? ""}${key}`;
      break;
    case "bearer":
      h.Authorization = `Bearer ${key}`;
      break;
    case "native":
      if (g.format === "google-generative-ai") h["x-goog-api-key"] = key;
      else h.Authorization = `Bearer ${key}`;
      break;
  }
  return h;
}
var AUDIO_EXTENSIONS = { "audio/webm": "webm", "audio/ogg": "ogg", "audio/mp4": "m4a", "audio/mpeg": "mp3", "audio/wav": "wav", "audio/x-wav": "wav", "audio/flac": "flac" };
function baseMime(mime) {
  return (mime.split(";")[0] ?? "").trim().toLowerCase();
}
async function transcribe(g, model, audio, mimeType, options = {}) {
  const doFetch = options.fetchImpl ?? fetch;
  const base = g.baseUrl.replace(/\/+$/, "");
  const mime = baseMime(mimeType) || "audio/webm";
  if (g.format === "anthropic-messages") {
    throw new AppError("transcribeUnsupported");
  }
  if (g.format === "google-generative-ai") {
    const modelPath = model.startsWith("models/") ? model : `models/${model}`;
    const hint = options.language ? ` The language is ${options.language}.` : "";
    const res2 = await doFetch(`${base}/${modelPath}:generateContent`, {
      method: "POST",
      headers: { "content-type": "application/json", ...authHeaders(g) },
      body: JSON.stringify({
        contents: [{ parts: [{ text: `Transcribe this audio verbatim.${hint} Reply with the transcript only, no commentary.` }, { inlineData: { mimeType: mime, data: Buffer.from(audio).toString("base64") } }] }]
      }),
      signal: options.signal
    });
    if (!res2.ok) throw new AppError("transcribeFailed", { status: res2.status, detail: await safeText(res2) });
    const json2 = await res2.json();
    return (json2.candidates?.[0]?.content?.parts ?? []).map((p) => p.text ?? "").join("").trim();
  }
  const form = new FormData();
  form.set("model", model);
  form.set("response_format", "json");
  if (options.language) form.set("language", options.language);
  form.set("file", new Blob([Buffer.from(audio)], { type: mime }), `speech.${AUDIO_EXTENSIONS[mime] ?? "webm"}`);
  const { "content-type": _ignored, ...headers } = authHeaders(g);
  const res = await doFetch(`${base}/audio/transcriptions`, { method: "POST", headers, body: form, signal: options.signal });
  if (!res.ok) throw new AppError("transcribeFailed", { status: res.status, detail: await safeText(res) });
  const json = await res.json();
  return (json.text ?? "").trim();
}
async function safeText(res) {
  try {
    return (await res.text()).slice(0, 300);
  } catch {
    return "";
  }
}

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

// ../../../plugins/voice/src/index.ts
var MAX_AUDIO_BYTES = 8 * 1024 * 1024;
var MIN_AUDIO_BYTES = 800;
var EMPTY = { gatewayId: "", model: "", language: "" };
var AUDIO_TYPES = /^audio\/(webm|ogg|mp4|mpeg|wav|x-wav|flac)$/;
function decodeAudio(base64) {
  if (typeof base64 !== "string" || !base64) throw new Error("\u6CA1\u6709\u6536\u5230\u5F55\u97F3");
  if (base64.length > Math.ceil(MAX_AUDIO_BYTES * 4 / 3) + 8) throw new Error("\u5F55\u97F3\u592A\u957F\uFF0C\u8BF7\u5206\u6BB5\u8BF4\u8BDD\uFF08\u5355\u6B21\u4E0A\u9650\u7EA6 2 \u5206\u949F\uFF09");
  const bytes = Buffer.from(base64, "base64");
  if (bytes.length < MIN_AUDIO_BYTES) throw new Error("\u5F55\u97F3\u592A\u77ED\uFF0C\u6CA1\u6709\u8BC6\u522B\u5230\u5185\u5BB9");
  return new Uint8Array(bytes.buffer, bytes.byteOffset, bytes.length);
}
function checkMime(mime) {
  const base = typeof mime === "string" ? (mime.split(";")[0] ?? "").trim().toLowerCase() : "";
  if (!AUDIO_TYPES.test(base)) throw new Error(`\u4E0D\u652F\u6301\u7684\u97F3\u9891\u683C\u5F0F\uFF1A${String(mime)}`);
  return typeof mime === "string" ? mime : base;
}
function pickGateway(gateways, id) {
  const g = gateways.find((x) => x.id === id);
  if (!g) throw new Error("\u8BED\u97F3\u8BC6\u522B\u4F7F\u7528\u7684\u7F51\u5173\u4E0D\u5B58\u5728\uFF0C\u8BF7\u5728 \u8BBE\u7F6E \u2192 \u8BED\u97F3 \u4E2D\u91CD\u65B0\u9009\u62E9");
  if (g.enabled === false) throw new Error(`\u7F51\u5173\u300C${g.name}\u300D\u5DF2\u505C\u7528`);
  return g;
}
var index_default = definePlugin({
  name: "voice",
  version: "0.1.0",
  optional: true,
  setup(ctx) {
    const store = createJsonStore(join(ctx.dataDir, "voice.json"), () => ({ ...EMPTY }));
    const read = async () => ({ ...EMPTY, ...await store.read() });
    const state = async () => {
      const s = await read();
      return { ...s, configured: Boolean(s.gatewayId && s.model && ctx.gateways().some((g) => g.id === s.gatewayId && g.enabled !== false)) };
    };
    ctx.rpc("state", state);
    ctx.rpc("configure", async (p) => {
      const gatewayId = typeof p.gatewayId === "string" ? p.gatewayId : "";
      const model = typeof p.model === "string" ? p.model.trim() : "";
      const language = typeof p.language === "string" ? p.language.trim().toLowerCase() : "";
      if (gatewayId) pickGateway(ctx.gateways(), gatewayId);
      if (language && !/^[a-z]{2,3}(-[a-z0-9]{2,8})?$/.test(language)) throw new Error("\u8BED\u8A00\u5E94\u4E3A zh\u3001en \u8FD9\u6837\u7684\u4EE3\u7801\uFF0C\u6216\u7559\u7A7A\u81EA\u52A8\u8BC6\u522B");
      await store.update(() => ({ gatewayId, model, language }));
      return state();
    });
    ctx.rpc("transcribe", async (p) => {
      const s = await read();
      if (!s.gatewayId || !s.model) throw new Error("\u5C1A\u672A\u914D\u7F6E\u8BED\u97F3\u8BC6\u522B\uFF0C\u8BF7\u5148\u5230 \u8BBE\u7F6E \u2192 \u8BED\u97F3 \u9009\u62E9\u7F51\u5173\u548C\u6A21\u578B");
      const gateway = pickGateway(ctx.gateways(), s.gatewayId);
      const audio = decodeAudio(p.audio);
      const mime = checkMime(p.mimeType);
      const text = await transcribe(gateway, s.model, audio, mime, { language: s.language || void 0, signal: AbortSignal.timeout(6e4) });
      if (!text) throw new Error("\u6CA1\u6709\u8BC6\u522B\u5230\u8BED\u97F3\u5185\u5BB9");
      return { text };
    });
  }
});
export {
  MAX_AUDIO_BYTES,
  checkMime,
  decodeAudio,
  index_default as default,
  pickGateway
};
