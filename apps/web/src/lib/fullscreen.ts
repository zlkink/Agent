/**
 * Window chrome for the desktop shell.
 *
 * macOS gets the system window buttons (`titleBarStyle: "Overlay"` in tauri.conf.json): the traffic lights float over
 * the title bar, so the app draws no close/minimize/maximize buttons there and none can end up under the menu bar.
 * That is also what makes the immersive full screen work — AppKit moves the traffic lights into its own auto-hiding
 * full-screen title bar, so the native controls stay usable while the app has nothing in that strip.
 *
 * Windows and Linux have no such thing, so the title bar keeps its own buttons there.
 */
export function isMacPlatform(): boolean {
	return /mac/i.test(String(navigator.platform || navigator.userAgent));
}

/** True when the platform supplies the window buttons itself, so the title bar must not draw any. */
export function hasNativeWindowControls(): boolean {
	return isMacPlatform();
}

/** Where the title bar's own controls start: right of the traffic lights, or their place when they are away. */
const CHROME_INSET_WINDOWED = "84px";
const CHROME_INSET_FULLSCREEN = "18px";

function applyChromeInset(fullscreen: boolean): void {
	document.documentElement.style.setProperty("--chrome-inset", fullscreen ? CHROME_INSET_FULLSCREEN : CHROME_INSET_WINDOWED);
}

async function syncChromeInset(): Promise<void> {
	if (!isMacPlatform() || !("__TAURI_INTERNALS__" in window)) return;
	try {
		const { invoke } = await import("@tauri-apps/api/core");
		applyChromeInset((await invoke<boolean>("window_fullscreen")) === true);
	} catch {
		// Not the desktop shell, or an older build: the stylesheet default (clearing the lights) still applies.
	}
}

/**
 * Keeps `--chrome-inset` in step with the window.
 *
 * A full-screen Space hides the traffic lights (they only slide over the content on hover), so the title bar's
 * controls take their place instead of leaving a gap — the same thing the system does for its own title bar.
 */
export function installWindowChrome(): void {
	if (!isMacPlatform()) return;
	void syncChromeInset();
	// Entering and leaving full screen both resize the window; a tray restore comes back through a focus change.
	window.addEventListener("resize", () => void syncChromeInset());
	window.addEventListener("focus", () => void syncChromeInset());
	document.addEventListener("visibilitychange", () => {
		if (document.visibilityState === "visible") void syncChromeInset();
	});
}

export async function minimizeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().minimize();
}

export async function toggleMaximizeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().toggleMaximize();
}

/** Closes the window; with close-to-tray the shell hides it to the menu bar instead of quitting. */
export async function closeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().close();
}
