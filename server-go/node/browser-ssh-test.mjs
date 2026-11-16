// Headless click-through for the SSH settings page: opens the real web UI,
// goes to Settings → SSH servers, checks that import is opt-in (no auto-import),
// opens the import dialog with ~/.ssh/config candidates, then cancels.
// Usage: node node/browser-ssh-test.mjs [appUrl]
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const APP = process.argv[2] || 'http://127.0.0.1:17420/';
const BROWSER = process.env.BROWSER || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const PORT = 9334;
const profile = mkdtempSync(join(tmpdir(), 'prom-ssh-'));
const child = spawn(BROWSER, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  '--window-size=1440,900',
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, APP,
], { stdio: 'ignore', windowsHide: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function pageTarget() {
  for (let i = 0; i < 60; i++) {
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
const clickText = (needles) => evalJS(`(() => {
  const needles = ${JSON.stringify(needles)};
  const nodes = [...document.querySelectorAll('button,[role="button"],a,[aria-label],[title]')];
  const hit = nodes.find((e) => {
    if (!e.offsetParent) return false;
    const hay = [e.textContent || '', e.getAttribute('aria-label') || '', e.getAttribute('title') || ''].join(' ');
    return needles.some((n) => hay.includes(n));
  });
  if (!hit) return null;
  hit.click();
  return ([hit.textContent || '', hit.getAttribute('aria-label') || '', hit.getAttribute('title') || ''].join(' ')).trim().slice(0, 80);
})()`);
const bodyText = () => evalJS('document.body.innerText');

await send('Runtime.enable');
await send('Page.enable');
await sleep(4000);

let text = await bodyText();
console.log('[boot]', text.slice(0, 200).replace(/\n+/g, ' | '));

const settings = await clickText(['设置', 'Settings']);
console.log('[settings]', settings);
if (!settings) {
  const buttons = await evalJS(`[...document.querySelectorAll('button')].map((e) => ({
    text: (e.textContent || '').trim().slice(0, 40),
    label: e.getAttribute('aria-label') || '', title: e.getAttribute('title') || '',
    visible: !!e.offsetParent, cls: (e.className || '').toString().slice(0, 60),
  })).filter((b) => b.text || b.label || b.title).slice(0, 25)`);
  console.log('[buttons]', JSON.stringify(buttons));
}
await sleep(1000);
const ssh = await clickText(['SSH 服务器', 'SSH servers', 'SSH']);
console.log('[ssh tab]', ssh);
await sleep(1200);

text = await bodyText();
console.log('[ssh page]', text.slice(0, 400).replace(/\n+/g, ' | '));
const hasImport = text.includes('导入服务器') || text.includes('Import servers');
const hasAdd = text.includes('添加服务器') || text.includes('Add server');
const empty = text.includes('还没有 SSH 服务器') || text.includes('No SSH servers yet');
console.log('[check] import button:', hasImport, '| add button:', hasAdd, '| empty state:', empty);
if (!hasImport || !hasAdd || !empty) {
  console.error('FAILED: SSH page is not in the expected opt-in state');
  ws.close(); child.kill();
  process.exit(1);
}

const opened = await clickText(['导入服务器', 'Import servers']);
console.log('[open import]', opened);
await sleep(1500);
text = await bodyText();
const dialog = text.includes('从 ~/.ssh/config 导入') || text.includes('Import from ~/.ssh/config');
const candidates = await evalJS(`(() => {
  const boxes = [...document.querySelectorAll('input[type="checkbox"]')];
  const labels = boxes.map((b) => (b.closest('label')?.textContent || '').trim()).filter(Boolean);
  return { total: boxes.length, checked: boxes.filter((b) => b.checked && !b.disabled).length, labels: labels.slice(0, 4) };
})()`);
console.log('[dialog]', dialog, JSON.stringify(candidates));
const imported = await evalJS(`(() => {
  const b = [...document.querySelectorAll('button')].find((e) => /导入\\s*\\d+|Import\\s+\\d+/.test(e.textContent || ''));
  return b ? b.textContent.trim() : null;
})()`);
console.log('[import button text]', imported);

if (!dialog || candidates.total < 2 || !imported) {
  console.error('FAILED: import dialog did not show ~/.ssh/config candidates');
  ws.close(); child.kill();
  process.exit(1);
}

const cancel = await clickText(['取消', 'Cancel']);
console.log('[cancel]', cancel);
await sleep(500);
text = await bodyText();
console.log('[after close]', text.includes('从 ~/.ssh/config 导入') || text.includes('Import from ~/.ssh/config') ? 'still open' : 'closed');
console.log('SSH import UI ok');

ws.close();
child.kill();
await sleep(500);
try { rmSync(profile, { recursive: true, force: true }); } catch {}
process.exit(0);
