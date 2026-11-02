import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./tokens.css";
import "./styles.css";

const root = createRoot(document.getElementById("root")!);
// Select once before App mounts; navigation must never replace a live controller.
if (new URLSearchParams(window.location.search).get("shell") !== "classic") {
  void import("./openbot-shell/OpenbotShell").then(({ OpenbotShell }) => {
    root.render(<StrictMode><App shell={OpenbotShell} /></StrictMode>);
  }).catch(() => {
    root.render(<div role="alert"><p>界面未能载入，请重载页面或使用经典界面。</p><a href="/?shell=classic">打开经典界面</a></div>);
  });
} else {
  root.render(<StrictMode><App /></StrictMode>);
}
