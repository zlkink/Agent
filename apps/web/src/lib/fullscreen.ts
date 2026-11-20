/**
 * Toggles the platform's full screen.
 *
 * macOS uses simple full screen (the pre-Lion borderless mode) instead of a native full-screen Space: a Space
 * survives closing the window to the tray, so swiping Mission Control still showed the hidden app. Simple full
 * screen covers the screen without creating a Space, and closes to the tray like any other window.
 */
export function isMacPlatform(): boolean {
	return /mac/i.test(String(navigator.platform || navigator.userAgent));
}

export async function togglePlatformFullscreen(): Promise<void> {
	if (isMacPlatform()) {
		const { invoke } = await import("@tauri-apps/api/core");
		await invoke("toggle_simple_fullscreen");
		return;
	}
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().toggleMaximize();
}
