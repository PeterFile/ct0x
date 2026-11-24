import { PageRail } from "./PageRail";
import { SidebarFrame } from "./SidebarFrame";
import { WorkspaceFrame } from "./WorkspaceFrame";
import type { AppShellProps } from "./types";
import "./openbot-shell.css";

export function OpenbotShell(props: AppShellProps) {
  return <WorkspaceFrame before={props.slots.skip} pane={props.pane} contextOpen={props.context !== null}
    globalOpen={props.globalPage !== null} left={<>
      <PageRail {...props} />
      <SidebarFrame {...props.groupsProps}>{props.slots.groups}</SidebarFrame>
    </>}>
    {props.slots.chat}
    {props.slots.context}
    {props.globalPages.map(page => <main key={page.id} className="app-frame-global" id={page.id}
      hidden={props.globalPage !== page.id} tabIndex={-1} aria-label={page.label}>{page.content}</main>)}
  </WorkspaceFrame>;
}
