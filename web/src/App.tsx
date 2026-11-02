import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ComponentType } from "react";
import type { Group, Member } from "./api";
import type { AppShellProps, ShellContext } from "./openbot-shell/types";
import { Discussion } from "./Discussion";
import { Groups } from "./Groups";
import { HistorySearch } from "./HistorySearch";
import { Members } from "./Members";
import { Tasks } from "./Tasks";
import "./im-shell.css";

type Context = ShellContext;
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
  const [tasksVisited, setTasksVisited] = useState(false);
  const [historyVisited, setHistoryVisited] = useState(false);
  const [viewVersion, setViewVersion] = useState(0);
  const [selected, setSelected] = useState<Group | null>(null);
  const [members, setMembers] = useState<Record<string, Member[]>>({});
  const groups = useRef<HTMLElement>(null), groupsHeading = useRef<HTMLHeadingElement>(null);
  const chatHeading = useRef<HTMLHeadingElement>(null), contextHeading = useRef<HTMLHeadingElement>(null);
  const membersTrigger = useRef<HTMLButtonElement>(null), tasksTrigger = useRef<HTMLButtonElement>(null), historyTrigger = useRef<HTMLButtonElement>(null);
  const focusNext = useRef<"list" | "chat" | "context" | Context | null>(null);
  const listVisible = !mobile || pane === "list";
  const contextVisible = context !== null && (!mobile || pane === "chat");
  const chatVisible = !mobile || (pane === "chat" && !contextVisible);
  const contextLabel = context === "history" ? "搜索" : context === "tasks" ? "任务" : "成员";
  const rememberMembers = useCallback((groupId: string, items: Member[]) => {
    setMembers(previous => ({ ...previous, [groupId]: items }));
  }, []);

  useLayoutEffect(() => {
    const requested = focusNext.current;
    focusNext.current = null;
    if (!requested && !document.activeElement?.closest("[hidden]")) return;
    const destination = requested ?? (contextVisible ? "context" : chatVisible ? "chat" : "list");
    const target = destination === "list"
      ? groups.current?.querySelector<HTMLElement>('.group-list button[aria-pressed="true"]') ?? groupsHeading.current
      : destination === "context" ? contextHeading.current : destination === "chat" ? chatHeading.current
      : destination === "members" ? membersTrigger.current : destination === "tasks" ? tasksTrigger.current : historyTrigger.current;
    target?.focus();
  }, [mobile, listVisible, chatVisible, contextVisible, context, viewVersion]);

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
    if (context === next) { closeContext(); return; }
    focusNext.current = "context";
    if (next === "tasks") setTasksVisited(true);
    if (next === "history") setHistoryVisited(true);
    if (Shell) setPane("chat");
    setContext(next); setViewVersion(previous => previous + 1);
  }
  function showChat() {
    focusNext.current = "chat";
    setPane("chat"); setContext(null); setViewVersion(previous => previous + 1);
  }
  function backToList() {
    focusNext.current = "list";
    setPane("list"); setContext(null); setViewVersion(previous => previous + 1);
  }
  const skipId = contextVisible && mobile ? "im-context" : chatVisible ? "im-chat" : "im-groups";
  const skipLabel = contextVisible && mobile ? `${contextLabel}详情` : chatVisible ? "讨论区" : "群组列表";

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

    chat: <main className="im-chat" id="im-chat" hidden={!chatVisible} tabIndex={-1} aria-label="讨论">
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
        <div className="discussion-content">
          <Discussion groupId={chatVisible ? selected?.id : undefined} members={members} />
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
            <Members groupId={selected?.id} onMembers={rememberMembers} catalogueActive={contextVisible && context === "members"} />
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
    onList={backToList} onChat={showChat} onContext={openContext}
    contextTriggers={{ members: membersTrigger, tasks: tasksTrigger, history: historyTrigger }} />;
  return <div className="app im-shell" data-context={contextVisible}>
    {slots.skip}
    <aside className="im-conversations" {...groupsProps}>{slots.groups}</aside>
    {slots.chat}
    {slots.context}
  </div>;
}
