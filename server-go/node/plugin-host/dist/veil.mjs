// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/veil/src/config.ts
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

// ../../../plugins/veil/src/engine/ip.ts
var DELIMS = /* @__PURE__ */ new Set([" ", "	", "\n", "\r", "/", ",", ";", "|", "(", ")", "[", "]", "=", "@", '"', "'", "<", ">", "{", "}", "\f", "\v"]);
var TRAILING_PUNCT = /* @__PURE__ */ new Set([".", ",", ":", ";", "!", "?", "\u3002", "\uFF0C", "\uFF1A"]);
function isDelim(ch) {
  return DELIMS.has(ch) || /\s/.test(ch);
}
function parseIpv4(token) {
  const parts = token.split(".");
  if (parts.length !== 4) return false;
  return parts.every((p) => p.length > 0 && p.length <= 3 && /^\d+$/.test(p) && Number(p) <= 255);
}
function parseIpv6(token) {
  if (!token.includes(":")) return false;
  const bare = token.replace(/^\[|\]$/g, "").split("%")[0] ?? "";
  try {
    new URL(`http://[${bare}]/`);
    return true;
  } catch {
    return false;
  }
}
function parseIp(token) {
  return parseIpv4(token) || parseIpv6(token);
}
function trimTrailingPunct(text, start, end) {
  let e = end;
  while (e > start) {
    const ch = String.fromCodePoint(text.codePointAt(e - 1) ?? 0);
    const cp = text.codePointAt(e - 1) ?? 0;
    const width = cp > 65535 ? 2 : 1;
    if (!TRAILING_PUNCT.has(ch) && ![...ch].some((c) => TRAILING_PUNCT.has(c))) break;
    e -= width;
  }
  return e;
}
function considerColonSplit(text, start, end, spans) {
  let subStart = start;
  for (const part of text.slice(start, end).split(":")) {
    const subEnd = subStart + part.length;
    if (parseIpv4(part)) spans.push([subStart, subEnd]);
    subStart = subEnd + 1;
  }
}
function considerToken(text, start, end, spans) {
  if (start >= end) return;
  const token = text.slice(start, end);
  if (parseIp(token)) {
    spans.push([start, end]);
    return;
  }
  const trimmed = trimTrailingPunct(text, start, end);
  if (trimmed !== end) {
    if (parseIp(text.slice(start, trimmed))) {
      spans.push([start, trimmed]);
      return;
    }
    considerColonSplit(text, start, trimmed, spans);
    return;
  }
  considerColonSplit(text, start, end, spans);
}
function findIpSpans(text) {
  const spans = [];
  let i = 0;
  while (i < text.length) {
    const ch = String.fromCodePoint(text.codePointAt(i) ?? 0);
    if (isDelim(ch)) {
      i += ch.length;
      continue;
    }
    const start = i;
    i += ch.length;
    while (i < text.length) {
      const c = String.fromCodePoint(text.codePointAt(i) ?? 0);
      if (isDelim(c)) break;
      i += c.length;
    }
    considerToken(text, start, i, spans);
  }
  return spans;
}

// ../../../plugins/veil/src/engine/rules.ts
var TYPE_PATTERN = /^[A-Z][A-Z0-9]{0,31}$/;
var RuleError = class extends Error {
};
var compiled = /* @__PURE__ */ new WeakMap();
function regexFor(rule2) {
  const cached = compiled.get(rule2);
  if (cached) return cached;
  let re;
  try {
    const { source, flags } = normalizeInlineFlags(rule2.pattern ?? "");
    re = new RegExp(source, flags.includes("g") ? flags : `${flags}g`);
  } catch (err) {
    throw new RuleError(`\u89C4\u5219 ${rule2.id} \u7684\u6B63\u5219\u65E0\u6548\uFF1A${err instanceof Error ? err.message : String(err)}`);
  }
  compiled.set(rule2, re);
  return re;
}
function normalizeInlineFlags(pattern) {
  const group = /^\(\?([a-z]+)\)/.exec(pattern);
  if (!group) return { source: pattern, flags: "" };
  const letters = group[1];
  let flags = "";
  if (letters.includes("i")) flags += "i";
  if (letters.includes("m")) flags += "m";
  if (letters.includes("s")) flags += "s";
  return { source: pattern.slice(group[0].length), flags };
}
function validateRule(rule2) {
  if (!rule2.id.trim()) throw new RuleError("\u89C4\u5219 id \u4E0D\u80FD\u4E3A\u7A7A");
  if (!TYPE_PATTERN.test(rule2.typePrefix)) throw new RuleError(`\u7C7B\u578B ${rule2.typePrefix} \u4E0D\u5408\u6CD5\uFF1A\u9700\u5927\u5199\u5B57\u6BCD\u5F00\u5934\uFF0C\u53EA\u542B\u5927\u5199\u5B57\u6BCD\u548C\u6570\u5B57\uFF0C\u6700\u957F 32 \u4F4D`);
  if (!Number.isFinite(rule2.priority)) throw new RuleError("\u4F18\u5148\u7EA7\u5FC5\u987B\u662F\u6570\u5B57");
  if (rule2.kind === "regex") {
    if (!rule2.pattern) throw new RuleError("\u6B63\u5219\u89C4\u5219\u9700\u8981\u586B\u5199\u6B63\u5219\u8868\u8FBE\u5F0F");
    try {
      new RegExp(rule2.pattern);
    } catch (err) {
      throw new RuleError(`\u6B63\u5219\u65E0\u6548\uFF1A${err instanceof Error ? err.message : String(err)}`);
    }
  }
  if (rule2.kind === "dictionary" && !(rule2.words ?? []).some((w) => w.length > 0)) throw new RuleError("\u8BCD\u8868\u89C4\u5219\u81F3\u5C11\u9700\u8981\u4E00\u4E2A\u8BCD");
}
function loadRules(rules) {
  for (const r of rules) {
    if (r.kind === "regex") regexFor(r);
  }
  return [...rules].sort((a, b) => b.priority - a.priority || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}
function gaps(o, len) {
  const out = [];
  let cursor = 0;
  for (const [start, end] of o.spans) {
    if (cursor < start) out.push([cursor, start]);
    cursor = Math.max(cursor, end);
  }
  if (cursor < len) out.push([cursor, len]);
  return out;
}
function overlaps(o, start, end) {
  return o.spans.some(([s, e]) => start < e && end > s);
}
function insert(o, start, end) {
  if (start >= end) return;
  o.spans.push([start, end]);
  o.spans.sort((a, b) => a[0] - b[0]);
  const merged = [];
  for (const [s, e] of o.spans) {
    const last = merged[merged.length - 1];
    if (last && s <= last[1]) last[1] = Math.max(last[1], e);
    else merged.push([s, e]);
  }
  o.spans = merged;
}
function regexSpans(rule2, text, gs, ge) {
  const re = regexFor(rule2);
  re.lastIndex = 0;
  const slice = text.slice(gs, ge);
  const spans = [];
  for (let m = re.exec(slice); m; m = re.exec(slice)) {
    if (m[0].length === 0) {
      re.lastIndex++;
      continue;
    }
    spans.push([gs + m.index, gs + m.index + m[0].length]);
  }
  return spans;
}
function dictionarySpans(words, text, gs, ge) {
  const slice = text.slice(gs, ge);
  const spans = [];
  for (const word of words) {
    if (!word) continue;
    let from = 0;
    for (; ; ) {
      const at = slice.indexOf(word, from);
      if (at === -1) break;
      spans.push([gs + at, gs + at + word.length]);
      from = at + word.length;
    }
  }
  spans.sort((a, b) => a[0] - b[0] || b[1] - a[1]);
  return spans;
}
var RuleSet = class {
  rules;
  allow;
  constructor(options) {
    this.rules = loadRules(options.rules);
    this.allow = new Set(options.allowlist);
  }
  /** Every match, ordered by position, non-overlapping. */
  findHits(text) {
    const occupied = { spans: [] };
    const hits = [];
    for (const rule2 of this.rules) {
      if (!rule2.enabled) continue;
      const taken = [];
      const candidates = [];
      for (const [gs, ge] of gaps(occupied, text.length)) {
        if (gs >= ge) continue;
        if (rule2.kind === "ip") candidates.push(...findIpSpans(text.slice(gs, ge)).map(([s, e]) => [gs + s, gs + e]));
        else if (rule2.kind === "dictionary") candidates.push(...dictionarySpans(rule2.words ?? [], text, gs, ge));
        else candidates.push(...regexSpans(rule2, text, gs, ge));
      }
      candidates.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
      for (const [start, end] of candidates) {
        if (start >= end || end > text.length) continue;
        if (overlaps(occupied, start, end) || taken.some(([s, e]) => start < e && end > s)) continue;
        const plaintext = text.slice(start, end);
        if (this.allow.has(plaintext)) continue;
        taken.push([start, end]);
        hits.push({ start, end, typePrefix: rule2.typePrefix, plaintext });
      }
      for (const [s, e] of taken) insert(occupied, s, e);
    }
    hits.sort((a, b) => a.start - b.start);
    return hits;
  }
};
var DEFAULT_ALLOWLIST = ["127.0.0.1", "localhost", "0.0.0.0", "::1"];
var rule = (id, typePrefix, pattern, priority, enabled = true) => ({
  id,
  name: id,
  typePrefix,
  enabled,
  kind: "regex",
  pattern,
  priority
});
function builtinRules(packs = { secrets: true, region: true }) {
  const rules = [];
  if (packs.secrets) {
    rules.push(
      rule("pem", "PEM", String.raw`-----BEGIN [A-Z0-9 ]+PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]+PRIVATE KEY-----`, 220),
      rule(
        "apikey",
        "APIKEY",
        String.raw`(?:api[_-]?key|access[_-]?key)\s*[:=]\s*\S{8,}|(?<![A-Za-z0-9])(?:sk-ant-|sk-|rk-|pk-live-|pk-test-|xai-|ghp_|gho_|github_pat_|AIza)[A-Za-z0-9_\-]{8,}|AKIA[0-9A-Z]{16}`,
        210
      ),
      rule(
        "token",
        "TOKEN",
        String.raw`(?:api[_-]?token|access[_-]?token|auth[_-]?token|refresh[_-]?token)\s*[:=]\s*\S{8,}|Bearer\s+[A-Za-z0-9\-._~+/]+=*|(?<![A-Za-z0-9])(?:xox[baprs]-|glpat-|npm_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.)`,
        205
      ),
      rule(
        "connstr",
        "CONNSTR",
        String.raw`(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|rediss|mssql|sqlserver|mariadb|amqp|amqps|jdbc:\w+)://\S+|(?:Server|Data Source|Initial Catalog)\s*=\s*[^;]+|(?:Pwd|Password|PWD)\s*=\s*[^;\s"]+`,
        180
      ),
      rule("password", "PASSWORD", String.raw`(?:password|passwd|passphrase|login[_-]?password|登录密码|口令)\s*[:=]\s*(?:['"][^'"]{4,}['"]|\S{4,})`, 170),
      rule("idcard", "IDCARD", String.raw`(?<!\d)\d{17}[\dXx](?!\d)`, 150),
      rule("phone", "PHONE", String.raw`(?<!\d)1[3-9]\d{9}(?!\d)`, 140),
      { id: "ip", name: "ip", typePrefix: "IP", enabled: true, kind: "ip", priority: 120 },
      rule("email", "EMAIL", String.raw`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`, 80, false)
    );
  }
  if (packs.region) {
    rules.push(
      {
        id: "timezone",
        name: "timezone",
        typePrefix: "TZ",
        enabled: true,
        kind: "dictionary",
        words: ["Asia/Shanghai", "Asia/Chongqing", "Asia/Urumqi", "Asia/Harbin", "Asia/Hong_Kong"],
        priority: 115
      },
      rule("locale", "LOCALE", String.raw`(?<![A-Za-z0-9])(?:zh[-_]CN|zh-Hans)(?![A-Za-z0-9])`, 110)
    );
  }
  return rules;
}

// ../../../plugins/veil/src/config.ts
var DEFAULT_ACCEPT_LANGUAGE = "en-US,en;q=0.9";
function defaultConfig() {
  return {
    redaction: true,
    relay: false,
    relayPort: 18787,
    upstream: "",
    routes: {},
    regionNeutral: true,
    acceptLanguage: DEFAULT_ACCEPT_LANGUAGE,
    proxy: { kind: "none", server: "" },
    rules: builtinRules(),
    allowlist: [...DEFAULT_ALLOWLIST],
    packs: { secrets: true, region: true },
    mappingTtlDays: 90
  };
}
var bool = (v, fallback) => typeof v === "boolean" ? v : fallback;
var str = (v, fallback) => typeof v === "string" ? v : fallback;
var num = (v, fallback) => typeof v === "number" && Number.isFinite(v) ? v : fallback;
function normalizeConfig(raw) {
  const base = defaultConfig();
  if (!raw || typeof raw !== "object") return base;
  const packs = {
    secrets: bool(raw.packs?.secrets, base.packs.secrets),
    region: bool(raw.packs?.region, base.packs.region)
  };
  const builtin = builtinRules(packs);
  const storedRules = Array.isArray(raw.rules) ? raw.rules.filter(isRule) : [];
  const rules = mergeRules(builtin, storedRules);
  const proxyRaw = raw.proxy ?? base.proxy;
  const kind = proxyRaw.kind === "http" || proxyRaw.kind === "socks5" || proxyRaw.kind === "none" ? proxyRaw.kind : "none";
  return {
    redaction: bool(raw.redaction, base.redaction),
    relay: bool(raw.relay, base.relay),
    relayPort: clampPort(num(raw.relayPort, base.relayPort), base.relayPort),
    upstream: str(raw.upstream, base.upstream).trim(),
    routes: isStringRecord(raw.routes) ? raw.routes : {},
    regionNeutral: bool(raw.regionNeutral, base.regionNeutral),
    acceptLanguage: str(raw.acceptLanguage, base.acceptLanguage).trim() || base.acceptLanguage,
    proxy: {
      kind,
      server: str(proxyRaw.server, "").trim(),
      ...proxyRaw.username ? { username: String(proxyRaw.username) } : {},
      ...proxyRaw.password ? { password: String(proxyRaw.password) } : {}
    },
    rules,
    allowlist: Array.isArray(raw.allowlist) ? raw.allowlist.filter((x) => typeof x === "string") : [...base.allowlist],
    packs,
    mappingTtlDays: Math.max(0, num(raw.mappingTtlDays, base.mappingTtlDays))
  };
}
function isStringRecord(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  return Object.values(value).every((v) => typeof v === "string");
}
function clampPort(value, fallback) {
  const port = Math.round(value);
  return port >= 1024 && port <= 65535 ? port : fallback;
}
function isRule(value) {
  if (!value || typeof value !== "object") return false;
  const r = value;
  return typeof r.id === "string" && r.id.length > 0 && typeof r.typePrefix === "string";
}
function mergeRules(builtin, stored) {
  const byId = new Map(stored.map((r) => [r.id, r]));
  const out = [];
  for (const rule2 of builtin) {
    const saved = byId.get(rule2.id);
    if (saved?.kind === rule2.kind) {
      out.push({ ...rule2, enabled: saved.enabled !== false, priority: typeof saved.priority === "number" ? saved.priority : rule2.priority });
      byId.delete(rule2.id);
    } else {
      out.push(rule2);
    }
  }
  for (const rule2 of byId.values()) out.push(rule2);
  return out;
}
var ConfigFile = class _ConfigFile {
  config = defaultConfig();
  writing = Promise.resolve();
  path;
  constructor(path) {
    this.path = path;
  }
  static in(dataDir) {
    return new _ConfigFile(join(dataDir, "config.json"));
  }
  get() {
    return this.config;
  }
  async load() {
    try {
      this.config = normalizeConfig(JSON.parse(await readFile(this.path, "utf8")));
    } catch (err) {
      if (err.code !== "ENOENT") this.config = defaultConfig();
    }
    return this.config;
  }
  async save(next) {
    this.config = normalizeConfig(next);
    const body = JSON.stringify(this.config, null, 2);
    const target = this.path;
    const run = async () => {
      await mkdir(dirname(target), { recursive: true });
      const tmp = `${target}.tmp`;
      await writeFile(tmp, body, { mode: 384 });
      await rename(tmp, target);
    };
    this.writing = this.writing.then(run, run);
    await this.writing;
    return this.config;
  }
  flush() {
    return this.writing;
  }
};

// ../../../plugins/veil/src/engine/store.ts
import { createHmac as createHmac2, randomBytes } from "node:crypto";
import { mkdir as mkdir2, readFile as readFile2, rename as rename2, writeFile as writeFile2 } from "node:fs/promises";
import { dirname as dirname2 } from "node:path";

// ../../../plugins/veil/src/engine/placeholder.ts
import { createHmac } from "node:crypto";
var CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
var placeholderPattern = () => /\{\{([A-Z][A-Z0-9]{0,31})_([0-9A-HJKMNP-TV-Z]{8,26})\}\}/g;
var MIN_ID_LEN = 8;
var MAX_ID_LEN = 26;
function keyedId(key, typePrefix, plaintext, len = MIN_ID_LEN) {
  const digest = createHmac("sha256", key).update(typePrefix, "utf8").update("\0").update(plaintext, "utf8").digest();
  let bits = 0;
  let acc = 0;
  let out = "";
  for (const byte of digest) {
    acc = acc << 8 | byte;
    bits += 8;
    while (bits >= 5 && out.length < len) {
      out += CROCKFORD[acc >> bits - 5 & 31];
      bits -= 5;
    }
    acc &= (1 << bits) - 1;
    if (out.length >= len) break;
  }
  return out;
}
function formatPlaceholder(p) {
  return `{{${p.typePrefix}_${p.id}}}`;
}
function parsePlaceholder(token) {
  const m = placeholderPattern().exec(token);
  if (!m || m[0] !== token) return void 0;
  return { typePrefix: m[1], id: m[2] };
}
function findPlaceholders(text) {
  const out = [];
  const pattern = placeholderPattern();
  for (let m = pattern.exec(text); m; m = pattern.exec(text)) {
    const parsed = parsePlaceholder(m[0]);
    if (parsed) out.push({ start: m.index, end: m.index + m[0].length, token: m[0], parsed });
  }
  return out;
}

// ../../../plugins/veil/src/engine/store.ts
async function loadOrCreateKey(path) {
  try {
    const existing = Buffer.from((await readFile2(path, "utf8")).trim(), "hex");
    if (existing.length >= 32) return existing;
  } catch {
  }
  const key = randomBytes(32);
  await mkdir2(dirname2(path), { recursive: true });
  await writeFile2(path, key.toString("hex"), { mode: 384 });
  return key;
}
var MappingStore = class {
  entries = /* @__PURE__ */ new Map();
  byToken = /* @__PURE__ */ new Map();
  dirty = false;
  writing = Promise.resolve();
  path;
  secret;
  ttlMs;
  cap;
  constructor(options) {
    this.path = options.path;
    this.secret = options.key;
    this.ttlMs = (options.ttlDays ?? 90) > 0 ? (options.ttlDays ?? 90) * 24 * 60 * 60 * 1e3 : 0;
    this.cap = options.cap ?? 1e5;
  }
  async load() {
    let raw;
    try {
      raw = JSON.parse(await readFile2(this.path, "utf8"));
    } catch {
      return { loaded: 0, evicted: 0 };
    }
    const now = Date.now();
    let evicted = 0;
    for (const [key, entry] of Object.entries(raw.entries ?? {})) {
      if (!entry?.token || typeof entry.plaintext !== "string" || !parsePlaceholder(entry.token)) {
        evicted++;
        continue;
      }
      if (this.ttlMs > 0 && now - (entry.lastSeen ?? 0) > this.ttlMs) {
        evicted++;
        continue;
      }
      this.entries.set(key, entry);
      this.byToken.set(entry.token, entry);
    }
    if (evicted > 0) this.dirty = true;
    return { loaded: this.entries.size, evicted };
  }
  /** The placeholder for `plaintext`, creating one on first sight. */
  getOrInsert(typePrefix, plaintext) {
    const key = this.plaintextKey(typePrefix, plaintext);
    const existing = this.entries.get(key);
    if (existing) {
      existing.lastSeen = Date.now();
      this.dirty = true;
      return existing.token;
    }
    let token = "";
    for (let len = MIN_ID_LEN; len <= MAX_ID_LEN; len++) {
      token = formatPlaceholder({ typePrefix, id: keyedId(this.secret, typePrefix, plaintext, len) });
      const owner = this.byToken.get(token);
      if (!owner || owner.plaintext === plaintext) break;
    }
    const entry = {
      token,
      plaintext,
      createdAt: Date.now(),
      lastSeen: Date.now()
    };
    this.entries.set(key, entry);
    this.byToken.set(entry.token, entry);
    this.evict();
    this.dirty = true;
    this.scheduleWrite();
    return entry.token;
  }
  /** Table key: HMAC of type + plaintext, so the file holds nothing that can be checked against a guess without the install key. */
  plaintextKey(typePrefix, plaintext) {
    return createHmac2("sha256", this.secret).update("k\0").update(typePrefix).update("\0").update(plaintext, "utf8").digest("hex").slice(0, 16);
  }
  /** The plaintext a token stands for, or undefined when this table never issued it. */
  lookup(token) {
    const entry = this.byToken.get(token);
    if (!entry) return void 0;
    entry.lastSeen = Date.now();
    this.dirty = true;
    return entry.plaintext;
  }
  size() {
    return this.entries.size;
  }
  /** Recent entries with the plaintext masked, for the panel. */
  preview(limit = 50) {
    return [...this.entries.values()].sort((a, b) => b.createdAt - a.createdAt).slice(0, limit).map((e) => ({ token: e.token, type: e.token.slice(2, e.token.indexOf("_")), masked: mask(e.plaintext), createdAt: e.createdAt }));
  }
  clear() {
    this.entries.clear();
    this.byToken.clear();
    this.dirty = true;
    return this.scheduleWrite();
  }
  evict() {
    if (this.entries.size <= this.cap) return;
    const sorted = [...this.entries.entries()].sort((a, b) => a[1].createdAt - b[1].createdAt);
    for (const [key, entry] of sorted.slice(0, this.entries.size - this.cap)) {
      this.entries.delete(key);
      this.byToken.delete(entry.token);
    }
  }
  /** Coalesces bursts of inserts into one write. */
  scheduleWrite() {
    if (!this.dirty) return this.writing;
    this.dirty = false;
    const snapshot = { version: 1, entries: Object.fromEntries(this.entries) };
    const body = JSON.stringify(snapshot);
    const run = async () => {
      await mkdir2(dirname2(this.path), { recursive: true });
      const tmp = `${this.path}.tmp`;
      await writeFile2(tmp, body, { mode: 384 });
      await rename2(tmp, this.path);
    };
    this.writing = this.writing.then(run, run);
    return this.writing;
  }
  /** Waits for pending writes; call before shutdown. */
  flush() {
    if (this.dirty) this.scheduleWrite();
    return this.writing;
  }
};
function mask(value) {
  if (value.length <= 8) return "\u2022".repeat(Math.max(4, value.length));
  return `${value.slice(0, 3)}${"\u2022".repeat(Math.min(8, value.length - 7))}${value.slice(-4)}`;
}

// ../../../plugins/veil/src/engine/binary.ts
function isDataUrlBase64(text) {
  const t = text.trim();
  if (t.length < 13 || !t.startsWith("data:")) return false;
  return /;base64,/i.test(t);
}
function looksLikeBase64Blob(text) {
  const t = text.trim();
  if (t.length < 32) return false;
  let alnum = 0;
  for (const ch of t) {
    if (/[A-Za-z0-9+/=_-]/.test(ch)) alnum++;
    else if (!/\s/.test(ch)) return false;
  }
  return alnum >= 32;
}
var BINARY_KEYS = /* @__PURE__ */ new Set(["data", "image", "b64_json", "inline_data", "file_data", "content_base64", "audio", "video"]);
function shouldSkipDesensitize(key, text) {
  if (isDataUrlBase64(text)) return true;
  if (!looksLikeBase64Blob(text)) return false;
  if (key !== void 0 && BINARY_KEYS.has(key)) return true;
  return text.trim().length >= 200;
}

// ../../../plugins/veil/src/engine/walk.ts
function mapStrings(value, key, f) {
  if (typeof value === "string") return f(key, value);
  if (Array.isArray(value)) return value.map((item) => mapStrings(item, void 0, f));
  if (value && typeof value === "object") {
    const sourceIsBinary = value.type === "base64" || value.type === "BASE64" || typeof value.media_type === "string" && /^(image|audio|video)\//i.test(value.media_type);
    const out = {};
    for (const [childKey, child] of Object.entries(value)) {
      const effective = sourceIsBinary && childKey === "data" ? "data" : childKey;
      out[childKey] = mapStrings(child, effective, f);
    }
    return out;
  }
  return value;
}
function desensitizeString(text, rules, store) {
  const hits = rules.findHits(text);
  if (hits.length === 0) return text;
  let out = text;
  for (let i = hits.length - 1; i >= 0; i--) {
    const hit = hits[i];
    out = out.slice(0, hit.start) + store.getOrInsert(hit.typePrefix, hit.plaintext) + out.slice(hit.end);
  }
  return out;
}
function restoreString(text, store) {
  const found = findPlaceholders(text);
  if (found.length === 0) return text;
  let out = text;
  for (let i = found.length - 1; i >= 0; i--) {
    const item = found[i];
    const plaintext = store.lookup(item.token);
    if (plaintext === void 0) continue;
    out = out.slice(0, item.start) + plaintext + out.slice(item.end);
  }
  return out;
}
function desensitizeJson(value, rules, store) {
  return mapStrings(value, void 0, (key, text) => {
    if (shouldSkipDesensitize(key, text)) return text;
    return desensitizeString(text, rules, store);
  });
}
function restoreJson(value, store) {
  return mapStrings(value, void 0, (_key, text) => restoreString(text, store));
}
function restoreAssistantMessage(message, store) {
  if (message.role !== "assistant" || !Array.isArray(message.content)) return message;
  let changed = false;
  const content = message.content.map((block) => {
    if (!block || typeof block !== "object") return block;
    const b = block;
    if (b.type === "text" && typeof b.text === "string") {
      const text = restoreString(b.text, store);
      if (text === b.text) return block;
      changed = true;
      return { ...b, text };
    }
    if (b.type === "thinking" && typeof b.thinking === "string" && !b.thinkingSignature && !b.redacted) {
      const thinking = restoreString(b.thinking, store);
      if (thinking === b.thinking) return block;
      changed = true;
      return { ...b, thinking };
    }
    if (b.type === "toolCall" && b.arguments && typeof b.arguments === "object") {
      const args = restoreJson(b.arguments, store);
      if (JSON.stringify(args) === JSON.stringify(b.arguments)) return block;
      changed = true;
      return { ...b, arguments: args };
    }
    return block;
  });
  return changed ? { ...message, content } : message;
}
function restoreInPlace(value, store) {
  let changed = false;
  const visit = (node) => {
    if (Array.isArray(node)) {
      node.forEach((item, i) => {
        if (typeof item === "string") {
          const next = restoreString(item, store);
          if (next !== item) {
            node[i] = next;
            changed = true;
          }
        } else visit(item);
      });
    } else if (node && typeof node === "object") {
      const obj = node;
      for (const key of Object.keys(obj)) {
        const item = obj[key];
        if (typeof item === "string") {
          const next = restoreString(item, store);
          if (next !== item) {
            obj[key] = next;
            changed = true;
          }
        } else visit(item);
      }
    }
  };
  visit(value);
  return changed;
}
function parseJson(text) {
  const trimmed = text.trimStart();
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return void 0;
  try {
    return JSON.parse(text);
  } catch {
    return void 0;
  }
}

// ../../../plugins/veil/src/proxy/relay.ts
import { createServer } from "node:http";
import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";

// ../../../plugins/veil/src/engine/sliding.ts
var MAX_HOLD = 96;
function holdSuffixStart(s) {
  const i = s.lastIndexOf("{");
  if (i === -1) return s.length;
  if (i > 0 && s[i - 1] === "{" && isPlaceholderPrefix(s.slice(i - 1))) return i - 1;
  if (isPlaceholderPrefix(s.slice(i))) return i;
  return s.length;
}
function isPlaceholderPrefix(s) {
  if (!s.startsWith("{")) return false;
  if (s.length === 1) return true;
  if (s[1] !== "{") return false;
  if (s.length === 2) return true;
  const rest = s.slice(2);
  let typeLen = 0;
  let ulidAt;
  for (let i = 0; i < rest.length; i++) {
    const c = rest[i];
    if (c === "_") {
      if (typeLen === 0) return false;
      ulidAt = 2 + i + 1;
      break;
    }
    if (typeLen === 0) {
      if (!/[A-Z]/.test(c)) return false;
    } else if (!/[A-Z0-9]/.test(c)) return false;
    typeLen++;
    if (typeLen > 32) return false;
  }
  if (ulidAt === void 0) return true;
  return isUlidClosePrefix(s.slice(ulidAt));
}
function isUlidClosePrefix(s) {
  let i = 0;
  while (i < s.length && i < 26) {
    const c = s[i];
    if (c === "}") return i >= 8 && s.slice(i) === "}";
    if (!/[0-9A-HJKMNP-TV-Z]/.test(c)) return false;
    i++;
  }
  const rest = s.slice(i);
  return rest === "" || rest === "}";
}
var RestoreWindow = class {
  tail = "";
  store;
  constructor(store) {
    this.store = store;
  }
  push(chunk) {
    this.tail += chunk;
    return this.emit(false);
  }
  flush() {
    return this.emit(true);
  }
  emit(flush) {
    const buf = this.tail;
    this.tail = "";
    let out = "";
    let last = 0;
    for (const found of findPlaceholders(buf)) {
      out += buf.slice(last, found.start);
      const plaintext = this.store.lookup(found.token);
      out += plaintext === void 0 ? found.token : plaintext;
      last = found.end;
    }
    const remaining = buf.slice(last);
    if (flush) return out + remaining;
    const holdAt = holdSuffixStart(remaining);
    out += remaining.slice(0, holdAt);
    const keep = remaining.slice(holdAt);
    if (keep.length > MAX_HOLD && !keep.startsWith("{{")) out += keep;
    else this.tail = keep;
    return out;
  }
};

// ../../../plugins/veil/src/proxy/transport.ts
import { Agent as HttpAgent } from "node:http";
import { Agent as HttpsAgent } from "node:https";
import { connect as netConnect } from "node:net";
import { connect as tlsConnect } from "node:tls";
function parseProxyUrl(raw) {
  const trimmed = raw.trim();
  if (!trimmed) return { kind: "none", server: "" };
  const withScheme = /^[a-z0-9+.-]+:\/\//i.test(trimmed) ? trimmed : `http://${trimmed}`;
  let url;
  try {
    url = new URL(withScheme);
  } catch {
    throw new Error(`\u4EE3\u7406\u5730\u5740\u65E0\u6CD5\u89E3\u6790\uFF1A${raw}`);
  }
  const scheme = url.protocol.replace(/:$/, "").toLowerCase();
  const kind = scheme === "socks5" || scheme === "socks5h" || scheme === "socks" ? "socks5" : scheme === "http" || scheme === "https" ? "http" : "none";
  if (kind === "none") throw new Error(`\u4E0D\u652F\u6301\u7684\u4EE3\u7406\u534F\u8BAE\uFF1A${scheme}\uFF08\u652F\u6301 http\u3001https\u3001socks5\uFF09`);
  const port = url.port ? Number(url.port) : kind === "socks5" ? 1080 : 8080;
  if (!url.hostname || !Number.isInteger(port) || port <= 0 || port > 65535) throw new Error(`\u4EE3\u7406\u5730\u5740\u7F3A\u5C11\u6709\u6548\u7684\u4E3B\u673A\u6216\u7AEF\u53E3\uFF1A${raw}`);
  return {
    kind,
    server: `${url.hostname}:${port}`,
    ...url.username ? { username: decodeURIComponent(url.username) } : {},
    ...url.password ? { password: decodeURIComponent(url.password) } : {}
  };
}
function splitServer(server) {
  const idx = server.lastIndexOf(":");
  if (idx === -1) return { host: server, port: 8080 };
  const port = Number(server.slice(idx + 1));
  return { host: server.slice(0, idx) || server, port: Number.isInteger(port) ? port : 8080 };
}
function connectRaw(target, timeoutMs) {
  return new Promise((resolve, reject) => {
    const socket = netConnect({ host: target.host, port: target.port });
    const timer = setTimeout(() => {
      socket.destroy();
      reject(new Error(`\u8FDE\u63A5\u4EE3\u7406\u8D85\u65F6\uFF1A${target.host}:${target.port}`));
    }, timeoutMs);
    socket.once("connect", () => {
      clearTimeout(timer);
      socket.setTimeout(0);
      resolve(socket);
    });
    socket.once("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}
async function httpTunnel(proxy, destination, timeoutMs) {
  const p = splitServer(proxy.server);
  const socket = await connectRaw(p, timeoutMs);
  const auth = proxy.username ? `Proxy-Authorization: Basic ${Buffer.from(`${proxy.username}:${proxy.password ?? ""}`).toString("base64")}\r
` : "";
  const request = `CONNECT ${destination.host}:${destination.port} HTTP/1.1\r
Host: ${destination.host}:${destination.port}\r
${auth}\r
`;
  return new Promise((resolve, reject) => {
    let buffered = "";
    const onData = (chunk) => {
      buffered += chunk.toString("latin1");
      const end = buffered.indexOf("\r\n\r\n");
      if (end === -1) {
        if (buffered.length > 16 * 1024) fail(new Error("\u4EE3\u7406\u8FD4\u56DE\u7684\u54CD\u5E94\u5934\u8FC7\u957F"));
        return;
      }
      cleanup();
      const statusLine = buffered.slice(0, buffered.indexOf("\r\n"));
      const status = Number(statusLine.split(" ")[1]);
      if (status !== 200) {
        socket.destroy();
        reject(new Error(`\u4EE3\u7406\u62D2\u7EDD\u5EFA\u7ACB\u96A7\u9053\uFF08HTTP ${status}\uFF09\uFF1A${statusLine}`));
        return;
      }
      const rest = Buffer.from(buffered.slice(end + 4), "latin1");
      if (rest.length > 0) socket.unshift(rest);
      resolve(socket);
    };
    const fail = (err) => {
      cleanup();
      socket.destroy();
      reject(err);
    };
    const onError = (err) => fail(err);
    const onClose = () => fail(new Error("\u4EE3\u7406\u5728\u5EFA\u7ACB\u96A7\u9053\u65F6\u5173\u95ED\u4E86\u8FDE\u63A5"));
    const timer = setTimeout(() => fail(new Error("\u4EE3\u7406\u5EFA\u7ACB\u96A7\u9053\u8D85\u65F6")), timeoutMs);
    function cleanup() {
      clearTimeout(timer);
      socket.off("data", onData);
      socket.off("error", onError);
      socket.off("close", onClose);
    }
    socket.on("data", onData);
    socket.once("error", onError);
    socket.once("close", onClose);
    socket.write(request);
  });
}
async function socksTunnel(proxy, destination, timeoutMs) {
  const p = splitServer(proxy.server);
  const socket = await connectRaw(p, timeoutMs);
  socket.setNoDelay(true);
  const read = () => new Promise((resolve, reject) => {
    const onData = (chunk) => {
      cleanup();
      resolve(chunk);
    };
    const onError = (err) => {
      cleanup();
      reject(err);
    };
    const onClose = () => {
      cleanup();
      reject(new Error("SOCKS5 \u4EE3\u7406\u5173\u95ED\u4E86\u8FDE\u63A5"));
    };
    const timer = setTimeout(() => {
      cleanup();
      reject(new Error("SOCKS5 \u4EE3\u7406\u54CD\u5E94\u8D85\u65F6"));
    }, timeoutMs);
    function cleanup() {
      clearTimeout(timer);
      socket.off("data", onData);
      socket.off("error", onError);
      socket.off("close", onClose);
    }
    socket.once("data", onData);
    socket.once("error", onError);
    socket.once("close", onClose);
  });
  let pending = Buffer.alloc(0);
  const readExact = async (n) => {
    while (pending.length < n) {
      const more = await read();
      pending = Buffer.concat([pending, more]);
    }
    const out = pending.subarray(0, n);
    pending = pending.subarray(n);
    return Buffer.from(out);
  };
  try {
    const wantAuth = Boolean(proxy.username);
    socket.write(Buffer.from([5, wantAuth ? 2 : 1, wantAuth ? 2 : 0, ...wantAuth ? [0] : []]));
    const greeting = await readExact(2);
    if (greeting[0] !== 5) throw new Error("SOCKS5 \u4EE3\u7406\u8FD4\u56DE\u4E86\u65E0\u6548\u7248\u672C");
    const method = greeting[1];
    if (method === void 0) throw new Error("SOCKS5 \u4EE3\u7406\u8FD4\u56DE\u4E86\u4E0D\u5B8C\u6574\u7684\u63E1\u624B\u54CD\u5E94");
    if (method === 255) throw new Error("SOCKS5 \u4EE3\u7406\u4E0D\u63A5\u53D7\u6211\u4EEC\u7684\u8BA4\u8BC1\u65B9\u5F0F");
    if (method === 2) {
      const user = Buffer.from(proxy.username ?? "", "utf8");
      const pass = Buffer.from(proxy.password ?? "", "utf8");
      if (user.length > 255 || pass.length > 255) throw new Error("SOCKS5 \u7528\u6237\u540D\u6216\u5BC6\u7801\u8FC7\u957F");
      socket.write(Buffer.concat([Buffer.from([1, user.length]), user, Buffer.from([pass.length]), pass]));
      const [, status] = await readExact(2);
      if (status !== 0) throw new Error("SOCKS5 \u8BA4\u8BC1\u5931\u8D25");
    } else if (method !== 0) {
      throw new Error(`SOCKS5 \u4EE3\u7406\u8981\u6C42\u4E0D\u652F\u6301\u7684\u8BA4\u8BC1\u65B9\u5F0F\uFF1A0x${method.toString(16)}`);
    }
    const host = Buffer.from(destination.host, "utf8");
    socket.write(Buffer.concat([Buffer.from([5, 1, 0, 3, host.length]), host, Buffer.from([destination.port >> 8 & 255, destination.port & 255])]));
    const head = await readExact(4);
    if (head[0] !== 5) throw new Error("SOCKS5 \u4EE3\u7406\u8FD4\u56DE\u4E86\u65E0\u6548\u7248\u672C");
    if (head[1] !== 0) throw new Error(`SOCKS5 \u8FDE\u63A5\u5931\u8D25\uFF08\u539F\u56E0\u7801 0x${head[1]?.toString(16)}\uFF09`);
    const atyp = head[3];
    const addressLength = atyp === 1 ? 4 : atyp === 4 ? 16 : atyp === 3 ? (await readExact(1))[0] : 0;
    if (!addressLength) throw new Error("SOCKS5 \u4EE3\u7406\u8FD4\u56DE\u4E86\u672A\u77E5\u7684\u5730\u5740\u7C7B\u578B");
    await readExact(addressLength + 2);
    if (pending.length > 0) socket.unshift(pending);
    return socket;
  } catch (err) {
    socket.destroy();
    throw err;
  }
}
async function openTunnel(proxy, destination, timeoutMs = 2e4) {
  if (proxy.kind === "socks5") return socksTunnel(proxy, destination, timeoutMs);
  return httpTunnel(proxy, destination, timeoutMs);
}
function connectThrough(proxy, destination, secure, servername, timeoutMs = 2e4) {
  const tlsOptions = {
    host: destination.host,
    port: destination.port,
    servername: servername ?? destination.host,
    ALPNProtocols: ["http/1.1"]
  };
  if (proxy.kind === "none") {
    if (!secure) return connectRaw(destination, timeoutMs);
    return new Promise((resolve, reject) => {
      const socket = tlsConnect({ ...tlsOptions, host: destination.host, port: destination.port });
      const timer = setTimeout(() => {
        socket.destroy();
        reject(new Error(`\u8FDE\u63A5\u8D85\u65F6\uFF1A${destination.host}:${destination.port}`));
      }, timeoutMs);
      socket.once("secureConnect", () => {
        clearTimeout(timer);
        resolve(socket);
      });
      socket.once("error", (err) => {
        clearTimeout(timer);
        reject(err);
      });
    });
  }
  return openTunnel(proxy, destination, timeoutMs).then(
    (raw) => new Promise((resolve, reject) => {
      if (!secure) {
        resolve(raw);
        return;
      }
      const socket = tlsConnect({ ...tlsOptions, socket: raw });
      socket.once("secureConnect", () => resolve(socket));
      socket.once("error", (err) => {
        raw.destroy();
        reject(err);
      });
    })
  );
}
function proxyAgent(proxy, secure) {
  const agent = secure ? new HttpsAgent({ keepAlive: true, maxSockets: 64 }) : new HttpAgent({ keepAlive: true, maxSockets: 64 });
  const createConnection = (options, callback) => {
    const host = options.hostname ?? options.host ?? "localhost";
    const port = Number(options.port ?? (secure ? 443 : 80));
    connectThrough(proxy, { host, port }, secure, host).then((socket) => callback(null, socket)).catch((err) => callback(err instanceof Error ? err : new Error(String(err))));
    return void 0;
  };
  agent.createConnection = createConnection;
  return agent;
}

// ../../../plugins/veil/src/proxy/relay.ts
var HOP_BY_HOP = /* @__PURE__ */ new Set(["host", "accept-encoding", "content-length", "transfer-encoding", "connection", "proxy-connection", "keep-alive", "upgrade", "te", "trailer"]);
var RESPONSE_DROP = /* @__PURE__ */ new Set(["transfer-encoding", "connection", "content-length", "keep-alive"]);
var VeilRelay = class {
  server;
  error;
  hits = [];
  agents = /* @__PURE__ */ new Map();
  options;
  constructor(options) {
    this.options = options;
  }
  status() {
    const addr = this.server?.address();
    const port = typeof addr === "object" && addr ? addr.port : this.options.port;
    return {
      running: Boolean(this.server?.listening),
      host: this.options.host,
      port,
      baseUrl: `http://${this.options.host}:${port}`,
      ...this.error ? { error: this.error } : {}
    };
  }
  recentHits(limit = 50) {
    return this.hits.slice(-limit).reverse();
  }
  clearHits() {
    this.hits.length = 0;
  }
  async start() {
    if (this.server?.listening) return this.status();
    this.error = void 0;
    const server = createServer((req, res) => {
      this.handle(req, res).catch((err) => {
        this.options.log.error(`\u8F6C\u53D1\u5931\u8D25\uFF1A${err instanceof Error ? err.message : String(err)}`);
        if (!res.headersSent) jsonError(res, 502, err instanceof Error ? err.message : "\u8F6C\u53D1\u5931\u8D25");
        else res.end();
      });
    });
    server.headersTimeout = 12e4;
    server.requestTimeout = 0;
    this.server = server;
    return new Promise((resolve) => {
      server.once("error", (err) => {
        this.error = err.code === "EADDRINUSE" ? `\u7AEF\u53E3 ${this.options.port} \u5DF2\u88AB\u5360\u7528` : err.message;
        this.server = void 0;
        resolve(this.status());
      });
      server.listen(this.options.port, this.options.host, () => resolve(this.status()));
    });
  }
  async stop() {
    for (const agent of this.agents.values()) agent.destroy();
    this.agents.clear();
    const server = this.server;
    if (!server) return;
    this.server = void 0;
    await new Promise((resolve) => server.close(() => resolve()));
  }
  agentFor(proxy, secure) {
    const key = `${secure ? "tls" : "tcp"}|${proxy.kind}|${proxy.server}|${proxy.username ?? ""}`;
    let agent = this.agents.get(key);
    if (!agent) {
      agent = proxyAgent(proxy, secure);
      this.agents.set(key, agent);
    }
    return agent;
  }
  async handle(req, res) {
    const started = Date.now();
    const { host, port, secure, path, query } = this.resolveTarget(req);
    const method = req.method ?? "GET";
    const upstream = `${secure ? "https" : "http"}://${host}${port ? `:${port}` : ""}`;
    if (!host) return jsonError(res, 500, "\u6CA1\u6709\u914D\u7F6E\u4E0A\u6E38\u5730\u5740\uFF1A\u8BF7\u5728\u9762\u7F69\u9762\u677F\u7684\u300C\u4EE3\u7406\u300D\u91CC\u586B\u5199\u8981\u8F6C\u53D1\u7684\u76EE\u6807");
    const raw = await readBody(req, this.options.bodyLimitBytes());
    if (raw === void 0) return jsonError(res, 413, "\u8BF7\u6C42\u4F53\u8FC7\u5927");
    const proxy = this.options.proxy();
    const rules = this.options.rules();
    const types = /* @__PURE__ */ new Set();
    let body = raw;
    if (raw.length > 0) {
      const rewritten = this.redact(raw, req.headers["content-type"], rules, types);
      if (!rewritten.ok) return jsonError(res, 502, rewritten.error);
      body = rewritten.body;
    }
    const headers = {};
    for (const [name, value] of Object.entries(req.headers)) {
      if (!value || HOP_BY_HOP.has(name.toLowerCase())) continue;
      headers[name] = value;
    }
    headers.host = port && !isDefaultPort(port, secure) ? `${host}:${port}` : host;
    if (body.length > 0) headers["content-length"] = String(body.length);
    if (this.options.regionNeutral()) {
      headers["accept-language"] = this.options.acceptLanguage();
      headers["accept-encoding"] = "identity";
    }
    const requestFn = secure ? httpsRequest : httpRequest;
    const request = requestFn(
      {
        host,
        port,
        path: query ? `${path}?${query}` : path,
        method,
        headers,
        agent: this.agentFor(proxy, secure),
        servername: host
      },
      (upstreamRes) => this.pipeResponse(upstreamRes, res, { method, path, upstream, types, started })
    );
    request.on("error", (err) => {
      if (!res.headersSent) jsonError(res, 502, `\u8FDE\u63A5\u4E0A\u6E38\u5931\u8D25\uFF1A${err.message}`);
      else res.end();
    });
    res.on("close", () => {
      if (!res.writableFinished) request.destroy();
    });
    if (body.length > 0) request.write(body);
    request.end();
  }
  /** Desensitises a request body, returning a 502 reason when the rule engine cannot complete. */
  redact(raw, contentType, rules, types) {
    try {
      return { ok: true, body: redactBuffer(raw, contentType, rules, this.options.store, types) };
    } catch (err) {
      return { ok: false, error: `\u8131\u654F\u5931\u8D25\uFF1A${err instanceof Error ? err.message : String(err)}` };
    }
  }
  pipeResponse(upstreamRes, res, ctx) {
    const status = upstreamRes.statusCode ?? 502;
    const contentType = String(upstreamRes.headers["content-type"] ?? "");
    const headers = {};
    for (const [name, value] of Object.entries(upstreamRes.headers)) {
      if (value === void 0 || RESPONSE_DROP.has(name.toLowerCase())) continue;
      headers[name] = value;
    }
    const isEventStream = contentType.toLowerCase().includes("text/event-stream");
    if (isEventStream) {
      res.writeHead(status, headers);
      const window = new RestoreWindow(this.options.store);
      let buffer = "";
      upstreamRes.setEncoding("utf8");
      upstreamRes.on("data", (chunk) => {
        buffer += chunk;
        const restored = restoreEventStream(buffer, window, false);
        buffer = restored.rest;
        if (restored.out) res.write(restored.out);
      });
      upstreamRes.on("end", () => {
        const tail = restoreEventStream(buffer, window, true);
        if (tail.out) res.write(tail.out);
        const flushed = window.flush();
        if (flushed) res.write(flushed);
        this.record(ctx, status);
        res.end();
      });
      upstreamRes.on("error", (err) => {
        this.options.log.warn(`\u4E0A\u6E38\u6D41\u4E2D\u65AD\uFF1A${err.message}`);
        res.end();
      });
      return;
    }
    const chunks = [];
    upstreamRes.on("data", (chunk) => chunks.push(chunk));
    upstreamRes.on("error", (err) => {
      if (!res.headersSent) jsonError(res, 502, `\u8BFB\u53D6\u4E0A\u6E38\u54CD\u5E94\u5931\u8D25\uFF1A${err.message}`);
      else res.end();
    });
    upstreamRes.on("end", () => {
      const raw = Buffer.concat(chunks);
      let out = raw;
      const looksJson = contentType.toLowerCase().includes("json") || /^[\s]*[[{]/.test(raw.subarray(0, 64).toString("utf8"));
      if (looksJson) {
        const parsed = parseJson(raw.toString("utf8"));
        if (parsed !== void 0) out = Buffer.from(JSON.stringify(restoreJson(parsed, this.options.store)), "utf8");
      } else if (!shouldSkipDesensitize(void 0, raw.toString("utf8"))) {
        out = Buffer.from(restoreString(raw.toString("utf8"), this.options.store), "utf8");
      }
      headers["content-length"] = String(out.length);
      res.writeHead(status, headers);
      this.record(ctx, status);
      res.end(out);
    });
  }
  /**
   * Splits an SSE buffer into complete events, restores the `data:` payloads, and returns the
   * unfinished remainder. Mirrors veil's `sse.rs`, but reuses one window for the whole stream.
   */
  record(ctx, status) {
    this.hits.push({
      at: Date.now(),
      method: ctx.method,
      path: ctx.path,
      upstream: ctx.upstream,
      types: [...ctx.types],
      status,
      durationMs: Date.now() - ctx.started,
      redacted: ctx.types.size
    });
    if (this.hits.length > 200) this.hits.splice(0, this.hits.length - 200);
  }
  /**
   * Upstream target for a forwarded request.
   *
   * The SDKs request bare paths (`/v1/messages`), so the destination comes from, in order:
   * an `x-veil-upstream` header (tests and manual calls), an absolute request URL, or the
   * gateway route table configured when the relay was switched on.
   */
  resolveTarget(req) {
    const raw = req.url ?? "/";
    const splitPath = (value) => {
      const q = value.indexOf("?");
      return q === -1 ? { path: value, query: "" } : { path: value.slice(0, q), query: value.slice(q + 1) };
    };
    const routeFor = (url) => {
      const secure2 = url.protocol === "https:";
      const { path: path2, query: query2 } = splitPath(raw);
      return {
        host: url.hostname,
        port: Number(url.port) || (secure2 ? 443 : 80),
        secure: secure2,
        path: joinPath(url.pathname, path2),
        query: query2
      };
    };
    const explicit = req.headers["x-veil-upstream"];
    if (explicit) {
      const url = safeUrl(String(explicit));
      if (url) return routeFor(url);
    }
    const absolute = safeUrl(raw);
    if (absolute) return routeFor(absolute);
    const { path, query } = splitPath(raw);
    const routes = this.options.routes();
    let best;
    for (const value of Object.values(routes)) {
      const url = safeUrl(value);
      if (!url) continue;
      const prefix = url.pathname.replace(/\/+$/, "");
      if (prefix && !path.startsWith(prefix)) continue;
      if (!best || prefix.length > best.route.length) best = { route: prefix, url };
    }
    const fallback = safeUrl(this.options.defaultUpstream());
    const chosen = best?.url ?? fallback;
    if (!chosen) return { host: "", port: 0, secure: false, path, query };
    const secure = chosen.protocol === "https:";
    const rest = best?.route ? path.slice(best.route.length) || "/" : path;
    return {
      host: chosen.hostname,
      port: Number(chosen.port) || (secure ? 443 : 80),
      secure,
      path: joinPath(best ? "" : chosen.pathname, rest),
      query
    };
  }
};
function joinPath(prefix, suffix) {
  const left = prefix.replace(/\/+$/, "");
  const right = suffix.startsWith("/") ? suffix : `/${suffix}`;
  return `${left}${right}` || "/";
}
function collectTypes(before, after, _rules, types) {
  const collect = (value, into) => {
    if (typeof value === "string") {
      for (const found of findPlaceholders(value)) into.add(found.parsed.typePrefix);
    } else if (Array.isArray(value)) {
      for (const item of value) collect(item, into);
    } else if (value && typeof value === "object") {
      for (const child of Object.values(value)) collect(child, into);
    }
  };
  const already = /* @__PURE__ */ new Set();
  collect(before, already);
  const result = /* @__PURE__ */ new Set();
  collect(after, result);
  for (const type of result) if (!already.has(type)) types.add(type);
}
function isDefaultPort(port, secure) {
  return secure && port === 443 || !secure && port === 80;
}
function safeUrl(raw) {
  try {
    return new URL(raw);
  } catch {
    return void 0;
  }
}
function readBody(req, limit) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (chunk) => {
      size += chunk.length;
      if (size > limit) {
        resolve(void 0);
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}
function jsonError(res, status, message) {
  const body = JSON.stringify({ error: { type: "veil_relay", message } });
  res.writeHead(status, { "content-type": "application/json; charset=utf-8", "content-length": Buffer.byteLength(body) });
  res.end(body);
}
function redactBuffer(raw, contentType, rules, store, types = /* @__PURE__ */ new Set()) {
  const looksJson = (contentType ?? "").toLowerCase().includes("json") || /^[\s]*[[{]/.test(raw.subarray(0, 64).toString("utf8"));
  if (looksJson) {
    const parsed = parseJson(raw.toString("utf8"));
    if (parsed === void 0) return raw;
    const redacted = desensitizeJson(parsed, rules, store);
    collectTypes(parsed, redacted, rules, types);
    return Buffer.from(JSON.stringify(redacted), "utf8");
  }
  const text = raw.toString("utf8");
  if (shouldSkipDesensitize(void 0, text) || isDataUrlBase64(text)) return raw;
  const hits = rules.findHits(text);
  if (hits.length === 0) return raw;
  for (const hit of hits) types.add(hit.typePrefix);
  return Buffer.from(desensitizeString(text, rules, store), "utf8");
}
function createRestorer(contentType, store) {
  if (contentType.toLowerCase().includes("text/event-stream")) {
    const window = new RestoreWindow(store);
    let buffer = "";
    return {
      write(text) {
        buffer += text;
        const restored = restoreEventStream(buffer, window, false);
        buffer = restored.rest;
        return restored.out;
      },
      end() {
        const tail = restoreEventStream(buffer, window, true);
        buffer = "";
        return tail.out + window.flush();
      }
    };
  }
  let whole = "";
  return {
    write(text) {
      whole += text;
      return "";
    },
    end() {
      const looksJson = contentType.toLowerCase().includes("json") || /^[\s]*[[{]/.test(whole.slice(0, 64));
      if (looksJson) {
        const parsed = parseJson(whole);
        if (parsed !== void 0) return JSON.stringify(restoreJson(parsed, store));
      }
      return shouldSkipDesensitize(void 0, whole) ? whole : restoreString(whole, store);
    }
  };
}
function restoreEventStream(buffer, window, flush) {
  let rest = buffer;
  let out = "";
  for (; ; ) {
    const crlf = rest.indexOf("\r\n\r\n");
    const lf = rest.indexOf("\n\n");
    const idx = crlf === -1 ? lf : lf === -1 ? crlf : Math.min(crlf, lf);
    if (idx === -1) break;
    const end = crlf !== -1 && idx === crlf ? idx + 4 : idx + 2;
    const event = rest.slice(0, idx);
    rest = rest.slice(end);
    out += `${restoreEvent(event, window)}

`;
  }
  if (flush && rest.trim()) {
    out += restoreEvent(rest.trimEnd(), window);
    rest = "";
  }
  return { out, rest };
}
function restoreEvent(event, window) {
  const meta = [];
  const dataParts = [];
  for (const rawLine of event.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.startsWith("data:")) dataParts.push(line.slice(5).trimStart());
    else meta.push(line);
  }
  if (dataParts.length === 0) return event;
  const data = dataParts.join("\n");
  meta.push(`data: ${restorePayload(data, window)}`);
  return meta.join("\n");
}
function restorePayload(data, window) {
  if (data === "[DONE]") return data;
  const parsed = parseJson(data);
  if (parsed === void 0) return restoreString(data, window.store);
  const slot = findDeltaText(parsed);
  if (slot) {
    slot.set(window.push(slot.get()));
    return JSON.stringify(parsed);
  }
  return JSON.stringify(restoreJson(parsed, window.store));
}
function findDeltaText(value) {
  if (!value || typeof value !== "object") return void 0;
  const obj = value;
  const slotFor = (holder, key) => typeof holder[key] === "string" ? { get: () => holder[key], set: (v) => holder[key] = v } : void 0;
  const delta = obj.delta;
  if (delta && typeof delta === "object") {
    const d = delta;
    const text = slotFor(d, "text") ?? slotFor(d, "partial_json");
    if (text) return text;
  }
  const choices = obj.choices;
  if (Array.isArray(choices) && choices.length > 0) {
    const first = choices[0];
    if (first && typeof first === "object") {
      const d = first.delta;
      if (d && typeof d === "object") return slotFor(d, "content");
    }
  }
  return void 0;
}

// ../../../plugins/veil/src/index.ts
var str2 = (v, fallback = "") => typeof v === "string" ? v : fallback;
var bool2 = (v, fallback) => typeof v === "boolean" ? v : fallback;
var index_default = definePlugin({
  name: "veil",
  version: "0.1.0",
  optional: true,
  async setup(ctx) {
    const configFile = ConfigFile.in(ctx.dataDir);
    await configFile.load();
    const store = new MappingStore({
      path: `${ctx.dataDir}/mappings.json`,
      key: await loadOrCreateKey(`${ctx.dataDir}/install.key`),
      ttlDays: configFile.get().mappingTtlDays
    });
    const loaded = await store.load();
    if (loaded.loaded > 0) ctx.log.info(`\u5DF2\u8F7D\u5165 ${loaded.loaded} \u6761\u5316\u540D\u6620\u5C04${loaded.evicted ? `\uFF0C\u6E05\u7406 ${loaded.evicted} \u6761\u8FC7\u671F/\u65E0\u6548` : ""}`);
    let ruleset = buildRuleSet(configFile.get());
    const rebuild = () => {
      ruleset = buildRuleSet(configFile.get());
    };
    const relay = new VeilRelay({
      host: "127.0.0.1",
      port: configFile.get().relayPort,
      rules: () => ruleset,
      store,
      proxy: () => configFile.get().proxy,
      acceptLanguage: () => configFile.get().acceptLanguage,
      regionNeutral: () => configFile.get().regionNeutral,
      bodyLimitBytes: () => 32 * 1024 * 1024,
      routes: () => configFile.get().routes,
      defaultUpstream: () => configFile.get().upstream,
      log: { info: (m) => ctx.log.info(m), warn: (m) => ctx.log.warn(m), error: (m) => ctx.log.error(m) }
    });
    const state = () => {
      const cfg = configFile.get();
      const relayStatus = relay.status();
      return {
        config: {
          redaction: cfg.redaction,
          relay: cfg.relay,
          relayPort: cfg.relayPort,
          upstream: cfg.upstream,
          regionNeutral: cfg.regionNeutral,
          acceptLanguage: cfg.acceptLanguage,
          mappingTtlDays: cfg.mappingTtlDays,
          packs: cfg.packs,
          proxyKind: cfg.proxy.kind,
          proxyServer: cfg.proxy.server,
          proxyAuth: Boolean(cfg.proxy.username),
          rules: cfg.rules,
          allowlist: cfg.allowlist
        },
        relay: { running: relayStatus.running, baseUrl: relayStatus.baseUrl, port: relayStatus.port, ...relayStatus.error ? { error: relayStatus.error } : {} },
        stats: {
          mappings: store.size(),
          hits: relay.recentHits().length,
          lastTypes: [...new Set(relay.recentHits(20).flatMap((h) => h.types))]
        }
      };
    };
    const apply = async (mutate) => {
      const draft = structuredClone(configFile.get());
      mutate(draft);
      if (draft.relay) draft.routes = { ...draft.routes, ...upstreamMap() };
      await configFile.save(draft);
      rebuild();
      await syncRelay();
      await ctx.reloadSessions();
      return state();
    };
    const upstreamMap = () => {
      const out = {};
      for (const g of ctx.gateways()) {
        if (g.enabled === false || !g.baseUrl) continue;
        out[g.id] = g.baseUrl;
      }
      return out;
    };
    const syncRelay = async () => {
      const cfg = configFile.get();
      if (!cfg.relay) {
        if (relay.status().running) await relay.stop();
        return;
      }
      const status = relay.status();
      if (status.running && status.port === cfg.relayPort) return;
      if (status.running) await relay.stop();
      const started = await relay.start();
      if (!started.running) ctx.log.warn(`\u51FA\u53E3\u4EE3\u7406\u672A\u80FD\u542F\u52A8\uFF1A${started.error ?? "\u672A\u77E5\u539F\u56E0"}`);
    };
    await syncRelay();
    const syncGatewayRoutes = (pi) => {
      const cfg = configFile.get();
      const status = relay.status();
      if (!cfg.relay || !status.running) return;
      for (const gateway of ctx.gateways()) {
        if (gateway.enabled === false) continue;
        const base = gateway.format === "anthropic-messages" ? status.baseUrl : `${status.baseUrl}/v1`;
        try {
          pi.registerProvider(gateway.id, { baseUrl: base });
        } catch (err) {
          ctx.log.warn(`\u65E0\u6CD5\u628A\u7F51\u5173 ${gateway.id} \u6307\u5411\u8F6C\u53D1\u7AEF\u53E3\uFF1A${err instanceof Error ? err.message : String(err)}`);
        }
      }
    };
    ctx.hooks.gatewayTraffic({
      request: (info, body) => {
        if (!configFile.get().redaction) return body;
        try {
          return redactBuffer(body, info.contentType, currentRules(), store);
        } catch (err) {
          ctx.log.error(`\u8131\u654F\u5931\u8D25\uFF0C\u5DF2\u62E6\u622A\u672C\u6B21\u8BF7\u6C42\uFF1A${err instanceof Error ? err.message : String(err)}`);
          throw new Error("\u9762\u7F69\uFF1A\u8131\u654F\u5931\u8D25\uFF0C\u5DF2\u62E6\u622A\u672C\u6B21\u8BF7\u6C42\uFF08\u672A\u53D1\u5F80\u4E0A\u6E38\uFF09");
        }
      },
      response: (_info, head) => configFile.get().redaction ? createRestorer(head.contentType, store) : void 0
    });
    ctx.extension((pi) => {
      syncGatewayRoutes(pi);
      pi.on("before_provider_request", (event) => {
        const cfg = configFile.get();
        if (!cfg.redaction) return void 0;
        const payload = event.payload;
        if (payload === void 0 || payload === null) return void 0;
        try {
          const parsed = typeof payload === "string" ? parseJson(payload) : payload;
          if (parsed === void 0) {
            return typeof payload === "string" ? desensitizeString(payload, currentRules(), store) : void 0;
          }
          return desensitizeJson(parsed, currentRules(), store);
        } catch (err) {
          ctx.log.error(`\u8131\u654F\u5931\u8D25\uFF0C\u5DF2\u62E6\u622A\u672C\u6B21\u8BF7\u6C42\uFF1A${err instanceof Error ? err.message : String(err)}`);
          throw new Error("\u9762\u7F69\uFF1A\u8131\u654F\u5931\u8D25\uFF0C\u5DF2\u62E6\u622A\u672C\u6B21\u8BF7\u6C42\uFF08\u672A\u53D1\u5F80\u4E0A\u6E38\uFF09");
        }
      });
      pi.on("message_end", (event) => {
        const message = event.message;
        const restored = restoreAssistantMessage(message, store);
        return restored === message ? void 0 : { message: restored };
      });
      pi.on("tool_call", (event) => {
        restoreInPlace(event.input, store);
        return void 0;
      });
      pi.on("before_provider_headers", (event) => {
        const cfg = configFile.get();
        if (!cfg.regionNeutral) return;
        const headers = event.headers;
        for (const key of Object.keys(headers)) {
          const lower = key.toLowerCase();
          if (lower === "accept-language") headers[key] = cfg.acceptLanguage;
          else if (lower === "accept-encoding") headers[key] = "identity";
        }
        if (!Object.keys(headers).some((k) => k.toLowerCase() === "accept-language")) headers["accept-language"] = cfg.acceptLanguage;
        if (!Object.keys(headers).some((k) => k.toLowerCase() === "accept-encoding")) headers["accept-encoding"] = "identity";
      });
    });
    const currentRules = () => ruleset;
    const handlers = {
      state: () => state(),
      setFlags: async (params) => apply((draft) => {
        if (params.redaction !== void 0) draft.redaction = bool2(params.redaction, draft.redaction);
        if (params.relay !== void 0) draft.relay = bool2(params.relay, draft.relay);
        if (params.regionNeutral !== void 0) draft.regionNeutral = bool2(params.regionNeutral, draft.regionNeutral);
      }),
      setProxy: async (params) => {
        const raw = str2(params.url).trim();
        let proxy;
        if (!raw) proxy = { kind: "none", server: "" };
        else {
          try {
            proxy = parseProxyUrl(raw);
          } catch (err) {
            throw new Error(err instanceof Error ? err.message : String(err));
          }
        }
        return apply((draft) => {
          draft.proxy = proxy;
        });
      },
      setRelay: async (params) => apply((draft) => {
        if (params.port !== void 0) {
          const port = Number(params.port);
          if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error("\u7AEF\u53E3\u9700\u5728 1024\u201365535 \u4E4B\u95F4");
          draft.relayPort = port;
        }
        if (params.upstream !== void 0) draft.upstream = str2(params.upstream).trim();
      }),
      setHeaders: async (params) => apply((draft) => {
        const lang = str2(params.acceptLanguage).trim();
        if (lang) draft.acceptLanguage = lang;
      }),
      setRule: async (params) => {
        const rule2 = params.rule;
        if (!rule2 || typeof rule2 !== "object") throw new Error("\u89C4\u5219\u683C\u5F0F\u4E0D\u6B63\u786E");
        validateRule(rule2);
        return apply((draft) => {
          const idx = draft.rules.findIndex((r) => r.id === rule2.id);
          if (idx === -1) draft.rules.push(rule2);
          else draft.rules[idx] = rule2;
        });
      },
      toggleRule: async (params) => {
        const id = str2(params.id);
        return apply((draft) => {
          const rule2 = draft.rules.find((r) => r.id === id);
          if (!rule2) throw new Error(`\u6CA1\u6709\u8FD9\u6761\u89C4\u5219\uFF1A${id}`);
          rule2.enabled = bool2(params.enabled, rule2.enabled);
        });
      },
      deleteRule: async (params) => {
        const id = str2(params.id);
        if (builtinRules().some((r) => r.id === id)) throw new Error("\u5185\u7F6E\u89C4\u5219\u4E0D\u80FD\u5220\u9664\uFF0C\u53EF\u4EE5\u5173\u6389\u5B83");
        return apply((draft) => {
          draft.rules = draft.rules.filter((r) => r.id !== id);
        });
      },
      setPacks: async (params) => apply((draft) => {
        draft.packs = {
          secrets: bool2(params.secrets, draft.packs.secrets),
          region: bool2(params.region, draft.packs.region)
        };
        const custom = draft.rules.filter((r) => !builtinRules().some((b) => b.id === r.id));
        const disabled = new Set(draft.rules.filter((r) => !r.enabled).map((r) => r.id));
        draft.rules = [...builtinRules(draft.packs).map((r) => ({ ...r, enabled: !disabled.has(r.id) })), ...custom];
      }),
      setAllowlist: async (params) => {
        const lines = Array.isArray(params.allowlist) ? params.allowlist.map((x) => String(x).trim()).filter(Boolean) : [];
        return apply((draft) => {
          draft.allowlist = lines;
        });
      },
      testRules: (params) => {
        const text = str2(params.text);
        const hits = currentRules().findHits(text);
        return {
          hits: hits.map((h) => ({ type: h.typePrefix, value: maskValue(h.plaintext) })),
          preview: desensitizeString(text, currentRules(), store)
        };
      },
      mappings: (params) => {
        const limit = typeof params.limit === "number" ? params.limit : 50;
        return { total: store.size(), entries: store.preview(limit) };
      },
      clearMappings: async () => {
        await store.clear();
        return { total: 0 };
      },
      hits: () => ({ entries: relay.recentHits() }),
      clearHits: () => {
        relay.clearHits();
        return { entries: [] };
      },
      restartRelay: async () => {
        if (relay.status().running) await relay.stop();
        await syncRelay();
        return state();
      }
    };
    for (const [method, handler] of Object.entries(handlers)) {
      const wrapped = async (params) => {
        const result = await handler(params);
        ctx.notifyChanged();
        return result;
      };
      ctx.rpc(method, wrapped);
    }
    ctx.onDispose(async () => {
      await relay.stop();
      await store.flush();
      await configFile.flush();
    });
  }
});
function buildRuleSet(cfg) {
  return new RuleSet({ rules: cfg.rules, allowlist: cfg.allowlist });
}
function maskValue(value) {
  if (value.length <= 6) return "\u2022".repeat(Math.max(3, value.length));
  return `${value.slice(0, 2)}${"\u2022".repeat(Math.min(10, value.length - 6))}${value.slice(-4)}`;
}
export {
  index_default as default
};
