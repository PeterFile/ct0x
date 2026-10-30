import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { createMember, listMembers, listProviders } from "./api";
import { group, json, other, page } from "./test/groups";
import { member, provider } from "./test/members";
const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("encodes the group and opaque member cursor with a 50-item bound", async () => {
  const groupId = "群/组?+";
  transport.mockResolvedValueOnce(page([{ ...member, group_id: groupId }], "next"));
  await listMembers(groupId, "a+/中文?=&");
  expect(transport).toHaveBeenCalledExactlyOnceWith(
    "/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/members?limit=50&before=a%2B%2F%E4%B8%AD%E6%96%87%3F%3D%26", { signal: undefined },
  );
});
it("uses actual bounded provider catalogue responses without synthesizing identities", async () => {
  transport.mockResolvedValueOnce(page([provider], "next"));
  expect(await listProviders("a+/" )).toEqual({ items: [provider], next_cursor: "next" });
  expect(transport).toHaveBeenCalledExactlyOnceWith("/api/v1/providers?limit=50&before=a%2B%2F", { signal: undefined });
});
it.each(["GET", "POST"])("rejects mixed-group %s data", async method => {
  const foreign = { ...member, group_id: other.id };
  transport.mockResolvedValueOnce(method === "GET" ? page([foreign]) : json(foreign, 201));
  await expect(method === "GET" ? listMembers(group.id) : createMember(group.id, { name: member.name, role: member.role }, "key"))
    .rejects.toMatchObject({ code: "invalid_response" });
});
it.each([200, 202])("does not confirm a member from HTTP %s", async status => {
  transport.mockResolvedValueOnce(json(member, status));
  await expect(createMember(group.id, { name: member.name, role: member.role }, "key"))
    .rejects.toMatchObject({ status, code: "invalid_response" });
});
