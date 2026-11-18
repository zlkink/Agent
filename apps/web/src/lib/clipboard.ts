/** Copy text; falls back to execCommand when the async Clipboard API is unavailable (insecure context, Android WebView). */
export async function copyToClipboard(text: string): Promise<void> {
	// Message text often ends with blank lines (and zero-width characters some
	// models stream). Pasting it elsewhere then adds an empty paragraph; strip
	// those at the boundary, keep everything inside the message intact.
	text = text.replace(/\r\n/g, "\n").replace(new RegExp("[\\u200B-\\u200D\\uFEFF]", "g"), "").trimEnd();
	if (typeof navigator !== "undefined" && navigator.clipboard && window.isSecureContext) {
		try {
			await navigator.clipboard.writeText(text);
			return;
		} catch {
			/* fall through to legacy path */
		}
	}
	const ta = document.createElement("textarea");
	ta.value = text;
	ta.setAttribute("readonly", "");
	ta.style.cssText = "position:fixed;top:0;left:0;opacity:0;font-size:16px";
	const prevFocus = document.activeElement as HTMLElement | null;
	document.body.appendChild(ta);
	ta.focus();
	ta.select();
	ta.setSelectionRange(0, text.length);
	let ok = false;
	try {
		ok = document.execCommand("copy");
	} finally {
		document.body.removeChild(ta);
		prevFocus?.focus?.();
	}
	if (!ok) throw new Error("Copy failed");
}
