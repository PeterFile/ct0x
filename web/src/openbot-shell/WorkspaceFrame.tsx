// Adapted from nightly-labs/openbot ac99196, src/renderer/src/WorkspaceFrame.tsx:11-39,62-103.
// PolyForm Noncommercial 1.0.0; see /licenses/openbot/index.html and docs/openbot-shell.md.
import type { ReactNode } from "react";

interface WorkspaceFrameProps {
  left: ReactNode;
  children: ReactNode;
  before: ReactNode;
  contextOpen: boolean;
  pane: "list" | "chat";
}

/** Keep the upstream left / panes / content paint order; App supplies stable pane slots. */
export function WorkspaceFrame(props: WorkspaceFrameProps) {
  return <div className="openbot-shell app-frame" data-context={props.contextOpen} data-mobile-pane={props.pane}>
    {props.before}
    {props.left}
    <div className="app-frame-panes">
      <div className="app-frame-content">{props.children}</div>
    </div>
  </div>;
}
