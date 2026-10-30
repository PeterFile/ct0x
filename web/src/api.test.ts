import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, createGroup, listGroups } from "./api";
import { group, json, page } from "./test/groups";

const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("bounds each page to 50 and encodes an opaque cursor without eager requests", async () => {
  transport.mockResolvedValue(page([group], "next"));
  const signal = new AbortController().signal;
  expect(await listGroups("a+/中文?=&", signal)).toEqual({ items: [group], next_cursor: "next" });
  expect(transport).toHaveBeenCalledExactlyOnceWith(
    "/api/v1/groups?limit=50&before=a%2B%2F%E4%B8%AD%E6%96%87%3F%3D%26", { signal },
  );
});

it.each([[400, "invalid"], [404, "not_found"], [409, "conflict"], [503, "unavailable"]])(
  "preserves HTTP status %s and the server error code", async (status, code) => {
    transport.mockResolvedValue(json({ error: { code, message: "服务拒绝请求" } }, Number(status)));
    await expect(listGroups()).rejects.toMatchObject({ status, code });
  },
);

it.each([200, 202])("does not confirm a creation with HTTP %s even with a valid object", async status => {
  transport.mockResolvedValue(json(group, status));
  await expect(createGroup({ name: group.name }, "operation-key")).rejects.toMatchObject({ status, code: "invalid_response" });
});

it("distinguishes disconnected transport from a malformed successful page", async () => {
  transport.mockRejectedValueOnce(new TypeError("offline"));
  await expect(listGroups()).rejects.toMatchObject({ status: 0, code: "disconnected" });
  transport.mockResolvedValueOnce(json({ items: null, next_cursor: "" }));
  await expect(listGroups()).rejects.toMatchObject({ status: 200, code: "invalid_response" });
  expect(new ApiError("测试", 400, "invalid")).toBeInstanceOf(Error);
});
