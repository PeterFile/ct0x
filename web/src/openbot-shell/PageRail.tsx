// Adapted from nightly-labs/openbot ac99196, packages/ui/src/features/window/PageRail.tsx:24-43,381-434.
// PolyForm Noncommercial 1.0.0; see /licenses/openbot/index.html and docs/openbot-shell.md.
import type { Ref } from "react";
import type { AppShellProps } from "./types";

interface RailButtonProps {
  label: string;
  text: string;
  controls: string;
  current: boolean;
  expanded?: boolean;
  buttonRef?: Ref<HTMLButtonElement>;
  onClick: () => void;
}

function RailButton(props: RailButtonProps) {
  return <button type="button" className="window-rail-button" ref={props.buttonRef}
    aria-label={props.label} title={props.label} aria-controls={props.controls}
    aria-current={props.current ? "page" : undefined} aria-expanded={props.expanded} onClick={props.onClick}>
    {props.text}
  </button>;
}

/** Only real App actions: no upstream account, host, usage, pins, or settings runtime. */
export function PageRail(props: Pick<AppShellProps, "pane" | "context" | "onList" | "onChat" | "onContext" | "contextTriggers">) {
  return <nav className="window-rail" aria-label="工作区导航">
    <div className="window-rail-group">
      <RailButton label="群组列表" text="群组" controls="im-groups" current={props.pane === "list" && !props.context} onClick={props.onList} />
      <RailButton label="聊天" text="聊天" controls="im-chat" current={props.pane === "chat" && !props.context} onClick={props.onChat} />
      <RailButton label="搜索" text="搜索" controls="im-history" current={props.context === "history"}
        expanded={props.context === "history"} buttonRef={props.contextTriggers.history} onClick={() => props.onContext("history")} />
      <RailButton label="成员" text="成员" controls="im-members" current={props.context === "members"}
        expanded={props.context === "members"} buttonRef={props.contextTriggers.members} onClick={() => props.onContext("members")} />
      <RailButton label="任务" text="任务" controls="im-tasks" current={props.context === "tasks"}
        expanded={props.context === "tasks"} buttonRef={props.contextTriggers.tasks} onClick={() => props.onContext("tasks")} />
    </div>
    <div className="window-rail-group">
      <a className="shell-license" href="/licenses/openbot/index.html" target="_blank" rel="noopener noreferrer">来源与许可</a>
    </div>
  </nav>;
}
