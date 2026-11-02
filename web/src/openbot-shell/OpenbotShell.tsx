import { PageRail } from "./PageRail";
import { SidebarFrame } from "./SidebarFrame";
import { WorkspaceFrame } from "./WorkspaceFrame";
import type { AppShellProps } from "./types";
import "./openbot-shell.css";

export function OpenbotShell(props: AppShellProps) {
  return <WorkspaceFrame before={props.slots.skip} pane={props.pane} contextOpen={props.context !== null}
    left={<>
      <PageRail pane={props.pane} context={props.context} onList={props.onList} onChat={props.onChat}
        onContext={props.onContext} contextTriggers={props.contextTriggers} />
      <SidebarFrame {...props.groupsProps}>{props.slots.groups}</SidebarFrame>
    </>}>
    {props.slots.chat}
    {props.slots.context}
  </WorkspaceFrame>;
}
