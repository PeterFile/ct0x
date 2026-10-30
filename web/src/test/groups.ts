export const group = {
  id: "group-1", name: "调研", description: "核对资料来源", created_at: "2026-10-30T12:00:00Z",
};
export const other = { ...group, id: "group-2", name: "写作" };
export const json = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status });
export const page = (items: unknown[] = [], next_cursor = "") => json({ items, next_cursor });
export function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(accept => { resolve = accept; });
  return { promise, resolve };
}
