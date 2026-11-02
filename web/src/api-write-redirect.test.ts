// @vitest-environment node
import { once } from "node:events";
import { createServer } from "node:http";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, createGroup, createMember, isDefinitiveRejection, listGroups, sendMessage } from "./api";
import type { Message } from "./api";
import { isRequestWorkDefinitiveRejection, requestWork } from "./coordination-api";
import type { AdmissionReceipt, RequestWorkInput } from "./coordination-api";
import { group } from "./test/groups";
import { member } from "./test/members";

const nativeFetch = globalThis.fetch;
const key = "stable-write-key";
const groupInput = { name: group.name, description: group.description };
const memberInput = { name: member.name, role: member.role };
const messageInput = { content: "  原始消息\n第二行\t  ", recipient_ids: [member.id], topic_id: "topic+/1" };
const message: Message = {
  id: "message-1", group_id: group.id, topic_id: messageInput.topic_id, sender_id: "user",
  content: messageInput.content, created_at: group.created_at,
  deliveries: [{ id: "delivery-1", message_id: "message-1", conversation_id: "conversation-1", recipient_id: member.id,
    state: "queued", turn_id: "", error: "", created_at: group.created_at }],
};
const workInput: RequestWorkInput = {
  topic_id: "topic+/1", message_id: message.id, mode: "create_new", reason: " 显式授权。 \n",
  new_task: { title: "任务目标", description: "原始描述", acceptance: "验收标准", assignee_id: member.id, dependency_ids: [] },
};
const workReceipt: AdmissionReceipt = {
  decision: {
    id: "decision-1", trigger_id: "trigger-1", group_id: group.id, topic_id: workInput.topic_id, kind: "create_new",
    actor_id: "user", policy_revision: "local-user-explicit/v1", reason: workInput.reason, task_id: "task-1",
    task_revision_at_admission: 1, mandate_id: "mandate-1", created_at: group.created_at,
  },
  mandate: {
    id: "mandate-1", decision_id: "decision-1", origin_trigger_id: "trigger-1", group_id: group.id,
    topic_id: workInput.topic_id, task_id: "task-1", task_revision_at_admission: 1,
    authority_kind: "local_user_explicit", authorized_by: "user", strength: "explicit", policy_revision: "local-user-explicit/v1",
    scope: { objective: workInput.new_task.title, description: workInput.new_task.description ?? "",
      acceptance: workInput.new_task.acceptance, assignee_id: workInput.new_task.assignee_id ?? "", dependency_ids: [] },
    created_at: group.created_at,
  },
  task: { id: "task-1", group_id: group.id, revision_at_admission: 1 },
};
const mutations = [
  { api: "createGroup", path: "/api/v1/groups", input: groupInput, receipt: group,
    invoke: () => createGroup(groupInput, key), definitive: isDefinitiveRejection },
  { api: "createMember", path: `/api/v1/groups/${group.id}/members`, input: memberInput, receipt: member,
    invoke: () => createMember(group.id, memberInput, key), definitive: isDefinitiveRejection },
  { api: "sendMessage", path: `/api/v1/groups/${group.id}/messages`, input: messageInput, receipt: message,
    invoke: () => sendMessage(group.id, messageInput, key), definitive: isDefinitiveRejection },
  { api: "requestWork", path: `/api/v1/groups/${group.id}/coordination/requests`, input: workInput, receipt: workReceipt,
    invoke: () => requestWork(group.id, workInput, key), definitive: isRequestWorkDefinitiveRejection },
];
type Observation = { path: string | undefined; method: string | undefined; body: string; key: string | string[] | undefined };
afterEach(() => vi.unstubAllGlobals());

it.each(mutations.flatMap(mutation => [303, 307].map(status => ({ ...mutation, status }))))(
  "$api keeps a committed HTTP $status write unknown until explicit same-key retry",
  async ({ path, input, receipt, invoke, definitive, status }) => {
    const source: Observation[] = [], target: Observation[] = [];
    const effects: unknown[] = [];
    const server = createServer((req, res) => {
      let body = "";
      req.setEncoding("utf8");
      req.on("data", chunk => { body += chunk; });
      req.on("end", () => {
        const observation = { path: req.url, method: req.method, body, key: req.headers["idempotency-key"] };
        if (req.url !== path || req.method !== "POST") {
          if (req.url === "/redirect-target") target.push(observation);
          res.writeHead(404, { "Content-Type": "application/json" }).end(JSON.stringify({ error: { code: "not_found" } }));
          return;
        }
        source.push(observation);
        if (source.length === 1) {
          effects.push(receipt);
          res.writeHead(status, { Location: "/redirect-target" }).end();
        } else if (observation.body === source[0].body && observation.key === source[0].key) {
          res.writeHead(201, { "Content-Type": "application/json" }).end(JSON.stringify(effects[0]));
        } else {
          res.writeHead(409, { "Content-Type": "application/json" }).end(JSON.stringify({ error: { code: "conflict" } }));
        }
      });
    });
    try {
      server.listen(0, "127.0.0.1");
      await once(server, "listening");
      const address = server.address();
      if (!address || typeof address === "string" || address.port === 7310) throw new Error("Unexpected loopback address");
      const base = `http://127.0.0.1:${address.port}`;
      vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => nativeFetch(new URL(String(url), base), init));

      const error: unknown = await invoke().then(() => null, failure => failure);
      const sent = { path, method: "POST", body: JSON.stringify(input), key };
      expect(source).toEqual([sent]);
      expect(effects).toEqual([receipt]);
      expect(error).toBeInstanceOf(ApiError);
      expect.soft(error).toMatchObject({ status: 0, code: "disconnected" });
      expect.soft(definitive(error)).toBe(false);

      await expect(invoke()).resolves.toEqual(receipt);
      expect(source).toEqual([sent, sent]);
      expect(effects).toEqual([receipt]);
      expect.soft(target).toEqual([]);
    } finally {
      if (server.listening) await new Promise<void>((resolve, reject) => {
        server.close(error => error ? reject(error) : resolve());
        server.closeAllConnections();
      });
    }
  },
);

it.each([303, 307])("preserves native GET redirect following for HTTP %s", async status => {
  const seen: Observation[] = [];
  const server = createServer((req, res) => {
    seen.push({ path: req.url, method: req.method, body: "", key: req.headers["idempotency-key"] });
    if (req.url === "/api/v1/groups?limit=50") res.writeHead(status, { Location: "/get-target" }).end();
    else res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ items: [group], next_cursor: "" }));
  });
  try {
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const address = server.address();
    if (!address || typeof address === "string" || address.port === 7310) throw new Error("Unexpected loopback address");
    const base = `http://127.0.0.1:${address.port}`;
    const redirects: (RequestRedirect | undefined)[] = [];
    vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => {
      redirects.push(init?.redirect);
      return nativeFetch(new URL(String(url), base), init);
    });
    await expect(listGroups()).resolves.toEqual({ items: [group], next_cursor: "" });
    expect(seen).toEqual([
      { path: "/api/v1/groups?limit=50", method: "GET", body: "", key: undefined },
      { path: "/get-target", method: "GET", body: "", key: undefined },
    ]);
    expect(redirects).toEqual([undefined]);
  } finally {
    if (server.listening) await new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve());
      server.closeAllConnections();
    });
  }
});
