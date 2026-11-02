import { once } from "node:events";
import { createServer } from "node:http";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ReceptionDefaults } from "./ReceptionDefaults";
import type { ReceptionConfiguration, ReceptionDefaultsInput } from "./reception-defaults-api";
import { deferred, json, page } from "./test/groups";
import { provider } from "./test/members";
import { endpoint, saved, settings } from "./test/reception-defaults";

const nativeFetch = globalThis.fetch;
const reads = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>(), catalogue = vi.fn<typeof fetch>();
const runtime = { ...provider, account: "unknown", version: "future-version", connectivity: "unchecked" };
const model = () => screen.getByRole("textbox", { name: "模型" });
const role = () => screen.getByRole("textbox", { name: "职责" });
const selection = () => screen.getByRole("combobox", { name: "运行器" });
const change = (name: "模型" | "职责", value: string) => fireEvent.change(screen.getByRole("textbox", { name }), { target: { value } });
const submit = () => fireEvent.submit(screen.getByRole("form", { name: "默认接待设置" }));
const summary = () => within(screen.getByRole("region", { name: "已保存设置" }));
const attempt = (index: number) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
async function open() {
  const view = render(<ReceptionDefaults />);
  await screen.findByText("尚未配置默认接待者。");
  return view;
}
async function draft() {
  await screen.findByRole("option", { name: /账户未知/ });
  fireEvent.change(selection(), { target: { value: runtime.id } });
  change("模型", saved.model); change("职责", saved.role);
}
beforeEach(() => {
  reads.mockReset().mockImplementation(async () => settings());
  catalogue.mockReset().mockImplementation(async () => page([runtime]));
  writes.mockReset().mockImplementation(async (_url, init) => {
    const input = JSON.parse(String(init?.body)) as ReceptionDefaultsInput;
    return json({ ...saved, provider_id: input.provider_id, model: input.model, role: input.role,
      id: `defaults-${input.expected_revision + 1}`, revision: input.expected_revision + 1 });
  });
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => String(input) === endpoint
    ? (init?.method === "PUT" ? writes : reads)(input, init)
    : String(input).startsWith("/api/v1/providers?") ? catalogue(input, init)
    : Promise.reject(new Error(`Unexpected URL: ${String(input)}`)));
});
afterEach(() => vi.unstubAllGlobals());

it("does not auto-select a provider or model, requires Save, and explains execution and managed scope", async () => {
  await open(); await screen.findByRole("option", { name: /账户未知/ });
  expect(selection()).toHaveValue(""); expect(model()).toHaveValue(""); expect(String((role() as HTMLTextAreaElement).value).trim()).not.toBe("");
  expect(screen.getByText(/执行未启用/)).toBeVisible(); expect(screen.getByText(/未来.*空群/)).toBeVisible();
  expect(screen.getByText(/平台管理的工作目录/)).toBeVisible(); expect(screen.getByText(/不.*授予.*读写权限/)).toBeVisible();
  expect(screen.queryByRole("textbox", { name: /名称|身份|工作目录/ })).not.toBeInTheDocument();
  expect(writes).not.toHaveBeenCalled(); submit();
  expect(await screen.findByRole("alert")).toHaveTextContent("运行器"); expect(writes).not.toHaveBeenCalled();
  fireEvent.change(selection(), { target: { value: runtime.id } }); submit();
  expect(screen.getByRole("alert")).toHaveTextContent("模型"); expect(writes).not.toHaveBeenCalled();
});
it("saves explicit unknown-account/future-version selection without claiming runnable, then reloads the actual snapshot", async () => {
  const view = await open(); await draft();
  expect(screen.getByRole("option", { name: /future-version.*账户未知/ })).toBeEnabled();
  expect(screen.getByText(/安装、账户状态和模型填写都不保证可运行/)).toBeVisible();
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise);
  submit(); submit(); expect(writes).toHaveBeenCalledTimes(1); expect(model()).toBeDisabled();
  expect(attempt(0).body).toBe(JSON.stringify({ provider_id: runtime.id, model: saved.model, role: saved.role, expected_revision: 0 }));
  expect(attempt(0).key).toMatch(/^[\da-f-]{36}$/); expect(summary().queryByText(saved.model)).not.toBeInTheDocument();
  await act(async () => receipt.resolve(json(saved)));
  expect(summary().getByText(saved.model)).toBeVisible(); expect(summary().getByText("版本 1")).toBeVisible();
  expect(model()).toBeEnabled(); view.unmount(); reads.mockResolvedValueOnce(settings(saved, true));
  render(<ReceptionDefaults />); await screen.findByText("版本 1");
  expect(model()).toHaveValue(saved.model); expect(role()).toHaveValue(saved.role); expect(selection()).toHaveValue(saved.provider_id);
  expect(screen.getByText(/实际执行为准/)).toBeVisible(); expect(screen.getByText(/执行已启用/)).toBeVisible();
  expect(writes).toHaveBeenCalledTimes(1);
});
it("keeps a missing saved provider visible, never substitutes the first entry, and requires an exact loaded selection", async () => {
  const missing = { ...saved, provider_id: "saved-exact-provider" };
  reads.mockResolvedValueOnce(settings(missing)); catalogue.mockResolvedValueOnce(page([runtime], "next+/"));
  render(<ReceptionDefaults />); await screen.findByText("版本 1"); await screen.findByRole("option", { name: /账户未知/ });
  expect(selection()).toHaveValue(missing.provider_id); expect(screen.getByText(/已保存的运行器不在已载入目录中/)).toHaveTextContent(missing.provider_id);
  change("职责", "保留的修改"); submit();
  expect(await screen.findByRole("alert")).toHaveTextContent("已载入目录"); expect(writes).not.toHaveBeenCalled();
  catalogue.mockResolvedValueOnce(page([{ ...runtime, id: missing.provider_id, name: "手动运行器" }]));
  fireEvent.click(screen.getByRole("button", { name: "加载更多运行器" }));
  await screen.findByRole("option", { name: /手动运行器/ }); expect(selection()).toHaveValue(missing.provider_id);
  submit(); await screen.findByText("版本 2");
  expect(JSON.parse(String(attempt(0).body))).toMatchObject({ provider_id: missing.provider_id, role: "保留的修改", expected_revision: 1 });
});
it("preserves edits while reads/catalogue fail or load, retries real errors, and pages only on demand", async () => {
  const pending = deferred<Response>(); catalogue.mockReturnValueOnce(pending.promise);
  await open(); change("模型", "编辑中的模型"); change("职责", "编辑中的职责");
  await act(async () => pending.resolve(json({}, 503)));
  expect(await screen.findByRole("alert")).toHaveTextContent("运行器目录");
  catalogue.mockResolvedValueOnce(page([runtime], "next+/"));
  fireEvent.click(screen.getByRole("button", { name: "重试运行器目录" }));
  await screen.findByRole("option", { name: /账户未知/ }); expect(catalogue).toHaveBeenCalledTimes(2);
  expect(model()).toHaveValue("编辑中的模型"); expect(role()).toHaveValue("编辑中的职责"); expect(selection()).toHaveValue("");
  catalogue.mockRejectedValueOnce(new TypeError("offline")); fireEvent.click(screen.getByRole("button", { name: "加载更多运行器" }));
  await screen.findByRole("button", { name: "重试运行器目录" });
  expect(screen.getByRole("option", { name: /账户未知/ })).toBeEnabled();
  catalogue.mockResolvedValueOnce(page([runtime, { ...runtime, id: "other-provider", name: "另一个运行器" }]));
  fireEvent.click(screen.getByRole("button", { name: "重试运行器目录" }));
  await screen.findByRole("option", { name: /另一个运行器/ });
  expect(screen.getAllByRole("option", { name: /future-version.*\/opt\/bin\/pi/ })).toHaveLength(2);
  expect(catalogue.mock.calls.map(([url]) => url)).toEqual(["/api/v1/providers?limit=50", "/api/v1/providers?limit=50",
    "/api/v1/providers?limit=50&before=next%2B%2F", "/api/v1/providers?limit=50&before=next%2B%2F"]);
  expect(model()).toHaveValue("编辑中的模型");
});
it("shows real empty catalogue and GET failure states, with explicit retries preserving input", async () => {
  reads.mockResolvedValueOnce(json({}, 404)); catalogue.mockResolvedValueOnce(page()); render(<ReceptionDefaults />);
  await screen.findByRole("button", { name: "重试载入设置" }); await screen.findByText("未检测到运行器条目。");
  change("模型", "离线草稿"); change("职责", "离线职责");
  fireEvent.click(screen.getByRole("button", { name: "重试载入设置" })); await screen.findByText("尚未配置默认接待者。");
  expect(model()).toHaveValue("离线草稿"); expect(role()).toHaveValue("离线职责"); expect(writes).not.toHaveBeenCalled();
});
it("shows backend provider-unavailable and execution-disabled states without trusting server prose", async () => {
  reads.mockResolvedValueOnce(json({ configuration: saved, availability: { code: "provider_unavailable", message: "SECRET ready!" }, execution_enabled: false }));
  render(<ReceptionDefaults />); await screen.findByText("版本 1");
  expect(screen.getByText(/已保存的运行器当前不受支持/)).toBeVisible(); expect(screen.getByText(/执行未启用/)).toBeVisible();
  expect(screen.queryByText(/SECRET/)).not.toBeInTheDocument(); expect(model()).toHaveValue(saved.model);
});
it.each([
  ["模型", "中".repeat(86), "256"], ["模型", "x\ny", "换行"], ["模型", "x\0y", "控制字符"],
  ["模型", "\ud800", "UTF-8"], ["职责", "😀".repeat(1025), "4096"], ["职责", " \n", "职责"],
] as const)("validates %s by UTF-8 bytes and content before sending %#", async (field, value, reason) => {
  await open(); await draft(); change(field, value); submit();
  expect(await screen.findByRole("alert")).toHaveTextContent(reason); expect(writes).not.toHaveBeenCalled();
  expect(screen.getByRole("textbox", { name: field })).toHaveValue(value);
});
it("saves exact UTF-8 boundary values without trimming or inventing a model", async () => {
  await open(); await draft(); const exactModel = "😀".repeat(64), exactRole = "😀".repeat(1024);
  change("模型", exactModel); change("职责", exactRole); submit(); await screen.findByText("版本 1");
  expect(JSON.parse(String(attempt(0).body))).toMatchObject({ model: exactModel, role: exactRole });
});
it.each(["network", "server", "malformed200", "201"])("locks the exact operation after unknown %s and retries the same body/revision/key", async failure => {
  await open(); await draft();
  if (failure === "network") writes.mockRejectedValueOnce(new TypeError("offline"));
  else writes.mockResolvedValueOnce(failure === "server" ? json({}, 503) : failure === "malformed200" ? json({}, 200) : json(saved, 201));
  submit(); expect(await screen.findByRole("alert")).toHaveTextContent("保存结果未确认");
  expect(model()).toBeDisabled(); expect(role()).toBeDisabled(); expect(selection()).toBeDisabled(); expect(model()).toHaveValue(saved.model);
  expect(summary().queryByText(saved.model)).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "重试同一次保存" })); await screen.findByText("版本 1");
  expect(attempt(1)).toEqual(attempt(0)); expect(model()).toBeEnabled();
});
it.each([303, 307])("keeps a real HTTP %s save redirect unknown without contacting its target and retries the exact operation", async status => {
  type Request = { method: string | undefined; body: string; key: string | string[] | undefined };
  const originals: Request[] = [], targets: Request[] = [];
  let committed: ReceptionConfiguration | undefined;
  const server = createServer((request, response) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", chunk => { body += chunk; });
    request.on("end", () => {
      const received = { method: request.method, body, key: request.headers["idempotency-key"] };
      if (request.url === endpoint && request.method === "PUT") {
        originals.push(received);
        if (originals.length === 1) {
          const input = JSON.parse(body) as ReceptionDefaultsInput;
          committed = { ...saved, id: "defaults-2", revision: input.expected_revision + 1,
            provider_id: input.provider_id, model: input.model, role: input.role };
          response.writeHead(status, { Location: "/redirect-target" }).end();
        } else {
          response.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(committed));
        }
      } else {
        targets.push(received);
        response.writeHead(404, { "Content-Type": "application/json" }).end(JSON.stringify({ error: { code: "not_found", message: "Not found" } }));
      }
    });
  });
  try {
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("Expected a loopback TCP address");
    writes.mockImplementation((_url, init) => nativeFetch(`http://127.0.0.1:${address.port}${endpoint}`, init));
    reads.mockResolvedValueOnce(settings(saved));
    render(<ReceptionDefaults />); await screen.findByText("版本 1"); await draft();
    const exact = { provider_id: runtime.id, model: " 自选模型 ", role: " 保留职责\n ", expected_revision: saved.revision };
    change("模型", exact.model); change("职责", exact.role); submit();
    const alert = await screen.findByRole("alert");
    expect(originals).toHaveLength(1);
    expect(originals[0].method).toBe("PUT"); expect(originals[0].body).toBe(JSON.stringify(exact));
    expect(originals[0].key).toMatch(/^[\da-f-]{36}$/);
    expect(targets).toEqual([]);
    expect(alert).toHaveTextContent("保存结果未确认");
    expect(model()).toBeDisabled(); expect(role()).toBeDisabled(); expect(selection()).toBeDisabled();
    expect(model()).toHaveValue(exact.model); expect(role()).toHaveValue(exact.role);
    expect(summary().getByText("版本 1")).toBeVisible(); expect(summary().queryByText("版本 2")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试同一次保存" })); await screen.findByText("版本 2");
    expect(originals).toHaveLength(2); expect(originals[1]).toEqual(originals[0]); expect(attempt(1)).toEqual(attempt(0));
    expect(targets).toEqual([]); expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(model()).toBeEnabled(); expect(role()).toBeEnabled(); expect(selection()).toBeEnabled();
    expect(summary().getByText(exact.model.trim())).toBeVisible();
    expect(screen.queryByRole("button", { name: "重试同一次保存" })).not.toBeInTheDocument();
  } finally {
    if (server.listening) await new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve());
      server.closeAllConnections();
    });
  }
});
it("does not reconcile an unknown save by GET or replace its draft, revision or key", async () => {
  await open(); await draft(); writes.mockRejectedValueOnce(new TypeError("offline")); submit(); await screen.findByRole("alert");
  const newer = { ...saved, id: "defaults-3", revision: 3, model: "远端模型", role: "远端职责" };
  reads.mockResolvedValueOnce(settings(newer)); fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  await screen.findByText("版本 3"); expect(model()).toBeDisabled(); expect(model()).toHaveValue(saved.model); expect(role()).toHaveValue(saved.role);
  expect(screen.getByRole("button", { name: "重试同一次保存" })).toBeEnabled();
  fireEvent.click(screen.getByRole("button", { name: "重试同一次保存" })); await screen.findByText(/本次保存已确认/);
  expect(attempt(1)).toEqual(attempt(0)); expect(summary().getByText("版本 3")).toBeVisible(); expect(summary().getByText("远端模型")).toBeVisible();
  expect(model()).toHaveValue(saved.model); expect(model()).toBeEnabled();
});
it.each([
  ["reception_defaults_required", "默认接待设置"], ["reception_provider_unavailable", "重新选择运行器"],
  ["reception_member_required", "添加或恢复成员"], ["conflict", "同一保存标识"], ["future_conflict", "发生冲突"],
])("preserves an editable draft after code-specific 409 %s without auto-refresh or leaking messages", async (code, text) => {
  await open(); await draft(); writes.mockResolvedValueOnce(json({ error: { code, message: "SECRET" } }, 409)); submit();
  const alert = await screen.findByRole("alert"); expect(alert).toHaveTextContent(text); expect(alert).not.toHaveTextContent("SECRET");
  expect(model()).toHaveValue(saved.model); expect(role()).toHaveValue(saved.role); expect(model()).toBeEnabled(); expect(reads).toHaveBeenCalledTimes(1);
  change("职责", "修订的职责"); submit(); await screen.findByText("版本 1");
  expect(attempt(1).key).not.toBe(attempt(0).key); expect(JSON.parse(String(attempt(1).body))).toMatchObject({ role: "修订的职责" });
});
it.each([400, 404, 413, 415])("unlocks and retains the draft after definitive HTTP %s", async status => {
  await open(); await draft(); writes.mockResolvedValueOnce(json({ error: { code: "rejected", message: "SECRET" } }, status)); submit();
  expect(await screen.findByRole("alert")).toHaveTextContent(String(status)); expect(model()).toBeEnabled(); expect(model()).toHaveValue(saved.model);
  change("模型", "改正后的模型"); submit(); await screen.findByText("版本 1"); expect(attempt(1).key).not.toBe(attempt(0).key);
});
it("refreshes after a revision conflict only by explicit action, preserving the draft for review before a new save", async () => {
  reads.mockResolvedValueOnce(settings(saved)); render(<ReceptionDefaults />); await screen.findByText("版本 1"); await draft();
  change("模型", "我的新模型"); change("职责", "我的职责");
  writes.mockResolvedValueOnce(json({ error: { code: "reception_revision_conflict", message: "SECRET" } }, 409)); submit();
  expect(await screen.findByRole("alert")).toHaveTextContent("重新载入"); expect(reads).toHaveBeenCalledTimes(1);
  expect(model()).toBeEnabled(); expect(screen.getByRole("button", { name: "保存默认接待设置" })).toBeDisabled();
  const newer = { ...saved, revision: 4, id: "defaults-4", model: "他人的模型", role: "他人的职责" };
  reads.mockResolvedValueOnce(settings(newer)); fireEvent.click(screen.getByRole("button", { name: "载入最新设置并保留草稿" }));
  await screen.findByText("版本 4"); expect(model()).toHaveValue("我的新模型"); expect(role()).toHaveValue("我的职责");
  expect(screen.getByText(/请对照已保存内容，确认后再保存/)).toBeVisible(); expect(writes).toHaveBeenCalledTimes(1);
  submit(); await screen.findByText("版本 5"); expect(JSON.parse(String(attempt(1).body))).toMatchObject({ expected_revision: 4, model: "我的新模型", role: "我的职责" });
  expect(attempt(1).key).not.toBe(attempt(0).key);
});
it("does not let an already-running GET silently resolve a later revision conflict", async () => {
  reads.mockResolvedValueOnce(settings(saved)); render(<ReceptionDefaults />); await screen.findByText("版本 1"); await draft();
  const olderRead = deferred<Response>(); reads.mockReturnValueOnce(olderRead.promise);
  fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  writes.mockResolvedValueOnce(json({ error: { code: "reception_revision_conflict" } }, 409)); submit(); await screen.findByRole("alert");
  await act(async () => olderRead.resolve(settings({ ...saved, revision: 2, id: "defaults-2" })));
  expect(screen.getByRole("button", { name: "保存默认接待设置" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "载入最新设置并保留草稿" })).toBeEnabled();
});
it("refreshes the saved state without erasing a dirty draft and uses its reviewed current revision on save", async () => {
  reads.mockResolvedValueOnce(settings(saved)); render(<ReceptionDefaults />); await screen.findByText("版本 1"); await draft();
  change("模型", "草稿模型"); change("职责", "草稿职责");
  const newer = { ...saved, revision: 2, id: "defaults-2", model: "服务器的新模型", role: "服务器的新职责" };
  reads.mockResolvedValueOnce(settings(newer)); fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  await screen.findByText("版本 2"); expect(summary().getByText(newer.model)).toBeVisible();
  expect(model()).toHaveValue("草稿模型"); expect(role()).toHaveValue("草稿职责"); expect(writes).not.toHaveBeenCalled();
  submit(); await screen.findByText("版本 3"); expect(JSON.parse(String(attempt(0).body))).toMatchObject({ expected_revision: 2, model: "草稿模型", role: "草稿职责" });
});
it.each(["GET-first", "PUT-first"])("never replaces newer GET revision 3 with PUT receipt revision 2: %s", async order => {
  reads.mockResolvedValueOnce(settings(saved)); render(<ReceptionDefaults />); await screen.findByText("版本 1"); await draft();
  change("模型", "本次提交模型"); const pendingPut = deferred<Response>(), pendingGet = deferred<Response>();
  writes.mockReturnValueOnce(pendingPut.promise); submit(); reads.mockReturnValueOnce(pendingGet.promise);
  fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  const newer = { ...saved, id: "defaults-3", revision: 3, model: "最新模型", role: "最新职责" };
  const older = { ...saved, id: "defaults-2", revision: 2, model: "本次提交模型" };
  if (order === "GET-first") {
    await act(async () => pendingGet.resolve(settings(newer)));
    await act(async () => pendingPut.resolve(json(older)));
  } else {
    await act(async () => pendingPut.resolve(json(older)));
    await act(async () => pendingGet.resolve(settings(newer)));
  }
  expect(summary().getByText("版本 3")).toBeVisible(); expect(summary().getByText("最新模型")).toBeVisible();
  expect(summary().queryByText("本次提交模型")).not.toBeInTheDocument(); expect(model()).toBeEnabled();
  reads.mockResolvedValueOnce(settings(saved)); fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" })); await act(async () => {});
  expect(summary().getByText("版本 3")).toBeVisible(); expect(summary().getByText("最新模型")).toBeVisible();
});
it.each(["GET-first", "PUT-first"])("never regresses a newer PUT when an older GET arrives: %s", async order => {
  reads.mockResolvedValueOnce(settings(saved)); render(<ReceptionDefaults />); await screen.findByText("版本 1"); await draft();
  change("模型", "新保存的模型"); const pendingGet = deferred<Response>(), pendingPut = deferred<Response>();
  reads.mockReturnValueOnce(pendingGet.promise); fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  writes.mockReturnValueOnce(pendingPut.promise); submit();
  const receipt = { ...saved, id: "defaults-2", revision: 2, model: "新保存的模型" };
  if (order === "GET-first") {
    await act(async () => pendingGet.resolve(settings(saved)));
    await act(async () => pendingPut.resolve(json(receipt)));
  } else {
    await act(async () => pendingPut.resolve(json(receipt)));
    await act(async () => pendingGet.resolve(settings(saved)));
  }
  expect(summary().getByText("版本 2")).toBeVisible(); expect(summary().getByText("新保存的模型")).toBeVisible(); expect(model()).toHaveValue("新保存的模型");
});
it("ignores a late not-configured GET after a save and aborts all in-flight work on unmount", async () => {
  const view = await open(); await draft(); const pendingGet = deferred<Response>(), pendingPut = deferred<Response>(), pendingCatalogue = deferred<Response>();
  reads.mockReturnValueOnce(pendingGet.promise); fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  submit(); await screen.findByText("版本 1"); await act(async () => pendingGet.resolve(settings()));
  expect(summary().getByText("版本 1")).toBeVisible(); expect(model()).toHaveValue(saved.model);
  reads.mockReturnValueOnce(pendingGet.promise); fireEvent.click(screen.getByRole("button", { name: "刷新已保存设置" }));
  writes.mockReturnValueOnce(pendingPut.promise); change("职责", "尚未确认的修改"); submit();
  catalogue.mockReturnValueOnce(pendingCatalogue.promise);
  // A fresh mount also owns and aborts its initial directory/read requests.
  view.unmount(); expect(writes.mock.calls[1][1]?.signal?.aborted).toBe(true);
  expect(reads.mock.calls[2][1]?.signal?.aborted).toBe(true);
  reads.mockReturnValueOnce(deferred<Response>().promise); const next = render(<ReceptionDefaults />);
  const directorySignal = catalogue.mock.calls.at(-1)?.[1]?.signal; next.unmount(); expect(directorySignal?.aborted).toBe(true);
  await act(async () => { pendingPut.resolve(json({ ...saved, revision: 2, role: "尚未确认的修改" })); pendingCatalogue.resolve(page([runtime])); });
});
