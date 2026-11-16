import clsx from "clsx";
import { confirmDialog } from "../lib/dialogs.ts";
import { Download, LoaderCircle, Plus, Terminal, Trash2, Zap } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { defineMessages, useT } from "../lib/i18n.ts";
import { onPluginChange, pluginCall, toast } from "../lib/store.ts";
import { Badge, Button, Dialog, Empty, Field, Input, PageHeader, Select } from "./ui.tsx";

interface SshHost {
	id: string;
	name: string;
	host: string;
	port: number;
	user: string;
	auth: "key" | "agent" | "password";
	identityFile?: string;
	secret?: string;
	note?: string;
}

interface ImportCandidate {
	name: string;
	host: string;
	user: string;
	port: number;
	identityFile?: string;
	existing: boolean;
}

const M = defineMessages({
	zh: {
		title: "SSH 服务器",
		subtitle: "配置可远程连接的服务器，Agent 可以用 ssh_exec / ssh_upload / ssh_download 工具操作它们",
		add: "添加服务器",
		import: "导入服务器",
		importTitle: "从 ~/.ssh/config 导入",
		importHint: "选择要导入的主机；已存在的同名主机会被跳过。",
		importEmpty: "~/.ssh/config 中没有找到可导入的主机",
		importMissing: "找不到 ~/.ssh/config",
		selectAll: "全选",
		importSelected: "导入 {n} 台",
		importDone: "已导入 {n} 台服务器",
		importSkipped: "跳过 {n} 台（已存在或不合法）",
		cancel: "取消",
		alreadyAdded: "已存在",
		emptyTitle: "还没有 SSH 服务器",
		emptyBody: "添加后，对话里可以让 Agent 在这些服务器上执行命令、上传下载文件。需要本机已安装 OpenSSH 客户端。",
		name: "名称",
		namePlaceholder: "测试服务器",
		host: "主机",
		port: "端口",
		user: "用户名",
		auth: "认证方式",
		authKey: "私钥（默认密钥或指定文件）",
		authAgent: "ssh-agent",
		authPassword: "密码",
		identityFile: "私钥文件路径",
		identityHint: "留空使用 ~/.ssh 下的默认密钥",
		secret: "密码",
		secretHint: "留空或保持掩码表示不修改。密码只保存在服务端，仅通过临时脚本交给 ssh",
		note: "备注",
		test: "测试连接",
		save: "保存",
		delete: "删除",
		close: "关闭",
		saved: "已保存 {name}",
		confirmDelete: "删除服务器「{name}」？",
		testOk: "连接成功",
		testFail: "连接失败",
		newHost: "新建服务器",
		editHost: "编辑服务器",
		hostKeyHint: "首次连接会自动信任并记住主机指纹（accept-new）",
	},
	en: {
		title: "SSH servers",
		subtitle: "Configure servers to connect to. The agent can use ssh_exec / ssh_upload / ssh_download on them",
		add: "Add server",
		import: "Import servers",
		importTitle: "Import from ~/.ssh/config",
		importHint: "Pick the hosts to import. Existing hosts with the same name are skipped.",
		importEmpty: "No importable hosts found in ~/.ssh/config",
		importMissing: "~/.ssh/config was not found",
		selectAll: "Select all",
		importSelected: "Import {n}",
		importDone: "Imported {n} servers",
		importSkipped: "Skipped {n} (already present or invalid)",
		cancel: "Cancel",
		alreadyAdded: "Already added",
		emptyTitle: "No SSH servers yet",
		emptyBody: "Once added, the agent can run commands and transfer files on these servers. Requires the OpenSSH client on this machine.",
		name: "Name",
		namePlaceholder: "Test server",
		host: "Host",
		port: "Port",
		user: "User",
		auth: "Authentication",
		authKey: "Private key (default or a file)",
		authAgent: "ssh-agent",
		authPassword: "Password",
		identityFile: "Private key path",
		identityHint: "Leave blank to use default keys in ~/.ssh",
		secret: "Password",
		secretHint: "Leave blank or masked to keep unchanged. Stored on the server only and handed to ssh through a temporary helper",
		note: "Note",
		test: "Test connection",
		save: "Save",
		delete: "Delete",
		close: "Close",
		saved: "Saved {name}",
		confirmDelete: "Delete server \"{name}\"?",
		testOk: "Connected",
		testFail: "Connection failed",
		newHost: "New server",
		editHost: "Edit server",
		hostKeyHint: "The host key is trusted and remembered on first connect (accept-new)",
	},
});

const blank = (): SshHost => ({ id: "", name: "", host: "", port: 22, user: "root", auth: "key" });

export function SshView() {
	const t = useT(M);
	const [hosts, setHosts] = useState<SshHost[]>([]);
	const [editing, setEditing] = useState<SshHost | null>(null);
	const [importing, setImporting] = useState<ImportCandidate[] | null>(null);
	const [selected, setSelected] = useState<Set<string>>(new Set());
	const [importBusy, setImportBusy] = useState(false);

	const load = useCallback(async () => {
		const r = await pluginCall<{ hosts: SshHost[] }>("ssh", "list");
		if (r) setHosts(r.hosts);
	}, []);
	useEffect(() => {
		void load();
		return onPluginChange("ssh", () => void load());
	}, [load]);

	const openImport = async () => {
		const r = await pluginCall<{ hosts: ImportCandidate[]; error?: string }>("ssh", "importCandidates");
		if (!r) return;
		if (r.error) {
			toast(t("importMissing"), "error");
			return;
		}
		if (!r.hosts.length) {
			toast(t("importEmpty"));
			return;
		}
		setImporting(r.hosts);
		setSelected(new Set(r.hosts.filter((h) => !h.existing).map((h) => h.name)));
	};

	const doImport = async () => {
		if (!selected.size) return;
		setImportBusy(true);
		const r = await pluginCall<{ imported: number; skipped: number }>("ssh", "import", { names: [...selected] });
		setImportBusy(false);
		if (!r) return;
		toast(t("importDone", { n: r.imported }));
		if (r.skipped) toast(t("importSkipped", { n: r.skipped }));
		setImporting(null);
		setSelected(new Set());
		await load();
	};

	const toggle = (name: string) => {
		setSelected((prev) => {
			const next = new Set(prev);
			if (next.has(name)) next.delete(name);
			else next.add(name);
			return next;
		});
	};

	return (
		<div className="flex min-h-0 flex-1 flex-col">
			<PageHeader
				title={t("title")}
				subtitle={t("subtitle")}
				actions={
					<>
						<Button variant="outline" onClick={() => void openImport()}>
							<Download className="size-4" /> {t("import")}
						</Button>
						<Button variant="primary" onClick={() => setEditing(blank())}>
							<Plus className="size-4" /> {t("add")}
						</Button>
					</>
				}
			/>
			<div className="flex min-h-0 flex-1">
				<div className={clsx("min-h-0 overflow-y-auto p-4 @3xl:p-6", editing ? "hidden w-96 shrink-0 border-r hairline @4xl:block" : "flex-1")}>
					{hosts.length === 0 ? (
						<Empty icon={<Terminal className="size-5" />} title={t("emptyTitle")}>
							{t("emptyBody")}
						</Empty>
					) : (
						<div className={clsx("grid gap-3", !editing && "@3xl:grid-cols-2 @5xl:grid-cols-3")}>
							{hosts.map((h) => (
								<button
									key={h.id}
									type="button"
									onClick={() => setEditing({ ...h })}
									className={clsx("animate-rise rounded-xl border bg-ink-900/60 p-4 text-left transition-colors hover:border-ink-600", editing?.id === h.id ? "border-ember-500/50" : "border-ink-800")}
								>
									<div className="font-display font-semibold">{h.name}</div>
									<div className="mt-1 truncate font-mono text-xs text-ink-500">
										{h.user}@{h.host}:{h.port}
									</div>
									<div className="mt-3 flex gap-1.5">
										<Badge tone="ember">{h.auth}</Badge>
									</div>
								</button>
							))}
						</div>
					)}
				</div>
				{editing && (
					<HostEditor
						key={editing.id || "new"}
						initial={editing}
						onClose={() => setEditing(null)}
						onSaved={(saved) => setEditing({ ...saved })}
						onChanged={async () => {
							await load();
						}}
					/>
				)}
			</div>
			{importing && (
				<Dialog title={t("importTitle")} onClose={() => setImporting(null)}>
					<p className="text-sm text-ink-400">{t("importHint")}</p>
					<div className="flex items-center justify-between">
						<label className="flex items-center gap-2 text-sm">
							<input
								type="checkbox"
								checked={selected.size > 0 && selected.size === importing.filter((h) => !h.existing).length}
								onChange={(e) =>
									setSelected(e.target.checked ? new Set(importing.filter((h) => !h.existing).map((h) => h.name)) : new Set())
								}
							/>
							{t("selectAll")}
						</label>
						<span className="font-mono text-xs text-ink-500">{selected.size}</span>
					</div>
					<div className="max-h-72 space-y-1 overflow-y-auto rounded-lg border border-ink-800 p-2">
						{importing.map((c) => (
							<label
								key={c.name}
								className={clsx(
									"flex items-center gap-3 rounded-md px-2 py-1.5 text-sm",
									c.existing ? "opacity-50" : "cursor-pointer hover:bg-ink-850",
								)}
							>
								<input type="checkbox" disabled={c.existing} checked={selected.has(c.name)} onChange={() => toggle(c.name)} />
								<span className="font-medium">{c.name}</span>
								<span className="ml-auto truncate font-mono text-xs text-ink-500">
									{c.user}@{c.host}:{c.port}
								</span>
								{c.existing && <Badge tone="muted">{t("alreadyAdded")}</Badge>}
							</label>
						))}
					</div>
					<div className="flex justify-end gap-2">
						<Button onClick={() => setImporting(null)}>{t("cancel")}</Button>
						<Button variant="primary" disabled={!selected.size || importBusy} onClick={() => void doImport()}>
							{importBusy ? <LoaderCircle className="size-4 animate-spin" /> : <Download className="size-4" />}
							{t("importSelected", { n: selected.size })}
						</Button>
					</div>
				</Dialog>
			)}
		</div>
	);
}

function HostEditor({
	initial,
	onClose,
	onSaved,
	onChanged,
}: {
	initial: SshHost;
	onClose: () => void;
	onSaved: (saved: SshHost) => void;
	onChanged: () => Promise<void>;
}) {
	const t = useT(M);
	const [h, setH] = useState<SshHost>(initial);
	const [busy, setBusy] = useState(false);
	const [test, setTest] = useState<{ ok: boolean; latencyMs: number; message: string } | null>(null);
	const isNew = !h.id;
	const patch = (p: Partial<SshHost>) => setH((x) => ({ ...x, ...p }));

	const save = async (): Promise<SshHost | undefined> => {
		setBusy(true);
		const saved = await pluginCall<SshHost>("ssh", "save", { host: h });
		setBusy(false);
		if (!saved) return undefined;
		setH(saved);
		toast(t("saved", { name: saved.name }));
		await onChanged();
		// 新建保存成功后立即切到编辑态：避免标题仍为“新建服务器”、看不到删除按钮，
		// 也避免用户以为还在新增、再次点保存时重复创建一条记录
		onSaved(saved);
		return saved;
	};

	const runTest = async () => {
		setTest(null);
		const saved = await save();
		if (!saved) return;
		setBusy(true);
		const r = await pluginCall<{ ok: boolean; latencyMs: number; message: string }>("ssh", "test", { id: saved.id });
		setBusy(false);
		if (r) setTest(r);
	};

	const remove = async () => {
		if (!(await confirmDialog(t("confirmDelete", { name: h.name })))) return;
		const ok = await pluginCall("ssh", "delete", { id: h.id });
		if (ok) {
			await onChanged();
			onClose();
		}
	};

	return (
		<div className="min-h-0 flex-1 overflow-y-auto">
			<div className="mx-auto max-w-2xl space-y-5 p-4 @3xl:p-6">
				<div className="flex items-center justify-between">
					<h2 className="font-display text-xl font-semibold">
						{isNew ? (
							t("newHost")
						) : (
							<>
								<span className="text-ink-500">{t("editHost")} · </span>
								{h.name}
							</>
						)}
					</h2>
					<Button onClick={onClose}>{t("close")}</Button>
				</div>

				<div className="grid gap-4 @xl:grid-cols-2">
					<Field label={t("name")}>
						<Input value={h.name} onChange={(e) => patch({ name: e.target.value })} placeholder={t("namePlaceholder")} />
					</Field>
					<Field label={t("user")}>
						<Input value={h.user} onChange={(e) => patch({ user: e.target.value })} className="font-mono" />
					</Field>
					<Field label={t("host")}>
						<Input value={h.host} onChange={(e) => patch({ host: e.target.value })} placeholder="host.example.com" className="font-mono" spellCheck={false} />
					</Field>
					<Field label={t("port")}>
						<Input type="number" min={1} max={65535} value={h.port} onChange={(e) => patch({ port: Number(e.target.value) || 22 })} className="font-mono" />
					</Field>
				</div>

				<Field label={t("auth")} hint={t("hostKeyHint")}>
					<Select value={h.auth} onChange={(e) => patch({ auth: e.target.value as SshHost["auth"] })}>
						<option value="key">{t("authKey")}</option>
						<option value="agent">{t("authAgent")}</option>
						<option value="password">{t("authPassword")}</option>
					</Select>
				</Field>

				{h.auth === "key" && (
					<Field label={t("identityFile")} hint={t("identityHint")}>
						<Input value={h.identityFile ?? ""} onChange={(e) => patch({ identityFile: e.target.value })} placeholder="~/.ssh/id_ed25519" className="font-mono" spellCheck={false} />
					</Field>
				)}
				{h.auth === "password" && (
					<Field label={t("secret")} hint={t("secretHint")}>
						<Input type="password" autoComplete="new-password" value={h.secret ?? ""} onChange={(e) => patch({ secret: e.target.value })} className="font-mono" />
					</Field>
				)}

				<Field label={t("note")}>
					<Input value={h.note ?? ""} onChange={(e) => patch({ note: e.target.value })} />
				</Field>

				{test && (
					<div className={clsx("rounded-lg border px-3.5 py-2.5 text-sm", test.ok ? "border-moss-400/30 bg-moss-400/8 text-moss-400" : "border-rose-400/30 bg-rose-400/8 text-rose-400")}>
						{test.ok ? t("testOk") : t("testFail")} · {test.latencyMs}ms
						{!test.ok && <div className="mt-1 font-mono text-xs break-all opacity-80">{test.message}</div>}
					</div>
				)}

				<div className="flex items-center gap-2 border-t hairline pt-5">
					{!isNew && (
						<Button variant="danger" onClick={() => void remove()}>
							<Trash2 className="size-4" /> {t("delete")}
						</Button>
					)}
					<div className="flex-1" />
					<Button variant="outline" disabled={busy || !h.host || !h.user} onClick={() => void runTest()}>
						{busy ? <LoaderCircle className="size-4 animate-spin" /> : <Zap className="size-4" />} {t("test")}
					</Button>
					<Button variant="primary" disabled={busy || !h.host || !h.user} onClick={() => void save()}>
						{t("save")}
					</Button>
				</div>
			</div>
		</div>
	);
}
