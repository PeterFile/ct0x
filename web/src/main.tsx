import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./tokens.css";
import "./styles.css";

const root = createRoot(document.getElementById("root")!);
// Select once before App mounts; navigation must never replace a live controller.
if (new URLSearchParams(window.location.search).get("shell") === "openbot") {
  void import("./openbot-shell/OpenbotShell").then(({ OpenbotShell }) => {
    root.render(<StrictMode><App shell={OpenbotShell} /></StrictMode>);
  }).catch(() => {
    root.render(<div role="alert"><p>界面未能载入，请重载页面或返回默认界面。</p><a href="/">返回默认界面</a></div>);
  });
} else {
  root.render(<StrictMode><App /></StrictMode>);
}
