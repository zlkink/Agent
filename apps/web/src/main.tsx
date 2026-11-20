import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App.tsx";
import { ErrorBoundary, recordError } from "./components/ErrorBoundary.tsx";
import { PanelApp } from "./components/PanelApp.tsx";
import { TrayMenuApp } from "./components/TrayMenuApp.tsx";
import { installBackHandler } from "./lib/back.ts";
import { installGlobalGuards } from "./lib/links.ts";
import "./lib/prefs.ts";
import { installKeyboardInset, installSafeArea } from "./lib/viewport.ts";
import "./styles.css";

installGlobalGuards();
installBackHandler();
installKeyboardInset();
installSafeArea();
// Keep uncaught errors (including errors thrown from event handlers, which no
// error boundary sees) so a crash can be diagnosed after a restart.
window.addEventListener("error", (e) => recordError("window error", `${e.message}\n${e.error?.stack ?? ""}`));
window.addEventListener("unhandledrejection", (e) => {
	const r = e.reason as { stack?: string; message?: string } | undefined;
	recordError("unhandled rejection", r?.stack ?? r?.message ?? String(e.reason));
});


// The desktop tray panel is a second window running this same bundle.
const isPanel = (window as { __PROMETHEUS_PANEL__?: boolean }).__PROMETHEUS_PANEL__ === true;
// ... and so is the tray's right-click menu, which draws only its card on a transparent window.
const isTrayMenu = (window as { __PROMETHEUS_TRAYMENU__?: boolean }).__PROMETHEUS_TRAYMENU__ === true;
if (isPanel || isTrayMenu) document.getElementById("splash")?.remove();
// The main window is decorated (see tauri.conf.json): AppKit draws and clips its rounded corners, so the web
// surface must not add a second radius or clear its background (see the `native-chrome` rules in styles.css).
if (!isPanel && !isTrayMenu) document.documentElement.classList.add("native-chrome");
if (isTrayMenu) {
	document.documentElement.style.background = "transparent";
	document.body.style.background = "transparent";
}

createRoot(document.getElementById("root") as HTMLElement).render(
	<StrictMode>
		<ErrorBoundary>{isTrayMenu ? <TrayMenuApp /> : isPanel ? <PanelApp /> : <App />}</ErrorBoundary>
	</StrictMode>,
);
