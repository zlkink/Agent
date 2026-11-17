// RamiCore TypeScript plugin host.
//
// Loads bundled `definePlugin` modules with a small PluginContext and exposes
// their tools/context over HTTP so the Go tool bus can merge them with its own
// tools. This is the bridge that keeps the TypeScript plugin ecosystem alive
// while the host is Go.
import { createServer } from 'node:http';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { mkdir } from 'node:fs/promises';

import { readdir } from 'node:fs/promises';


const HOME = process.env.PROMETHEUS_HOME || join(homedir(), '.prometheus');
const BASE = process.env.PROMETHEUS_PLUGIN_DATA || join(HOME, 'go-poc', 'plugin-data');
const CWD = process.env.PROMETHEUS_CWD || process.cwd();
const GATEWAYS = JSON.parse(process.env.PROMETHEUS_GATEWAYS || '[]');
let WORKSPACES = [{ id: 'workspace', path: CWD, name: 'workspace' }];

const tools = new Map();
const providers = [];
const rpcs = new Map();
const routes = [];
const panels = [];
const plugins = [];
const knowledgeStores = new Map();
const openStores = new Map();

for (const file of (await readdir(join(import.meta.dirname, 'dist'))).filter((f) => f.endsWith('.mjs'))) {
  const name = file.slice(0, -4);
  try {
    const mod = await import(`./dist/${file}`);
    plugins.push([name, mod.default ?? mod]);
  } catch (err) {
    console.error(`[plugin-host] ${name} failed to load:`, err?.message || err);
  }
}

function makeContext(name) {
  const dataDir = join(BASE, name);
  return {
    dataDir,
    cwd: CWD,
    agentDir: BASE,
    gateways: () => GATEWAYS,
    workspace: () => ({ path: CWD, name: 'workspace' }),
    workspaces: () => WORKSPACES,
    onDispose(fn) {
      if (typeof fn === 'function') process.on('beforeExit', fn);
    },
    ui: {
      notify: () => {},
      confirm: async () => false,
      select: async () => undefined,
      input: async () => undefined,
      editor: async () => undefined,
    },
    tools: {
      register(tool) {
        if (!tool?.name) return;
        tools.set(tool.name, { ...tool, plugin: name });
      },
    },
    context: {
      provide(fn) {
        providers.push({ plugin: name, fn });
      },
    },
    rpc(id, fn) {
      rpcs.set(`${name}.${id}`, fn);
    },
    route(method, path, handler) {
      const full = path.startsWith("/plugins/") ? path : "/plugins/" + name + (path.startsWith("/") ? path : "/" + path);
      routes.push({ plugin: name, method, path: full, handler });
    },
    panel(panel) {
      if (panel?.id) panels.push({ ...panel, plugin: name });
    },
    knowledgeStore(id, factory) {
      knowledgeStores.set(id, factory);
    },
    extension(factory) {
      const handlers = [];
      const pi = {
        on(event, fn) {
          handlers.push({ event, fn });
          return () => {};
        },
        registerTool(tool) {
          if (tool?.name) tools.set(tool.name, { ...tool, plugin: name });
        },
        registerCommand() {},
        registerProvider() {},
        registerNativeProvider() {},
        registerVirtualModel() {},
        registerMcpServer() {},
        getActiveTools: () => [...tools.keys()],
        getAllTools: () => [...tools.values()].map((t) => ({ name: t.name })),
        setActiveTools() {},
        sendMessage() {},
        setEditorText() {},
      };
      try {
        factory(pi);
      } catch (err) {
        console.error(`[${name}] extension failed:`, err?.message || err);
      }
    },
    settings: { registry: '' },
    host: 'server',
    runAgent: async () => ({ sessionId: '', status: 'error' }),
    openSession: async () => '',
    sessionExists: () => false,
    sessionAccess: () => ({ mode: 'read' }),
    sessionId: '',
    callPlugin: async () => undefined,
    hooks: {
      providerRequest: undefined,
      providerResponse: undefined,
      gatewayTraffic: () => () => {},
    },
    stream: {
      publish() {},
      watchers: () => 0,
      onWatch: () => () => {},
    },
    notifyChanged() {},
    async reloadSessions() {},
    log: {
      info: (...a) => console.error(`[${name}]`, ...a),
      warn: (...a) => console.error(`[${name}]`, ...a),
      error: (...a) => console.error(`[${name}]`, ...a),
    },
  };
}

for (const [name, mod] of plugins) {
  const plugin = mod?.default ?? mod;
  await mkdir(join(BASE, name), { recursive: true });
  try {
    await plugin.setup(makeContext(name));
  } catch (err) {
    console.error(`[plugin-host] ${name} setup failed:`, err?.message || err);
  }
}



function toolList() {
  return [...tools.values()].map((t) => ({
    name: t.name,
    description: t.description || '',
    inputSchema: t.parameters ?? { type: 'object', properties: {} },
    plugin: t.plugin,
  }));
}

async function callTool(name, args, cwd, sessionId) {
  const tool = tools.get(name);
  if (!tool) throw new Error(`unknown plugin tool: ${name}`);
  const controller = new AbortController();
  const toolCtx = {
    cwd: cwd || CWD,
    sessionId: sessionId || '',
    hasUI: false,
    signal: controller.signal,
    ui: {
      confirm: async () => false,
      select: async () => undefined,
      input: async () => undefined,
      notify: () => {},
    },
  };
  const result = await tool.execute(`call_${Date.now()}`, args ?? {}, controller.signal, () => {}, toolCtx);
  return {
    content: result?.content ?? [],
    details: result?.details ?? {},
  };
}

async function collectContext(prompt, cwd, sessionId) {
  const request = { prompt: prompt || '', cwd: cwd || CWD, sessionId: sessionId || undefined, runtime: 'pi', permissionMode: 'auto' };
  const out = { static: { sections: {}, guidelines: [] }, turn: [] };
  for (const { plugin, fn } of providers) {
    try {
      const got = await fn(request);
      if (!got) continue;
      if (got.static?.sections) Object.assign(out.static.sections, got.static.sections);
      if (Array.isArray(got.static?.guidelines)) out.static.guidelines.push(...got.static.guidelines);
      if (typeof got.turn === 'string' && got.turn.trim()) out.turn.push(got.turn);
    } catch (err) {
      console.error(`[${plugin}] context provider failed:`, err?.message || err);
    }
  }
  return out;
}

function readBody(req) {
  return new Promise((resolve) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
  });
}

const server = createServer(async (req, res) => {
  try {
    if (req.method === 'GET' && req.url === '/health') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ ok: true, tools: tools.size, plugins: plugins.map(([name]) => name) }));
      return;
    }
    if (req.method === 'GET' && req.url === '/plugins') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify(plugins.map(([name]) => ({
        id: name,
        name,
        status: 'loaded',
        source: 'builtin',
        panels: panels.filter((p) => p.plugin === name),
      }))));
      return;
    }
    if (req.method === 'POST' && req.url === '/rpc') {
      const body = JSON.parse((await readBody(req)) || '{}');
      if (Array.isArray(body.workspaces) && body.workspaces.length) WORKSPACES = body.workspaces;
      const fn = rpcs.get(`${body.plugin}.${body.method}`);
      if (!fn) {
        res.writeHead(404, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: `unknown plugin rpc: ${body.plugin}.${body.method}` }));
        return;
      }
      const result = await fn(body.params ?? {});
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ result }));
      return;
    }
    if (req.method === 'GET' && req.url === '/tools') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify(toolList()));
      return;
    }
    if (req.method === 'POST' && req.url === '/call') {
      const body = JSON.parse((await readBody(req)) || '{}');
      const result = await callTool(body.name, body.args, body.cwd, body.sessionId);
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify(result));
      return;
    }
    if (req.method === 'POST' && req.url === '/context') {
      const body = JSON.parse((await readBody(req)) || '{}');
      if (Array.isArray(body.workspaces) && body.workspaces.length) WORKSPACES = body.workspaces;
      const result = await collectContext(body.prompt, body.cwd, body.sessionId);
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify(result));
      return;
    }
    if (req.method === 'GET' && req.url === '/kb/stores') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify([...knowledgeStores.keys()]));
      return;
    }
    if (req.method === 'POST' && req.url?.startsWith('/kb/')) {
      const body = JSON.parse((await readBody(req)) || '{}');
      const action = req.url.slice(4);
      const factory = knowledgeStores.get(body.store || 'lancedb');
      if (!factory) {
        res.writeHead(404, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: `unknown knowledge store: ${body.store}` }));
        return;
      }
      const key = `${body.store}:${body.baseId}`;
      let store = openStores.get(key);
      if (!store) {
        store = await factory({
          baseId: body.baseId,
          dataDir: body.dataDir,
          dimensions: body.dimensions,
          settings: body.settings || {},
        });
        openStores.set(key, store);
      }
      let result;
      switch (action) {
        case 'upsert':
          await store.upsert(body.chunks || []);
          result = { ok: true };
          break;
        case 'search':
          result = await store.search(body.vector || [], body.topK || 6);
          break;
        case 'delete':
          await store.deleteDocument(body.documentId);
          result = { ok: true };
          break;
        case 'count':
          result = { count: await store.count() };
          break;
        case 'drop':
          await store.drop();
          openStores.delete(key);
          result = { ok: true };
          break;
        default:
          res.writeHead(404).end();
          return;
      }
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ result }));
      return;
    }
    if (req.url?.startsWith('/plugins/')) {
      const pathname = req.url.split('?')[0];
      const route = routes.find((r) => r.method === req.method && r.path === pathname);
      if (!route) {
        res.writeHead(404).end();
        return;
      }
      await route.handler(req, res);
      return;
    }
    res.writeHead(404).end();
  } catch (err) {
    res.writeHead(500, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ error: err?.message || String(err) }));
  }
});

const port = Number(process.env.PROMETHEUS_PLUGIN_PORT || 0);
server.listen(port, '127.0.0.1', () => {
  console.log(`PLUGIN_HOST_READY ${server.address().port}`);
});

function shutdown() {
  try {
    server.close(() => process.exit(0));
  } catch {
    process.exit(0);
  }
  setTimeout(() => process.exit(0), 500).unref?.();
}

process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);

// If the Go host is force-killed its children are not reaped by Windows; exit
// as soon as the parent is gone so node.exe never keeps the app folder locked.
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
