/**
 * Window full screen / maximize for the desktop shell.
 *
 * macOS fills the screen's usable area instead of using a borderless "simple" full screen or a native Space:
 * - a Space survives closing the window to the tray, so Mission Control kept showing the hidden app;
 * - the borderless mode covered the menu bar strip, so the auto-hidden menu bar slid down over the window buttons
 *   and macOS dropped the style bit that makes minimizing work.
 *
 * Filling the work area leaves the menu bar and the Dock in place, which keeps the title bar exactly where a normal
 * window has it — no reserved band, no restyled bar — and leaves minimize and close native.
 *
 * The window owns the state, so it is read back from the shell after every toggle, on load and whenever the window
 * regains focus instead of being cached here.
 */
export function isMacPlatform(): boolean {
	return /mac/i.test(String(navigator.platform || navigator.userAgent));
}

type Listener = (on: boolean) => void;
const listeners = new Set<Listener>();
let fullscreen = false;

function publish(on: boolean): void {
	if (fullscreen === on) return;
	fullscreen = on;
	for (const fn of listeners) fn(on);
}

/** Whether the window fills the screen right now; the shell is the authority, this mirrors it. */
export function isPlatformFullscreen(): boolean {
	return fullscreen;
}

/** Subscribes to state changes; returns the unsubscribe function. */
export function onPlatformFullscreen(fn: Listener): () => void {
	listeners.add(fn);
	return () => listeners.delete(fn);
}

/** Re-reads the window's state from the shell; undefined outside the desktop shell or on an older build. */
async function shellState(): Promise<boolean | undefined> {
	if (!("__TAURI_INTERNALS__" in window)) return undefined;
	try {
		const { invoke } = await import("@tauri-apps/api/core");
		return (await invoke<boolean>("fullscreen_state")) === true;
	} catch {
		return undefined;
	}
}

/** Re-syncs after a reload, a tray round trip, or the system menu changed the window behind our back. */
export async function syncPlatformFullscreen(): Promise<void> {
	if (!isMacPlatform()) return;
	const on = await shellState();
	if (on !== undefined) publish(on);
}

/** Keeps the button glyph in step with the window for the whole session. */
export function installFullscreenSync(): void {
	void syncPlatformFullscreen();
	// A tray restore and the system menu both come back through a focus change.
	window.addEventListener("focus", () => void syncPlatformFullscreen());
	document.addEventListener("visibilitychange", () => {
		if (document.visibilityState === "visible") void syncPlatformFullscreen();
	});
}

/** Toggles fill-screen (macOS) or maximize, and mirrors the state the shell reports. */
export async function togglePlatformFullscreen(): Promise<void> {
	if (isMacPlatform()) {
		try {
			const { invoke } = await import("@tauri-apps/api/core");
			publish((await invoke<boolean>("toggle_fullscreen")) === true);
		} catch {
			// A failed call still has to leave the glyph describing the real window.
			await syncPlatformFullscreen();
		}
		return;
	}
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().toggleMaximize();
}

/** Minimize to the Dock; only a native Space has to be left first, which the shell does. */
export async function minimizePlatformWindow(): Promise<void> {
	if (isMacPlatform()) {
		const { invoke } = await import("@tauri-apps/api/core");
		await invoke("minimize_main_window");
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
