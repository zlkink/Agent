/**
 * Window buttons for the desktop shell.
 *
 * macOS uses a normal window title bar (`tauri.macos.conf.json`), so the system draws the traffic lights there and the
 * app draws none — nothing in the app sits near them. Windows and Linux keep the frameless window and draw the
 * buttons themselves, see WindowControls.
 */
export function isMacPlatform(): boolean {
	return /mac/i.test(String(navigator.platform || navigator.userAgent));
}

export async function minimizeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().minimize();
}

export async function toggleMaximizeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().toggleMaximize();
}

export async function closeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().close();
}
