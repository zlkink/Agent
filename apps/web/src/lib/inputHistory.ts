/**
 * What was sent from the chat box, newest first, so ArrowUp can bring it back
 * (like a shell). Stored per conversation: switching chats shows that chat's
 * own history, not everything typed across the app.
 *
 * Bounded on purpose: at most 50 entries per chat and 100 chats. The in-memory
 * cache mirrors localStorage, so memory cannot grow without limit.
 */
const KEY = "prometheus.inputHistory.v2";
const LEGACY_KEY = "prometheus.inputHistory";
const MAX = 50;
const MAX_CHATS = 100;

type Entry = { items: string[]; at: number };
type Store = Record<string, Entry>;

function read(): Store {
	try {
		const raw = JSON.parse(localStorage.getItem(KEY) ?? "{}") as unknown;
		return raw && typeof raw === "object" && !Array.isArray(raw) ? (raw as Store) : {};
	} catch {
		return {};
	}
}

let store: Store | undefined;
const all = () => (store ??= read());

function legacy(): string[] {
	try {
		const raw = JSON.parse(localStorage.getItem(LEGACY_KEY) ?? "[]") as unknown;
		return Array.isArray(raw) ? raw.filter((x): x is string => typeof x === "string").slice(0, MAX) : [];
	} catch {
		return [];
	}
}

function itemsFor(key: string): string[] {
	const entry = all()[key];
	if (entry) return entry.items;
	// Before per-chat history existed everything shared one list; keep it
	// useful for the new-chat page so the first prompt can still be recalled.
	return key.startsWith("new") ? legacy() : [];
}

export function rememberInput(key: string, text: string): void {
	const value = text.trim();
	if (!value) return;
	const s = all();
	const rest = itemsFor(key).filter((x) => x !== value);
	s[key] = { items: [value, ...rest].slice(0, MAX), at: Date.now() };
	// Evict the least recently used conversations so localStorage and memory stay bounded.
	const keys = Object.keys(s);
	if (keys.length > MAX_CHATS) {
		keys.sort((a, b) => (s[a]?.at ?? 0) - (s[b]?.at ?? 0));
		for (const k of keys.slice(0, keys.length - MAX_CHATS)) delete s[k];
	}
	try {
		localStorage.setItem(KEY, JSON.stringify(s));
	} catch {
		// Storage full or blocked: the in-memory list still works for this run.
	}
}

/** The n-th most recent entry for this chat (0 = latest), or undefined past the end. */
export const inputAt = (key: string, n: number): string | undefined => itemsFor(key)[n];
