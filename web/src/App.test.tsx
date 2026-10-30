import { StrictMode } from "react";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { deferred, group, json, other, page } from "./test/groups";

const transport = vi.fn<typeof fetch>();
const posts = () => transport.mock.calls.filter(([, init]) => init?.method === "POST");
const nameInput = () => screen.getByRole("textbox", { name: "群组名称" });
function submit(name = "调研") {
  fireEvent.change(nameInput(), { target: { value: name } });
  fireEvent.submit(screen.getByRole("form", { name: "创建群组" }));
}
beforeEach(() => {
  transport.mockReset();
  transport.mockImplementation(async () => page());
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) =>
    String(input).includes("/members") || String(input).startsWith("/api/v1/providers")
      ? Promise.resolve(page()) : transport(input, init));
});
afterEach(() => vi.unstubAllGlobals());

describe("workspace groups", () => {
  it("names the workspace and its three semantic columns", async () => {
    render(<App />);
    expect(screen.getByRole("banner")).toContainElement(
      screen.getByRole("heading", { level: 1, name: "ct0x" }),
    );
    expect(screen.getByRole("complementary", { name: "群组" })).toBeVisible();
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    expect(screen.getByRole("complementary", { name: "成员" })).toBeVisible();
    await screen.findByText("尚无群组，可在下方创建。");
  });

  it("provides a skip link to a focusable discussion landmark", async () => {
    render(<App />);
    const link = screen.getByRole("link", { name: "跳至讨论区" });
    const main = screen.getByRole("main", { name: "讨论" });
    expect(link).toHaveAttribute("href", `#${main.id}`);
    expect(main.id).not.toBe("");
    expect(main).toHaveAttribute("tabindex", "-1");
    main.focus();
    expect(main).toHaveFocus();
    await screen.findByText("尚无群组，可在下方创建。");
  });

  it("reports loading before a real empty page without enabling future operations", async () => {
    render(<App />);
    expect(screen.getByText("正在载入群组…")).toBeVisible();
    await screen.findByText("尚无群组，可在下方创建。");
    expect(nameInput()).toBeRequired();
    expect(within(screen.getByRole("main")).getByText("消息尚未载入，发送功能尚未开放。")).toBeVisible();
    expect(within(screen.getByRole("main")).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(screen.getByRole("complementary", { name: "成员" })).queryByRole("button")).not.toBeInTheDocument();
  });

  it("distinguishes disconnection and HTTP rejection, then retries explicitly", async () => {
    transport.mockRejectedValueOnce(new TypeError("offline"));
    render(<App />);
    expect(await screen.findByRole("alert")).toHaveTextContent("无法连接本地服务");
    expect(screen.queryByText("尚无群组，可在下方创建。")).not.toBeInTheDocument();
    transport.mockResolvedValueOnce(json({ error: { code: "not_found", message: "未找到" } }, 404));
    fireEvent.click(screen.getByRole("button", { name: "重试载入" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("404");
    fireEvent.click(screen.getByRole("button", { name: "重试载入" }));
    await screen.findByText("尚无群组，可在下方创建。");
    expect(transport).toHaveBeenCalledTimes(3);
  });

  it("loads another page only on demand, deduplicates IDs and preserves selection", async () => {
    transport.mockResolvedValueOnce(page([group], "opaque+/"));
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: group.name }));
    expect(within(screen.getByRole("main")).getByText(group.description)).toBeVisible();
    expect(transport).toHaveBeenCalledTimes(1);
    transport.mockResolvedValueOnce(page([group, other]));
    fireEvent.click(screen.getByRole("button", { name: "加载更多群组" }));
    await screen.findByRole("button", { name: other.name });
    expect(screen.getAllByRole("button", { name: group.name })).toHaveLength(1);
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("button", { name: "加载更多群组" })).not.toBeInTheDocument();
    expect(transport.mock.calls[1][0]).toBe("/api/v1/groups?limit=50&before=opaque%2B%2F");
    expect(transport).toHaveBeenCalledTimes(2);
  });

  it("submits JSON once while pending and selects only the returned server group", async () => {
    render(<App />);
    await screen.findByText("尚无群组，可在下方创建。");
    const response = deferred<Response>();
    transport.mockReturnValueOnce(response.promise);
    fireEvent.change(screen.getByRole("textbox", { name: "群组说明（可选）" }), { target: { value: group.description } });
    submit();
    fireEvent.submit(screen.getByRole("form", { name: "创建群组" }));
    expect(screen.getByRole("button", { name: "正在创建…" })).toBeDisabled();
    expect(nameInput()).toBeDisabled();
    expect(posts()).toHaveLength(1);
    expect(posts()[0][0]).toBe("/api/v1/groups");
    expect(JSON.parse(String(posts()[0][1]?.body))).toEqual({ name: group.name, description: group.description });
    expect(new Headers(posts()[0][1]?.headers).get("Content-Type")).toBe("application/json");
    expect(new Headers(posts()[0][1]?.headers).get("Idempotency-Key")).toMatch(/^[\da-f-]{36}$/);
    expect(within(screen.getByRole("main")).queryByText(group.name)).not.toBeInTheDocument();
    await act(async () => response.resolve(json(group, 201)));
    expect(within(screen.getByRole("main")).getByRole("heading", { name: group.name })).toBeVisible();
    expect(nameInput()).toHaveValue("");
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
  });

  it.each(["network", "server", "malformed", "accepted-202", "unexpected-200"])("retains the exact operation after an uncertain %s outcome", async failure => {
    render(<App />);
    await screen.findByText("尚无群组，可在下方创建。");
    if (failure === "network") transport.mockRejectedValueOnce(new TypeError("offline"));
    else if (failure === "accepted-202" || failure === "unexpected-200") transport.mockResolvedValueOnce(json(group, failure === "accepted-202" ? 202 : 200));
    else transport.mockResolvedValueOnce(failure === "server" ? json({ error: { code: "internal", message: "失败" } }, 503) : json({}, 201));
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("创建结果未确认");
    expect(nameInput()).toBeDisabled();
    expect(nameInput()).toHaveValue(group.name);
    expect(screen.queryByRole("button", { name: group.name })).not.toBeInTheDocument();
    expect(within(screen.getByRole("main")).queryByText(group.name)).not.toBeInTheDocument();
    transport.mockResolvedValueOnce(json(group, 201));
    fireEvent.click(screen.getByRole("button", { name: "重试同一次创建" }));
    await screen.findByRole("button", { name: group.name });
    const [first, retry] = posts().map(([, init]) => ({ body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
    expect(retry).toEqual(first);
    expect(posts()).toHaveLength(2);
  });

  it.each([400, 413, 415])("allows correction after definitive validation rejection HTTP %s with a new key", async status => {
    render(<App />);
    await screen.findByText("尚无群组，可在下方创建。");
    const rejectedName = status === 413 ? group.name.repeat(100) : group.name;
    const code = status === 413 ? "body_too_large" : status === 415 ? "unsupported_media_type" : "invalid";
    transport.mockResolvedValueOnce(json({ error: { code, message: "请求被拒绝" } }, status));
    submit(rejectedName);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(String(status));
    if (status === 413) expect(alert).toHaveTextContent("请求内容过大");
    if (status === 415) expect(alert).toHaveTextContent("仅接受 JSON");
    expect(nameInput()).toBeEnabled();
    expect(nameInput()).toHaveValue(rejectedName);
    transport.mockResolvedValueOnce(json(other, 201));
    submit(other.name);
    await screen.findByRole("button", { name: other.name });
    expect(posts()[1][1]?.body).toBe(JSON.stringify({ name: other.name }));
    expect(new Headers(posts()[1][1]?.headers).get("Idempotency-Key")).not.toBe(new Headers(posts()[0][1]?.headers).get("Idempotency-Key"));
  });

  it("ignores older reads even when the transport ignores abort", async () => {
    const old = deferred<Response>();
    transport.mockReturnValueOnce(old.promise).mockResolvedValueOnce(page([other]));
    render(<App />);
    fireEvent.click(screen.getByRole("button", { name: "刷新群组" }));
    fireEvent.click(await screen.findByRole("button", { name: other.name }));
    expect(transport.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => old.resolve(page([group])));
    expect(screen.queryByRole("button", { name: group.name })).not.toBeInTheDocument();
    expect(within(screen.getByRole("main")).getByRole("heading", { name: other.name })).toBeVisible();
  });

  it("does not remove or overwrite a confirmed creation with an older GET", async () => {
    const old = deferred<Response>();
    transport.mockReturnValueOnce(old.promise).mockResolvedValueOnce(json(group, 201));
    render(<App />);
    submit();
    await screen.findByRole("button", { name: group.name });
    await act(async () => old.resolve(page([{ ...group, name: "旧名称" }, other])));
    expect(screen.getByRole("button", { name: group.name })).toBeVisible();
    expect(screen.queryByRole("button", { name: "旧名称" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "刷新群组" }));
    await act(async () => {});
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
  });

  it("cleans up StrictMode reads and an in-flight mutation on unmount", async () => {
    const stale = deferred<Response>();
    transport.mockReturnValueOnce(stale.promise);
    const view = render(<StrictMode><App /></StrictMode>);
    await screen.findByText("尚无群组，可在下方创建。");
    expect(transport).toHaveBeenCalledTimes(2);
    expect(transport.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => stale.resolve(page([group])));
    expect(screen.queryByRole("button", { name: group.name })).not.toBeInTheDocument();
    const pending = deferred<Response>();
    transport.mockReturnValueOnce(pending.promise);
    submit();
    view.unmount();
    expect(transport.mock.calls[1][1]?.signal?.aborted).toBe(true);
    expect(posts()[0][1]?.signal?.aborted).toBe(true);
    await act(async () => pending.resolve(json(group, 201)));
  });
});
