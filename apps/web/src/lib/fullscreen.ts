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

let fullscreen = false;
let inset = "";

/** True while the window spans the screen — so, for the whole time a full-screen Space is on screen. */
function spansScreen(): boolean {
	const width = Number(window.screen.width);
	return Number.isFinite(width) && width > 0 && window.innerWidth >= width - 1;
}

function apply(): void {
	// A zoomed window also spans the screen, but it keeps its traffic lights, so only a real full screen frees the
	// space up — hence `fullscreen && spansScreen()` rather than geometry alone. Resize fires on every frame of the
	// full-screen animation, so this only touches the style when the answer actually changes.
	const next = fullscreen && spansScreen() ? CHROME_INSET_FULLSCREEN : CHROME_INSET_WINDOWED;
	if (next === inset) return;
	inset = next;
	document.documentElement.style.setProperty("--chrome-inset", next);
}

/** Asks the shell whether a Space full screen is active (zoom is not one) and re-applies the inset. */
async function sync(): Promise<void> {
	if (!isMacPlatform() || !("__TAURI_INTERNALS__" in window)) return;
	try {
		const { invoke } = await import("@tauri-apps/api/core");
		fullscreen = (await invoke<boolean>("window_fullscreen")) === true;
	} catch {
		// Not the desktop shell, or an older build: keep the stylesheet default (clearing the lights).
		return;
	}
	apply();
}

/**
 * Keeps `--chrome-inset` in step with the window.
 *
 * A full-screen Space hides the traffic lights (they only slide over the content on hover), so the title bar's own
 * controls take their place instead of leaving a gap — the same shift the system title bar does. The two directions
 * have to happen at different moments, which is why this watches `resize` as well as asking the shell:
 *
 * - leaving: AppKit puts the lights back at the *end* of the exit animation while the frame has already been
 *   shrinking for several frames, so the controls move on the first shrink — the shell's answer arrives too late and
 *   the returning lights would land on top of them;
 * - entering: the lights only leave at the *end* of the entry animation, so the controls wait for the window to
 *   settle before moving into their place.
 */
export function installWindowChrome(): void {
	if (!isMacPlatform()) return;
	apply();
	void sync();
	let settle: ReturnType<typeof setTimeout> | undefined;
	window.addEventListener("resize", () => {
		if (fullscreen && !spansScreen()) {
			fullscreen = false;
			apply();
		}
		// One round trip per gesture instead of one per animation frame.
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

/** Closes the window; with close-to-tray the shell hides it to the menu bar instead of quitting. */
export async function closeWindow(): Promise<void> {
	const { getCurrentWindow } = await import("@tauri-apps/api/window");
	await getCurrentWindow().close();
}
