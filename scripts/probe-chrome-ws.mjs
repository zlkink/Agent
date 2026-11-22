#!/usr/bin/env node
// Diagnoses the "system" browser mode (attaching to a Chrome that is already running).
//
//   node scripts/probe-chrome-ws.mjs [timeoutSeconds] [--port 53197] [--ws ws://127.0.0.1:PORT/devtools/browser/<uuid>]
//
// Prints what DevToolsActivePort says, whether the port listens, what /json answers, and then
// actually performs the WebSocket handshake the plugin performs. Chrome 144+ serves no /json at
// all and may hold the handshake until the user approves "Allow remote debugging" in the browser,
// so a hang here is an authorization problem, not a wrong port. Its server does not check the UUID
// in the path, so --port alone is enough when the file is unreadable (macOS protects the profile).
//
// Exit codes: 0 connected · 2 handshake timed out (approval pending) · 3 no endpoint · 4 other error.
import { readFileSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { connect } from "node:net";

const args = process.argv.slice(2);
const flag = (name) => {
	const i = args.indexOf(name);
	return i >= 0 ? args[i + 1] : undefined;
};
const explicitWs = flag("--ws");
const explicitPort = Number.parseInt(flag("--port") ?? "", 10);
const timeoutArg = args.find((a) => !a.startsWith("--") && a !== explicitWs && a !== flag("--port"));
const timeoutMs = Math.max(1, Number(timeoutArg ?? 20)) * 1000;

function profileFiles() {
	const home = homedir();
	if (process.platform === "darwin") {
		const app = join(home, "Library", "Application Support");
		return ["Google/Chrome", "Google/Chrome Beta", "Google/Chrome Dev", "Google/Chrome Canary", "Microsoft Edge", "Chromium"].map((b) => join(app, b, "DevToolsActivePort"));
	}
	if (process.platform === "win32") {
		const local = process.env.LOCALAPPDATA ?? join(home, "AppData", "Local");
		return ["Google/Chrome/User Data", "Microsoft/Edge/User Data", "Chromium/User Data"].map((b) => join(local, b, "DevToolsActivePort"));
	}
	return [join(home, ".config/google-chrome/DevToolsActivePort"), join(home, ".config/chromium/DevToolsActivePort"), join(home, ".config/microsoft-edge/DevToolsActivePort")];
}

function portOpen(port, ms = 700) {
	return new Promise((resolve) => {
		const socket = connect({ host: "127.0.0.1", port });
		const done = (ok) => {
			socket.destroy();
			resolve(ok);
		};
		socket.setTimeout(ms, () => done(false));
		socket.once("connect", () => done(true));
		socket.once("error", () => done(false));
	});
}

async function jsonStatus(port, path) {
	try {
		const res = await fetch(`http://127.0.0.1:${port}${path}`, { signal: AbortSignal.timeout(1500) });
		return `HTTP ${res.status}`;
	} catch {
		return "no response";
	}
}

let wsUrl = explicitWs;
let port;
if (!wsUrl) {
	let found;
	let denied = false;
	for (const file of profileFiles()) {
		try {
			const text = readFileSync(file, "utf8");
			const [rawPort, rawPath] = text.split("\n").map((l) => l.trim()).filter(Boolean);
			const n = Number.parseInt(rawPort ?? "", 10);
			if (!Number.isInteger(n) || n <= 0 || !rawPath?.startsWith("/devtools/browser/")) continue;
			found = { file, port: n, path: rawPath, mtime: statSync(file).mtime.toISOString() };
			break;
		} catch (err) {
			if (err?.code === "EPERM" || err?.code === "EACCES") denied = true;
		}
	}
	if (!found) {
		console.log(`DevToolsActivePort: ${denied ? "exists but cannot be read (macOS protects the Chrome profile; no Full Disk Access here)" : "not found in any known profile"}`);
		const fallback = Number.isInteger(explicitPort) && explicitPort > 0 ? explicitPort : 9222;
		if (!(await portOpen(fallback))) {
			console.log("→ open chrome://inspect/#remote-debugging and tick \"Allow remote debugging\" (Chrome only writes the file then), then pass its port with --port.");
			process.exit(3);
		}
		// The 144+ server upgrades any path under /devtools/browser, so the port is all it needs.
		console.log(`→ port ${fallback} listens; trying ws://127.0.0.1:${fallback}/devtools/browser (path not checked; no need to read the file)`);
		port = fallback;
		wsUrl = `ws://127.0.0.1:${fallback}/devtools/browser`;
	}
	if (found) {
		console.log(`DevToolsActivePort: ${found.file}`);
		console.log(`  written: ${found.mtime}`);
		console.log(`  port:    ${found.port}`);
		console.log(`  path:    ${found.path}`);
		port = found.port;
		wsUrl = `ws://127.0.0.1:${found.port}${found.path}`;
	}
} else {
	console.log(`ws (explicit): ${wsUrl}`);
	try {
		port = Number.parseInt(new URL(wsUrl).port || "80", 10);
	} catch {
		console.log("→ that is not a ws:// URL");
		process.exit(3);
	}
}

console.log(`tcp 127.0.0.1:${port}: ${(await portOpen(port)) ? "LISTEN" : "closed"}`);
for (const p of ["/json/version", "/json/list"]) console.log(`${p}: ${await jsonStatus(port, p)}`);
console.log(`handshake: ${wsUrl} (timeout ${timeoutMs / 1000}s)`);

const started = Date.now();
const ws = new WebSocket(wsUrl);
const outcome = await new Promise((resolve) => {
	const timer = setTimeout(() => resolve({ kind: "timeout" }), timeoutMs);
	ws.addEventListener("open", () => {
		ws.send(JSON.stringify({ id: 1, method: "Browser.getVersion" }));
	});
	ws.addEventListener("message", (ev) => {
		try {
			const frame = JSON.parse(typeof ev.data === "string" ? ev.data : "");
			if (frame.id === 1) {
				clearTimeout(timer);
				resolve({ kind: "ok", frame });
			}
		} catch {}
	});
	ws.addEventListener("error", () => {
		clearTimeout(timer);
		resolve({ kind: "error" });
	});
	ws.addEventListener("close", (ev) => {
		clearTimeout(timer);
		resolve({ kind: "close", code: ev.code, reason: ev.reason });
	});
});
const elapsed = Date.now() - started;

if (outcome.kind === "ok") {
	console.log(`connected in ${elapsed} ms`);
	console.log(`  product: ${outcome.frame.result?.product}`);
	console.log(`  agent:   ${String(outcome.frame.result?.userAgent).slice(0, 90)}`);
	ws.send(JSON.stringify({ id: 2, method: "Target.getTargets" }));
	const targets = await new Promise((resolve) => {
		const timer = setTimeout(() => resolve(undefined), 3000);
		ws.addEventListener("message", (ev) => {
			try {
				const frame = JSON.parse(typeof ev.data === "string" ? ev.data : "");
				if (frame.id === 2) {
					clearTimeout(timer);
					resolve(frame.result?.targetInfos ?? []);
				}
			} catch {}
		});
	});
	if (targets) {
		const pages = targets.filter((t) => t.type === "page");
		console.log(`  page targets: ${pages.length}`);
		for (const t of pages.slice(0, 8)) console.log(`    - ${t.title || "(untitled)"} · ${String(t.url).slice(0, 80)}`);
	}
	ws.close();
	process.exit(0);
}

if (outcome.kind === "timeout") {
	console.log(`handshake hung for ${elapsed} ms → Chrome is waiting for approval`);
	console.log("→ keep chrome://inspect/#remote-debugging in front, click \"Allow remote debugging\" on the prompt,");
	console.log("  or untick/tick the switch to restart the debugging server (this rewrites DevToolsActivePort).");
	process.exit(2);
}

console.log(`handshake failed after ${elapsed} ms: ${outcome.kind === "close" ? `closed ${outcome.code} ${outcome.reason}` : "error"}`);
console.log("→ the port is up but not a debuggable browser: stale DevToolsActivePort, or another app took the port.");
process.exit(4);
