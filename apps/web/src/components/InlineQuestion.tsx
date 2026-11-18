import { useEffect, useState } from "react";
import { Check, ChevronRight, MessageCircleQuestion } from "lucide-react";
import { defineMessages, useT } from "../lib/i18n.ts";
import { respondUI, useStore } from "../lib/store.ts";
import { Button, Input } from "./ui.tsx";

const M = defineMessages({
	zh: {
		heading: "需要你确认",
		custom: "以上都不合适？直接输入回答",
		placeholder: "输入你的回答…",
		submit: "提交",
		cancel: "取消",
		timeout: "5 分钟未回答会自动采用第一个（推荐）选项。",
		autoSuffix: "后自动采用第一个选项",
	},
	en: {
		heading: "Waiting for you",
		custom: "None of these? Type your answer",
		placeholder: "Type your answer…",
		submit: "Submit",
		cancel: "Cancel",
		timeout: "Without an answer for 5 minutes, the first (recommended) option is used.",
		autoSuffix: "until the first option is used",
	},
});

/**
 * assistant_ask rendered inside the chat instead of a modal: it follows the
 * conversation, survives switching chats (the server sends it again in the
 * snapshot) and leaves other windows free to work.
 */
export function InlineQuestion() {
	const t = useT(M);
	const sessionId = useStore((s) => s.session?.id ?? "");
	const head = useStore((s) => s.uiQueue.find((q) => q.sessionId === sessionId && q.request.question === true));
	const [value, setValue] = useState("");
	const [now, setNow] = useState(Date.now());
	const expiresAt = head?.request.expiresAt ?? 0;
	useEffect(() => {
		if (!expiresAt) return;
		const tick = setInterval(() => setNow(Date.now()), 1000);
		return () => clearInterval(tick);
	}, [expiresAt]);
	if (!head) return null;
	const left = expiresAt ? Math.max(0, expiresAt - now) : 0;
	const countdown = `${String(Math.floor(left / 60000)).padStart(2, "0")}:${String(Math.floor((left % 60000) / 1000)).padStart(2, "0")}`;
	const { request } = head;
	const options = request.method === "select" ? request.options : [];
	const answer = (v: string) => {
		if (!v.trim()) return;
		setValue("");
		void respondUI(head.sessionId, request.id, { value: v });
	};
	return (
		<div className="chat-col mx-auto w-full px-4 pb-2 md:px-6">
			<div className="rounded-xl border border-ember-500/30 bg-ink-900/95 p-4 shadow-lg shadow-black/10">
				<div className="mb-1.5 flex items-center gap-2 font-mono text-[10.5px] tracking-widest text-ember-400 uppercase">
					<MessageCircleQuestion className="size-3.5" />
					{t("heading")}
				</div>
				<div className="mb-3 text-sm leading-relaxed whitespace-pre-wrap text-ink-200">{"title" in request ? request.title : ""}</div>
				{options.length > 0 && (
					<div className="mb-3 space-y-1.5">
						{options.map((o, i) => (
							<button
								key={o}
								type="button"
								onClick={() => answer(o)}
								className="flex w-full items-center gap-2 rounded-lg border border-ink-700 px-3 py-2 text-left text-sm text-ink-200 transition-colors hover:border-ember-500/40 hover:bg-ink-850"
							>
								<span className="grid size-5 shrink-0 place-items-center rounded border border-ink-600 font-mono text-[10px] text-ink-400">{i + 1}</span>
								<span className="min-w-0 flex-1 break-words">{o}</span>
								{i === 0 && <Check className="size-3.5 shrink-0 text-moss-400" />}
								<ChevronRight className="size-3.5 shrink-0 text-ink-600" />
							</button>
						))}
					</div>
				)}
				<form
					className="space-y-1.5"
					onSubmit={(e) => {
						e.preventDefault();
						answer(value);
					}}
				>
					<div className="text-xs text-ink-500">{t("custom")}</div>
					<div className="flex gap-2">
						<Input value={value} placeholder={t("placeholder")} onChange={(e) => setValue(e.target.value)} />
						<Button type="submit" variant="primary" disabled={!value.trim()}>
							{t("submit")}
						</Button>
					</div>
				</form>
				<div className="mt-2 flex items-center justify-between gap-2">
					<span className="text-[11px] text-ink-600">
						{expiresAt && left > 0 ? (
							<>
								<span className={left < 60000 ? "font-mono tabular-nums text-amber-400" : "font-mono tabular-nums text-ink-400"}>{countdown}</span>{" "}
								{t("autoSuffix")}
							</>
						) : (
							t("timeout")
						)}
					</span>
					<Button variant="ghost" onClick={() => void respondUI(head.sessionId, request.id, { cancelled: true })}>
						{t("cancel")}
					</Button>
				</div>
			</div>
		</div>
	);
}
