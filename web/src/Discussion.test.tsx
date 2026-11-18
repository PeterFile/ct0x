import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { Discussion } from "./Discussion";
import { deferred, group, json, other, page } from "./test/groups";
import { member } from "./test/members";
import { delivery, message } from "./test/messages";
const reads = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>();
const main = () => screen.getByRole("main", { name: "讨论" });
const panel = () => within(within(main()).queryByRole("form", { name: "发送消息" })?.closest(".discussion-feed") ?? main());
const content = () => panel().getByRole("textbox", { name: "消息内容" });
const change = (value: string) => fireEvent.change(content(), { target: { value } });
const submit = () => fireEvent.submit(panel().getByRole("form", { name: "发送消息" }));
const select = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
const article = (id = message.id) => panel().getByRole("article", { name: `消息 ${id}` });
const route = (value: string) => {
  const options = panel().getByText("发送选项", { selector: "summary" });
  if (!options.closest("details")!.open) fireEvent.click(options);
  fireEvent.change(panel().getByRole("combobox", { name: "接收方式" }), { target: { value } });
};
function expandDetails(record: HTMLElement) {
  const disclosure = record.querySelector("details");
  expect(disclosure).toBeInTheDocument();
  if (!disclosure!.open) fireEvent.click(disclosure!.querySelector("summary")!);
  expect(disclosure).toHaveAttribute("open");
  return within(disclosure!);
}
const refresh = (id = message.id) => fireEvent.click(expandDetails(article(id)).getByRole("button", { name: `刷新消息 ${id} 投递` }));
// Native textarea values use LF; received records may still contain CRLF.
const raw = "  <script>window.bad = true</script>\n@研究员\n原文尾部  ";
async function open() {
  render(<App />); select((await screen.findByRole("button", { name: group.name })).textContent!);
  fireEvent.click(screen.getByRole("button", { name: "成员" }));
  fireEvent.click(screen.getByRole("button", { name: "关闭详情" }));
}
beforeEach(() => {
  reads.mockReset().mockImplementation(async () => page());
  details.mockReset().mockImplementation(async () => json(message));
  writes.mockReset().mockImplementation(async (_url, init) => {
    const input = JSON.parse(String(init?.body));
    return json({ ...message, content: input.content, deliveries: input.recipient_ids?.length === 0 ? [] : [delivery] }, 201);
  });
  vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes("/messages")) return (init?.method === "POST" ? writes : /\/messages\//.test(url) ? details : reads)(input, init);
    if (url.includes("/members")) return Promise.resolve(page(url.includes(other.id) ? [] : [member, { ...member, id: "member-2", role: "复核" }]));
    return Promise.resolve(url.startsWith("/api/v1/providers") ? page() : page([group, other]));
  }));
});
afterEach(() => vi.unstubAllGlobals());
it("keeps discussion inactive before selection, then distinguishes loading, failure and real emptiness", async () => {
  render(<App />); await screen.findByRole("button", { name: group.name });
  expect(panel().getByText("请先选择群组，再查看讨论和发送消息。")).toBeVisible();
  expect(panel().queryByRole("form")).not.toBeInTheDocument(); expect(reads).not.toHaveBeenCalled();
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise); select(group.name);
  expect(panel().getByText("正在载入消息…")).toBeVisible();
  await act(async () => pending.resolve(json({}, 503)));
  expect(panel().getByRole("alert")).toHaveTextContent("503");
  fireEvent.click(panel().getByRole("button", { name: "重试载入消息" }));
  expect(await panel().findByText("尚无消息，可发送第一条讨论。")).toBeVisible();
});
it.each([
  { identity: "unverified legacy user", sender: "user", roster: [{ ...member, id: "user", name: "不能覆盖本地身份" }], author: "用户（来源未核验）" },
  { identity: "scoped member", sender: member.id, roster: [member], author: member.name },
  { identity: "unloaded member", sender: "missing-member", roster: [], author: "未载入成员" },
  { identity: "foreign same-ID member", sender: member.id, roster: [{ ...member, group_id: other.id, name: "外群名称" }], author: "未载入成员" },
  { identity: "scoped match after foreign same-ID member", sender: member.id, roster: [{ ...member, group_id: other.id, name: "外群名称" }, member], author: member.name },
  { identity: "departed member", sender: member.id, roster: [{ ...member, status: "left", name: "历史研究员" }], author: "历史研究员" },
])("uses only real scoped author names for $identity", async ({ sender, roster, author }) => {
  reads.mockResolvedValueOnce(page([{ ...message, sender_id: sender, deliveries: [] }]));
  render(<Discussion groupId={group.id} members={{ [group.id]: roster, [other.id]: [{ ...member, group_id: other.id, name: "外群缓存" }] }} />);
  const record = await screen.findByRole("article", { name: `消息 ${message.id}` });
  expect(within(record).getByText(author, { exact: true })).toBeVisible();
  expect(record).not.toHaveTextContent(/外群名称|外群缓存|不能覆盖本地身份/);
  expect(within(record).getByText(sender, { exact: true })).not.toBeVisible();
  expect(expandDetails(record).getByText(sender, { exact: true })).toBeVisible();
  expect(vi.mocked(fetch).mock.calls.map(([url]) => url)).toEqual([`/api/v1/groups/${group.id}/messages?limit=50`]);
});
it("uses newly supplied members without fetching or caching a separate roster", async () => {
  reads.mockResolvedValueOnce(page([{ ...message, sender_id: member.id, deliveries: [] }]));
  const view = render(<Discussion groupId={group.id} members={{}} />);
  const record = await screen.findByRole("article", { name: `消息 ${message.id}` });
  expect(within(record).getByText("未载入成员")).toBeVisible();
  view.rerender(<Discussion groupId={group.id} members={{ [group.id]: [member] }} />);
  expect(within(record).getByText(member.name)).toBeVisible();
  view.rerender(<Discussion groupId={group.id} members={{}} />);
  expect(within(record).getByText("未载入成员")).toBeVisible();
  expect(fetch).toHaveBeenCalledTimes(1);
});
const question = { ...message, id: "root /+%", topic_id: "discussion-topic", content: "需要核对哪些数据？", deliveries: [],
  source_kind: "local_user_chat" as const, source_id: "question-command" };
const helper = { ...member, id: "discussion-helper", name: "复核员", status: "left" };
const invitation = { ...question, id: "invite-1", sender_id: member.id, source_kind: "member" as const, source_id: "invite-command",
  content: "请独立核对这份数据", discussion: { kind: "invite" as const, root_message_id: question.id, invitation_message_id: "invite-1",
    inviter_id: member.id, assignee_id: helper.id, expected_reply: "列出待复核的数据项" },
  deliveries: [{ ...delivery, message_id: "invite-1", recipient_id: helper.id, state: "accepted", turn_id: "receiving-turn" }] };
const reply = { ...invitation, id: "reply-1", sender_id: helper.id, source_id: "reply-command", content: "这里是核对意见",
  discussion: { ...invitation.discussion, kind: "reply" as const }, deliveries: [] };
const resolution = { ...invitation, id: "resolve-1", source_id: "resolve-command", content: "综合以上意见，这是本次讨论的结论",
  discussion: { ...invitation.discussion, kind: "resolve" as const }, deliveries: [] };
const discussionWork = { task_id: "task-actual", mandate_id: "mandate-hidden", dispatch_request_id: "dispatch-hidden" };

it("links persisted invitation, return and resolution in the existing timeline without annotating legacy prose", async () => {
  const legacy = { ...message, deliveries: [], content: `@${member.name} 请邀请并答复这个问题。`, discussion: null };
  reads.mockResolvedValueOnce(page([resolution, reply, question, legacy, invitation]));
  render(<main aria-label="讨论"><Discussion groupId={group.id} members={{ [group.id]: [member, helper,
    { ...member, name: "外群同 ID", group_id: other.id }] }} /></main>);
  await panel().findByRole("article", { name: `消息 ${resolution.id}` });
  for (const [row, label, actors] of [
    [invitation, "邀请协作", `${member.name} 邀请 ${helper.name}（已离开）`],
    [reply, "回送答复", `${helper.name}（已离开） 回送给 ${member.name}`],
    [resolution, "讨论总结", `${member.name} 向群组总结`],
  ] as const) {
    const record = article(row.id);
    const summary = within(record).getByText(label, { selector: "summary strong" }).closest("summary")!;
    const disclosure = summary.closest("details")!;
    expect(summary).toBeVisible(); expect(summary).toHaveTextContent(actors);
    expect(record).not.toHaveTextContent("外群同 ID");
    expect(disclosure).not.toHaveAttribute("open");
    fireEvent.click(summary);
    const evidence = within(disclosure);
    const rootLink = evidence.getByRole("link", { name: "原始问题" });
    expect(rootLink).toHaveAttribute("href", `#${article(question.id).id}`);
    expect(article(question.id)).toHaveAttribute("tabindex", "-1");
    expect(evidence.getByText(question.content)).toBeVisible();
    if (row !== invitation) {
      expect(evidence.getByRole("link", { name: "对应邀请" })).toHaveAttribute("href", `#${article(invitation.id).id}`);
      expect(evidence.getByText(invitation.content)).toBeVisible();
    }
    for (const value of [question.id, invitation.id, member.id, helper.id, invitation.discussion.expected_reply]) {
      expect(evidence.getByText(value, { exact: true })).toBeVisible();
    }
    expect(within(record).getByText("成员消息", { exact: true })).toBeVisible();
    expect(within(record).getByText(row === invitation ? /投递详情.*CLI已接收 1/ : /投递详情.*暂无投递记录/)).toBeVisible();
    expect(within(record).getByRole("button", { name: `转为任务，消息 ${row.id}` })).toBeEnabled();
  }
  for (const row of [question, legacy]) expect(article(row.id).querySelectorAll("summary")).toHaveLength(1);
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("shows the interrupted reply on its row when the existing newest-page poll supplies the reason", async () => {
  const target = { ...reply, deliveries: [{ ...delivery, message_id: reply.id, recipient_id: member.id,
    state: "accepted", turn_id: "receiving-turn" }] };
  const reason = "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作";
  vi.useFakeTimers();
  try {
    reads.mockResolvedValueOnce(page([target, invitation, question]));
    render(<main aria-label="讨论"><Discussion groupId={group.id} members={{ [group.id]: [member, helper] }} /></main>);
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    const record = article(target.id);
    expect(within(record).getByText(/投递详情.*CLI已接收 1/)).toBeVisible();
    expect(panel().queryByText(reason)).not.toBeInTheDocument();

    reads.mockResolvedValueOnce(page([{ ...target, discussion_closure_reason: reason }, invitation, question]));
    await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
    expect(within(record).getByText(reason)).toBeVisible();
    expect(panel().getAllByText(reason)).toHaveLength(1);
    expect(within(record).getByText(/投递详情.*CLI已接收 1/)).toBeVisible();
    expect(vi.mocked(fetch).mock.calls.map(([url]) => url)).toEqual(Array(2).fill(`/api/v1/groups/${group.id}/messages?limit=50`));
  } finally {
    vi.useRealTimers();
  }
});

it("shows proved work alongside failed delivery and adds only a separately proved Assignment after exact refresh", async () => {
  const label = "已转为正式工作";
  const worker = { ...member, id: "actual-worker", name: "执行成员" };
  const foreign = { ...worker, group_id: other.id, name: "外群执行成员" };
  const legacy = { ...message, id: "legacy", deliveries: [], content: "可以开始，但没有关联证据。", discussion_work: discussionWork };
  const queued = { ...invitation.deliveries[0], state: "queued" };
  const failed = { ...invitation, deliveries: [{ ...queued, state: "failed", error: "执行未启动" }] };
  const flush = (ms = 0) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  vi.useFakeTimers();
  try {
    reads.mockResolvedValueOnce(page([{ ...invitation, deliveries: [queued] }, question, legacy]));
    const view = (roster: typeof member[]) => <main aria-label="讨论"><Discussion groupId={group.id} members={{ [group.id]: roster }} /></main>;
    const { rerender } = render(view([member, helper, worker, foreign]));
    await flush();
    const card = within(article(invitation.id));
    expect(card.getByText(invitation.content)).toBeVisible();
    expect(panel().queryByText(label)).not.toBeInTheDocument();

    change("保留这份讨论草稿");
    reads.mockResolvedValueOnce(page([{ ...failed, discussion_work: discussionWork }, question, legacy]));
    await flush(5000);
    expect(card.getByText(label)).toBeVisible();
    expect(card.getByText(discussionWork.task_id, { selector: "code" })).toBeVisible();
    expect(card.getByText(/投递详情.*投递失败 1/)).toBeVisible();
    expect(panel().getAllByText(label)).toHaveLength(1);
    expect(card.queryByText(/分配 ID/)).not.toBeInTheDocument();
    expect(card.queryByText(worker.name)).not.toBeInTheDocument();
    expect(details).not.toHaveBeenCalled();

    details.mockResolvedValueOnce(json({ ...failed, discussion_work: { ...discussionWork,
      assignment_id: "assignment-actual", worker_member_id: worker.id, claim_digest: "private-claim-digest" } }));
    fireEvent.click(card.getByRole("button", { name: `刷新消息 ${invitation.id} 讨论状态` }));
    await flush();
    const status = card.getByText(label).closest("p")!;
    expect(status).toHaveTextContent("任务 ID：task-actual · 分配 ID：assignment-actual（执行成员）");
    expect(status.querySelector("button, a")).toBeNull();
    expect(status).not.toHaveTextContent(/已启动|已提交|已验收|报告/);
    expect(article(invitation.id)).not.toHaveTextContent(/CLI已接收|已中断|mandate-hidden|dispatch-hidden|private-claim-digest|外群执行成员/);
    expect(card.getByText(/投递详情.*投递失败 1/)).toBeVisible();
    expect(content()).toHaveValue("保留这份讨论草稿");
    expect(card.getByRole("button", { name: `转为任务，消息 ${invitation.id}` })).toBeVisible();
    rerender(view([member, helper, foreign]));
    expect(status).toHaveTextContent("分配 ID：assignment-actual");
    expect(status).not.toHaveTextContent(worker.name);
    const url = `/api/v1/groups/${group.id}/messages`;
    expect(vi.mocked(fetch).mock.calls.map(([path]) => path)).toEqual([`${url}?limit=50`, `${url}?limit=50`, `${url}/${invitation.id}`]);
    expect(writes).not.toHaveBeenCalled();
  } finally {
    vi.useRealTimers();
  }
});

it("refreshes an older accepted handoff in place without starting another polling path", async () => {
  const latest = Array.from({ length: 50 }, (_, index) => ({ ...message, id: `newest-${index}`, deliveries: [],
    created_at: "2026-10-30T12:01:00Z" }));
  const reason = "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作";
  const flush = (ms = 0) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  vi.useFakeTimers();
  try {
    reads.mockResolvedValueOnce(page(latest, "older+/"));
    render(<main aria-label="讨论"><Discussion groupId={group.id} members={{ [group.id]: [member, helper] }} /></main>);
    await flush();
    expect(panel().queryByRole("article", { name: `消息 ${invitation.id}` })).not.toBeInTheDocument();
    reads.mockResolvedValueOnce(page([invitation, question]));
    fireEvent.click(panel().getByRole("button", { name: "加载更早消息" }));
    await flush();
    const older = within(article(invitation.id));
    expect(older.getByText(/投递详情.*CLI已接收 1/)).toBeVisible();

    reads.mockResolvedValueOnce(page(latest, "older+/"));
    await flush(5000);
    expect(details).not.toHaveBeenCalled();
    expect(older.queryByText(reason)).not.toBeInTheDocument();
    expect(panel().getByText("最新页自动更新")).toBeVisible();

    const pending = deferred<Response>();
    details.mockReturnValueOnce(pending.promise);
    const button = older.getByRole("button", { name: `刷新消息 ${invitation.id} 讨论状态` });
    expect(button).toBeVisible();
    fireEvent.click(button);
    expect(button).toBeDisabled();
    await act(async () => pending.resolve(json({ ...invitation, discussion_closure_reason: reason })));
    expect(older.getByText(reason)).toBeVisible();
    expect(older.getByText(/投递详情.*CLI已接收 1/)).toBeVisible();
    expect(button).toBeEnabled();
    expect(panel().getAllByRole("article")).toHaveLength(latest.length + 2);
    const listURL = `/api/v1/groups/${group.id}/messages?limit=50`;
    expect(vi.mocked(fetch).mock.calls.map(([url]) => url)).toEqual([
      listURL, `${listURL}&before=older%2B%2F`, listURL, `/api/v1/groups/${group.id}/messages/${invitation.id}`,
    ]);
  } finally {
    vi.useRealTimers();
  }
});

it("keeps unloaded relation IDs, but does not assert provenance from contradictory records", async () => {
  const unloaded = { ...reply, id: "unloaded-reply", sender_id: "missing-assignee",
    discussion: { ...reply.discussion, root_message_id: "earlier-question", invitation_message_id: "earlier-invitation",
      parent_invitation_message_id: "earlier-parent", assignee_id: "missing-assignee", inviter_id: "missing-inviter" } };
  const otherTopic = { ...question, id: "other-topic-root", topic_id: "different-topic" };
  const wrongTopic = { ...reply, id: "wrong-topic", discussion: { ...reply.discussion,
    root_message_id: otherTopic.id, invitation_message_id: "not-loaded" } };
  const policy = { ...question, id: "policy-row", source_kind: "local_user_policy", content: "不是用户问题" };
  const fromPolicy = { ...invitation, id: "from-policy", discussion_work: discussionWork, discussion: { ...invitation.discussion,
    invitation_message_id: "from-policy", root_message_id: policy.id },
    deliveries: [{ ...invitation.deliveries[0], message_id: "from-policy" }] };
  const wrongRecipient = { ...reply, id: "wrong-recipient", discussion: { ...reply.discussion, inviter_id: "other-inviter" } };
  const wrongSender = { ...reply, id: "wrong-sender", sender_id: member.id };
  const wrongSource = { ...reply, id: "wrong-source", source_kind: "platform" };
  const oversized = { ...reply, id: "oversized", discussion: { ...unloaded.discussion, assignee_id: helper.id, expected_reply: "答".repeat(5500) } };
  const mismatches = [wrongTopic, fromPolicy, wrongRecipient, wrongSender, wrongSource, oversized];
  reads.mockResolvedValueOnce(page([unloaded, question, invitation, otherTopic, policy, ...mismatches]));
  render(<main aria-label="讨论"><Discussion groupId={group.id} members={{ [group.id]: [member, helper,
    { ...member, id: "missing-inviter", name: "外群邀请者", group_id: other.id }] }} /></main>);
  await panel().findByRole("article", { name: `消息 ${unloaded.id}` });
  const summary = within(article(unloaded.id)).getByText("回送答复", { selector: "summary strong" }).closest("summary")!;
  expect(summary).not.toHaveTextContent("外群邀请者");
  fireEvent.click(summary);
  const evidence = within(summary.closest("details")!);
  for (const id of ["earlier-question", "earlier-invitation", "earlier-parent", "missing-assignee", "missing-inviter"]) {
    expect(evidence.getByText(id, { exact: true })).toBeVisible();
  }
  expect(evidence.queryByRole("link")).not.toBeInTheDocument();
  for (const row of mismatches) {
    expect(article(row.id).querySelectorAll("summary")).toHaveLength(1);
    expect(within(article(row.id)).queryByRole("button", { name: /讨论状态/ })).not.toBeInTheDocument();
    expect(within(article(row.id)).queryByText("已转为正式工作")).not.toBeInTheDocument();
    expect(within(article(row.id)).getByText(row.content)).toBeVisible();
  }
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("renders exact inert raw text with a truthful topic and folded technical records", async () => {
  const received = raw.replace("@研究员\n", "@研究员\r\n");
  reads.mockResolvedValueOnce(page([{ ...message, content: received, topic_id: "topic-A", deliveries: ["queued", "queued", "accepted", "failed", "future"].map((state, index) => ({ ...delivery, id: `d-${index}`, state })) }]));
  await open(); const record = await panel().findByRole("article");
  expect(record.querySelector(".message-content")?.textContent).toBe(received);
  expect(record.querySelector("script")).toBeNull();
  expect(within(record).getByText("用户（来源未核验）", { exact: true })).toBeVisible();
  expect(within(record).getByText("话题消息", { exact: true })).toBeVisible();
  expect(record.querySelector("details")).not.toHaveAttribute("open");
  for (const value of [message.id, message.sender_id, "topic-A", message.created_at]) expect(within(record).getByText(value, { exact: true })).not.toBeVisible();
  expect(record.querySelector(".message-meta time")).toHaveAttribute("datetime", message.created_at);
  expect(record.querySelector(".message-meta time")).not.toHaveTextContent(message.created_at);
  for (const label of ["已入队 2", "CLI已接收 1", "投递失败 1", "状态未知 1"]) expect(record.querySelector("summary")).toHaveTextContent(label);
  expect(record).not.toHaveTextContent(/已读|已完成|已回复/);
  expect(within(record).getByText("刷新投递")).not.toBeVisible();
  const technical = expandDetails(record);
  for (const value of [message.id, message.sender_id, "topic-A", message.created_at]) expect(technical.getByText(value, { exact: true })).toBeVisible();
  expect(technical.getByText(/状态未知.*future/)).toBeVisible();
  expect(panel().getByText("最新页自动更新")).toBeVisible();
  expect(panel().getByText("已载入 1 条（含话题，片段可能有间隔）。")).toBeVisible();
});
it.each([
  { state: "failed", error: "", label: "投递失败 1" },
  { state: "future", error: "", label: "状态未知 1" },
  { state: "", error: "", label: "状态未知 1" },
  { state: "accepted", error: "仍有投递错误", label: "CLI已接收 1" },
  { state: "queued", error: "入队时存在错误", label: "已入队 1" },
])("keeps $state receipts and nonempty errors visible while details are closed", async ({ state, error, label }) => {
  reads.mockResolvedValueOnce(page([{ ...message, deliveries: [{ ...delivery, state, error }] }]));
  await open(); await panel().findByRole("article");
  expect(article().querySelector("details")).not.toHaveAttribute("open");
  const summary = article().querySelector("summary");
  expect(summary).toBeVisible(); expect(summary).toHaveTextContent(label);
  if (error) expect(summary).toHaveTextContent("投递错误 1");
  expect(article()).not.toHaveTextContent(/已读|已完成|已回复/);
  const technical = expandDetails(article());
  expect(technical.getByText(new RegExp(`（${state}）`))).toBeVisible();
  if (error) expect(technical.getByText(`投递错误：${error}`)).toBeVisible();
  fireEvent.click(article().querySelector("summary")!);
  expect(summary).toBeVisible(); expect(summary).toHaveTextContent(label);
  if (error) expect(summary).toHaveTextContent("投递错误 1");
});
it("keeps zero deliveries honest without repeated main-channel or inferred public routing claims", async () => {
  reads.mockResolvedValueOnce(page([{ ...message, deliveries: [] }]));
  await open(); await panel().findByRole("article");
  expect(article().querySelector("summary")).toHaveTextContent("暂无投递记录");
  expect(article()).not.toHaveTextContent(/主频道消息|话题消息|仅公开可见|已读|已完成|已回复/);
});
it("expands locally, resolves real recipient records, and refreshes only the chosen message with visible failures", async () => {
  const departed = { ...member, id: "departed", status: "left", name: "历史成员" };
  const foreign = { ...member, id: "foreign", group_id: other.id, name: "外群名称" };
  const target = { ...message, id: "message-refresh", deliveries: [member, departed, foreign, { ...member, id: "unloaded" }].map((recipient, index) => ({ ...delivery, id: `receipt-${index}`, message_id: "message-refresh", recipient_id: recipient.id })) };
  reads.mockResolvedValueOnce(page([message, target]));
  render(<main aria-label="讨论"><Discussion groupId={group.id} members={{ [group.id]: [member, departed, foreign] }} /></main>);
  await panel().findByRole("article", { name: `消息 ${target.id}` });
  const record = article(target.id), technical = expandDetails(record);
  const receipts = technical.getAllByRole("listitem");
  for (const [index, name] of [member.name, departed.name, "未载入成员", "未载入成员"].entries()) {
    expect(receipts[index]).toHaveTextContent(name);
    expect(receipts[index]).toHaveTextContent(target.deliveries[index].recipient_id);
    expect(receipts[index]).toHaveTextContent(target.deliveries[index].id);
    expect(receipts[index]).toHaveTextContent("已入队（queued）");
  }
  expect(record).not.toHaveTextContent(foreign.name);
  fireEvent.click(record.querySelector("summary")!);
  expect(record.querySelector("details")).not.toHaveAttribute("open");
  expect(fetch).toHaveBeenCalledTimes(1); expect(details).not.toHaveBeenCalled(); expect(writes).not.toHaveBeenCalled();
  const pending = deferred<Response>(); details.mockReturnValueOnce(pending.promise); refresh(target.id);
  expect(within(record).getByRole("button", { name: `刷新消息 ${target.id} 投递` })).toBeDisabled();
  fireEvent.click(record.querySelector("summary")!);
  await act(async () => pending.resolve(json({}, 503)));
  expect(within(record).getByRole("alert")).toBeVisible(); expect(within(record).getByRole("alert")).toHaveTextContent("503");
  details.mockResolvedValueOnce(json({ ...target, deliveries: [{ ...target.deliveries[0], state: "accepted" }] })); refresh(target.id);
  await within(record).findByText(/投递详情.*CLI已接收 1/);
  expect(within(record).queryByRole("alert")).not.toBeInTheDocument();
  expect(article().querySelector("summary")).toHaveTextContent("已入队 1");
  expect(details.mock.calls.map(([url]) => url)).toEqual([`/api/v1/groups/${group.id}/messages/${target.id}`, `/api/v1/groups/${group.id}/messages/${target.id}`]);
  expect(reads).toHaveBeenCalledTimes(1); expect(writes).not.toHaveBeenCalled();
});
it("pages only explicitly, deduplicates and updates server values, sorts nanoseconds, and resets the history window", async () => {
  const earlier = { ...message, id: "z-earlier", content: "纳秒较早", created_at: "2026-10-30T12:00:00.000000001Z", deliveries: [] };
  const later = { ...message, id: "a-later", content: "纳秒较晚", created_at: "2026-10-30T12:00:00.000000002Z", deliveries: [] };
  reads.mockResolvedValueOnce(page([later, earlier], "opaque+/")); await open(); await panel().findByRole("article", { name: `消息 ${later.id}` });
  expect(panel().getAllByRole("article").map(node => node.getAttribute("aria-label"))).toEqual([`消息 ${earlier.id}`, `消息 ${later.id}`]);
  expect(reads).toHaveBeenCalledTimes(1);
  reads.mockResolvedValueOnce(page([earlier, message])); fireEvent.click(panel().getByRole("button", { name: "加载更早消息" }));
  await panel().findByRole("article", { name: `消息 ${message.id}` }); expect(panel().getAllByRole("article")).toHaveLength(3);
  expect(reads.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/messages?limit=50&before=opaque%2B%2F`);
  reads.mockResolvedValueOnce(page([{ ...message, deliveries: [{ ...delivery, state: "accepted" }] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await panel().findByText(/投递详情.*CLI已接收/); expect(panel().getAllByRole("article")).toHaveLength(1);
});
it.each(["default", "public", "selected"])("sends raw content with exact %s routing from actual scoped members", async mode => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw); route(mode);
  if (mode === "selected") {
    fireEvent.click(await panel().findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
    expect(panel().getByText("仅列出已载入的在群成员；更多成员请打开成员详情加载。")).toBeVisible();
  }
  submit(); expect(await panel().findByText(/消息已持久化/)).toBeVisible();
  expect(JSON.parse(String(writes.mock.calls[0][1]?.body))).toEqual({ content: raw, ...(mode === "default" ? {} : { recipient_ids: mode === "public" ? [] : [member.id] }) });
  expect(content()).toHaveValue(""); expect(article().querySelector(".message-content")?.textContent).toBe(raw);
});
it("requires nonblank content and explicit valid selected recipients, and never posts optimistically or twice", async () => {
  await open(); change(" \n "); submit(); expect(panel().getByRole("alert")).toHaveTextContent("请输入消息内容");
  change(raw); route("selected"); submit(); expect(panel().getByRole("alert")).toHaveTextContent("请选择本群已载入的在群成员");
  fireEvent.click(await panel().findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit(); submit();
  expect(writes).toHaveBeenCalledTimes(1); expect(content()).toBeDisabled(); expect(panel().queryByRole("article")).not.toBeInTheDocument();
  await act(async () => pending.resolve(json({ ...message, content: raw }, 201)));
  expect(article()).toHaveTextContent("已入队"); expect(content()).toHaveValue("");
});
it.each(["network", 503, 200, 202, "malformed", "foreign", "topic"])("freezes unknown %s operations across A to B to A with identical retry", async failure => {
  await open(); change(raw); route("selected");
  fireEvent.click(await panel().findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
  if (failure === "network") writes.mockRejectedValueOnce(new TypeError("offline"));
  else writes.mockResolvedValueOnce(json(failure === "malformed" ? {} : failure === "foreign" ? { ...message, group_id: other.id } : failure === "topic" ? { ...message, topic_id: "wrong" } : message, typeof failure === "number" ? failure : 201));
  submit(); expect(await panel().findByRole("alert")).toHaveTextContent("发送结果未确认，消息可能已保存"); expect(content()).toBeDisabled();
  expect(panel().getByRole("combobox", { name: "接收方式" })).toBeDisabled();
  expect(panel().getByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` })).toBeDisabled();
  expect(panel().getByRole("button", { name: "重试同一次发送" })).toBeEnabled();
  expect(panel().getByText(/重载会丢失/)).toBeVisible();
  select(other.name); await panel().findByText("尚无消息，可发送第一条讨论。"); change("B 的草稿"); route("public");
  expect(panel().queryByRole("alert")).not.toBeInTheDocument(); select(group.name);
  expect(content()).toHaveValue(raw); fireEvent.click(panel().getByRole("button", { name: "重试同一次发送" }));
  await panel().findByText(/消息已持久化/);
  const attempts = writes.mock.calls.map(([url, init]) => [url, init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]); expect(attempts[0][0]).toBe(`/api/v1/groups/${group.id}/messages`);
  select(other.name); expect(content()).toHaveValue("B 的草稿");
  expect(panel().getByRole("combobox", { name: "接收方式" })).toHaveValue("public");
});
it.each([413, 415])("retains an editable draft after HTTP %s and uses a new key for corrected content", async status => {
  await open(); change(raw.repeat(50)); writes.mockResolvedValueOnce(json({}, status)); submit();
  expect(await panel().findByRole("alert")).toHaveTextContent(String(status)); expect(content()).toBeEnabled(); expect(content()).toHaveValue(raw.repeat(50));
  change("缩短的内容"); submit(); await panel().findByText(/消息已持久化/);
  expect(writes.mock.calls[1][1]?.body).toBe(JSON.stringify({ content: "缩短的内容" }));
  expect(new Headers(writes.mock.calls[1][1]?.headers).get("Idempotency-Key")).not.toBe(new Headers(writes.mock.calls[0][1]?.headers).get("Idempotency-Key"));
});
it.each([false, true])("protects a POST receipt from an earlier active GET (empty=%s), then accepts fresh detail data", async empty => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); await open(); change(raw); submit();
  await panel().findByText(/消息已持久化/);
  await act(async () => old.resolve(page(empty ? [] : [{ ...message, content: "旧快照" }])));
  expect(article().querySelector(".message-content")?.textContent).toBe(raw);
  details.mockResolvedValueOnce(json({ ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] }));
  refresh();
  await panel().findByText(/投递详情.*CLI已接收/);
});
it("keeps late A receipts out of B and protects creation from an earlier empty GET", async () => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); await open(); change(raw);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit(); select(other.name);
  await panel().findByText("尚无消息，可发送第一条讨论。"); change("B draft");
  await act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
  expect(content()).toHaveValue("B draft"); expect(panel().queryByRole("article")).not.toBeInTheDocument();
  reads.mockResolvedValueOnce(page([{ ...message, content: raw }])); select(group.name); await panel().findByRole("article");
  await act(async () => old.resolve(page())); expect(article()).toHaveTextContent(raw.trim().split("\n")[0]);
});
it("does not let an older page downgrade a newer detail, nor an older detail downgrade a new latest read", async () => {
  reads.mockResolvedValueOnce(page([message], "older")); await open(); await panel().findByRole("article");
  const oldPage = deferred<Response>(); reads.mockReturnValueOnce(oldPage.promise);
  fireEvent.click(panel().getByRole("button", { name: "加载更早消息" }));
  details.mockResolvedValueOnce(json({ ...message, deliveries: [{ ...delivery, state: "accepted" }] }));
  refresh(); await panel().findByText(/投递详情.*CLI已接收/);
  await act(async () => oldPage.resolve(page([message]))); expect(article()).toHaveTextContent("CLI已接收");
  const oldDetail = deferred<Response>(); details.mockReturnValueOnce(oldDetail.promise);
  refresh();
  reads.mockResolvedValueOnce(page([{ ...message, deliveries: [{ ...delivery, state: "failed" }] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByText(/投递详情.*投递失败/);
  await act(async () => oldDetail.resolve(json(message))); expect(article()).toHaveTextContent("投递失败");
});
it("confirms a late POST without downgrading data read after that attempt began", async () => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit();
  reads.mockResolvedValueOnce(page([{ ...message, content: raw }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByRole("article");
  details.mockResolvedValueOnce(json({ ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] }));
  refresh(); await panel().findByText(/投递详情.*CLI已接收/);
  await act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
  expect(article()).toHaveTextContent("CLI已接收"); expect(article()).not.toHaveTextContent("已入队");
  expect(content()).toHaveValue(""); expect(panel().getByText(/消息已持久化/)).toBeVisible();
});
it("inserts a late 201 receipt even after an empty reset begun during its attempt", async () => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit();
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await panel().findByText("尚无消息，可发送第一条讨论。");
  const olderRead = deferred<Response>(); reads.mockReturnValueOnce(olderRead.promise);
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
  await act(async () => olderRead.resolve(page()));
  expect(article()).toHaveTextContent("已入队"); expect(content()).toHaveValue("");
});
it.each([["history", "receipt-first"], ["history", "read-first"], ["detail", "receipt-first"], ["detail", "read-first"]])(
  "arrival matrix: post-attempt %s accepted data wins with %s", async (kind, arrival) => {
    await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
    const receipt = deferred<Response>(), read = deferred<Response>();
    if (kind === "detail") {
      writes.mockRejectedValueOnce(new TypeError("offline")); submit(); await panel().findByRole("alert");
      reads.mockResolvedValueOnce(page([{ ...message, content: raw }]));
      fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByRole("article");
      writes.mockReturnValueOnce(receipt.promise); fireEvent.click(panel().getByRole("button", { name: "重试同一次发送" }));
      details.mockReturnValueOnce(read.promise);
      refresh();
    } else {
      writes.mockReturnValueOnce(receipt.promise); submit(); reads.mockReturnValueOnce(read.promise);
      fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
    }
    const accepted = { ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] };
    const resolveRead = () => act(async () => read.resolve(kind === "history" ? page([accepted]) : json(accepted)));
    const resolveReceipt = () => act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
    if (arrival === "read-first") { await resolveRead(); await resolveReceipt(); }
    else { await resolveReceipt(); await resolveRead(); }
    expect(article()).toHaveTextContent("CLI已接收"); expect(article()).not.toHaveTextContent("已入队");
    expect(content()).toHaveValue(""); expect(panel().getByText(/消息已持久化/)).toBeVisible();
  },
);
it("preserves an already-read accepted row when an idempotent retry returns its stale queued receipt", async () => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
  writes.mockRejectedValueOnce(new TypeError("offline")); submit(); await panel().findByRole("alert");
  reads.mockImplementation(async () => page([{ ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await within(await panel().findByRole("article", { name: `消息 ${message.id}` })).findByText(/投递详情.*CLI已接收/);
  fireEvent.click(panel().getByRole("button", { name: "重试同一次发送" })); await panel().findByText(/消息已持久化/);
  expect(article()).toHaveTextContent("CLI已接收"); expect(article()).not.toHaveTextContent("已入队");
  expect(writes).toHaveBeenCalledTimes(2);
  const attempts = writes.mock.calls.map(([, init]) => [init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]);
});
it("does not resurrect a row outside a reset window from an earlier detail read", async () => {
  reads.mockResolvedValueOnce(page([message])); await open(); await panel().findByRole("article");
  const old = deferred<Response>(); details.mockReturnValueOnce(old.promise);
  refresh();
  reads.mockResolvedValueOnce(page([{ ...message, id: "new-window", content: "新窗口", deliveries: [] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await panel().findByRole("article", { name: "消息 new-window" }); await act(async () => old.resolve(json(message)));
  expect(panel().getAllByRole("article")).toHaveLength(1);
});
it("ignores an obsolete A read after A to B to A and aborts reads and POST on full unmount", async () => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); const view = render(<App />);
  select((await screen.findByRole("button", { name: group.name })).textContent!); select(other.name);
  await panel().findByText("尚无消息，可发送第一条讨论。"); reads.mockResolvedValueOnce(page([{ ...message, content: "新的 A" }]));
  select(group.name); await panel().findByRole("article"); await act(async () => old.resolve(page([{ ...message, content: "旧的 A" }])));
  expect(article()).toHaveTextContent("新的 A"); expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); change(raw); submit();
  const detail = deferred<Response>(); details.mockReturnValueOnce(detail.promise); refresh();
  view.unmount(); expect(writes.mock.calls[0][1]?.signal?.aborted).toBe(true); expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => { pending.resolve(json(message, 201)); detail.resolve(json(message)); });
});
