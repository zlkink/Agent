// Copies the built web UI into the desktop shell and applies the two desktop
// tweaks: the window title and the Tauri-only language/mode defaults.
// Usage: node patch-web.mjs <apps/web/dist> <desktop-shell/web>
import { cpSync, existsSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const [src, dest] = process.argv.slice(2);
if (!src || !dest) {
  console.error('usage: node patch-web.mjs <built-web-dir> <desktop-web-dir>');
  process.exit(2);
}
if (!existsSync(join(src, 'index.html'))) {
  console.error(`no index.html in ${src}`);
  process.exit(2);
}
rmSync(dest, { recursive: true, force: true });
cpSync(src, dest, { recursive: true });
const file = join(dest, 'index.html');
let html = readFileSync(file, 'utf8');
html = html.replace(/<title>[^<]*</title>/, '<title>RamiCore</title>');
const injection = `<script>try{if(window.__TAURI_INTERNALS__||location.hostname==="tauri.localhost"){document.documentElement.classList.add("tauri");if(String(navigator.platform).indexOf("Mac")>=0)document.documentElement.classList.add("tauri-mac");if(!localStorage.getItem("prometheus.mode"))localStorage.setItem("prometheus.mode","local");var pr={};try{pr=JSON.parse(localStorage.getItem("prometheus.prefs")||"{}")}catch(e){}if(pr.lang!=="zh"&&pr.lang!=="en"){pr.lang="zh";localStorage.setItem("helixagent.langInit","1");localStorage.setItem("prometheus.prefs",JSON.stringify(pr));}}}catch(e){}</script>`;
html = html.replace('</head>', `${injection}</head>`);
writeFileSync(file, html);
console.log(`patched ${file}`);
