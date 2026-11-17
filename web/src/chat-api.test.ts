import { afterEach, expect, it, vi } from "vitest";
import { getChatAdmission, getChatPolicy, isChatPolicyDefinitiveRejection, setChatPolicy } from "./chat-api";
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

it("omits false workspace input without changing either v17 or v16 bytes", async () => {
  const old = { enabled: true, allow_preparation: false, preauthorization_message_id: "", action: "text_submission" as const, max_admissions: 2, expected_revision: 0 };
  const transport = vi.fn<typeof fetch>().mockImplementation(async () => json(policy)); vi.stubGlobal("fetch", transport);
  await setChatPolicy(group.id, { ...input, allow_workspace_changes: false }, "v17-key");
  await setChatPolicy(group.id, { ...old, allow_workspace_changes: false }, "v16-key");
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
