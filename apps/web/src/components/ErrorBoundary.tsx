import { Component, type ErrorInfo, type ReactNode } from "react";

/**
 * Last line of defence for render errors: instead of a black window or the
 * startup splash, show what broke and offer a reload. The error is also kept in
 * localStorage so a crash can be reported after the fact.
 */
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
	state: { error: Error | null } = { error: null };

	static getDerivedStateFromError(error: Error): { error: Error } {
		return { error };
	}

	componentDidCatch(error: Error, info: ErrorInfo): void {
		recordError("React render error", `${error.stack ?? error.message}\n\n${info.componentStack ?? ""}`);
	}

	render(): ReactNode {
		const error = this.state.error;
		if (!error) return this.props.children;
		const details = `${error.message}\n\n${error.stack ?? ""}`;
		return (
			<div style={box}>
				<div style={{ maxWidth: 860, width: "100%" }}>
					<div style={{ fontSize: 18, fontWeight: 600, color: "#f2c9a0", marginBottom: 8 }}>RamiCore 界面出错 / Something broke</div>
					<div style={{ fontSize: 13, color: "#c8bdb2", marginBottom: 12 }}>对话数据没有丢失，可以复制下面的错误后重新加载。</div>
					<pre style={pre}>{details}</pre>
					<div style={{ display: "flex", gap: 8, marginTop: 12 }}>
						<button type="button" style={button} onClick={() => location.reload()}>
							重新加载 / Reload
						</button>
						<button type="button" style={button} onClick={() => void navigator.clipboard?.writeText(details)}>
							复制错误 / Copy
						</button>
						<button type="button" style={button} onClick={() => this.setState({ error: null })}>
							返回 / Back
						</button>
					</div>
				</div>
			</div>
		);
	}
}

const KEY = "ramicore.lastError";

/** Keeps the most recent uncaught error for the About page / support. */
export function recordError(where: string, details: string): void {
	try {
		localStorage.setItem(KEY, JSON.stringify({ where, details, at: new Date().toISOString(), version: __APP_VERSION__ }));
	} catch {
		// Storage can be full or disabled; the boundary still shows the error.
	}
}

const box = {
	position: "fixed" as const,
	inset: 0,
	display: "flex",
	alignItems: "center",
	justifyContent: "center",
	padding: 24,
	overflow: "auto",
	background: "#1c1814",
	color: "#e9e0d5",
	font: "13px/1.5 ui-monospace, Menlo, Consolas, monospace",
	zIndex: 99999,
};
const pre = {
	maxHeight: "50vh",
	overflow: "auto",
	margin: 0,
	padding: 12,
	borderRadius: 8,
	background: "#14110e",
	border: "1px solid #3a322b",
	color: "#e7b48a",
	whiteSpace: "pre-wrap" as const,
	wordBreak: "break-word" as const,
};
const button = {
	padding: "8px 12px",
	borderRadius: 8,
	border: "1px solid #5a4a3d",
	background: "#2a231d",
	color: "#e9e0d5",
	cursor: "pointer",
	font: "inherit",
};
