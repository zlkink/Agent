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
