/**
 * macOS window chrome.
 *
 * macOS gets the system traffic lights (`tauri.macos.conf.json`), so the title bar draws no window buttons there and
 * keeps a left inset for them. A full-screen Space hides the lights — they only slide over the content on hover — and
 * the controls take their place. Windows/Linux keep their own buttons, see WindowControls.
 */
export function isMacPlatform(): boolean {
	return /mac/i.test(String(navigator.platform || navigator.userAgent));
}

const INSET_WINDOWED = "84px";
const INSET_FULLSCREEN = "18px";

let fullscreen = false;
let inset = "";

/** True while the window spans the screen, i.e. for as long as a full-screen Space is up. */
function spansScreen(): boolean {
	const width = Number(window.screen.width);
	return Number.isFinite(width) && width > 0 && window.innerWidth >= width - 1;
}

function apply(): void {
	// Zoomed spans the screen too but keeps its lights, so the shell's answer decides, not the geometry.
	const next = fullscreen && spansScreen() ? INSET_FULLSCREEN : INSET_WINDOWED;
	if (next === inset) return;
	inset = next;
	document.documentElement.style.setProperty("--chrome-inset", next);
}

async function sync(): Promise<void> {
	if (!isMacPlatform() || !("__TAURI_INTERNALS__" in window)) return;
	try {
		const { invoke } = await import("@tauri-apps/api/core");
		fullscreen = (await invoke<boolean>("window_fullscreen")) === true;
	} catch {
		return;
	}
	apply();
}

export function installWindowChrome(): void {
	if (!isMacPlatform()) return;
	apply();
	void sync();
	let settle: ReturnType<typeof setTimeout> | undefined;
	window.addEventListener("resize", () => {
		// AppKit adds the lights back at the *end* of the exit animation, after the frame has already shrunk: move
		// on the first shrink or they land on the controls. Entering is the opposite, so it waits for the settle.
		if (fullscreen && !spansScreen()) {
			fullscreen = false;
			apply();
		}
		if (settle !== undefined) clearTimeout(settle);
		settle = setTimeout(() => {
			settle = undefined;
			void sync();
		}, 150);
	});
	// A tray restore comes back through a focus change rather than a resize.
	window.addEventListener("focus", () => void sync());
	document.addEventListener("visibilitychange", () => {
		if (document.visibilityState === "visible") void sync();
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

export async function closeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().close();
}
