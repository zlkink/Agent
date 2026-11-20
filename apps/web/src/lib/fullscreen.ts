/**
 * Window full screen / maximize for the desktop shell.
 *
 * macOS uses the borderless "simple" full screen (the pre-Lion mode) instead of a native full-screen Space: a
 * Space survives closing the window to the tray, so swiping Mission Control still showed the hidden app. It covers
 * the screen without creating a Space and closes to the tray like any other window.
 *
 * Two consequences are handled here:
 * - the mode auto-hides the menu bar, which slides back down over the top edge of the window — right where the
 *   window buttons are — so the title bar reserves that band (`--fullscreen-top`) to stay clickable;
 * - the window itself owns the state, so it is read back from the shell after every toggle, on load and whenever
 *   the window regains focus instead of being cached here.
 *
 * The band height comes from `screen.availTop` (37 on a notched MacBook); `window.screenY` looks like it could
 * detect the overlap, but WebKit reports the screen height there for every window, so the shell has to say whether
 * the window is covering the screen.
 */
export function isMacPlatform(): boolean {
	return /mac/i.test(String(navigator.platform || navigator.userAgent));
}

const TOP_VAR = "--fullscreen-top";

type Listener = (on: boolean) => void;
const listeners = new Set<Listener>();
let fullscreen = false;

/** Menu bar height in CSS pixels; `availTop` is the matching top inset (37 on a notched MacBook). */
function menuBarInset(): number {
	const top = Number((window.screen as Screen & { availTop?: number }).availTop);
	if (Number.isFinite(top) && top > 0) return Math.round(top);
	// A WebView without `availTop` still has to clear the classic 24pt menu bar over the button row.
	return 24;
}

/**
 * Whether the window covers the whole screen, i.e. the menu bar is layered over its top edge.
 *
 * This is measured instead of remembered: a full-screen window is exactly as tall as the screen (a windowed or
 * zoomed one is shorter — it sits below the menu bar), so the reserved band stays correct even when the window
 * changed mode behind our back, and `resize` fires as it changes.
 */
function coversScreen(): boolean {
	const height = Number(window.screen.height);
	return Number.isFinite(height) && height > 0 && window.innerHeight >= height - 1;
}

/** Publishes the reserved band for the current window geometry. */
function applyBand(): void {
	const band = coversScreen() ? menuBarInset() : 0;
	const root = document.documentElement;
	if (band > 0) root.style.setProperty(TOP_VAR, `${band}px`);
	else root.style.removeProperty(TOP_VAR);
}

function publish(on: boolean): void {
	applyBand();
	if (fullscreen === on) return;
	fullscreen = on;
	for (const fn of listeners) fn(on);
}

/** Whether the window is full screen right now; the shell is the authority, this mirrors it. */
export function isPlatformFullscreen(): boolean {
	return fullscreen;
}

/** Subscribes to full-screen changes; returns the unsubscribe function. */
export function onPlatformFullscreen(fn: Listener): () => void {
	listeners.add(fn);
	return () => listeners.delete(fn);
}

/** Re-reads the window's mode from the shell; undefined outside the desktop shell or an older build. */
async function shellState(): Promise<boolean | undefined> {
	if (!("__TAURI_INTERNALS__" in window)) return undefined;
	try {
		const { invoke } = await import("@tauri-apps/api/core");
		return (await invoke<boolean>("simple_fullscreen_state")) === true;
	} catch {
		return undefined;
	}
}

/** Re-syncs after a reload, a tray round trip or the system menu changed the window behind our back. */
export async function syncPlatformFullscreen(): Promise<void> {
	if (!isMacPlatform()) return;
	const on = await shellState();
	if (on !== undefined) publish(on);
}

/** Keeps the reserved band and the button glyph in step with the window for the whole session. */
export function installFullscreenSync(): void {
	void syncPlatformFullscreen();
	// A tray restore and the system menu both come back through a focus change.
	window.addEventListener("focus", () => void syncPlatformFullscreen());
	// The band follows the window size, so it is right the moment the window covers the screen — even before (or
	// without) the shell reporting the mode.
	window.addEventListener("resize", applyBand);
	document.addEventListener("visibilitychange", () => {
		if (document.visibilityState === "visible") void syncPlatformFullscreen();
	});
}

/** Toggles full screen (macOS) or maximize, and mirrors the state the shell reports. */
export async function togglePlatformFullscreen(): Promise<void> {
	if (isMacPlatform()) {
		try {
			const { invoke } = await import("@tauri-apps/api/core");
			publish((await invoke<boolean>("toggle_simple_fullscreen")) === true);
		} catch {
			// A failed call still has to leave the band and the glyph describing the real window.
			await syncPlatformFullscreen();
		}
		return;
	}
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().toggleMaximize();
}

/** macOS cannot minimize the borderless full screen, so the shell leaves it first. */
export async function minimizePlatformWindow(): Promise<void> {
	if (isMacPlatform()) {
		const { invoke } = await import("@tauri-apps/api/core");
		await invoke("minimize_main_window");
		// The shell left full screen, so drop the reserved band too.
		publish(false);
		return;
	}
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().minimize();
}

/** Closes the window; with close-to-tray the shell hides it to the menu bar instead of quitting. */
export async function closePlatformWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().close();
}
