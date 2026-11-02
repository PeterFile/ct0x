import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { CoordinationSettings } from "./CoordinationSettings";
import type { Member } from "./api";

// Only the transport is replaced; every request uses the frozen C1 client.
type Kind = "coordinator" | "policy";
const kinds: Kind[] = ["coordinator", "policy"];
const labels = { coordinator: "协调员", policy: "分派策略" };
const paths = { coordinator: "coordinator", policy: "dispatch-policy" };
const reads = { coordinator: vi.fn<typeof fetch>(), policy: vi.fn<typeof fetch>() };
const puts = { coordinator: vi.fn<typeof fetch>(), policy: vi.fn<typeof fetch>() };
const members = vi.fn<typeof fetch>();
const requests = vi.fn<typeof fetch>();
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
const binding = (id = "A", revision = 1, group_id = "group-A") => ({ group_id, member_id: id, revision });
const policy = (ids: string[] = ["B"], revision = 1, group_id = "group-A") => ({ group_id, revision, allowed_worker_ids: ids });
const errorResponse = (status: number, code = "not_found") => json({ error: { code, message: "请求未完成" } }, status);
const rosterMember = (id: string, status = "active", group_id = "group-A", role = "普通职责"): Member => ({
  id, group_id, name: `${id}成员`, role, status, provider_id: "", model: "", creator_id: "user",
  owner_id: "user", workspace: "", created_at: "2026-11-02T00:00:00Z",
});
const page = (items: Member[], next_cursor = "") => json({ items, next_cursor });
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(accept => { resolve = accept; });
  return { promise, resolve };
}
const form = (kind: Kind, scope: HTMLElement = document.body) => within(scope).getByRole("form", { name: `保存${labels[kind]}` });
const current = (kind: Kind, scope: HTMLElement = document.body) => within(within(scope).getByRole("region", { name: `当前${labels[kind]}` }));
const history = (kind: Kind) => within(screen.getByRole("region", { name: `${labels[kind]}历史保存回执` }));
const save = (kind: Kind, scope?: HTMLElement) => fireEvent.submit(form(kind, scope));
const choose = (kind: Kind, id: string, scope?: HTMLElement) => fireEvent.change(within(form(kind, scope)).getByRole("combobox"), { target: { value: id } });
const remove = (id: string, scope?: HTMLElement) => fireEvent.click(within(form("policy", scope)).getByRole("button", { name: `移除 ${id}` }));
const refresh = (scope: HTMLElement = document.body) => fireEvent.click(within(scope).getByRole("button", { name: "刷新配置并保留草稿" }));
const flush = async () => { await act(async () => {}); };
const attempts = (kind: Kind) => puts[kind].mock.calls.map(([url, init]) => ({
  url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key"),
}));
async function open(groupId = "group-A") {
  const view = render(<CoordinationSettings groupId={groupId} active />);
  fireEvent.click(screen.getByRole("button", { name: "展开协调配置" }));
  await flush();
  return view;
}
function confirm(kind: Kind, value: ReturnType<typeof binding> | ReturnType<typeof policy>) {
  puts[kind].mockImplementationOnce(async () => json(value));
  reads[kind].mockImplementation(async () => json(value));
}

beforeEach(() => {
  for (const kind of kinds) {
    reads[kind].mockReset().mockImplementation(async input => {
      const group = decodeURIComponent(String(input).split("/")[4]);
      return json(kind === "coordinator" ? binding("A", 1, group) : policy(["B"], 1, group));
    });
    puts[kind].mockReset().mockRejectedValue(new TypeError("connection lost"));
  }
  members.mockReset().mockImplementation(async input => {
    const group = decodeURIComponent(String(input).split("/")[4]);
    return page(["A", "B", "C", "D"].map(id => rosterMember(id, "active", group)));
  });
  requests.mockReset().mockImplementation((input, init) => {
    const url = new URL(String(input), "http://fixture.invalid");
    if (url.pathname.endsWith("/members")) return members(input, init);
    const kind = kinds.find(value => url.pathname.endsWith(`/coordination/${paths[value]}`));
    if (!kind) throw new Error(`Unexpected request: ${init?.method ?? "GET"} ${input}`);
    return (init?.method === "PUT" ? puts[kind] : reads[kind])(input, init);
  });
  vi.stubGlobal("fetch", requests);
});
afterEach(() => vi.unstubAllGlobals());

it("reads only when expanded and active, keeping the disclosure body mounted", async () => {
  const view = render(<CoordinationSettings groupId="group-A" active={false} />);
  fireEvent.click(screen.getByRole("button", { name: "展开协调配置" }));
  expect(requests).not.toHaveBeenCalled();
  view.rerender(<CoordinationSettings groupId="group-A" active />);
  await flush();
  expect(reads.coordinator).toHaveBeenCalledTimes(1);
  expect(reads.policy).toHaveBeenCalledTimes(1);
  expect(members).toHaveBeenCalledTimes(1);
  const body = form("coordinator");
  choose("coordinator", "C");
  fireEvent.click(screen.getByRole("button", { name: "收起协调配置" }));
  expect(body).toBeInTheDocument();
  expect(body).not.toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "展开协调配置" }));
  await flush();
  expect(form("coordinator")).toBe(body);
  expect(within(body).getByRole("combobox")).toHaveValue("C");
  expect(reads.coordinator).toHaveBeenCalledTimes(2);
});

it("starts the two configuration reads and roster in parallel", async () => {
  const coordinator = deferred<Response>();
  const dispatch = deferred<Response>();
  reads.coordinator.mockReturnValueOnce(coordinator.promise);
  reads.policy.mockReturnValueOnce(dispatch.promise);
  await open();
  expect(reads.coordinator).toHaveBeenCalledTimes(1);
  expect(reads.policy).toHaveBeenCalledTimes(1);
  expect(members).toHaveBeenCalledTimes(1);
  expect(within(form("coordinator")).getByRole("button", { name: "保存协调员" })).toBeDisabled();
  await act(async () => {
    coordinator.resolve(json(binding()));
    dispatch.resolve(json(policy()));
  });
  expect(current("coordinator").getByText(/版本 1/)).toBeVisible();
});

it.each(["coordinator", "policy"] as const)("keeps %s unknown through retry 404 and 409 with the original identity", async kind => {
  await open();
  choose(kind, "C");
  save(kind);
  await screen.findByRole("button", { name: `重试同一次${labels[kind]}保存` });
  const first = attempts(kind)[0];
  expect(first.key).toMatch(/^[\da-f-]{36}$/);
  const expected = kind === "coordinator" ? { member_id: "C", expected_revision: 1 }
    : { allowed_worker_ids: ["B", "C"], expected_revision: 1 };
  expect(JSON.parse(String(first.body))).toEqual(expected);
  expect(within(form(kind)).getByRole("combobox")).toBeDisabled();
  const opposite = kind === "coordinator" ? "policy" : "coordinator";
  save(opposite);
  expect(puts[opposite]).not.toHaveBeenCalled();

  // Current reads and membership can change; neither can release the old operation.
  reads.coordinator.mockImplementation(async () => json(binding("D", 7)));
  reads.policy.mockImplementation(async () => json(policy([], 8)));
  members.mockImplementation(async () => page([rosterMember("C", "left")]));
  refresh();
  await flush();
  expect(within(form(kind)).getByRole("combobox")).toBeDisabled();
  for (const status of [404, 409]) {
    const delayed = deferred<Response>();
    puts[kind].mockReturnValueOnce(delayed.promise);
    fireEvent.click(screen.getByRole("button", { name: `重试同一次${labels[kind]}保存` }));
    save(kind);
    expect(attempts(kind)).toHaveLength(status === 404 ? 2 : 3);
    expect(screen.getByText(`${labels[kind]}：结果未知，正在重试原操作。`)).toBeVisible();
    await act(async () => delayed.resolve(errorResponse(status, status === 409 ? "conflict" : "not_found")));
    expect(screen.getByText(`${labels[kind]}：结果未知，请保留原操作。`)).toBeVisible();
    expect(within(form(kind)).getByRole("combobox")).toBeDisabled();
  }
  const receipt = kind === "coordinator" ? binding("C", 2) : policy(["C", "B"], 2);
  puts[kind].mockImplementationOnce(async () => json(receipt));
  fireEvent.click(screen.getByRole("button", { name: `重试同一次${labels[kind]}保存` }));
  await flush();
  expect(history(kind).getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(current(kind).getByText(kind === "coordinator" ? /版本 7/ : /版本 8/)).toBeVisible();
  expect(attempts(kind)).toHaveLength(4);
  expect(attempts(kind).every(attempt => JSON.stringify(attempt) === JSON.stringify(first))).toBe(true);
  expect(puts[opposite]).not.toHaveBeenCalled();
});

it.each(["coordinator", "policy"] as const)("does not release %s on a mismatched 200 or later trusted rejection", async kind => {
  await open();
  choose(kind, "C");
  puts[kind].mockImplementationOnce(async () => json(kind === "coordinator" ? binding("D", 2) : policy(["D"], 2)));
  save(kind);
  await screen.findByRole("button", { name: `重试同一次${labels[kind]}保存` });
  puts[kind].mockImplementationOnce(async () => errorResponse(400, "invalid"));
  fireEvent.click(screen.getByRole("button", { name: `重试同一次${labels[kind]}保存` }));
  await flush();
  expect(screen.getByText(`${labels[kind]}：结果未知，请保留原操作。`)).toBeVisible();
  expect(attempts(kind)[1]).toEqual(attempts(kind)[0]);
  expect(history(kind).queryByText(/本次保存已确认/)).not.toBeInTheDocument();
});

it.each(["coordinator", "policy"] as const)("freezes the %s caller snapshot while a request is pending", async kind => {
  await open();
  choose(kind, "C");
  const delayed = deferred<Response>();
  puts[kind].mockReturnValueOnce(delayed.promise);
  save(kind);
  // Even a programmatically delivered change must not mutate a frozen DTO.
  choose(kind, "D");
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  await act(async () => delayed.resolve(json({}, 503)));
  puts[kind].mockImplementationOnce(async () => errorResponse(409, "conflict"));
  fireEvent.click(screen.getByRole("button", { name: `重试同一次${labels[kind]}保存` }));
  await flush();
  expect(attempts(kind)[1]).toEqual(attempts(kind)[0]);
  const sent = JSON.parse(String(attempts(kind)[0].body));
  expect(kind === "coordinator" ? sent.member_id : sent.allowed_worker_ids).toEqual(kind === "coordinator" ? "C" : ["B", "C"]);
});

it.each(["coordinator", "policy"] as const)("lets a first definitive %s rejection be corrected without reusing its key", async kind => {
  await open();
  choose(kind, "C");
  puts[kind].mockImplementationOnce(async () => errorResponse(400, "invalid"));
  save(kind);
  await flush();
  expect(within(form(kind)).getByRole("combobox")).toBeEnabled();
  if (kind === "policy") remove("C");
  choose(kind, "D");
  save(kind);
  await flush();
  expect(attempts(kind)).toHaveLength(2);
  expect(attempts(kind)[1].key).not.toBe(attempts(kind)[0].key);
  expect(attempts(kind)[1].body).not.toEqual(attempts(kind)[0].body);
});

it.each([
  ["coordinator", 404], ["coordinator", 409], ["policy", 404], ["policy", 409],
] as const)("requires explicit fresh reads after a first %s HTTP %s rejection, preserving drafts", async (kind, status) => {
  await open();
  choose(kind, "C");
  puts[kind].mockImplementationOnce(async () => errorResponse(status, status === 404 ? "not_found" : "conflict"));
  save(kind);
  await flush();
  const first = attempts(kind)[0];
  expect(within(form(kind)).getByRole("alert")).toHaveTextContent(status === 404
    ? "未找到协调配置或关联记录" : "协调配置与当前版本或成员配置冲突");
  expect(within(form(kind)).getByRole("combobox")).toBeEnabled();
  if (kind === "policy") remove("C");
  choose(kind, "D");
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  // Reopening cannot clear a scope/version refresh requirement.
  fireEvent.click(screen.getByRole("button", { name: "收起协调配置" }));
  fireEvent.click(screen.getByRole("button", { name: "展开协调配置" }));
  await flush();
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("A", 4) : policy(["B"], 4)));
  const rosterRead = deferred<Response>();
  members.mockReturnValueOnce(rosterRead.promise);
  refresh();
  await flush();
  expect(reads.coordinator).toHaveBeenCalledTimes(3);
  expect(reads.policy).toHaveBeenCalledTimes(3);
  expect(members).toHaveBeenCalledTimes(3);
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  await act(async () => rosterRead.resolve(page(["A", "B", "C", "D"].map(id => rosterMember(id)))));
  if (kind === "coordinator") expect(within(form(kind)).getByRole("combobox")).toHaveValue("D");
  else expect(within(form(kind)).getByRole("button", { name: "移除 D" })).toBeVisible();
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  save(kind);
  await flush();
  expect(JSON.parse(String(attempts(kind)[1].body))).toEqual(kind === "coordinator"
    ? { member_id: "D", expected_revision: 4 } : { allowed_worker_ids: ["B", "D"], expected_revision: 4 });
  expect(attempts(kind)[1].url).toBe(first.url);
  expect(attempts(kind)[1].key).not.toBe(first.key);
});

it.each(["coordinator", "policy"] as const)("keeps %s receipt separate when current confirmation fails and rejects stale GETs", async kind => {
  await open();
  choose(kind, "C");
  const receipt = kind === "coordinator" ? binding("C", 2) : policy(["B", "C"], 2);
  puts[kind].mockImplementationOnce(async () => json(receipt));
  reads[kind].mockImplementation(async () => errorResponse(403, "forbidden"));
  save(kind);
  await flush();
  expect(history(kind).getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(current(kind).getByText(/当前配置尚未重新确认/)).toBeVisible();
  expect(current(kind).getByText(/版本 1/)).toBeVisible();
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("A", 1) : policy(["B"], 1)));
  refresh();
  await flush();
  expect(current(kind).getByText(/读取已过期/)).toBeVisible();
  expect(history(kind).getByText(/版本 2/)).toBeVisible();
  reads[kind].mockImplementation(async () => json(receipt));
  refresh();
  await flush();
  expect(current(kind).getByText(/版本 2/)).toBeVisible();
  expect(current(kind).queryByText(/当前配置尚未重新确认/)).not.toBeInTheDocument();
});

it.each(["coordinator", "policy"] as const)("does not let a late old %s GET overwrite a newer current read or edited draft", async kind => {
  await open();
  choose(kind, "C");
  const late = deferred<Response>();
  reads[kind].mockReturnValueOnce(late.promise);
  refresh();
  const oldSignal = reads[kind].mock.calls.at(-1)?.[1]?.signal;
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("D", 6) : policy(["D"], 6)));
  refresh();
  await flush();
  expect(oldSignal?.aborted).toBe(true);
  expect(current(kind).getByText(/版本 6/)).toBeVisible();
  await act(async () => late.resolve(json(kind === "coordinator" ? binding("A", 2) : policy(["B"], 2))));
  expect(current(kind).getByText(/版本 6/)).toBeVisible();
  if (kind === "coordinator") expect(within(form(kind)).getByRole("combobox")).toHaveValue("C");
  else expect(within(form(kind)).getByRole("button", { name: "移除 C" })).toBeVisible();
});

it.each(["coordinator", "policy"] as const)("rejects same-revision %s content changes instead of replacing current", async kind => {
  await open();
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("D", 1) : policy(["D"], 1)));
  refresh();
  await flush();
  expect(current(kind).getByText(/读取已过期|读取内容不一致/)).toBeVisible();
  expect(current(kind).getByText(kind === "coordinator" ? /A成员/ : /B成员/)).toBeVisible();
  expect(current(kind).queryByText(/D成员/)).not.toBeInTheDocument();
});

it("allows revision zero only for a valid coordinator not_found paired with the same refresh's policy 200", async () => {
  reads.coordinator.mockImplementation(async () => errorResponse(404));
  reads.policy.mockImplementation(async () => json(policy([], 0)));
  await open();
  expect(current("coordinator").getByText(/尚未绑定协调员.*版本 0/)).toBeVisible();
  choose("coordinator", "C");
  confirm("coordinator", binding("C", 1));
  save("coordinator");
  await flush();
  expect(JSON.parse(String(attempts("coordinator")[0].body))).toEqual({ member_id: "C", expected_revision: 0 });
});

it.each(["both404", "policy404", "bad404", "wrongCode", "policyWrongGroup", "policyNetwork"])("does not invent revision zero for %s", async failure => {
  reads.coordinator.mockImplementation(async () => failure === "bad404" ? json({}, 404)
    : errorResponse(404, failure === "wrongCode" ? "other" : "not_found"));
  reads.policy.mockImplementation(async () => {
    if (failure === "policyNetwork") throw new TypeError("offline");
    return failure === "both404" || failure === "policy404" ? errorResponse(404)
      : json(policy([], 0, failure === "policyWrongGroup" ? "other" : "group-A"));
  });
  await open();
  expect(current("coordinator").queryByText(/尚未绑定协调员/)).not.toBeInTheDocument();
  choose("coordinator", "C");
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
});

it.each(["coordinator", "policy"] as const)("preserves known %s through 404 without synthesizing defaults", async kind => {
  await open();
  reads[kind].mockImplementation(async () => errorResponse(404));
  refresh();
  await flush();
  expect(current(kind).getByText(/版本 1/)).toBeVisible();
  expect(current(kind).getByText(/当前配置尚未重新确认/)).toBeVisible();
  expect(current(kind).queryByText(/版本 0/)).not.toBeInTheDocument();
});

it("does not infer an unbound coordinator after a known save receipt even if GET never confirmed it", async () => {
  reads.coordinator.mockImplementation(async () => errorResponse(404));
  reads.policy.mockImplementation(async () => json(policy([], 0)));
  await open();
  choose("coordinator", "C");
  puts.coordinator.mockImplementationOnce(async () => json(binding("C", 1)));
  save("coordinator");
  await flush();
  expect(history("coordinator").getByText(/版本 1/)).toBeVisible();
  expect(current("coordinator").getByText(/当前配置尚未重新确认/)).toBeVisible();
  expect(current("coordinator").queryByText(/尚未绑定协调员.*版本 0/)).not.toBeInTheDocument();
  save("coordinator");
  expect(puts.coordinator).toHaveBeenCalledTimes(1);
});

it("keeps unknown identity through current 404 reads", async () => {
  await open();
  choose("coordinator", "C");
  save("coordinator");
  await flush();
  reads.coordinator.mockImplementation(async () => errorResponse(404));
  reads.policy.mockImplementation(async () => errorResponse(404));
  refresh();
  await flush();
  puts.coordinator.mockImplementationOnce(async () => errorResponse(409, "conflict"));
  fireEvent.click(screen.getByRole("button", { name: "重试同一次协调员保存" }));
  await flush();
  expect(attempts("coordinator")[1]).toEqual(attempts("coordinator")[0]);
  expect(screen.getByText("协调员：结果未知，请保留原操作。")).toBeVisible();
});

it("uses explicit 50-member pages, merges updated IDs, and preserves unloaded or left selections", async () => {
  const firstPage = Array.from({ length: 50 }, (_, index) => rosterMember(`m${index}`));
  members.mockImplementationOnce(async () => page(firstPage, "opaque+/=群"));
  reads.coordinator.mockImplementation(async () => json(binding("A")));
  reads.policy.mockImplementation(async () => json(policy(["legacy", "m1"])));
  await open();
  expect(members).toHaveBeenCalledTimes(1);
  expect(within(within(form("policy")).getByRole("list", { name: "已选分派成员" })).getByText(/legacy.*成员未载入/)).toBeVisible();
  save("policy");
  expect(puts.policy).not.toHaveBeenCalled();
  members.mockImplementationOnce(async () => page([
    rosterMember("m1", "left"), rosterMember("A", "active", "group-A", "worker"),
    rosterMember("legacy", "active", "group-A", "coordinator"), rosterMember("extra"),
  ]));
  fireEvent.click(screen.getByRole("button", { name: "加载更多成员" }));
  await flush();
  expect(members).toHaveBeenCalledTimes(2);
  expect(String(members.mock.calls[0][0])).toBe("/api/v1/groups/group-A/members?limit=50");
  expect(String(members.mock.calls[1][0])).toBe("/api/v1/groups/group-A/members?limit=50&before=opaque%2B%2F%3D%E7%BE%A4");
  expect(within(within(form("policy")).getByRole("list", { name: "已选分派成员" })).getByText(/m1.*已离群/)).toBeVisible();
  expect(within(form("policy")).getAllByRole("button", { name: "移除 m1" })).toHaveLength(1);
  expect(within(form("policy")).getByRole("button", { name: "移除 legacy" })).toBeVisible();
  save("policy");
  expect(puts.policy).not.toHaveBeenCalled();
  remove("m1");
  confirm("policy", policy(["legacy"], 2));
  save("policy");
  await flush();
  expect(JSON.parse(String(attempts("policy")[0].body)).allowed_worker_ids).toEqual(["legacy"]);
  // A head refresh may discard the window, never the user's selected IDs.
  members.mockImplementation(async () => page(firstPage));
  refresh();
  await flush();
  expect(within(form("policy")).getByRole("button", { name: "移除 legacy" })).toBeVisible();
  expect(within(within(form("policy")).getByRole("list", { name: "已选分派成员" })).getByText(/legacy.*成员未载入/)).toBeVisible();
  expect(members.mock.calls.at(-1)?.[0]).toBe("/api/v1/groups/group-A/members?limit=50");
});

it("treats roster errors as stale and keeps drafts instead of granting new-write eligibility", async () => {
  await open();
  choose("coordinator", "C");
  members.mockRejectedValueOnce(new TypeError("offline"));
  refresh();
  await flush();
  expect(screen.getByText(/成员目录未确认，已有目录已过期/)).toBeVisible();
  expect(within(form("coordinator")).getByRole("combobox")).toHaveValue("C");
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
});

it("uses active same-group membership, not role, and cannot save an empty coordinator", async () => {
  members.mockImplementation(async () => page([
    rosterMember("A", "active", "group-A", "worker"), rosterMember("B", "active", "group-A", "coordinator"),
    rosterMember("C", "active", "group-A", "observer"), rosterMember("left", "left", "group-A", "coordinator"),
    rosterMember("future", "future"),
  ]));
  await open();
  expect(within(form("coordinator")).queryByRole("option", { name: /left成员/ })).not.toBeInTheDocument();
  expect(within(form("policy")).queryByRole("option", { name: /future成员/ })).not.toBeInTheDocument();
  choose("coordinator", "");
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
  choose("coordinator", "C");
  confirm("coordinator", binding("C", 2));
  save("coordinator");
  await flush();
  expect(puts.coordinator).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(attempts("coordinator")[0].body)).member_id).toBe("C");
});

it("rejects a cross-group roster response without admitting its members", async () => {
  members.mockImplementation(async () => page([rosterMember("intruder", "active", "group-B")]));
  await open();
  expect(screen.getByText(/成员目录未确认，已有目录已过期/)).toBeVisible();
  expect(screen.queryByRole("option", { name: /intruder/ })).not.toBeInTheDocument();
  choose("coordinator", "intruder");
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
});

it("requires the explicit remove-worker, save-policy, confirm-current, save-coordinator sequence", async () => {
  await open();
  choose("coordinator", "B");
  remove("B");
  // Another form's unsaved draft does not change the saved exclusion constraint.
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
  const receipt = deferred<Response>();
  const currentPolicy = deferred<Response>();
  puts.policy.mockReturnValueOnce(receipt.promise);
  reads.policy.mockReturnValueOnce(currentPolicy.promise);
  fireEvent.click(within(form("policy")).getByRole("button", { name: "保存空策略，禁止向任何 worker 分派" }));
  expect(puts.policy).toHaveBeenCalledTimes(1);
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
  await act(async () => receipt.resolve(json(policy([], 2))));
  expect(history("policy").getByText(/版本 2/)).toBeVisible();
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
  await act(async () => currentPolicy.resolve(json(policy([], 2))));
  expect(puts.coordinator).not.toHaveBeenCalled();
  reads.policy.mockImplementation(async () => json(policy([], 2)));
  confirm("coordinator", binding("B", 2));
  save("coordinator");
  await flush();
  expect(puts.coordinator).toHaveBeenCalledTimes(1);
  // A can be explicitly added only after B is actually current.
  choose("policy", "A");
  confirm("policy", policy(["A"], 3));
  save("policy");
  await flush();
  expect(puts.policy).toHaveBeenCalledTimes(2);
  expect(JSON.parse(String(attempts("policy")[0].body))).toEqual({ allowed_worker_ids: [], expected_revision: 1 });
  expect(JSON.parse(String(attempts("coordinator")[0].body))).toEqual({ member_id: "B", expected_revision: 1 });
  expect(JSON.parse(String(attempts("policy")[1].body))).toEqual({ allowed_worker_ids: ["A"], expected_revision: 2 });
  expect(requests.mock.calls.filter(([, init]) => init?.method && init.method !== "GET")).toHaveLength(3);
  expect(requests.mock.calls.every(([url]) => /\/(members\?|coordination\/)/.test(String(url)))).toBe(true);
});

it("stops dependent coordinator steps on an unknown policy save without rollback or automatic writes", async () => {
  await open();
  choose("coordinator", "B");
  remove("B");
  save("policy");
  await flush();
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
  expect(puts.policy).toHaveBeenCalledTimes(1);
  expect(within(form("policy")).queryByRole("button", { name: "移除 B" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /放弃|取消保存|派发|授权/ })).not.toBeInTheDocument();
  expect(screen.getByText(/重载页面会丢失本地待确认身份/)).toBeVisible();
});

it("blocks a policy draft containing the saved coordinator even when the coordinator draft was changed", async () => {
  await open();
  choose("coordinator", "C");
  choose("policy", "A");
  save("policy");
  expect(puts.policy).not.toHaveBeenCalled();
  expect(within(form("policy")).getByText(/已保存的协调员/)).toBeVisible();
  remove("A");
  confirm("policy", policy(["B"], 2));
  save("policy");
  await flush();
  expect(puts.policy).toHaveBeenCalledTimes(1);
});

it("isolates stable mounted A/B instances, aborting only reads on hide and only ending writes on real unmount", async () => {
  function Scopes({ active }: { active: string }) {
    return <>
      {["group-A", "group-B"].map(id => <div key={id} hidden={active !== id} data-testid={id}>
        <CoordinationSettings groupId={id} active={active === id} />
      </div>)}
    </>;
  }
  const view = render(<Scopes active="group-A" />);
  const a = screen.getByTestId("group-A");
  const b = screen.getByTestId("group-B");
  fireEvent.click(within(a).getByRole("button", { name: "展开协调配置" }));
  await flush();
  choose("coordinator", "C", a);
  const receipt = deferred<Response>();
  puts.coordinator.mockReturnValueOnce(receipt.promise);
  save("coordinator", a);
  const writeSignal = puts.coordinator.mock.calls[0][1]?.signal;
  const delayedRead = deferred<Response>();
  reads.coordinator.mockReturnValueOnce(delayedRead.promise);
  refresh(a);
  const readSignal = reads.coordinator.mock.calls.at(-1)?.[1]?.signal;
  const aForm = form("coordinator", a);
  view.rerender(<Scopes active="group-B" />);
  expect(writeSignal?.aborted).toBe(false);
  expect(readSignal?.aborted).toBe(true);
  expect(aForm).toBeInTheDocument();
  fireEvent.click(within(b).getByRole("button", { name: "展开协调配置" }));
  await flush();
  choose("coordinator", "D", b);
  expect(within(form("coordinator", b)).getByRole("combobox")).toHaveValue("D");
  await act(async () => {
    delayedRead.resolve(json(binding("D", 9)));
    receipt.resolve(json({}, 503));
  });
  expect(within(b).queryByText(/结果未知，请保留原操作/)).not.toBeInTheDocument();
  view.rerender(<Scopes active="group-A" />);
  await flush();
  expect(form("coordinator", a)).toBe(aForm);
  expect(within(form("coordinator", a)).getByRole("combobox")).toHaveValue("C");
  expect(current("coordinator", a).queryByText(/版本 9/)).not.toBeInTheDocument();
  const retry = deferred<Response>();
  puts.coordinator.mockReturnValueOnce(retry.promise);
  fireEvent.click(within(a).getByRole("button", { name: "重试同一次协调员保存" }));
  expect(attempts("coordinator")[1]).toEqual(attempts("coordinator")[0]);
  const retrySignal = puts.coordinator.mock.calls[1][1]?.signal;
  fireEvent.click(within(a).getByRole("button", { name: "收起协调配置" }));
  expect(retrySignal?.aborted).toBe(false);
  expect(within(a).getByText("协调员：结果未知，正在重试原操作。")).toBeVisible();
  view.rerender(<Scopes active="group-B" />);
  await flush();
  expect(within(form("coordinator", b)).getByRole("combobox")).toHaveValue("D");
  view.unmount();
  expect(retrySignal?.aborted).toBe(true);
  await act(async () => retry.resolve(json(binding("C", 2))));
});

it("resets pagination to the head when inactive, without passing A's cursor into B", async () => {
  members.mockImplementationOnce(async () => page([rosterMember("A")], "A+/cursor"));
  const view = await open();
  members.mockImplementationOnce(async () => page([rosterMember("B")]));
  fireEvent.click(screen.getByRole("button", { name: "加载更多成员" }));
  await flush();
  view.rerender(<CoordinationSettings groupId="group-A" active={false} />);
  view.rerender(<CoordinationSettings groupId="group-A" active />);
  await flush();
  expect(members.mock.calls.at(-1)?.[0]).toBe("/api/v1/groups/group-A/members?limit=50");
  view.unmount();
  await open("group-B");
  expect(members.mock.calls.at(-1)?.[0]).toBe("/api/v1/groups/group-B/members?limit=50");
});

it.each(["coordinator", "policy"] as const)("shows MAX_SAFE_INTEGER %s revisions but does not send an unsafe next write", async kind => {
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("A", Number.MAX_SAFE_INTEGER)
    : policy(["B"], Number.MAX_SAFE_INTEGER)));
  await open();
  expect(current(kind).getByText(/9007199254740991/)).toBeVisible();
  choose(kind, "C");
  save(kind);
  expect(puts[kind]).not.toHaveBeenCalled();
});

it("does not use localStorage or claim runtime readiness", async () => {
  const storage = vi.spyOn(Storage.prototype, "setItem");
  await open();
  choose("coordinator", "C");
  confirm("coordinator", binding("C", 2));
  save("coordinator");
  await flush();
  expect(storage).not.toHaveBeenCalled();
  expect(screen.getByText(/保存仅登记配置，不代表运行就绪/)).toBeVisible();
  expect(screen.queryByText(/已就绪|执行已启用|已派发/)).not.toBeInTheDocument();
  storage.mockRestore();
});

it.each(["coordinator", "policy"] as const)("keeps a newer %s GET current when an older pending PUT receipt arrives", async kind => {
  await open();
  choose(kind, "C");
  const delayed = deferred<Response>();
  puts[kind].mockReturnValueOnce(delayed.promise);
  save(kind);
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("D", 9) : policy(["D"], 9)));
  refresh();
  await flush();
  expect(current(kind).getByText(/版本 9.*D成员/)).toBeVisible();
  reads[kind].mockImplementationOnce(async () => errorResponse(404));
  await act(async () => delayed.resolve(json(kind === "coordinator" ? binding("C", 2) : policy(["B", "C"], 2))));
  expect(current(kind).getByText(/版本 9.*D成员/)).toBeVisible();
  expect(current(kind).getByText(/当前配置尚未重新确认/)).toBeVisible();
  expect(history(kind).getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(puts[kind]).toHaveBeenCalledTimes(1);
});

it.each(["coordinator", "policy"] as const)("rejects a %s current GET whose content contradicts the same-version confirmed receipt", async kind => {
  await open();
  choose(kind, "C");
  puts[kind].mockImplementationOnce(async () => json(kind === "coordinator" ? binding("C", 2) : policy(["B", "C"], 2)));
  reads[kind].mockImplementation(async () => json(kind === "coordinator" ? binding("D", 2) : policy(["D"], 2)));
  save(kind);
  await flush();
  expect(history(kind).getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(current(kind).getByText(/读取已过期|读取内容不一致/)).toBeVisible();
  expect(current(kind).queryByText(/版本 2.*D成员/)).not.toBeInTheDocument();
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
});

it.each(["coordinator", "policy"] as const)("preserves a pending %s write and its receipt across collapse without background reads", async kind => {
  await open();
  choose(kind, "C");
  const delayed = deferred<Response>();
  puts[kind].mockReturnValueOnce(delayed.promise);
  save(kind);
  const writeSignal = puts[kind].mock.calls[0][1]?.signal;
  const body = form(kind);
  fireEvent.click(screen.getByRole("button", { name: "收起协调配置" }));
  expect(body).not.toBeVisible();
  expect(screen.getByText(`${labels[kind]}：正在保存，结果待确认。`)).toBeVisible();
  expect(writeSignal?.aborted).toBe(false);
  expect(reads.coordinator.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(reads.policy.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(members.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => delayed.resolve(json(kind === "coordinator" ? binding("C", 2) : policy(["B", "C"], 2))));
  expect(body).toHaveTextContent("本次保存已确认（版本 2）");
  expect(reads[kind]).toHaveBeenCalledTimes(1);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  const fresh = deferred<Response>();
  reads[kind].mockReturnValueOnce(fresh.promise);
  fireEvent.click(screen.getByRole("button", { name: "展开协调配置" }));
  save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  await act(async () => fresh.resolve(json(kind === "coordinator" ? binding("C", 2) : policy(["B", "C"], 2))));
  expect(current(kind).getByText(/版本 2/)).toBeVisible();
  expect(form(kind)).toBe(body);
});

it("cannot use a policy 200 from an earlier refresh to reinterpret a later coordinator 404", async () => {
  reads.coordinator.mockImplementationOnce(async () => json({}, 503));
  await open();
  reads.coordinator.mockImplementation(async () => errorResponse(404));
  reads.policy.mockImplementation(async () => errorResponse(404));
  refresh();
  await flush();
  expect(current("coordinator").queryByText(/尚未绑定协调员/)).not.toBeInTheDocument();
  choose("coordinator", "C");
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
});

it("retains a selected left coordinator and requires an explicit active replacement", async () => {
  members.mockImplementation(async () => page([rosterMember("A", "left"), rosterMember("C")]));
  await open();
  expect(current("coordinator").getByText(/A成员.*已离群/)).toBeVisible();
  expect(within(form("coordinator")).getByRole("combobox")).toHaveValue("A");
  expect(within(form("coordinator")).getByRole("option", { name: /A成员.*已离群/ })).toBeDisabled();
  save("coordinator");
  expect(puts.coordinator).not.toHaveBeenCalled();
  choose("coordinator", "C");
  confirm("coordinator", binding("C", 2));
  save("coordinator");
  await flush();
  expect(puts.coordinator).toHaveBeenCalledTimes(1);
});

it("records a resolved coordinator GET independently of a hanging policy read and never forgets that known binding", async () => {
  const delayed = deferred<Response>();
  reads.policy.mockReturnValueOnce(delayed.promise);
  await open();
  expect(current("coordinator").getByText(/版本 1.*A成员/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "收起协调配置" }));
  await act(async () => delayed.resolve(json(policy())));
  reads.coordinator.mockImplementation(async () => errorResponse(404));
  fireEvent.click(screen.getByRole("button", { name: "展开协调配置" }));
  await flush();
  expect(current("coordinator").getByText(/版本 1.*A成员/)).toBeVisible();
  expect(current("coordinator").getByText(/当前配置尚未重新确认/)).toBeVisible();
  expect(current("coordinator").queryByText(/尚未绑定协调员/)).not.toBeInTheDocument();
});

it("does not initialize over a user draft edited while the initial GET is pending", async () => {
  const delayed = deferred<Response>();
  reads.coordinator.mockReturnValueOnce(delayed.promise);
  await open();
  choose("coordinator", "C");
  await act(async () => delayed.resolve(json(binding())));
  expect(current("coordinator").getByText(/版本 1.*A成员/)).toBeVisible();
  expect(within(form("coordinator")).getByRole("combobox")).toHaveValue("C");
});
