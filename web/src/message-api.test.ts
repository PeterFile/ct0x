import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { decodeMessage, getMessage, isDefinitiveRejection, listMessages, sendMessage } from "./api";
import type { Message } from "./api";
import { group, json, other } from "./test/groups";

const transport = vi.fn<typeof fetch>();
const content = "  <b>仅作为文本</b>\n@同名成员\r\n第二行\t  ";
const delivery = { id: "delivery-1", message_id: "message-1", conversation_id: "conversation-1",
  recipient_id: "member-1", state: "queued", turn_id: "", error: "", created_at: group.created_at };
const message: Message = { id: "message-1", group_id: group.id, topic_id: "", sender_id: "user",
  content, created_at: group.created_at, deliveries: [delivery] };
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("encodes the group, sends only the JSON DTO, and returns a direct 201 receipt", async () => {
  const groupId = "群/组?+";
  const input = { content, recipient_ids: ["member+/1"], topic_id: "话题 /?+", sender_id: "forged",
    actor: "forged", group_id: other.id, owner_id: "forged", creator_id: "forged" };
  const receipt = { ...message, group_id: groupId, topic_id: input.topic_id };
  transport.mockResolvedValueOnce(json(receipt, 201));
  const signal = new AbortController().signal;
  expect(await sendMessage(groupId, input, "opaque-key /+", signal)).toEqual(receipt);
  expect(transport).toHaveBeenCalledExactlyOnceWith("/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/messages", {
    method: "POST", redirect: "error", signal, headers: { "Content-Type": "application/json", "Idempotency-Key": "opaque-key /+" },
    body: JSON.stringify({ content, recipient_ids: input.recipient_ids, topic_id: input.topic_id }),
  });
});
it.each([
  { content },
  { content, recipient_ids: [] },
  { content, recipient_ids: ["stable-member-1", "stable-member-2"] },
])("preserves recipient routing payload $recipient_ids without parsing mentions", async input => {
  transport.mockResolvedValueOnce(json({ ...message, deliveries: [] }, 201));
  await sendMessage(group.id, input, "routing-key");
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toStrictEqual(input);
  expect(transport).toHaveBeenCalledTimes(1);
});
it.each([undefined, "", "opaque-topic /?+"])("passes topic %s through without inventing a default", async topic => {
  const input = topic === undefined ? { content } : { content, topic_id: topic };
  transport.mockResolvedValueOnce(json({ ...message, topic_id: topic ?? "" }, 201));
  await sendMessage(group.id, input, "topic-key");
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toStrictEqual(input);
});
it.each([undefined, "topic-A"])("rejects a 201 receipt outside requested topic %s", async topic => {
  transport.mockResolvedValueOnce(json({ ...message, topic_id: "topic-B" }, 201));
  const error: unknown = await sendMessage(group.id, { content, topic_id: topic }, "key").catch(error => error);
  expect(error).toMatchObject({ status: 201, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
});
it("preserves raw content and the same key/body on explicit caller retry, never retrying automatically", async () => {
  const input = { content, recipient_ids: [], topic_id: "" };
  transport.mockRejectedValueOnce(new TypeError("offline"));
  const error: unknown = await sendMessage(group.id, input, "same-key").catch(error => error);
  expect(error).toMatchObject({ status: 0, code: "disconnected" });
  expect(isDefinitiveRejection(error)).toBe(false);
  expect(transport).toHaveBeenCalledTimes(1);
  transport.mockResolvedValueOnce(json({ ...message, deliveries: [] }, 201));
  await sendMessage(group.id, input, "same-key");
  expect(transport).toHaveBeenCalledTimes(2);
  const attempts = transport.mock.calls.map(([url, init]) => ({ url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
  expect(attempts[1]).toEqual(attempts[0]);
  expect(JSON.parse(String(attempts[0].body)).content).toBe(content);
});
it.each([200, 202])("does not confirm HTTP %s even with a valid receipt", async status => {
  transport.mockResolvedValueOnce(json(message, status));
  const error: unknown = await sendMessage(group.id, { content }, "key").catch(error => error);
  expect(error).toMatchObject({ status, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
});
it.each([
  ["foreign group", { ...message, group_id: other.id }],
  ["foreign message delivery", { ...message, deliveries: [{ ...delivery, message_id: "another-message" }] }],
  ["null deliveries", { ...message, deliveries: null }],
  ["missing deliveries", { ...message, deliveries: undefined }],
  ["non-array deliveries", { ...message, deliveries: {} }],
  ["null delivery", { ...message, deliveries: [null] }],
  ["missing delivery scalar", { ...message, deliveries: [{ ...delivery, recipient_id: undefined }] }],
  ["non-string delivery scalar", { ...message, deliveries: [{ ...delivery, turn_id: null }] }],
  ["missing message scalar", { ...message, sender_id: undefined }],
])("does not confirm a malformed or cross-scope 201: %s", async (_label, receipt) => {
  transport.mockResolvedValueOnce(json(receipt, 201));
  const error: unknown = await sendMessage(group.id, { content }, "key").catch(error => error);
  expect(error).toMatchObject({ status: 201, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
});
it("drops incompatible discussion metadata without losing a matching message receipt", async () => {
  const discussion = { kind: "invite", root_message_id: "question-1", invitation_message_id: message.id,
    inviter_id: "member-1", assignee_id: "member-2", expected_reply: "核对结论" };
  const input = { content, discussion };
  transport.mockResolvedValueOnce(json({ ...message, discussion }, 201));
  expect(await sendMessage(group.id, input, "discussion-is-read-only")).toEqual(message);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual({ content });
});

const invitation = { ...message, sender_id: "inviter", source_kind: "member", source_id: "invite-command",
  discussion: { kind: "invite", root_message_id: "question", invitation_message_id: message.id,
    inviter_id: "inviter", assignee_id: delivery.recipient_id, expected_reply: "核对结论" } } satisfies Message;
const reply = { ...invitation, id: "reply", sender_id: delivery.recipient_id, source_id: "reply-command", deliveries: [],
  discussion: { ...invitation.discussion, kind: "reply" } } satisfies Message;
const summary = { ...invitation, id: "summary", source_id: "summary-command", deliveries: [],
  discussion: { ...invitation.discussion, kind: "resolve" } } satisfies Message;
const returnDelivery = { ...delivery, message_id: reply.id, recipient_id: invitation.sender_id };

it("preserves both public and targeted replies, invitations, summaries and legacy source-optional wire", async () => {
  const { source_kind: _kind, source_id: _id, ...legacyInvitation } = invitation;
  const items = [legacyInvitation, reply, { ...reply, id: "targeted", deliveries: [{ ...returnDelivery, message_id: "targeted" }] }, summary];
  transport.mockResolvedValueOnce(json({ items, next_cursor: "" }));
  expect((await listMessages(group.id)).items).toEqual(items);
});
it.each([
  ["invitation without a delivery", invitation, []],
  ["invitation delivered to the wrong member", invitation, [{ ...delivery, recipient_id: invitation.sender_id }]],
  ["invitation with an extra delivery", invitation, [delivery, { ...delivery, id: "extra" }]],
  ["invitation without a conversation ID", invitation, [{ ...delivery, conversation_id: "" }]],
  ["invitation with a malformed delivery ID", invitation, [{ ...delivery, id: " delivery-1" }]],
  ["reply delivered to the wrong member", reply, [{ ...returnDelivery, recipient_id: reply.sender_id }]],
  ["reply with an extra delivery", reply, [returnDelivery, { ...returnDelivery, id: "extra" }]],
  ["reply with an oversized conversation ID", reply, [{ ...returnDelivery, conversation_id: "会".repeat(1366) }]],
  ["reply with an oversized delivery ID", reply, [{ ...returnDelivery, id: "d".repeat(4097) }]],
  ["summary with a delivery", summary, [{ ...delivery, message_id: summary.id }]],
] as const)("keeps a message row but not the relation: %s", async (_label, { discussion, ...row }, deliveries) => {
  const plain = { ...row, deliveries };
  transport.mockResolvedValueOnce(json({ items: [{ ...plain, discussion }], next_cursor: "next" }));
  expect(await listMessages(group.id)).toEqual({ items: [plain], next_cursor: "next" });
});

it("omits a summary relation with an unloaded parent invitation", async () => {
  const { discussion, ...row } = summary;
  transport.mockResolvedValueOnce(json({ items: [{ ...row, discussion: { ...discussion, parent_invitation_message_id: "earlier-parent" } }], next_cursor: "" }));
  expect((await listMessages(group.id)).items).toStrictEqual([row]);
});

const notStarted = "接收成员的讨论轮未启动";
const departed = "接收成员已离开，讨论交接未启动";
const noAction = "讨论已中断：接收成员已完成本轮，但未提交结构化讨论动作";
const interrupted = "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作";
const accepted = { ...delivery, state: "accepted", turn_id: "receiving-turn" };
const failed = { ...accepted, state: "failed", error: "执行未启动" };
const unclaimed = { ...delivery, state: "failed", error: departed };
it.each([
  ["accepted Turn that ended abnormally", accepted, interrupted],
  ["accepted Turn that completed without a handoff", accepted, noAction],
  ["bound Turn that did not start", failed, notStarted],
  ["departure after binding a Turn", failed, departed],
  ["unclaimed departure", unclaimed, departed],
] as const)("preserves closure for consistent delivery: %s", (_label, receipt, reason) => {
  const closed = { ...invitation, deliveries: [receipt], discussion_closure_reason: reason };
  expect(decodeMessage(closed)).toEqual(closed);
});
it.each([
  ["acceptance without a Turn", { ...accepted, turn_id: "" }, interrupted],
  ["acceptance with an error", { ...accepted, error: "投递失败" }, interrupted],
  ["acceptance with an unstarted reason", accepted, notStarted],
  ["acceptance with a departure reason", accepted, departed],
  ["acceptance with prose", accepted, "本轮已核对结论，无需交接。"],
  ["bound failure without an error", { ...failed, error: "" }, notStarted],
  ["bound failure with a blank error", { ...failed, error: " " }, notStarted],
  ["bound failure with an abnormal-exit reason", failed, interrupted],
  ["bound failure with an unknown execution result", { ...failed, error: "执行结果未知，需对账后再处理" }, notStarted],
  ["abnormal exit claimed for an unknown execution result", { ...failed, error: "执行结果未知，需对账后再处理" }, interrupted],
  ["bound failure with the unclaimed marker", { ...failed, error: departed }, departed],
  ["unclaimed failure without the marker", { ...failed, turn_id: "" }, notStarted],
  ["departure text without the matching delivery marker", { ...unclaimed, error: `${departed} ` }, departed],
  ["departure marker with unrelated closure text", unclaimed, notStarted],
  ["departure with a malformed Turn ID", { ...unclaimed, turn_id: " " }, departed],
  ["pending delivery", delivery, interrupted],
] as const)("discards closure but keeps the relation for %s", (_label, receipt, reason) => {
  const row = { ...invitation, deliveries: [receipt] };
  expect(decodeMessage({ ...row, discussion_closure_reason: reason })).toEqual(row);
});
it("discards malformed closure text and closure without a verified targeted member handoff", () => {
  const row = { ...invitation, deliveries: [accepted] };
  for (const invalid of [null, 1, {}, "", "   ", ` ${interrupted}`, `${interrupted}\n附注`, "答".repeat(150)]) {
    expect(decodeMessage({ ...row, discussion_closure_reason: invalid })).toEqual(row);
  }
  for (const plain of [message, reply, summary, { ...row, discussion: null }, { ...row, source_kind: "unknown" as const, source_id: undefined }]) {
    expect(decodeMessage({ ...plain, discussion_closure_reason: interrupted })).toEqual(plain);
  }
});

const work = { task_id: "task-actual", mandate_id: "mandate-actual", dispatch_request_id: "dispatch-actual" };
const assignment = { assignment_id: "assignment-actual", worker_member_id: "actual-worker" };
it.each([
  ["claimed but queued", { ...accepted, state: "queued" }, work],
  ["accepted with separate Assignment", accepted, { ...work, ...assignment }],
  ["bound Turn that did not start", failed, work],
] as const)("decodes the same read-only work references in list and detail: %s", async (_label, receipt, discussion_work) => {
  const row = { ...invitation, deliveries: [receipt], discussion_work };
  const wire = { ...row, discussion_work: { ...discussion_work, authority: "private authority", claim_token: "private claim" } };
  transport.mockResolvedValueOnce(json({ items: [wire], next_cursor: "" })).mockResolvedValueOnce(json(wire));
  expect((await listMessages(group.id)).items).toEqual([row]);
  expect(await getMessage(group.id, row.id)).toEqual(row);
});

it("drops malformed work and unpaired Assignment fields without dropping the public invitation", () => {
  const row = { ...invitation, deliveries: [accepted] };
  for (const invalid of [null, [], {}, { ...work, task_id: " " }, { ...work, task_id: "task\u202e1" },
    { ...work, mandate_id: "任".repeat(43) }, { ...work, dispatch_request_id: undefined },
    { ...work, assignment_id: assignment.assignment_id }, { ...work, worker_member_id: assignment.worker_member_id },
    { ...work, ...assignment, worker_member_id: "user" }, { ...work, ...assignment, assignment_id: null }]) {
    expect(decodeMessage({ ...row, discussion_work: invalid })).toEqual(row);
  }
});

it("requires a verified member invitation and its bound delivery; work cannot replace a valid closure", () => {
  const row = { ...invitation, deliveries: [accepted] };
  for (const unproved of [message, { ...reply, deliveries: [{ ...returnDelivery, state: "accepted", turn_id: accepted.turn_id }] }, summary,
    { ...row, discussion: null }, { ...row, source_kind: "unknown" as const, source_id: undefined },
    { ...row, deliveries: [{ ...accepted, recipient_id: "wrong-member" }] },
    { ...row, deliveries: [{ ...accepted, conversation_id: "" }] },
    { ...row, deliveries: [{ ...accepted, turn_id: "" }] }, { ...row, deliveries: [delivery] },
    { ...row, deliveries: [unclaimed] }, { ...row, deliveries: [{ ...failed, turn_id: " " }] },
    { ...row, deliveries: [{ ...failed, error: " " }] }, { ...row, deliveries: [{ ...accepted, error: "有投递错误" }] },
    { ...row, deliveries: [{ ...accepted, state: "future" }] }, { ...row, discussion_closure_reason: interrupted }]) {
    expect(decodeMessage({ ...unproved, discussion_work: work })).toEqual(decodeMessage(unproved));
  }
});

it("preserves known and future delivery states as data without promoting acknowledgements", async () => {
  const receipt = { ...message, deliveries: ["queued", "accepted", "failed", "future-state"].map((state, index) => ({ ...delivery, id: `delivery-${index}`, state })) };
  transport.mockResolvedValueOnce(json(receipt, 201));
  expect(await sendMessage(group.id, { content }, "key")).toEqual(receipt);
});
it.each([[503, "unavailable", false], [413, "body_too_large", true], [415, "unsupported_media_type", true]] as const)(
  "retains message rejection status/code HTTP %s and its uncertainty classification", async (status, code, definitive) => {
    transport.mockResolvedValueOnce(json({ error: { code, message: "请求未完成" } }, status));
    const error: unknown = await sendMessage(group.id, { content }, "key").catch(error => error);
    expect(error).toMatchObject({ status, code });
    expect(isDefinitiveRejection(error)).toBe(definitive);
    expect(transport).toHaveBeenCalledTimes(1);
  },
);
