import { afterEach, expect, it, vi } from "vitest";
import { getChatAdmission, getChatPolicy, isChatPolicyDefinitiveRejection, setChatPolicy } from "./chat-api";
import type { ChatPolicyInput } from "./chat-api";
import { group, json } from "./test/groups";
import { task } from "./test/tasks";
import { chatAdmission } from "./test/chat";

const input = { enabled: true, allow_preparation: false, action: "text_submission" as const, max_admissions: 2, expected_revision: 0 };
const policy = { group_id: group.id, topic_id: "", enabled: true, allow_preparation: false, preauthorization_message_id: "",
  action: "text_submission", max_admissions: 2, revision: 1, authorized_by: "user", preparation_supported: false,
  configuration_source_id: "policy-command-1" };
afterEach(() => vi.unstubAllGlobals());

const workspaceScope = {
  group_id: group.id, topic_id: "", task_id: task.id, task_revision: 1,
  mandate_id: chatAdmission.mandate_id, candidate_id: chatAdmission.candidate_id, assessment_id: chatAdmission.id,
  dispatch_request_id: chatAdmission.dispatch_request_id, policy_revision: chatAdmission.policy_revision,
  resource_key: "workspace:device:inode", node_id: "local",
  grants: [{ grant_id: "grant-1", member_id: "worker-1", root: "/existing/project", resolved_root: "/existing/project", grant_schema_version: 19 }],
};
const workspaceAdmission = { ...chatAdmission, action: "workspace_change", workspace_scope: workspaceScope };

const repairLink = {
  id: "repair-link-1", group_id: group.id, topic_id: "", repair_task_id: task.id, parent_task_id: "parent-task",
  parent_submission_id: "parent-submission", parent_review_id: "parent-review", reviewed_task_revision: 7,
  parent_mandate_id: "parent-mandate", candidate_id: chatAdmission.candidate_id, assessment_id: chatAdmission.id,
  mandate_id: chatAdmission.mandate_id, dispatch_request_id: chatAdmission.dispatch_request_id, policy_revision: chatAdmission.policy_revision,
  authorization: "review_request", authorization_source_kind: "local_user_review", authorization_source_id: "parent-review",
  authorization_policy_revision: chatAdmission.policy_revision,
  artifact_versions: [{ artifact_id: "artifact /+?雪", version_id: "frozen /+?旧" }], created_at: "2026-11-17T14:00:01.123456789Z",
};
const reviewRepair = { ...chatAdmission, source_kind: "platform", source_id: "review:parent-review", source_sender_id: "notice-sender",
  purpose: "chat_intake/v1", authority_evidence_verified: true, outcome: "admitted", authorization_message_id: "", repair_link: repairLink };
const chatRepair = { ...reviewRepair, source_kind: "local_user_chat", source_sender_id: "user", source_id: "current-chat-command",
  authorization_message_id: chatAdmission.message_id, repair_link: { ...repairLink, authorization: "chat_request",
    authorization_source_kind: "local_user_chat", authorization_source_id: "current-chat-command", authorization_message_id: chatAdmission.message_id } };
const policyRepair = { ...reviewRepair, authority: "written_preauthorization", source_sender_id: "user", authorization_message_id: "original-policy-message",
  repair_link: { ...repairLink, authorization: "original_agreement", authorization_source_kind: "local_user_policy",
    authorization_source_id: "original-policy-command", authorization_message_id: "original-policy-message", authorization_policy_revision: 1, artifact_versions: [] } };
async function rejectRepair(value: unknown, label = "invalid repair receipt") {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(value)));
  await expect(getChatAdmission(group.id, task.id), label).rejects.toMatchObject({ code: "invalid_response" });
}
it.each([
  ["review_request workspace", { ...reviewRepair, action: "workspace_change", workspace_scope: workspaceScope }],
  ["chat_request", chatRepair],
  ["original_agreement chat", { ...reviewRepair, authorization_message_id: "original-chat-message", repair_link: { ...repairLink,
    authorization: "original_agreement", authorization_source_kind: "local_user_chat", authorization_source_id: "original-chat-command",
    authorization_message_id: "original-chat-message", authorization_policy_revision: 1, artifact_versions: [] } }],
  ["original_agreement policy", policyRepair],
  ["original_agreement earlier Review", { ...reviewRepair, repair_link: { ...repairLink, authorization: "original_agreement",
    authorization_source_id: "earlier-original-review", authorization_policy_revision: 2 } }],
] as const)("repair: accepts exact %s evidence without promoting the platform sender", async (_, receipt) => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(receipt)));
  await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(receipt);
});
it("repair: preserves exact nonempty topic, independent timestamp and distinct versions of one Artifact", async () => {
  const topic = "话题 /+? ", receipt = { ...reviewRepair, topic_id: topic, repair_link: { ...repairLink, topic_id: topic,
    artifact_versions: [repairLink.artifact_versions[0], { ...repairLink.artifact_versions[0], version_id: "another-frozen-version" }] } };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(receipt)));
  await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(receipt);
});
it("repair: requires every frozen field and rejects unknown fields rather than widening authority", async () => {
  for (const field of Object.keys(repairLink)) {
    const link: Record<string, unknown> = { ...repairLink }; delete link[field];
    await rejectRepair({ ...reviewRepair, repair_link: link }, `missing ${field}`);
  }
  for (const link of [null, [], "review", { ...repairLink, authorized_by: "user" }, { ...repairLink, member_id: "worker-1" }])
    await rejectRepair({ ...reviewRepair, repair_link: link });
  await rejectRepair({ ...reviewRepair, repair_source: { review_id: repairLink.parent_review_id } });
});
it("repair: rejects foreign enclosing IDs, topic or Policy and reuse of the parent Task", async () => {
  for (const field of ["group_id", "topic_id", "repair_task_id", "candidate_id", "assessment_id", "mandate_id", "dispatch_request_id", "parent_review_id"])
    await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, [field]: "foreign" } }, `foreign ${field}`);
  await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, policy_revision: 4 } });
  await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, parent_task_id: task.id } });
  const missingTopic: Record<string, unknown> = { ...reviewRepair }; delete missingTopic.topic_id; await rejectRepair(missingTopic);
});
it("repair: rejects malformed IDs, nonpositive or unsafe revisions and invalid UTC instants", async () => {
  for (const field of ["id", "parent_task_id", "parent_submission_id", "parent_review_id", "parent_mandate_id", "authorization_source_id"])
    for (const value of ["", " ", "id\n", 7, null]) await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, [field]: value } }, field);
  for (const field of ["reviewed_task_revision", "authorization_policy_revision"])
    for (const value of [0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "3", null]) await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, [field]: value } }, field);
  for (const created_at of ["", "2026-02-30T00:00:00Z", "2026-11-17T24:00:00Z", "2026-11-17T14:00:00+00:00", 7, null])
    await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, created_at } });
});
it("repair: enforces all three authorization combinations, real source kinds, exact messages and revisions", async () => {
  for (const change of [
    { authorization: "explicit_review_request" }, { authorization_source_kind: "platform" }, { authorization_source_id: "other-review" },
    { authorization_message_id: "" }, { authorization_message_id: chatAdmission.message_id }, { authorization_policy_revision: 2 },
  ]) await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, ...change } });
  for (const change of [
    { authorization_source_kind: "local_user_review" }, { authorization_source_id: "wrong-chat-command" },
    { authorization_message_id: "wrong-message" }, { authorization_message_id: "" }, { authorization_policy_revision: 2 },
  ]) await rejectRepair({ ...chatRepair, repair_link: { ...chatRepair.repair_link, ...change } });
  for (const change of [{ source_kind: "platform" }, { source_sender_id: "member-agent" }, { authorization_message_id: "other-message" }])
    await rejectRepair({ ...chatRepair, ...change });
  await rejectRepair({ ...policyRepair, authority: "explicit_request" });
  await rejectRepair({ ...policyRepair, authorization_message_id: chatAdmission.message_id });
  await rejectRepair({ ...policyRepair, repair_link: { ...policyRepair.repair_link, authorization_message_id: "" } });
  await rejectRepair({ ...reviewRepair, authority: "written_preauthorization" });
  await rejectRepair({ ...reviewRepair, authorization_message_id: chatAdmission.message_id });
});
it("repair: admits only bound verified request_work metadata and preserves first-workspace scope fences", async () => {
  for (const change of [
    { disposition: "prepare_work" }, { authority: "explicit_review_request" }, { outcome: "" }, { outcome: undefined },
    { authority_evidence_verified: false }, { authority_evidence_verified: undefined }, { purpose: "" }, { purpose: undefined },
    { source_kind: "local_user_review" }, { source_kind: "member", source_sender_id: "member-agent" }, { source_kind: "unknown" },
    { source_kind: "local_user_chat", source_sender_id: "user", source_id: "current-chat" }, { source_id: "review:foreign-review" },
  ]) await rejectRepair({ ...reviewRepair, ...change });
  await rejectRepair({ ...reviewRepair, workspace_scope: workspaceScope });
  await rejectRepair({ ...reviewRepair, action: "workspace_change" });
  await rejectRepair({ ...reviewRepair, action: "workspace_change", workspace_scope: { ...workspaceScope, grants: [] } });
  await rejectRepair({ ...reviewRepair, action: "workspace_change", workspace_scope: workspaceScope, repair_link: { ...repairLink, artifact_versions: [] } });
});
it("repair: requires exact unique frozen Artifact/Version pairs without accepting mutable head or path segments", async () => {
  for (const artifact_versions of [null, {}, [null], [repairLink.artifact_versions[0], repairLink.artifact_versions[0]],
    [{ artifact_id: "", version_id: "frozen" }], [{ artifact_id: "artifact", version_id: ".." }],
    [{ artifact_id: ".", version_id: "version" }], [{ artifact_id: "artifact", version_id: "" }],
    [{ artifact_id: "artifact", version_id: "version", head: true }], [{ artifact_id: "artifact", head_version_id: "head" }]])
    await rejectRepair({ ...reviewRepair, repair_link: { ...repairLink, artifact_versions } });
});
it("repair: rejects duplicate and escaped-equivalent new keys before JSON can erase the ambiguity", async () => {
  const raw = JSON.stringify(reviewRepair);
  for (const ambiguous of [
    raw.replace('"repair_link":', '"repair_link":null,"repair_link":'),
    raw.replace('"repair_link":', String.raw`"repair_l\u0069nk":null,"repair_link":`),
    raw.replace('"authorization":"review_request"', '"authorization":"chat_request","authorization":"review_request"'),
    raw.replace('"authorization_source_id":"parent-review"', String.raw`"authorization_source_\u0069d":"foreign","authorization_source_id":"parent-review"`),
    raw.replace('"version_id":', '"version_id":"head","version_id":'),
    raw.replace('"reviewed_task_revision":7', '"reviewed_task_revision":7.0000000000000001'),
  ]) {
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(ambiguous, { status: 200 })));
    await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ code: "invalid_response" });
  }
});
it("repair: never relaxes legacy platform, Agent or empty-message authority without a validated link", async () => {
  for (const receipt of [
    { ...reviewRepair, repair_link: undefined }, { ...reviewRepair, repair_link: undefined, source_sender_id: "user" },
    { ...chatAdmission, authorization_message_id: "" }, { ...chatAdmission, source_sender_id: "member-agent" },
    { ...reviewRepair, repair_link: null, source_sender_id: "user", authorization_message_id: chatAdmission.message_id },
  ]) await rejectRepair(receipt);
});

const investigationScope = { objective: "  核对讨论中未决事项  ", allow_topic_messages: false,
  artifact_versions: [{ artifact_id: "成果 /+?雪", version_id: "准确版本 /+?旧" }, { artifact_id: "成果 /+?雪", version_id: "另一个版本" }],
  max_delegations: 4 };
const topicInvestigation = { ...investigationScope, allow_topic_messages: true, artifact_versions: null, max_delegations: 0 };
const investigationAdmission = { ...chatAdmission, action: "read_only_investigation", disposition: "prepare_work",
  authority: "written_preauthorization", authorization_message_id: "policy-rule", purpose: "chat_intake/v1", outcome: "admitted",
  source_kind: "member", source_id: "member-command", source_sender_id: "member-1", authority_evidence_verified: true };

it.each([
  { automatic_text_dispatch: true }, { enabled: false, automatic_text_dispatch: true }, { automatic_text_dispatch: false },
])("reads the optional automatic text policy flag %j", async change => {
  const actual = { ...policy, ...change };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)));
  await expect(getChatPolicy(group.id)).resolves.toEqual(actual);
});
it("requires an existing, source-bound policy and a boolean for automatic text dispatch", async () => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const change of [
    { automatic_text_dispatch: "true" }, { automatic_text_dispatch: null }, { automatic_text_dispatch: 1 },
    { enabled: false, revision: 0, authorized_by: "", action: "", max_admissions: 0, configuration_source_id: "" },
    { configuration_source_id: undefined, preparation_supported: undefined }, { configuration_source_id: "" },
    { configuration_source_id: "invalid\nsource" }, { group_id: "foreign-group" }, { topic_id: "foreign-topic" },
  ]) {
    transport.mockResolvedValueOnce(json({ ...policy, automatic_text_dispatch: true, ...change }));
    await expect(getChatPolicy(group.id), JSON.stringify(change)).rejects.toMatchObject({ code: "invalid_response" });
  }
});
it("sends an automatic text opt-in even on a disabled policy and checks the receipt's flag", async () => {
  const command = { ...input, enabled: false, automatic_text_dispatch: true };
  const actual = { ...policy, enabled: false, automatic_text_dispatch: true };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, command, "automatic-text")).resolves.toEqual(actual);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(command));
  for (const [sent, received] of [[true, undefined], [true, false], [false, true]] as const) {
    transport.mockResolvedValueOnce(json({ ...actual, automatic_text_dispatch: received }));
    const error = await setChatPolicy(group.id, { ...command, automatic_text_dispatch: sent }, "automatic-text").catch(error => error);
    expect(error).toMatchObject({ code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
  }
});
it("rejects non-boolean automatic text inputs and legacy opt-ins before fetch", async () => {
  const transport = vi.fn<typeof fetch>().mockImplementation(async () => json(policy)); vi.stubGlobal("fetch", transport);
  for (const automatic_text_dispatch of ["true", null, 1]) {
    const malformed = { ...input, automatic_text_dispatch } as unknown as ChatPolicyInput;
    await expect(setChatPolicy(group.id, malformed, "malformed-opt-in")).rejects.toMatchObject({ code: "invalid_input" });
  }
  await expect(setChatPolicy(group.id, { ...input, preauthorization_message_id: "", automatic_text_dispatch: true }, "legacy-opt-in"))
    .rejects.toMatchObject({ code: "invalid_input" });
  expect(transport).not.toHaveBeenCalled();
});

const automaticTextAdmission = { ...chatAdmission, source_kind: "local_user_chat", source_id: "current-chat-command",
  purpose: "chat_intake/v1", outcome: "admitted", authority_evidence_verified: true };
it("reads automatic text routing on verified current text work and accepts explicit false on a legacy receipt", async () => {
  const checked = { ...automaticTextAdmission, automatic_text_routing_checked: true };
  const unchecked = { ...chatAdmission, automatic_text_routing_checked: false };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(checked)).mockResolvedValueOnce(json(unchecked)));
  await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(checked);
  await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(unchecked);
});
it("rejects malformed or unbound automatic text routing evidence", async () => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const change of [
    { automatic_text_routing_checked: "true" }, { automatic_text_routing_checked: null }, { automatic_text_routing_checked: 1 },
    { outcome: undefined }, { outcome: "continued" }, { purpose: "" }, { authority_evidence_verified: false },
    { source_kind: "local_user_policy" }, { source_id: "" }, { source_sender_id: "member-1" },
    { authorization_message_id: "earlier-message" }, { authority: "written_preauthorization" },
    { disposition: "prepare_work", authority: "written_preauthorization", authorization_message_id: "policy-rule" },
    { task_id: "foreign-task" }, { group_id: "foreign-group" },
    { action: "workspace_change", workspace_scope: workspaceScope }, investigationAdmission, chatRepair,
  ]) {
    transport.mockResolvedValueOnce(json({ ...automaticTextAdmission, automatic_text_routing_checked: true, ...change }));
    await expect(getChatAdmission(group.id, task.id), JSON.stringify(change)).rejects.toMatchObject({ code: "invalid_response" });
  }
});

it("reads optional investigation scope and capability observations separately without upgrading an old reply", async () => {
  const replies = [policy, { ...policy, investigation: null, investigation_supported: true, investigation_execution_enabled: false },
    { ...policy, enabled: false, investigation: topicInvestigation },
    { ...policy, investigation: investigationScope, investigation_supported: false, investigation_execution_enabled: true },
    { ...policy, investigation: { ...investigationScope, objective: "字".repeat(85) + "x",
      artifact_versions: Array.from({ length: 16 }, (_, n) => ({ artifact_id: "雪".repeat(42) + "xy",
        version_id: "版".repeat(42) + String(n).padStart(2, "0") })) } }];
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const reply of replies) {
    transport.mockResolvedValueOnce(json(reply));
    await expect(getChatPolicy(group.id)).resolves.toEqual(reply);
  }
});
it("rejects malformed investigation scope and capability fields in policy reads", async () => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const change of [
    { investigation_supported: "true" }, { investigation_execution_enabled: null },
    { configuration_source_id: "" }, { revision: 0 }, { investigation: true },
    ...[
      { objective: " \t\u3000" }, { objective: "目标\0" }, { objective: "字".repeat(86) }, { objective: "\ud800" },
      { allow_topic_messages: 1 }, { artifact_versions: undefined, allow_topic_messages: true }, { artifact_versions: null },
      { artifact_versions: [] }, { max_delegations: -1 }, { max_delegations: 5 }, { max_delegations: 1.5 }, { path: "/unapproved" },
      { artifact_versions: Array.from({ length: 17 }, (_, n) => ({ artifact_id: "成果", version_id: String(n) })) },
      { artifact_versions: [...investigationScope.artifact_versions, investigationScope.artifact_versions[0]] },
      ...[{ version_id: " " }, { version_id: "雪".repeat(43) }, { artifact_id: "\tartifact" }, { artifact_id: "a\nb" },
        { version_id: "a\0b" }, { version_id: undefined, head: true }, { group_id: "other-group" }].map(ref =>
        ({ artifact_versions: [{ ...investigationScope.artifact_versions[0], ...ref }] })),
    ].map(scope => ({ investigation: { ...investigationScope, ...scope } })),
  ]) {
    transport.mockResolvedValueOnce(json({ ...policy, investigation: investigationScope, ...change }));
    await expect(getChatPolicy(group.id), JSON.stringify(change)).rejects.toMatchObject({ code: "invalid_response" });
  }
  const raw = JSON.stringify({ ...policy, investigation: investigationScope });
  for (const ambiguous of [raw.replace('"version_id":', String.raw`"version_\u0069d":"other","version_id":`),
    raw.replace('"max_delegations":4', '"max_delegations":4.0000000000000001')]) {
    transport.mockResolvedValueOnce(new Response(ambiguous, { status: 200 }));
    await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
  }
});
it("PUT sends only the owned fields and requires the exact investigation scope on its receipt", async () => {
  const actual = { ...policy, investigation: investigationScope, investigation_supported: true, investigation_execution_enabled: false };
  const command = { ...input, investigation: investigationScope };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)); vi.stubGlobal("fetch", transport);
  const withClaims = { ...command, investigation_supported: true, investigation_execution_enabled: true,
    source_id: "raw-chat-source", configuration_source_id: "raw-config-source", authorized_by: "user", outcome: "admitted" };
  await expect(setChatPolicy(group.id, withClaims, "investigation-opt-in")).resolves.toEqual(actual);
  const [url, init] = transport.mock.calls[0];
  expect(url).toBe(`/api/v1/groups/${group.id}/coordination/chat-policy`);
  expect(init?.method).toBe("PUT"); expect(new Headers(init?.headers).get("Idempotency-Key")).toBe("investigation-opt-in");
  expect(JSON.parse(String(init?.body))).toEqual(command);
  for (const investigation of [undefined, null, { ...investigationScope, objective: investigationScope.objective.trim() },
    { ...investigationScope, allow_topic_messages: true }, { ...investigationScope, max_delegations: 3 },
    { ...investigationScope, artifact_versions: investigationScope.artifact_versions.slice().reverse() },
    { ...investigationScope, artifact_versions: [{ ...investigationScope.artifact_versions[0], version_id: "foreign" }] }]) {
    transport.mockResolvedValueOnce(json({ ...actual, investigation }));
    const error = await setChatPolicy(group.id, command, "investigation-opt-in").catch(error => error);
    expect(error).toMatchObject({ code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
  }
});
it("reads the bounded fbe95f8 combined witness on GET and on a matching PUT receipt", async () => {
  // B uses Go json.Marshal for both the scope inside the rule and the outer WriteJSON body.
  const goJSON = (value: unknown) => JSON.stringify(value).replace(/[<>&\u2028\u2029]/gu,
    char => `\\u${char.charCodeAt(0).toString(16).padStart(4, "0")}`);
  const investigation = { objective: "<".repeat(256), allow_topic_messages: false,
    artifact_versions: Array.from({ length: 16 }, (_, n) => ({ artifact_id: "<".repeat(128),
      version_id: "<".repeat(124) + n.toString(2).padStart(4, "0").replace(/[01]/g, bit => bit === "0" ? "<" : ">") })),
    max_delegations: 4 };
  const preauthorization_text = "本机用户已配置本群、本话题聊天准入策略：启用=false；允许纯文本交付，以及真实用户明确请求且已有读写目录授权的目录工作；本版总准入额度=100。"
    + "\n预授权范围 topic_summary：仅依据本群本话题已有消息准备纯文本摘要；目标与验收由服务端固定。不得改为其他目标、引用Artifact版本、读写文件或执行外部动作。"
    + "\n预授权范围 read_only_investigation：只读调查并交付纯文本；只允许以下明确配置的资料，不授予文件、进程或网络权限。\n" + goJSON(investigation);
  const grants = { enabled: false, allow_preparation: true, preparation_scope_kind: "topic_summary" as const,
    allow_workspace_changes: true, max_admissions: 100, investigation };
  const actual = { ...policy, ...grants, preauthorization_text, preauthorization_message_id: "normative-command-message-id",
    preparation_supported: true, workspace_changes_supported: true, investigation_supported: true, investigation_execution_enabled: false };
  const raw = goJSON(actual) + "\n";
  expect(new TextEncoder().encode(preauthorization_text).length).toBe(27334);
  expect(raw).toContain(String.raw`"investigation":{"objective":"\u003c`);
  expect(raw).toContain(String.raw`\"objective\":\"\\u003c`);
  const transport = vi.fn<typeof fetch>().mockImplementation(async () =>
    new Response(raw, { status: 200, headers: { "Content-Type": "application/json; charset=utf-8" } }));
  vi.stubGlobal("fetch", transport);
  await expect(getChatPolicy(group.id)).resolves.toEqual(actual);
  await expect(setChatPolicy(group.id, { ...input, ...grants }, "combined-exact-scope")).resolves.toEqual(actual);
});
it("limits combined preauthorization text to 32 KiB and keeps the 8192-byte limit for other shapes", async () => {
  const canonical = { ...policy, allow_preparation: true, preparation_scope_kind: "topic_summary", preparation_supported: true,
    preauthorization_message_id: "server-rule-id" };
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const [shape, max] of [
    [{ ...canonical, investigation: topicInvestigation }, 32 * 1024],
    [canonical, 8192],
    [{ ...canonical, preparation_scope_kind: "", preparation_supported: false, investigation: topicInvestigation }, 8192],
  ] as const) {
    const preauthorization_text = "字".repeat(Math.floor(max / 3)) + "x".repeat(max % 3);
    transport.mockResolvedValueOnce(json({ ...shape, preauthorization_text }));
    await expect(getChatPolicy(group.id)).resolves.toMatchObject({ preauthorization_text });
    transport.mockResolvedValueOnce(json({ ...shape, preauthorization_text: preauthorization_text + "x" }));
    await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
  }
});
it("accepts Go's nil version list for topic-only scope and omits absent scope on a legacy-compatible save", async () => {
  const staged = { ...policy, investigation: topicInvestigation };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(staged)).mockResolvedValueOnce(json({ ...policy, investigation: null }))
    .mockResolvedValueOnce(json(staged)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, { ...input, investigation: { ...topicInvestigation, artifact_versions: [] } }, "topic-only")).resolves.toEqual(staged);
  const off = { ...input, investigation: null };
  await expect(setChatPolicy(group.id, off, "investigation-off")).resolves.toEqual({ ...policy, investigation: null });
  expect(transport.mock.calls[1][1]?.body).toBe(JSON.stringify(input));
  await expect(setChatPolicy(group.id, input, "legacy-off")).rejects.toMatchObject({ code: "invalid_response" });
});
it("rejects invalid investigation input before fetch", async () => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const investigation of [{ ...investigationScope, objective: "字".repeat(86) }, { ...investigationScope, artifact_versions: [] },
    { ...investigationScope, artifact_versions: Array(1) as typeof investigationScope.artifact_versions },
    { ...investigationScope, artifact_versions: [{ ...investigationScope.artifact_versions[0], current: true }] }]) {
    const command = { ...input, investigation };
    await expect(setChatPolicy(group.id, command, "invalid-investigation")).rejects.toMatchObject({ code: "invalid_input" });
  }
  expect(transport).not.toHaveBeenCalled();
});
it("reads verified investigation preparation from a member or local chat without inventing scope fields", async () => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const receipt of [investigationAdmission, { ...investigationAdmission, topic_id: "话题 /+? ",
    source_kind: "local_user_chat", source_id: "chat-command", source_sender_id: "user", created_at: "2028-02-29T00:00:00.123456789Z" }]) {
    transport.mockResolvedValueOnce(json(receipt)); await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(receipt);
  }
});
it.each(["2026-02-30T00:00:00Z", "2026-11-17T24:00:00Z"])("rejects normalized calendar timestamp %s for investigation only", async created_at => {
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...investigationAdmission, created_at }));
  vi.stubGlobal("fetch", transport);
  await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ code: "invalid_response" });
  for (const legacy of [chatAdmission, workspaceAdmission]) {
    const receipt = { ...legacy, created_at };
    transport.mockResolvedValueOnce(json(receipt));
    await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(receipt);
  }
});
it("requires exact task/group and genuine written preauthorization for investigation admission", async () => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  for (const change of [{ task_id: "other-task" }, { group_id: "other-group" }, { disposition: "request_work" },
    { authority: "explicit_request" }, { authorization_message_id: "" }, { authorization_message_id: investigationAdmission.message_id },
    { authority_evidence_verified: undefined }, { authority_evidence_verified: false }, { outcome: undefined }, { purpose: "" },
    { source_kind: "unknown", source_id: "" }, { source_kind: "platform" }, { source_kind: "local_user_policy", source_sender_id: "user" },
    { source_kind: "local_user_chat" }, { source_sender_id: "user" }, { source_id: "" },
    { workspace_scope: workspaceScope }, { repair_link: repairLink }, { investigation_scope: investigationScope }]) {
    transport.mockResolvedValueOnce(json({ ...investigationAdmission, ...change }));
    await expect(getChatAdmission(group.id, task.id), JSON.stringify(change)).rejects.toMatchObject({ code: "invalid_response" });
  }
});

it("reads supported default-false workspace policy separately from an old instance", async () => {
  const actual = { ...policy, workspace_changes_supported: true };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)).mockResolvedValueOnce(json(policy)));
  await expect(getChatPolicy(group.id)).resolves.toEqual(actual);
  await expect(getChatPolicy(group.id)).resolves.toEqual(policy);
});
it.each([
  { allow_workspace_changes: "true" }, { workspace_changes_supported: 1 },
  { allow_workspace_changes: true, workspace_changes_supported: false },
])("rejects malformed or contradictory workspace policy metadata %j", async change => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...policy, ...change })));
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it("sends only an explicit true workspace extension and requires a capability-bound matching receipt", async () => {
  const actual = { ...policy, workspace_changes_supported: true, allow_workspace_changes: true };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)).mockResolvedValueOnce(json(policy)); vi.stubGlobal("fetch", transport);
  const command = { ...input, allow_workspace_changes: true };
  await expect(setChatPolicy(group.id, command, "workspace-opt-in")).resolves.toEqual(actual);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(command));
  const error = await setChatPolicy(group.id, command, "workspace-opt-in").catch(error => error);
  expect(error).toMatchObject({ code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
});
it("does not accept duplicate or escaped-equivalent workspace capability metadata as support", async () => {
  const raw = JSON.stringify({ ...policy, workspace_changes_supported: true })
    .replace('"workspace_changes_supported":true', String.raw`"workspace_changes_suppor\u0074ed":false,"workspace_changes_supported":true`);
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(raw, { status: 200 })));
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it("does not collapse duplicate workspace scope group IDs into trusted admission facts", async () => {
  const raw = JSON.stringify(workspaceAdmission).replace('"workspace_scope":{"group_id":', '"workspace_scope":{"group_id":"foreign","group_id":');
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(raw, { status: 200 })));
  await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ code: "invalid_response" });
});

it("omits false workspace and automatic text inputs without changing either v17 or v16 bytes", async () => {
  const old = { enabled: true, allow_preparation: false, preauthorization_message_id: "", action: "text_submission" as const, max_admissions: 2, expected_revision: 0 };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...policy, automatic_text_dispatch: false }))
    .mockResolvedValueOnce(json(policy)); vi.stubGlobal("fetch", transport);
  const off = { allow_workspace_changes: false, automatic_text_dispatch: false };
  await setChatPolicy(group.id, { ...input, ...off }, "v17-key");
  await setChatPolicy(group.id, { ...old, ...off }, "v16-key");
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(input)); expect(transport.mock.calls[1][1]?.body).toBe(JSON.stringify(old));
});
it("preserves an unknown v17 snapshot after a newer workspace-enabled read", async () => {
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({}, 503))
    .mockResolvedValueOnce(json({ ...policy, revision: 9, workspace_changes_supported: true, allow_workspace_changes: true }))
    .mockResolvedValueOnce(json(policy)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, input, "old-v17-key")).rejects.toMatchObject({ status: 503 }); await getChatPolicy(group.id);
  await setChatPolicy(group.id, input, "old-v17-key");
  expect(transport.mock.calls[2]).toEqual(transport.mock.calls[0]); expect(transport.mock.calls[2][1]?.body).toBe(JSON.stringify(input));
});
it("accepts a workspace admission with exact immutable scope and keeps text admissions unchanged", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(workspaceAdmission)).mockResolvedValueOnce(json(chatAdmission)));
  await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(workspaceAdmission);
  await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(chatAdmission);
});
it.each([
  { group_id: "other-group" }, { topic_id: "wrong-topic" }, { task_id: "wrong-task" }, { task_revision: 0 },
  { mandate_id: "other-mandate" }, { candidate_id: "other-candidate" }, { assessment_id: "other-assessment" },
  { dispatch_request_id: "other-request" }, { policy_revision: 99 }, { node_id: "remote" }, { resource_key: "path-not-resource" },
  { grants: [] }, { grants: [workspaceScope.grants[0], workspaceScope.grants[0]] },
  { grants: [{ ...workspaceScope.grants[0], root: "relative" }] },
  { grants: [{ ...workspaceScope.grants[0], grant_schema_version: 18 }] }, { proposedRoot: "/not-authority" },
])("rejects a workspace scope with malformed or foreign provenance %j", async change => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...workspaceAdmission, workspace_scope: { ...workspaceScope, ...change } })));
  await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([
  { ...workspaceAdmission, workspace_scope: undefined },
  { ...chatAdmission, workspace_scope: workspaceScope },
  { ...workspaceAdmission, disposition: "prepare_work", authority: "written_preauthorization" },
])("rejects scope/action or preparation contradictions %j", async value => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(value)));
  await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ code: "invalid_response" });
});

it("uses an exact optional topic, the standard idempotency header and only the owned PUT DTO", async () => {
  const topic = " topic /+? ", transport = vi.fn<typeof fetch>().mockImplementation(async () => json({ ...policy, topic_id: topic })); vi.stubGlobal("fetch", transport);
  await getChatPolicy(group.id, undefined, topic);
  await setChatPolicy(group.id, { ...input, authorized_by: "agent", grants: ["write"], workspace_changes_supported: true } as typeof input, "exact-policy-key", topic);
  const path = `/api/v1/groups/${group.id}/coordination/chat-policy?${new URLSearchParams({ topic_id: topic })}`;
  expect(transport.mock.calls[0][0]).toBe(path); expect(transport.mock.calls[1][0]).toBe(path);
  const init = transport.mock.calls[1][1]; expect(init?.method).toBe("PUT"); expect(init?.redirect).toBe("error");
  expect(new Headers(init?.headers).get("Idempotency-Key")).toBe("exact-policy-key"); expect(JSON.parse(String(init?.body))).toEqual(input);
});
it("accepts a server-allocated canonical source instead of comparing it with an empty client ID", async () => {
  const request = { ...input, allow_preparation: true, preparation_scope_kind: "topic_summary" as const };
  const actual = { ...policy, allow_preparation: true, preparation_scope_kind: "topic_summary", preparation_supported: true,
    preauthorization_message_id: "server-rule-id", preauthorization_text: "只允许本话题已有消息的纯文本摘要" };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, request, "canonical-source-key")).resolves.toEqual(actual);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(request);
  expect(String(transport.mock.calls[0][1]?.body)).not.toContain("preauthorization_message_id");
});
it("replays a previously submitted v16 input byte-for-byte with its original key after new-scope reads", async () => {
  const old = Object.freeze({ enabled: true, allow_preparation: true, preauthorization_message_id: "legacy-message-id",
    action: "text_submission" as const, max_admissions: 2, expected_revision: 16 });
  const historical = { ...policy, allow_preparation: true, revision: 17, preauthorization_message_id: old.preauthorization_message_id,
    preauthorization_text: "历史的更窄自由文字规则", preparation_supported: false, configuration_source_id: "" };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({}, 503))
    .mockResolvedValueOnce(json({ ...policy, revision: 20 })) .mockResolvedValueOnce(json(historical)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, old, "original-v16-key")).rejects.toMatchObject({ status: 503 });
  await getChatPolicy(group.id); await expect(setChatPolicy(group.id, old, "original-v16-key")).resolves.toEqual(historical);
  const capture = (index: number) => { const [url, init] = transport.mock.calls[index]; return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }; };
  expect(capture(2)).toEqual(capture(0)); expect(capture(0).body).toBe(JSON.stringify(old)); expect(capture(2).body).not.toContain("preparation_scope_kind");
});
it("does not accept claimed preparation support without the actual rule and configuration source", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...policy, allow_preparation: true,
    preparation_scope_kind: "topic_summary", preparation_supported: true, configuration_source_id: "" })));
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it.each(["revision", "max_admissions"])("rejects a raw rounded %s before treating policy reads or PUT receipts as facts", async field => {
  const raw = JSON.stringify(policy).replace(`\"${field}\":${policy[field as "revision" | "max_admissions"]}`, `\"${field}\":${policy[field as "revision" | "max_admissions"]}.0000000000000001`);
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => new Response(raw, { status: 200 })));
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
  const error = await setChatPolicy(group.id, input, "exact-policy-key").catch(error => error);
  expect(error).toMatchObject({ code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
});
it("does not treat an unrecognized negative envelope as proof that a policy PUT failed", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => json({ error: { code: "conflict", message: "版本冲突" }, extra: "unrecognized" }, 409)));
  const error = await setChatPolicy(group.id, input, "exact-policy-key").catch(error => error);
  expect(isChatPolicyDefinitiveRejection(error)).toBe(false); expect(error).toMatchObject({ status: 409, code: "invalid_response" });
});
it("quietly supports legacy plain 404 admission reads but never treats a malformed PUT rejection as proof of failure", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => new Response("404 page not found", { status: 404 })));
  await expect(getChatAdmission(group.id, task.id)).resolves.toBeNull();
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ status: 404 });
  const error = await setChatPolicy(group.id, input, "exact-policy-key").catch(error => error);
  expect(error).toMatchObject({ status: 404, code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
});
it.each([
  { label: "unexpected code", body: JSON.stringify({ error: { code: "unexpected_code", message: "not not_found" } }), type: "application/json" },
  { label: "malformed envelope", body: JSON.stringify({ error: { code: "not_found" } }), type: "application/json" },
  { label: "extra envelope fields", body: JSON.stringify({ error: { code: "not_found", message: "missing" }, extra: true }), type: "application/json" },
  { label: "invalid JSON", body: '{"error":', type: "application/json" },
  { label: "unknown plain body", body: "not a recognizable legacy response", type: "text/plain" },
  { label: "JSON mime with plain legacy text", body: "404 page not found", type: "application/json" },
])("PUI-4 does not silence a 404 with $label", async ({ body, type }) => {
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(body, { status: 404, headers: { "Content-Type": type } }));
  vi.stubGlobal("fetch", transport);
  await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ status: 404 }); expect(transport).toHaveBeenCalledTimes(1);
});
it("PUI-4 still silences a protocol not_found and the identifiable Go plain 404", async () => {
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({ error: { code: "not_found", message: "旧任务" } }, 404))
    .mockResolvedValueOnce(new Response("404 page not found\n", { status: 404, headers: { "Content-Type": "text/plain; charset=utf-8" } }));
  vi.stubGlobal("fetch", transport);
  await expect(getChatAdmission(group.id, task.id)).resolves.toBeNull(); await expect(getChatAdmission(group.id, task.id)).resolves.toBeNull();
});
it.each([
  '{"error":{"code":"unexpected_code","code":"not_found","message":"ambiguous"}}',
  '{"error":{"code":"unexpected_code","message":"first"},"error":{"code":"not_found","message":"last"}}',
  String.raw`{"error":{"co\u0064e":"unexpected_code","code":"not_found","message":"ambiguous"}}`,
  String.raw`{"erro\u0072":{"code":"unexpected_code","message":"first"},"error":{"code":"not_found","message":"last"}}`,
  '{"error":{"code":"not_found","code":"not_found","message":"duplicate"}}',
  '{"error":{"code":"not_found","message":"first","message":"last"}}',
  String.raw`{"error":{"code":"not_found","message":"first","mess\u0061ge":"last"}}`,
])("delta-4 never silences duplicate or escaped-equivalent 404 fields %s", async raw => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(raw, { status: 404, headers: { "Content-Type": "application/json" } })));
  await expect(getChatAdmission(group.id, task.id)).rejects.toMatchObject({ status: 404 });
});
it("delta-4 accepts canonical WriteJSON escaping and key-like text inside an error message", async () => {
  const raw = String.raw`{"error":{"code":"not_found","message":"\u003c缺失\u003e \u0026 \u2028\u2029 \"code\":\"not_found\""}}` + "\n";
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(raw, { status: 404, headers: { "Content-Type": "application/json" } })));
  await expect(getChatAdmission(group.id, task.id)).resolves.toBeNull();
});

const originalAgreements = [
  ["chat", { ...reviewRepair, authorization_message_id: "original-chat-message", repair_link: { ...repairLink,
    authorization: "original_agreement", authorization_source_kind: "local_user_chat", authorization_source_id: "original-chat-command",
    authorization_message_id: "original-chat-message", authorization_policy_revision: 1, artifact_versions: [] } }],
  ["Policy", policyRepair],
  ["Review", { ...reviewRepair, repair_link: { ...repairLink, authorization: "original_agreement",
    authorization_source_id: "earlier-original-review", authorization_policy_revision: 1 } }],
] as const;
it.each(originalAgreements)("repair: accepts earlier and equal original %s policy revisions", async (_, receipt) => {
  for (const revision of [1, receipt.policy_revision]) {
    const actual = { ...receipt, repair_link: { ...receipt.repair_link, authorization_policy_revision: revision } };
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)));
    await expect(getChatAdmission(group.id, task.id)).resolves.toEqual(actual);
  }
});
it.each(originalAgreements)("repair: rejects future original %s policy revisions in the same topic", async (_, receipt) => {
  for (const topic of ["", "topic-雪"]) await rejectRepair({ ...receipt, topic_id: topic,
    repair_link: { ...receipt.repair_link, topic_id: topic, authorization_policy_revision: receipt.policy_revision + 1 } });
});
it.each([["Review", reviewRepair], ["chat", chatRepair]] as const)("repair: still requires current %s authorization to equal the receipt policy revision", async (_, receipt) => {
  for (const revision of [receipt.policy_revision - 1, receipt.policy_revision + 1])
    await rejectRepair({ ...receipt, repair_link: { ...receipt.repair_link, authorization_policy_revision: revision } });
});
