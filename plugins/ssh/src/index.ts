import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { homedir, userInfo } from "node:os";
import { join } from "node:path";
import { Type } from "typebox";
import { createJsonStore, definePlugin } from "@prometheus/plugin-sdk";
import { formatResult, scpTransfer, type SshHost, sshExec, validateHost } from "./ssh.ts";

interface State {
	hosts: SshHost[];
}

const MASK = "••••••";
const str = (v: unknown) => (typeof v === "string" ? v.trim() : "");
const text = (value: string) => ({ content: [{ type: "text" as const, text: value }], details: {} });
/** Clients never see the stored credential, only a mask telling them one exists. */
const view = (h: SshHost) => ({ ...h, secret: h.secret ? MASK : "" });

interface ImportCandidate {
	name: string;
	host: string;
	user: string;
	port: number;
	identityFile?: string;
}

function expandHome(value: string): string {
	const home = homedir();
	return value.replace(/^~(?=$|[\\/])/, home).replace(/%d/g, home);
}

/** Parse concrete (non-wildcard) hosts from ~/.ssh/config. */
function parseSshConfig(content: string): ImportCandidate[] {
	const defaultUser = (() => {
		try {
			return userInfo().username;
		} catch {
			return process.env.USERNAME || process.env.USER || "root";
		}
	})();
	type Block = { aliases: string[]; host: string; user: string; port: number; identityFile: string };
	const blocks: Block[] = [];
	let current: Block | null = null;
	const flush = () => {
		if (current && current.aliases.length) blocks.push(current);
		current = null;
	};
	for (const raw of content.split(/\r?\n/)) {
		const line = raw.trim();
		if (!line || line.startsWith("#")) continue;
		const match = /^([A-Za-z][A-Za-z0-9]*)\s+(.*)$/.exec(line);
		if (!match) continue;
		const key = match[1].toLowerCase();
		const value = match[2].trim().replace(/^"(.*)"$/, "$1");
		if (key === "host") {
			flush();
			current = {
				aliases: value.split(/\s+/).filter((a) => a && !a.startsWith("!") && !/[*?]/.test(a) && !a.startsWith("%")),
				host: "",
				user: "",
				port: 0,
				identityFile: "",
			};
		} else if (current) {
			if (key === "hostname") current.host = value;
			else if (key === "user") current.user = value;
			else if (key === "port") current.port = Number(value) || 0;
			else if (key === "identityfile") current.identityFile = value;
		}
	}
	flush();
	const out: ImportCandidate[] = [];
	for (const block of blocks) {
		if (block.host && /%[a-z]/i.test(block.host)) continue;
		for (const name of block.aliases) {
			out.push({
				name,
				host: block.host || name,
				user: block.user || defaultUser,
				port: block.port || 22,
				identityFile: block.identityFile ? expandHome(block.identityFile) : undefined,
			});
		}
	}
	return out;
}

export default definePlugin({
	name: "ssh",
	version: "0.1.0",
	optional: true,
	setup(ctx) {
		const store = createJsonStore<State>(join(ctx.dataDir, "hosts.json"), () => ({ hosts: [] }));

		const find = async (ref: unknown): Promise<SshHost> => {
			const key = str(ref).toLowerCase();
			const { hosts } = await store.read();
			const hit = hosts.find((h) => h.id === key || h.name.toLowerCase() === key);
			if (!hit) throw new Error(`没有名为 ${str(ref)} 的 SSH 主机，请先在 设置 → SSH 中添加`);
			return hit;
		};

		ctx.rpc("list", async () => ({ hosts: (await store.read()).hosts.map(view) }));

		ctx.rpc("save", async (p) => view(await saveHost((p.host ?? {}) as Partial<SshHost>)));

		const saveHost = async (h: Partial<SshHost>): Promise<SshHost> => {
			const auth = h.auth === "password" || h.auth === "agent" ? h.auth : "key";
			const next: SshHost = {
				id: str(h.id) || randomUUID().slice(0, 8),
				name: str(h.name),
				host: str(h.host),
				port: Number(h.port) || 22,
				user: str(h.user),
				auth,
				identityFile: str(h.identityFile) || undefined,
				secret: typeof h.secret === "string" ? h.secret : undefined,
				note: str(h.note) || undefined,
			};
			if (!next.name) next.name = `${next.user}@${next.host}`;
			validateHost(next);
			await store.update((s) => {
				const old = s.hosts.find((x) => x.id === next.id);
				if (next.auth !== "password") next.secret = undefined;
				else if (!next.secret || next.secret === MASK) next.secret = old?.secret;
				if (s.hosts.some((x) => x.id !== next.id && x.name.toLowerCase() === next.name.toLowerCase())) throw new Error("名称已被使用");
				return { hosts: old ? s.hosts.map((x) => (x.id === next.id ? next : x)) : [...s.hosts, next] };
			});
			ctx.notifyChanged();
			return next;
		};

		ctx.rpc("importCandidates", async () => {
			const { hosts } = await store.read();
			const existing = new Set(hosts.map((h) => h.name.toLowerCase()));
			const file = join(homedir(), ".ssh", "config");
			let text = "";
			try {
				text = await readFile(file, "utf8");
			} catch {
				return { hosts: [], error: "找不到 ~/.ssh/config" };
			}
			const candidates = parseSshConfig(text).map((c) => ({ ...c, existing: existing.has(c.name.toLowerCase()) }));
			return { hosts: candidates };
		});

		ctx.rpc("import", async (p) => {
			const names = Array.isArray(p.names) ? p.names.filter((n): n is string => typeof n === "string") : [];
			if (!names.length) return { imported: 0, skipped: 0 };
			let text = "";
			try {
				text = await readFile(join(homedir(), ".ssh", "config"), "utf8");
			} catch {
				return { imported: 0, skipped: names.length };
			}
			const byName = new Map(parseSshConfig(text).map((c) => [c.name, c]));
			let imported = 0;
			let skipped = 0;
			for (const name of names) {
				const c = byName.get(name);
				if (!c) {
					skipped++;
					continue;
				}
				try {
					await saveHost({
						name: c.name,
						host: c.host,
						user: c.user,
						port: c.port,
						auth: c.identityFile ? "key" : "agent",
						identityFile: c.identityFile,
						note: "来自 ~/.ssh/config",
					});
					imported++;
				} catch {
					skipped++;
				}
			}
			ctx.notifyChanged();
			return { imported, skipped };
		});

		ctx.rpc("delete", async (p) => {
			const id = str(p.id);
			await store.update((s) => ({ hosts: s.hosts.filter((h) => h.id !== id) }));
			ctx.notifyChanged();
			return {};
		});

		ctx.rpc("test", async (p) => {
			const h = await find(p.id);
			const started = Date.now();
			try {
				const r = await sshExec(h, "echo prometheus-ok", 20_000);
				const ok = r.code === 0 && r.stdout.includes("prometheus-ok");
				return { ok, latencyMs: Date.now() - started, message: ok ? "连接成功" : (r.stderr || r.stdout || "连接失败").trim().slice(0, 400) };
			} catch (err) {
				return { ok: false, latencyMs: Date.now() - started, message: err instanceof Error ? err.message : String(err) };
			}
		});

		ctx.tools.register({
			name: "ssh_hosts",
			label: "SSH 主机列表",
			description: "List the SSH servers the user has configured (name, host, user). Use the name with ssh_exec.",
			promptSnippet: "ssh_hosts: list configured SSH servers",
			parameters: Type.Object({}),
			async execute() {
				const { hosts } = await store.read();
				if (!hosts.length) return text("尚未配置任何 SSH 主机。");
				return text(hosts.map((h) => `${h.name}\t${h.user}@${h.host}:${h.port}${h.note ? `\t${h.note}` : ""}`).join("\n"));
			},
		});
		ctx.tools.register({
			name: "ssh_exec",
			label: "SSH 执行命令",
			description: "Run a shell command on a configured SSH server and return stdout, stderr and the exit code. Non-interactive only (no TTY, no stdin).",
			promptSnippet: "ssh_exec: run a command on a configured remote server over SSH",
			parameters: Type.Object({
				host: Type.String({ description: "Configured host name or id (see ssh_hosts)" }),
				command: Type.String({ description: "Shell command to run remotely", maxLength: 20000 }),
				timeoutSeconds: Type.Optional(Type.Number({ description: "Default 60, max 600" })),
			}),
			async execute(_id, p) {
				const h = await find(p.host);
				const secs = Math.min(600, Math.max(1, Number(p.timeoutSeconds) || 60));
				return text(formatResult(await sshExec(h, p.command, secs * 1000)));
			},
		});
		ctx.tools.register({
			name: "ssh_upload",
			label: "SSH 上传",
			description: "Copy a local file or folder to a configured SSH server with scp.",
			promptSnippet: "ssh_upload: copy a local path to a remote server",
			parameters: Type.Object({ host: Type.String(), localPath: Type.String(), remotePath: Type.String() }),
			async execute(_id, p) {
				return text(formatResult(await scpTransfer(await find(p.host), "up", p.localPath, p.remotePath)));
			},
		});
		ctx.tools.register({
			name: "ssh_download",
			label: "SSH 下载",
			description: "Copy a file or folder from a configured SSH server to the local machine with scp.",
			promptSnippet: "ssh_download: copy a remote path to the local machine",
			parameters: Type.Object({ host: Type.String(), remotePath: Type.String(), localPath: Type.String() }),
			async execute(_id, p) {
				return text(formatResult(await scpTransfer(await find(p.host), "down", p.localPath, p.remotePath)));
			},
		});
	},
});
