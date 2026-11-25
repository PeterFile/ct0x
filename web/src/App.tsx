import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ComponentType } from "react";
import type { Group, Member } from "./api";
import type { AppShellProps, ShellContext, ShellPage } from "./openbot-shell/types";
import { AgentRunLogPage } from "./AgentRunLogPage";
import { Discussion } from "./Discussion";
import type { RecipientSelection } from "./Discussion";
import { Groups } from "./Groups";
import { HistorySearch } from "./HistorySearch";
import { Members } from "./Members";
import { PendingDecisions } from "./PendingDecisions";
import { PendingReviews } from "./PendingReviews";
import { Tasks } from "./Tasks";
import { usePendingReminder } from "./usePendingReminder";
import "./im-shell.css";

type Context = ShellContext;
const pendingReviewsId = "im-pending-reviews";
const agentRunLogId = "im-agent-run-log";
function useMobile() {
  const [mobile, setMobile] = useState(() => window.matchMedia?.("(max-width: 56rem)").matches ?? false);
  useEffect(() => {
    const query = window.matchMedia?.("(max-width: 56rem)");
    if (!query) return;
    const update = () => setMobile(query.matches);
    update(); query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  }, []);
  return mobile;
}

export function App({ shell: Shell }: { shell?: ComponentType<AppShellProps> } = {}) {
  const mobile = useMobile();
  const [pane, setPane] = useState<"list" | "chat">("list");
  const [context, setContext] = useState<Context | null>(null);
  const [globalPage, setGlobalPage] = useState<string | null>(null);
  const [visitedPages, setVisitedPages] = useState<string[]>([]);
  const [tasksVisited, setTasksVisited] = useState(false);
  const [historyVisited, setHistoryVisited] = useState(false);
  const [viewVersion, setViewVersion] = useState(0);
  const [selected, setSelected] = useState<Group | null>(null);
  const [members, setMembers] = useState<Record<string, Member[]>>({});
  const [recipientSelection, setRecipientSelection] = useState<RecipientSelection>();
  const groups = useRef<HTMLElement>(null), groupsHeading = useRef<HTMLHeadingElement>(null);
  const chatHeading = useRef<HTMLHeadingElement>(null), contextHeading = useRef<HTMLHeadingElement>(null);
  const membersTrigger = useRef<HTMLButtonElement>(null), tasksTrigger = useRef<HTMLButtonElement>(null), historyTrigger = useRef<HTMLButtonElement>(null);
  const focusNext = useRef<"list" | "chat" | "restore-chat" | "context" | Context | null>(null);
  const lastChatFocus = useRef<{ groupId?: string; target: HTMLElement } | null>(null);
  const listVisible = !globalPage && (!mobile || pane === "list");
  const hasContext = context !== null && (!mobile || pane === "chat");
  const contextVisible = !globalPage && hasContext;
  const inChatPane = !mobile || (pane === "chat" && !hasContext);
  const chatVisible = !globalPage && inChatPane;
  const contextLabel = context === "history" ? "搜索" : context === "tasks" ? "任务" : "成员";
  const rememberMembers = useCallback((groupId: string, items: Member[]) => {
    setMembers(previous => ({ ...previous, [groupId]: items }));
  }, []);

  useLayoutEffect(() => {
    const requested = focusNext.current;
    focusNext.current = null;
    if (globalPage) {
      const target = document.getElementById(globalPage);
      if (target && !target.contains(document.activeElement)) target.focus({ preventScroll: true });
      return;
    }
    if (!requested && !document.activeElement?.closest("[hidden]")) return;
    const destination = requested ?? (contextVisible ? "context" : chatVisible ? "chat" : "list");
    if (destination === "restore-chat") {
      const saved = lastChatFocus.current;
      if (saved && saved.groupId === selected?.id && saved.target.isConnected && !saved.target.closest("[hidden], [inert]")) {
        saved.target.focus({ preventScroll: true });
        if (document.activeElement === saved.target) return;
      }
      chatHeading.current?.focus({ preventScroll: true });
      return;
    }
    const target = destination === "list"
      ? groups.current?.querySelector<HTMLElement>('.group-list button[aria-pressed="true"]') ?? groupsHeading.current
      : destination === "context" ? contextHeading.current : destination === "chat" ? chatHeading.current
      : destination === "members" ? membersTrigger.current : destination === "tasks" ? tasksTrigger.current : historyTrigger.current;
    target?.focus();
  }, [mobile, listVisible, chatVisible, contextVisible, context, viewVersion, globalPage, selected?.id]);

  function selectGroup(group: Group) {
    focusNext.current = "chat";
    setSelected(group); setPane("chat");
    if (mobile) setContext(null);
    setViewVersion(previous => previous + 1);
  }
  function closeContext() {
    focusNext.current = context;
    setContext(null); setViewVersion(previous => previous + 1);
  }
  function openContext(next: Context) {
    if (!globalPage && context === next) { closeContext(); return; }
    setGlobalPage(null);
    focusNext.current = "context";
    if (next === "tasks") setTasksVisited(true);
    if (next === "history") setHistoryVisited(true);
    if (Shell) setPane("chat");
    setContext(next); setViewVersion(previous => previous + 1);
  }
  function selectRecipient(groupId: string, memberId: string) {
    if (groupId !== selected?.id) return;
    setRecipientSelection(previous => ({ groupId, memberId, version: (previous?.version ?? 0) + 1 }));
    showChat();
  }
  function openGlobalPage(id: string) {
    setVisitedPages(previous => previous.includes(id) ? previous : [...previous, id]);
    setGlobalPage(id); setViewVersion(previous => previous + 1);
  }
  function showChat() {
    focusNext.current = globalPage ? "restore-chat" : "chat";
    setGlobalPage(null); setPane("chat"); setContext(null); setViewVersion(previous => previous + 1);
  }
  function backToList() {
    focusNext.current = "list";
    setGlobalPage(null); setPane("list"); setContext(null); setViewVersion(previous => previous + 1);
  }
  const inboxActive = globalPage === pendingReviewsId;
  const { pending, onSaved } = usePendingReminder(Boolean(Shell), !inboxActive);
  const globalPages: ShellPage[] = [
    { id: pendingReviewsId, label: "待我决定",
      reminder: pending ? "有待处理事项" : "",
      content: visitedPages.includes(pendingReviewsId) ? <>
        <PendingReviews active={inboxActive} onSaved={onSaved} />
        <PendingDecisions active={inboxActive} onSaved={onSaved} />
      </> : null },
    { id: agentRunLogId, label: "Agent 运行日志",
      content: visitedPages.includes(agentRunLogId) ? <AgentRunLogPage active={globalPage === agentRunLogId} /> : null },
  ];
  const skipId = globalPage ?? (contextVisible && mobile ? "im-context" : chatVisible ? "im-chat" : "im-groups");
  const skipLabel = globalPages.find(page => page.id === globalPage)?.label
    ?? (contextVisible && mobile ? `${contextLabel}详情` : chatVisible ? "讨论区" : "群组列表");

  const groupsProps = { id: "im-groups", ref: groups, hidden: !listVisible, tabIndex: -1, "aria-labelledby": "groups-heading" };
  const slots = {
    skip: <a className="skip-link" href={`#${skipId}`}>跳至{skipLabel}</a>,
    groups: <>
        <header className="im-brand" role="banner">
          <span className="brand-mark" aria-hidden="true" /><h1>ct0x</h1>
        </header>
        <h2 className="im-list-heading" id="groups-heading" ref={groupsHeading} tabIndex={-1}>群组</h2>
        <div className="im-group-body">
          <Groups selectedId={selected?.id} onSelect={selectGroup} active={listVisible} selectionVersion={`${mobile}:${viewVersion}`} />
        </div>
      </>,

    chat: <main className="im-chat" id="im-chat" hidden={!chatVisible} tabIndex={-1} aria-label="讨论"
      onFocusCapture={event => {
        if (chatVisible && event.target instanceof HTMLElement) lastChatFocus.current = { groupId: selected?.id, target: event.target };
      }}>
        <div className="im-chat-header">
          <button type="button" hidden={!mobile} onClick={backToList}>返回群组</button>
          <div className="im-chat-summary" role={selected ? "region" : undefined}
            aria-label={selected ? "群组简介" : undefined} tabIndex={selected ? 0 : undefined}>
            <h2 ref={chatHeading} tabIndex={-1} title={selected?.name}>{selected?.name ?? "讨论"}</h2>
            {selected ? <p>{selected.description || "此群组尚无说明。"}</p> : null}
          </div>
          {!Shell ? <div className="im-chat-actions">
            <button type="button" ref={historyTrigger} aria-expanded={contextVisible && context === "history"}
              aria-controls="im-history" onClick={() => openContext("history")}>搜索</button>
            <button type="button" ref={membersTrigger} aria-expanded={contextVisible && context === "members"}
              aria-controls="im-members" onClick={() => openContext("members")}>成员</button>
            <button type="button" ref={tasksTrigger} aria-expanded={contextVisible && context === "tasks"}
              aria-controls="im-tasks" onClick={() => openContext("tasks")}>任务</button>
          </div> : null}
        </div>
        {selected ? <ChatDecisionDisclosure key={selected.id} groupId={selected.id} active={chatVisible} onSaved={onSaved} /> : null}
        <div className="discussion-content">
          <Discussion groupId={inChatPane ? selected?.id : undefined} active={chatVisible}
            rosterGroupId={contextVisible && context === "members" ? selected?.id : undefined}
            members={members} recipientSelection={recipientSelection} />
        </div>
      </main>,

    context: <aside className="im-context" id="im-context" hidden={!contextVisible} tabIndex={-1} aria-labelledby="context-heading">
        <div className="im-context-header">
          <div><p title={selected?.name}>{selected?.name ?? "未选择群组"}</p>
            <h2 id="context-heading" ref={contextHeading} tabIndex={-1}>{contextLabel}</h2></div>
          <button type="button" onClick={closeContext}>{mobile ? "返回聊天" : "关闭详情"}</button>
        </div>
        {/* Visibility suspends reads, not group-scoped drafts or mutations. The roster also feeds the composer. */}
        <div className="im-context-body">
          <div id="im-members" hidden={context !== "members"}>
            <Members groupId={selected?.id} onMembers={rememberMembers} onRecipient={selectRecipient} catalogueActive={contextVisible && context === "members"} />
          </div>
          <div id="im-tasks" hidden={context !== "tasks"}>
            {tasksVisited ? <Tasks group={contextVisible && context === "tasks" ? selected : null} /> : null}
          </div>
          <div id="im-history" hidden={context !== "history"}>
            {historyVisited ? <HistorySearch group={selected} active={contextVisible && context === "history"} /> : null}
          </div>
        </div>
      </aside>,
  };
  if (Shell) return <Shell slots={slots} groupsProps={groupsProps} pane={pane} context={contextVisible ? context : null}
    globalPages={globalPages} globalPage={globalPage} onGlobalPage={openGlobalPage}
    onList={backToList} onChat={showChat} onContext={openContext}
    contextTriggers={{ members: membersTrigger, tasks: tasksTrigger, history: historyTrigger }} />;
  return <div className="app im-shell" data-context={contextVisible}>
    {slots.skip}
    <aside className="im-conversations" {...groupsProps}>{slots.groups}</aside>
    {slots.chat}
    {slots.context}
  </div>;
}

function ChatDecisionDisclosure({ groupId, active, onSaved }: { groupId: string; active: boolean; onSaved: () => void }) {
  const [open, setOpen] = useState(false), [visited, setVisited] = useState(false);
  return <details className="chat-decision-disclosure" onToggle={event => {
    if (event.target !== event.currentTarget) return;
    const expanded = event.currentTarget.open;
    setOpen(expanded);
    if (expanded) setVisited(true);
  }}>
    <summary>本群待决事项</summary>
    {visited ? <PendingDecisions groupId={groupId} active={active && open} onSaved={onSaved} /> : null}
  </details>;
}
