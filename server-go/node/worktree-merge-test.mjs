// Worktree merge end-to-end test. Creates a temp git repo supplied by the
// caller (REPO), opens a worktree session, commits a file there, merges it back
// into the base branch, and checks the project folder sees the change.
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const repo = process.env.REPO;
const url = process.env.WS || 'ws://127.0.0.1:17420/ws';
if (!repo) throw new Error('REPO is required');

const ws = new WebSocket(url);
let seq = 0;
const pending = new Map();
const send = (o) => ws.send(JSON.stringify(o));
const rpc = (method, params = {}) =>
  new Promise((resolve, reject) => {
    const id = `m${++seq}`;
    pending.set(id, { resolve, reject });
    send({ type: 'rpc', id, method, params });
  });

ws.onmessage = (ev) => {
  const f = JSON.parse(String(ev.data));
  if (f.type !== 'response') return;
  const p = pending.get(f.id);
  if (!p) return;
  pending.delete(f.id);
  f.ok ? p.resolve(f.data) : p.reject(new Error(f.error));
};

ws.onopen = async () => {
  try {
    await new Promise((r) => setTimeout(r, 200));
    const s = await rpc('session.create', { cwd: repo, runtime: 'claude-code', worktree: true });
    console.log('[create]', JSON.stringify({ id: s.id, branch: s.worktree?.branch }));
    const wt = s.worktree.cwd;
    writeFileSync(join(wt, 'merged.txt'), 'merged from worktree\n');
    execFileSync('git', ['-C', wt, 'add', 'merged.txt']);
    execFileSync('git', ['-C', wt, 'commit', '-m', 'worktree change']);
    const st = await rpc('worktree.merge', { sessionId: s.id, strategy: 'squash', message: 'merge worktree test', resolve: 'overwrite' });
    console.log('[merge]', JSON.stringify({ ahead: st.ahead, files: st.files }));
    const text = readFileSync(join(repo, 'merged.txt'), 'utf8').trim();
    console.log('[project file]', text);
    await rpc('worktree.discard', { sessionId: s.id });
    console.log('[discard] ok');
    ws.close();
    process.exit(0);
  } catch (err) {
    console.error('[error]', err.message);
    process.exit(1);
  }
};
ws.onerror = (e) => {
  console.error('[ws]', e.message || e);
  process.exit(1);
};
