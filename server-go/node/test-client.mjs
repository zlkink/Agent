// Minimal Prometheus wire-protocol client for the Go host POC.
// It speaks the same frames as apps/web/src/lib/client.ts:
//   -> {type:"subscribe", sessionId}
//   -> {type:"rpc", id, method, params}
//   <- {type:"hello"}, {type:"response", ...}, {type:"event", ...}
//
// Usage: node node/test-client.mjs [ws://127.0.0.1:17420/ws] [prompt]

import { writeFileSync } from 'node:fs';
import { join } from 'node:path';

const url = process.argv[2] || 'ws://127.0.0.1:17420/ws';
const prompt = process.argv[3] || '只回答：11+12 等于几？';

const ws = new WebSocket(url);
const pending = new Map();
let seq = 0;

const send = (obj) => ws.send(JSON.stringify(obj));
const rpc = (method, params = {}) =>
  new Promise((resolve, reject) => {
    const id = `t${++seq}`;
    pending.set(id, { resolve, reject });
    send({ type: 'rpc', id, method, params });
  });

ws.onmessage = (ev) => {
  let frame;
  try {
    frame = JSON.parse(String(ev.data));
  } catch {
    return;
  }
  if (frame.type === 'response') {
    const p = pending.get(frame.id);
    if (!p) return;
    pending.delete(frame.id);
    if (frame.ok) p.resolve(frame.data);
    else p.reject(new Error(frame.error));
    return;
  }
  if (frame.type === 'ui.request') {
    const req = frame.request || {};
    console.log(`[ui.request] ${req.method} ${req.toolName || ''}`.trim());
    if (req.method === 'permission') {
      void rpc('ui.respond', {
        sessionId: frame.sessionId,
        requestId: req.id,
        response: { value: 'allow' },
      }).catch((err) => console.error('[ui.respond]', err.message));
    } else if (req.method === 'select') {
      const value = Array.isArray(req.options) && req.options.length ? req.options[0] : 'yes';
      void rpc('ui.respond', {
        sessionId: frame.sessionId,
        requestId: req.id,
        response: { value },
      }).catch((err) => console.error('[ui.respond]', err.message));
    } else if (req.method === 'input') {
      void rpc('ui.respond', {
        sessionId: frame.sessionId,
        requestId: req.id,
        response: { value: '测试回答' },
      }).catch((err) => console.error('[ui.respond]', err.message));
    }
    return;
  }
  if (frame.type === 'ui.resolved') {
    console.log('[ui.resolved]', frame.requestId);
    return;
  }
  if (frame.type === 'event') {
    const e = frame.event || {};
    if (e.type === 'message_update' && e.delta?.type === 'text_delta') {
      process.stdout.write(e.delta.delta);
    } else if (e.type === 'message_start' && e.message?.role === 'assistant') {
      process.stdout.write('\n[assistant] ');
    } else if (e.type === 'message_end' && e.message?.role === 'assistant') {
      console.log(`\n[end] stopReason=${e.message.stopReason} usage=${JSON.stringify(e.message.usage)}${e.message.errorMessage ? '\n[errorMessage] ' + e.message.errorMessage : ''}`);
    } else if (e.type === 'tool_execution_start') {
      console.log(`\n[tool] ${e.toolName}`);
    } else if (e.type === 'agent_settled') {
      ws.close();
      process.exit(0);
    } else {
      console.log(`[event] ${e.type}`);
    }
  }
};

ws.onerror = (err) => {
  console.error('ws error', err?.message || err);
  process.exit(1);
};

ws.onopen = async () => {
  try {
    if (process.env.TOKEN) {
      send({ type: 'auth', token: process.env.TOKEN });
      await new Promise((r) => setTimeout(r, 200));
    }
    await new Promise((resolve) => {
      const timer = setTimeout(resolve, 500);
      ws.addEventListener('message', () => {
        clearTimeout(timer);
        resolve();
      }, { once: true });
    });

    const info = await rpc('host.info');
    console.log('[host.info]', JSON.stringify(info));

    const sessions = await rpc('session.list');
    console.log('[session.list]', JSON.stringify(sessions));

    for (const [method, params] of [
      ['workspace.list', {}],
      ['runtime.list', {}],
      ['runtime.models', { runtime: 'claude-code' }],
      ['model.list', {}],
      ['settings.get', {}],
      ['gateway.list', {}],
      ['plugin.list', {}],
      ['agent.checkUpdates', { refresh: false }],
    ]) {
      const data = await rpc(method, params);
      const text = JSON.stringify(data);
      console.log(`[${method}]`, text.length > 500 ? text.slice(0, 500) + '…' : text);
    }

    let session = sessions[0];
    if (process.env.RUNTIME) {
      session = await rpc('session.create', {
        cwd: process.env.CWD || undefined,
        runtime: process.env.RUNTIME,
      });
      console.log('[session.create]', JSON.stringify({ id: session.id, runtime: session.runtime, model: session.model }));
    }
    if (!session) throw new Error('no session');
    send({ type: 'subscribe', sessionId: session.id });

    const snap = await rpc('session.open', { sessionId: session.id });
    console.log('[session.open]', JSON.stringify({ id: snap.id, messages: snap.messages.length, runtime: snap.runtime }));

    if (process.env.NO_PROMPT === '1' && process.env.EXTRA !== '1') {
      console.log('[history]', JSON.stringify(snap.messages.map((m) => m.role)));
      send({ type: 'unsubscribe', sessionId: session.id });
      ws.close();
      process.exit(0);
    }

    if (process.env.EXTRA === '1') {
      if (process.env.ADD_WS) {
        console.log('[workspace.add]', JSON.stringify(await rpc('workspace.add', { path: process.env.ADD_WS })));
      }
      const kb = await rpc('kb.create', {
        name: 'POC knowledge',
        store: process.env.KB_STORE || '',
        embedding: {
          gatewayId: process.env.KB_GATEWAY || 'magpie',
          model: process.env.KB_MODEL || 'group/deepseek',
        },
      });
      console.log('[kb.stores]', JSON.stringify(await rpc('kb.stores', {})));
      await rpc('kb.ingest', { id: kb.id, title: 'Hello', text: 'Prometheus Go knowledge base works' });
      const hits = await rpc('kb.search', { id: kb.id, query: process.env.KB_QUERY || 'knowledge', topK: 3 });
      console.log('[kb]', JSON.stringify({ id: kb.id, hits }));
      console.log('[tools.list]', JSON.stringify(await rpc('tools.list', { sessionId: session.id })));
      console.log('[commands.list]', JSON.stringify(await rpc('commands.list', { sessionId: session.id })));
      console.log('[usage.stats]', JSON.stringify(await rpc('usage.stats', {})));
      console.log('[plugin.list]', JSON.stringify(await rpc('plugin.list', {})));
      if (process.env.WT_TEST === '1') {
        const created = await rpc('session.create', {
          cwd: process.env.REPO || 'C:/Users/1/Desktop/Prometheus',
          runtime: 'claude-code',
          worktree: true,
        });
        console.log('[wt.create]', JSON.stringify({ id: created.id, worktree: created.worktree }));
        writeFileSync(join(created.worktree.cwd, 'wt-test.txt'), 'hello worktree\n');
        const st = await rpc('worktree.status', { sessionId: created.id });
        console.log('[wt.status]', JSON.stringify({ ahead: st.ahead, files: st.files }));
        const diff = await rpc('worktree.diff', { sessionId: created.id, path: 'wt-test.txt' });
        console.log('[wt.diff]', JSON.stringify(diff).slice(0, 200));
        await rpc('worktree.discard', { sessionId: created.id });
        console.log('[wt.discard] ok');
      }
      if (process.env.REPO) {
        const status = await rpc('plugin.call', {
          plugin: 'git',
          method: 'status',
          params: { workspace: process.env.REPO || 'C:/Users/1/Desktop/Prometheus' },
        });
        console.log('[plugin.call git.status]', JSON.stringify(status).slice(0, 400));
      }
      const fork = await rpc('message.fork', { sessionId: session.id, ref: { index: 0, role: 'user', timestamp: 0 } });
      console.log('[message.fork]', JSON.stringify({ messages: fork.snapshot.messages.length, editorText: fork.editorText }));
      await rpc('worktree.status', { sessionId: session.id }).then(
        (v) => console.log('[worktree.status]', JSON.stringify(v)),
        (e) => console.log('[worktree.status expected]', e.message),
      );
      if (process.env.NO_PROMPT === '1') {
        send({ type: 'unsubscribe', sessionId: session.id });
        ws.close();
        process.exit(0);
      }
    }

    if (process.env.PERMISSION_MODE) {
      await rpc('permission.set', { sessionId: session.id, mode: process.env.PERMISSION_MODE });
      console.log('[permission.set]', process.env.PERMISSION_MODE);
    }

    const result = await rpc('chat.prompt', { sessionId: session.id, text: prompt });
    console.log('[chat.prompt]', JSON.stringify(result));
  } catch (err) {
    console.error('[error]', err?.message || err);
    process.exit(1);
  }
};
