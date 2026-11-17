import { afterEach, expect, it, vi } from "vitest";
import { authorizeWorkspaceGrant, getWorkspaceGrant, isWorkspaceGrantDefinitiveRejection } from "./workspace-grant-api";
import { group, json, other } from "./test/groups";

const memberId = "worker /1";
const input = Object.freeze({ root: "/existing/project ", scope: "read_write" as const });
const grant = { id: "grant-1", group_id: group.id, member_id: memberId, root: input.root, resolved_root: "/physical/project",
  resource_key: "workspace:device:inode", scope: "read_write", node_id: "local", authorized_by: "user", created_at: "2026-11-17T12:00:00.123456789Z" };
const path = `/api/v1/groups/${group.id}/members/${encodeURIComponent(memberId)}/workspace-grant`;
afterEach(() => vi.unstubAllGlobals());

it("uses the existing exact-member entry and verifies the immutable receipt without inventing version metadata", async () => {
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(grant)).mockResolvedValueOnce(json(grant, 201)); vi.stubGlobal("fetch", transport);
  await expect(getWorkspaceGrant(group.id, memberId)).resolves.toEqual(grant);
  await expect(authorizeWorkspaceGrant(group.id, memberId, { ...input, actor: "agent", member_id: "other" } as typeof input, "exact-grant-key")).resolves.toEqual(grant);
  expect(transport.mock.calls.map(([url]) => url)).toEqual([path, path]);
  const init = transport.mock.calls[1][1]; expect(init?.method).toBe("POST"); expect(init?.redirect).toBe("error");
  expect(new Headers(init?.headers).get("Idempotency-Key")).toBe("exact-grant-key"); expect(init?.body).toBe(JSON.stringify(input));
});
it("retains original path bytes and key across a network-unknown replay", async () => {
  const transport = vi.fn<typeof fetch>().mockRejectedValueOnce(new TypeError("Lost receipt")).mockResolvedValueOnce(json(grant, 201)); vi.stubGlobal("fetch", transport);
  const error = await authorizeWorkspaceGrant(group.id, memberId, input, "original-key").catch(error => error);
  expect(isWorkspaceGrantDefinitiveRejection(error)).toBe(false);
  await authorizeWorkspaceGrant(group.id, memberId, input, "original-key"); expect(transport.mock.calls[1]).toEqual(transport.mock.calls[0]);
});
it("accepts a real legacy read-only grant but never matches it to a write command", async () => {
  const actual = { ...grant, scope: "read_only" };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)).mockResolvedValueOnce(json(actual, 201)));
  await expect(getWorkspaceGrant(group.id, memberId)).resolves.toEqual(actual);
  const error = await authorizeWorkspaceGrant(group.id, memberId, input, "write-key").catch(error => error);
  expect(error).toMatchObject({ code: "invalid_response" }); expect(isWorkspaceGrantDefinitiveRejection(error)).toBe(false);
});
it.each([
  { id: "" }, { group_id: other.id }, { member_id: "wrong-member" }, { root: "relative" }, { resolved_root: "relative" },
  { resource_key: "workspace:" }, { resource_key: "not-physical" }, { scope: "all" }, { node_id: "remote" }, { authorized_by: "agent" },
  { created_at: "2026-02-30T12:00:00Z" }, { created_at: "0001-01-01T00:00:00Z" }, { created_at: "not-a-time" },
  { created_at: "2026-11-17T12:00:00Z\n" }, { extra: "unrecognized" },
])("does not trust malformed or foreign receipts %j even on successful HTTP", async change => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...grant, ...change }))
    .mockResolvedValueOnce(json({ ...grant, ...change }, 201)));
  await expect(getWorkspaceGrant(group.id, memberId)).rejects.toMatchObject({ code: "invalid_response" });
  const error = await authorizeWorkspaceGrant(group.id, memberId, input, "write-key").catch(error => error);
  expect(error).toMatchObject({ code: "invalid_response" }); expect(isWorkspaceGrantDefinitiveRejection(error)).toBe(false);
});
it("requires the exact original root in a write receipt and the specified HTTP status", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...grant, root: "/other/absolute" }, 201)).mockResolvedValueOnce(json(grant, 200)));
  await expect(authorizeWorkspaceGrant(group.id, memberId, input, "write-key")).rejects.toMatchObject({ code: "invalid_response" });
  await expect(authorizeWorkspaceGrant(group.id, memberId, input, "write-key")).rejects.toMatchObject({ code: "invalid_response" });
});
it("recognizes only an unambiguous protocol not_found as absence", async () => {
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({ error: { code: "not_found", message: "没有目录授权" } }, 404))
    .mockResolvedValueOnce(new Response("404 page not found\n", { status: 404 }))
    .mockResolvedValueOnce(json({ error: { code: "not_found", message: "missing" }, extra: true }, 404)); vi.stubGlobal("fetch", transport);
  await expect(getWorkspaceGrant(group.id, memberId)).resolves.toBeNull();
  await expect(getWorkspaceGrant(group.id, memberId)).rejects.toMatchObject({ code: "invalid_response" });
  await expect(getWorkspaceGrant(group.id, memberId)).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([
  '{"error":{"code":"conflict","code":"not_found","message":"ambiguous"}}',
  String.raw`{"error":{"co\u0064e":"conflict","code":"not_found","message":"ambiguous"}}`,
])("never treats duplicate error fields as a definitive rejection or absence: %s", async raw => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => new Response(raw, { status: 404 })));
  await expect(getWorkspaceGrant(group.id, memberId)).rejects.toMatchObject({ code: "invalid_response" });
  const error = await authorizeWorkspaceGrant(group.id, memberId, input, "write-key").catch(error => error);
  expect(isWorkspaceGrantDefinitiveRejection(error)).toBe(false);
});
it("rejects duplicate receipt identity keys before they can be collapsed by JSON.parse", async () => {
  const raw = JSON.stringify(grant).replace('"member_id":', '"member_id":"foreign","member_id":');
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(raw, { status: 201 })));
  await expect(authorizeWorkspaceGrant(group.id, memberId, input, "write-key")).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([400, 403, 404, 405, 409, 413, 415])("recognizes a well-formed HTTP %s rejection but never a 5xx as definitive", async status => {
  const codes: Record<number, string> = { 400: "invalid_input", 403: "invalid_origin", 404: "not_found", 405: "method_not_allowed", 409: "conflict", 413: "body_too_large", 415: "unsupported_media_type" };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ error: { code: codes[status], message: "rejected" } }, status))
    .mockResolvedValueOnce(json({ error: { code: "internal_error", message: "unconfirmed" } }, 500)));
  expect(isWorkspaceGrantDefinitiveRejection(await authorizeWorkspaceGrant(group.id, memberId, input, "write-key").catch(error => error))).toBe(true);
  expect(isWorkspaceGrantDefinitiveRejection(await authorizeWorkspaceGrant(group.id, memberId, input, "write-key").catch(error => error))).toBe(false);
});
it.each(["relative", " /absolute", "/invalid\0root", "/bad\ud800"])("rejects an invalid root %s before transport", async root => {
  const transport = vi.fn<typeof fetch>(); vi.stubGlobal("fetch", transport);
  await expect(authorizeWorkspaceGrant(group.id, memberId, { ...input, root }, "write-key")).rejects.toMatchObject({ code: "invalid_input" });
  expect(transport).not.toHaveBeenCalled();
});
