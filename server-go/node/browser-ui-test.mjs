// Real browser click-through: launches Edge/Chrome headless, opens the Go host
// UI, clicks a session, types a prompt and waits for the streamed answer.
// Usage: node node/browser-ui-test.mjs [appUrl]
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const APP = process.argv[2] || 'http://127.0.0.1:17420/';
const BROWSER = process.env.BROWSER ||
  'C:/Program Files/Google/Chrome/Application/chrome.exe';
const PORT = 9333;
const profile = mkdtempSync(join(tmpdir(), 'prom-ui-'));

const child = spawn(BROWSER, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, APP,
], { stdio: 'ignore', windowsHide: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function firstSession() {
  const ws = new WebSocket('ws://127.0.0.1:17420/ws');
  let id = 0;
  const pending = new Map();
  ws.onmessage = (ev) => {
    const m = JSON.parse(String(ev.data));
    if (m.type === 'hello') {
      ws.send(JSON.stringify({ type: 'rpc', id: 's1', method: 'session.list', params: {} }));
    } else if (m.type === 'response' && m.id === 's1') {
      const list = Array.isArray(m.data) ? m.data : [];
      const hit = list.find((s) => s.runtime === 'claude-code' && !s.archived) || list[0];
      id = hit?.id || '';
      ws.close();
    }
  };
  await new Promise((res) => { ws.onopen = res; ws.onerror = res; });
  await sleep(300);
  return id;
}

const sessionId = await firstSession();
console.log('[session]', sessionId || 'none');

async function pageTarget() {
  for (let i = 0; i < 50; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
      const page = list.find((t) => t.type === 'page' && t.webSocketDebuggerUrl);
      if (page) return page;
    } catch {}
    await sleep(200);
  }
  throw new Error('browser did not expose a page');
}

const target = await pageTarget();
const ws = new WebSocket(target.webSocketDebuggerUrl);
let seq = 0;
const pending = new Map();
ws.onmessage = (ev) => {
  const msg = JSON.parse(String(ev.data));
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)(msg);
    pending.delete(msg.id);
  }
};
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
const send = (method, params = {}) => new Promise((resolve) => {
  const id = ++seq;
  pending.set(id, resolve);
  ws.send(JSON.stringify({ id, method, params }));
});
const evalJS = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (r.result?.exceptionDetails) throw new Error(r.result.exceptionDetails.text || 'evaluate failed');
  return r.result?.result?.value;
};

await send('Runtime.enable');
await send('Page.enable');
await sleep(3000);
if (sessionId) {
  await evalJS(`localStorage.setItem('prometheus.lastSession', ${JSON.stringify(sessionId)}); location.reload(); 'ok'`);
  await sleep(5000);
}

let body = await evalJS('document.body.innerText');
console.log('[page]', body.slice(0, 600).replace(/\n+/g, ' | '));

const clicked = await evalJS(`(() => {
  const all = [...document.querySelectorAll('*')].filter((e) => e.offsetParent && e.textContent.trim().includes('Prometheus Go POC'));
  const leaf = all.sort((a, b) => a.children.length - b.children.length)[0];
  if (!leaf) return 'not found';
  let n = leaf;
  for (let i = 0; i < 6 && n; i++, n = n.parentElement) {
    if (n.tagName === 'BUTTON' || n.getAttribute('role') === 'button' || n.className?.toString().includes('cursor')) {
      n.click();
      return n.tagName + ':' + (n.textContent || '').trim().slice(0, 60);
    }
  }
  leaf.click();
  return 'leaf:' + leaf.tagName;
})()`);
console.log('[click]', clicked);
await sleep(1500);

const ta = await evalJS(`(() => {
  const t = document.querySelector('textarea');
  return t ? (t.placeholder || 'textarea') : 'none';
})()`);
console.log('[textarea]', ta);

const typed = await evalJS(`(() => {
  const t = document.querySelector('textarea');
  if (!t) return 'no textarea';
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
  setter.call(t, '只回答：2+2 等于几');
  t.dispatchEvent(new Event('input', { bubbles: true }));
  t.focus();
  t.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true, cancelable: true }));
  return 'sent';
})()`);
console.log('[send]', typed);

let answer = '';
for (let i = 0; i < 90; i++) {
  await sleep(1000);
  body = await evalJS('document.body.innerText');
  if (body.includes('4') && !body.includes('思考中')) {
    answer = body;
    break;
  }
}
console.log('[result]', answer ? answer.slice(-500).replace(/\n+/g, ' | ') : 'no answer within 90s');

ws.close();
child.kill();
await sleep(500);
try { rmSync(profile, { recursive: true, force: true }); } catch {}
process.exit(answer ? 0 : 1);
