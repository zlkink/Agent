import type { BrowserMode } from "./types.ts";

export interface BrowserSettings {
	/** Empty: find one automatically. */
	executablePath: string;
	mode: BrowserMode;
	/** Chrome DevTools port used by the "system" mode. */
	debugPort: number;
	/**
	 * `/devtools/browser/<uuid>` for the "system" mode, optionally as a full `ws://127.0.0.1:PORT/...` URL.
	 * Chrome 144+ serves no /json at all, so that second line of DevToolsActivePort is the only way in;
	 * pasting it covers what the file cannot: custom user-data dirs, a remote Chrome, a non-standard profile name.
	 */
	wsPath: string;
	/** `%s` is replaced by the query. */
	searchUrl: string;
	/** http://host:port or socks5://host:port; empty for direct. */
	proxy: string;
	/** Close the browser after this many idle minutes with nobody watching; 0 keeps it running. */
	idleMinutes: number;
	/** JPEG quality of the streamed view. */
	quality: number;
	maxFps: number;
	restoreTabs: boolean;
	/** Empty: the plugin's data folder. */
	downloadDir: string;
	/** Accept-Language and the browser UI language. */
	lang: string;
	/** Let the agent run JavaScript in pages (browser_eval). */
	allowEval: boolean;
	/** On a phone-sized screen show pages as a phone would. */
	mobile: boolean;
}

function defaultLang(): string {
	try {
		return Intl.DateTimeFormat().resolvedOptions().locale || "en-US";
	} catch {
		return "en-US";
	}
}

export const DEFAULT_SETTINGS: BrowserSettings = {
	executablePath: "",
	mode: "headless",
	debugPort: 9222,
	wsPath: "",
	searchUrl: "https://www.bing.com/search?q=%s",
	proxy: "",
	idleMinutes: 15,
	quality: 70,
	maxFps: 30,
	restoreTabs: true,
	downloadDir: "",
	lang: defaultLang(),
	allowEval: true,
	mobile: true,
};

const str = (v: unknown, max = 2000) => (typeof v === "string" ? v.trim().slice(0, max) : undefined);
const num = (v: unknown, lo: number, hi: number) => (typeof v === "number" && Number.isFinite(v) ? Math.round(Math.min(hi, Math.max(lo, v))) : undefined);
const bool = (v: unknown) => (typeof v === "boolean" ? v : undefined);

/**
 * The "WebSocket path" setting: what follows the port in DevToolsActivePort's second line.
 * Only the loopback host is meaningful (Chrome binds the debugging server to 127.0.0.1), so a
 * pasted `ws://127.0.0.1:PORT/devtools/browser/<uuid>` is accepted and split into port + path.
 */
export function parseWsTarget(raw: string): { port?: number; path: string } | undefined {
	const value = raw.trim();
	if (!value) return undefined;
	const m = /^(?:ws:\/\/(?:127\.0\.0\.1|localhost)(?::(\d{1,5}))?)?(\/devtools\/browser\/[A-Za-z0-9._-]+)$/.exec(value);
	if (!m) return undefined;
	const path = m[2]!;
	if (m[1] === undefined) return { path };
	const port = Number(m[1]);
	if (!Number.isInteger(port) || port < 1 || port > 65535) return undefined;
	return { port, path };
}

/** Keeps known keys with sane values; anything else from a client or config is dropped. */
export function sanitizeSettings(raw: unknown): Partial<BrowserSettings> {
	const r = (raw ?? {}) as Record<string, unknown>;
	const proxy = str(r.proxy);
	const searchUrl = str(r.searchUrl);
	const lang = str(r.lang, 35);
	const wsPath = str(r.wsPath, 300);
	const out: Partial<BrowserSettings> = {
		executablePath: str(r.executablePath),
		mode: r.mode === "headless" || r.mode === "window" || r.mode === "system" ? r.mode : undefined,
		debugPort: num(r.debugPort, 1, 65535),
		wsPath: wsPath === undefined ? undefined : wsPath === "" || parseWsTarget(wsPath) ? wsPath : undefined,
		searchUrl: searchUrl === undefined ? undefined : /^https?:\/\//i.test(searchUrl) ? searchUrl : undefined,
		proxy: proxy === undefined ? undefined : proxy === "" || /^(https?|socks[45]?):\/\/[^\s]+$/i.test(proxy) ? proxy : undefined,
		idleMinutes: num(r.idleMinutes, 0, 24 * 60),
		quality: num(r.quality, 20, 95),
		maxFps: num(r.maxFps, 5, 60),
		restoreTabs: bool(r.restoreTabs),
		downloadDir: str(r.downloadDir),
		lang: lang === undefined ? undefined : /^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/.test(lang) ? lang : undefined,
		allowEval: bool(r.allowEval),
		mobile: bool(r.mobile),
	};
	for (const k of Object.keys(out) as (keyof BrowserSettings)[]) if (out[k] === undefined) delete out[k];
	return out;
}
