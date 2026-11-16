// Prometheus Go host — Node agent-host sidecar.
//
// It owns the official Claude Agent SDK (the supported Claude integration
// surface) and translates SDK messages into a small neutral event protocol:
//
//   {"method":"event","params":{"type":"text_delta","text":"..."}}
//   {"method":"event","params":{"type":"done","result":"...","usage":{...}}}
//
// Go only sees those neutral events, so SDK upgrades stay inside this file.

import { pathToFileURL } from 'node:url';
import { randomUUID } from 'node:crypto';
import readline from 'node:readline';

const out = (obj) => process.stdout.write(JSON.stringify(obj) + '\n');
const emit = (params) => out({ method: 'event', params });
const log = (text) => {
  const t = String(text ?? '').trimEnd();
  if (t) emit({ type: 'log', message: t });
};

const sdkPath = process.env.CLAUDE_SDK_PATH;
if (!sdkPath) {
  log('CLAUDE_SDK_PATH is empty');
  process.exit(2);
}

let sdk;
try {
  sdk = await import(pathToFileURL(sdkPath).href);
} catch (err) {
  log(`cannot import Claude Agent SDK at ${sdkPath}: ${err?.message || err}`);
  process.exit(2);
}
const { query } = sdk;

const cwd = process.env.PROMETHEUS_CWD || process.cwd();
const model = process.env.PROMETHEUS_MODEL || '';
const gateway = process.env.PROMETHEUS_GATEWAY === '1';
const executable = process.env.CLAUDE_EXECUTABLE || '';
const READ_TOOLS = ['Read', 'Grep', 'Glob', 'WebFetch', 'WebSearch'];
const allowWrite = process.env.PROMETHEUS_ALLOW_WRITE === '1';
const TOOLS = allowWrite ? [...READ_TOOLS, 'Bash', 'Edit', 'MultiEdit', 'Write'] : READ_TOOLS;
const DISALLOWED = allowWrite
  ? ['Task', 'Agent', 'ExitPlanMode', 'NotebookEdit']
  : ['Bash', 'Edit', 'MultiEdit', 'Write', 'NotebookEdit', 'Task', 'Agent', 'ExitPlanMode'];
const mcpUrl = process.env.PROMETHEUS_MCP_URL || '';
const mcpToken = process.env.PROMETHEUS_MCP_TOKEN || '';
const staticContext = process.env.PROMETHEUS_STATIC_CONTEXT || '';
let turnContext = '';
const mcpServers = {};
if (mcpUrl) {
  mcpServers.prometheus = {
    type: 'http',
    url: mcpUrl,
    headers: { Authorization: `Bearer ${mcpToken}` },
    alwaysLoad: true,
  };
}

let q = null;
let pendingModel = '';
let finished = false;
let busy = false;
let sawTextDelta = false;
let wake = null;
const queue = [];
const seenTools = new Set();
const pendingPermissions = new Map();

function askPermission(toolName, input) {
  const id = randomUUID();
  emit({ type: 'permission_request', id, tool: toolName, input });
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      pendingPermissions.delete(id);
      resolve({ behavior: 'deny', message: 'permission timed out' });
    }, 120_000);
    pendingPermissions.set(id, (allow) => {
      clearTimeout(timer);
      resolve(
        allow
          ? { behavior: 'allow', updatedInput: input }
          : { behavior: 'deny', message: 'denied by the user' },
      );
    });
  });
}

function loop() {
  if (!wake) return;
  const w = wake;
  wake = null;
  w();
}

async function* input() {
  while (!finished) {
    if (queue.length) {
      yield queue.shift();
      continue;
    }
    await new Promise((resolve) => {
      wake = resolve;
    });
  }
}

function baseEnv() {
  const env = { ...process.env };
  env.CLAUDE_AGENT_SDK_CLIENT_APP = 'prometheus-go';
  env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = '1';
  const noProxy = [env.NO_PROXY ?? env.no_proxy ?? '', '127.0.0.1', 'localhost', '::1']
    .filter(Boolean)
    .join(',');
  env.NO_PROXY = noProxy;
  env.no_proxy = noProxy;
  if (gateway) {
    env.ANTHROPIC_BASE_URL = process.env.ANTHROPIC_BASE_URL;
    env.ANTHROPIC_AUTH_TOKEN = process.env.ANTHROPIC_AUTH_TOKEN;
    env.ANTHROPIC_MODEL = model;
    env.ANTHROPIC_SMALL_FAST_MODEL = model;
    delete env.ANTHROPIC_API_KEY;
  }
  return env;
}

function ensureQuery() {
  if (q) return;
  const options = {
    cwd,
    includePartialMessages: true,
    permissionMode: 'default',
    tools: TOOLS,
    disallowedTools: DISALLOWED,
    strictMcpConfig: true,
    mcpServers,
    systemPrompt: {
      type: 'preset',
      preset: 'claude_code',
      append:
        'You are running inside the Prometheus Go host proof-of-concept. Keep answers short.' +
        (staticContext ? `\n\n${staticContext}` : ''),
    },
    hooks: {
      UserPromptSubmit: [
        {
          hooks: [
            async () =>
              turnContext
                ? { hookSpecificOutput: { hookEventName: 'UserPromptSubmit', additionalContext: turnContext } }
                : {},
          ],
        },
      ],
    },
    env: baseEnv(),
    canUseTool: async (toolName, input) => askPermission(toolName, input),
    stderr: (data) => log(data),
  };
  if (pendingModel || model) options.model = pendingModel || model;
  if (executable) options.pathToClaudeCodeExecutable = executable;
  if (gateway) options.settingSources = ['project', 'local'];

  q = query({ prompt: input(), options });
  (async () => {
    try {
      for await (const msg of q) await handle(msg);
    } catch (err) {
      emit({ type: 'error', message: err?.message || String(err) });
    } finally {
      busy = false;
    }
  })();
}

function toolStart(id, name) {
  if (!id || seenTools.has(id)) return;
  seenTools.add(id);
  emit({ type: 'tool_start', id, name });
}

async function handle(msg) {
  if (!msg || typeof msg !== 'object') return;
  switch (msg.type) {
    case 'system':
      if (msg.subtype === 'init') {
        emit({ type: 'init', model: msg.model, sessionId: msg.session_id });
      }
      return;

    case 'stream_event': {
      const event = msg.event || {};
      if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
        toolStart(event.content_block.id, event.content_block.name);
        return;
      }
      if (event.type === 'content_block_delta' && event.delta) {
        if (event.delta.type === 'text_delta' && event.delta.text) {
          sawTextDelta = true;
          emit({ type: 'text_delta', text: event.delta.text });
        } else if (event.delta.type === 'thinking_delta' && event.delta.thinking) {
          emit({ type: 'thinking_delta', text: event.delta.thinking });
        }
        return;
      }
      return;
    }

    case 'assistant': {
      const content = msg.message?.content;
      if (!Array.isArray(content)) return;
      for (const block of content) {
        if (block?.type === 'text' && block.text && !sawTextDelta) {
          emit({ type: 'text_delta', text: block.text });
        } else if (block?.type === 'tool_use') {
          toolStart(block.id, block.name);
        }
      }
      return;
    }

    case 'user': {
      const content = msg.message?.content;
      if (!Array.isArray(content)) return;
      for (const block of content) {
        if (block?.type === 'tool_result') {
          emit({ type: 'tool_result', id: block.tool_use_id, isError: !!block.is_error });
        }
      }
      return;
    }

    case 'result': {
      const isError = !!msg.is_error || (!!msg.subtype && msg.subtype !== 'success');
      // Claude Code knows its own system prompt, tools and window; report the
      // fill before `done` so the host has it when the turn settles.
      try {
        const u = await q?.getContextUsage?.({ detail: 'summary' });
        if (u && u.maxTokens > 0) {
          emit({ type: 'context_usage', tokens: u.totalTokens, max: u.maxTokens });
        }
      } catch {
        // Older CLIs do not answer this control request.
      }
      emit({
        type: 'done',
        result: typeof msg.result === 'string' ? msg.result : '',
        isError,
        subtype: msg.subtype,
        usage: msg.usage,
      });
      busy = false;
      return;
    }

    default:
      return;
  }
}

function onPrompt(text, context, images = []) {
  if (busy) {
    emit({ type: 'error', message: 'a prompt is already running' });
    return;
  }
  busy = true;
  turnContext = context || '';
  sawTextDelta = false;
  seenTools.clear();
  ensureQuery();
  const parts = [];
  if (text) parts.push({ type: 'text', text });
  for (const img of images) {
    const data = String(img?.data ?? '');
    const mimeType = String(img?.mimeType ?? '');
    if (data && mimeType) parts.push({ type: 'image', source: { type: 'base64', media_type: mimeType, data } });
  }
  queue.push({
    type: 'user',
    message: { role: 'user', content: parts.length === 1 && parts[0].type === 'text' ? text : parts },
    parent_tool_use_id: null,
  });
  loop();
}

let shuttingDown = false;
function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;
  finished = true;
  loop();
  for (const resolve of pendingPermissions.values()) resolve(false);
  pendingPermissions.clear();
  try {
    q?.close?.();
  } catch {
    // ignore
  }
  setTimeout(() => process.exit(0), 30);
}

process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
// The Go host may be force-killed (installer, task manager); do not linger as an
// orphan holding node.exe and native modules open.
const PARENT_PID = process.ppid;
if (PARENT_PID > 0) {
  const watchdog = setInterval(() => {
    try {
      process.kill(PARENT_PID, 0);
    } catch {
      shutdown();
    }
  }, 5000);
  watchdog.unref?.();
}
process.on('uncaughtException', (err) => {
  log(`uncaught: ${err?.stack || err}`);
  process.exit(1);
});
process.on('unhandledRejection', (err) => {
  log(`unhandled rejection: ${err?.stack || err}`);
});

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on('line', (line) => {
  const raw = line.trim();
  if (!raw) return;
  let msg;
  try {
    msg = JSON.parse(raw);
  } catch {
    return;
  }
  if (msg.method === 'prompt') {
    onPrompt(String(msg.params?.text ?? ''), String(msg.params?.context ?? ''), Array.isArray(msg.params?.images) ? msg.params.images : []);
  } else if (msg.method === 'set_model') {
    const model = String(msg.params?.model ?? '');
    if (model) {
      pendingModel = model;
      q?.setModel?.(model)?.catch?.(() => {});
    }
  } else if (msg.method === 'abort') {
    q?.interrupt?.().catch(() => {});
  } else if (msg.method === 'permission') {
    const resolve = pendingPermissions.get(msg.id);
    if (resolve) {
      pendingPermissions.delete(msg.id);
      resolve(msg.decision === 'allow' || msg.decision === 'always');
    }
  } else if (msg.method === 'shutdown') {
    shutdown();
  }
});
rl.on('close', shutdown);
