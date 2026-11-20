import { randomUUID } from "node:crypto";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PendingReviews } from "./PendingReviews";
import type { PendingReview, TaskReview } from "./task-api";
import { deferred, json, page } from "./test/groups";

const title = "周末样本验证", groupName = "告警";
const reviewItem = (group_id: string, task_revision: number): PendingReview => ({
  group: { id: group_id, name: groupName },
  task: { id: `task-${group_id}`, title, acceptance: "核对样本来源" },
  submission: { id: `submission-${group_id}`, group_id, task_id: `task-${group_id}`, task_revision,
    attempt_id: `attempt-${group_id}`, summary: `${group_id} 交付摘要`, artifact_versions: [],
    submitted_by: "worker", created_at: "2026-11-19T12:00:00Z" },
});

afterEach(() => vi.unstubAllGlobals());

it("distinguishes same-name review cards and the chosen receipt by verified project", async () => {
  const payments = reviewItem("group-payments", 3), settlement = reviewItem("group-settlement", 7);
  const proof = deferred<Response>();
  const workspace_id = "workspace-1";
  const identity = (group_id: string, project_id: string) => json({ group_id, project_id, workspace_id, channel_id: `channel-${group_id}` });
  const project = (id: string, name: string) => json({ id, name, workspace_id });
  const { group, task, submission } = settlement;
  const postPath = `/api/v1/groups/${group.id}/tasks/${task.id}/reviews`;
  const reason = "结算样本已核对。";
  const receipt: TaskReview = { id: "settlement-review", group_id: group.id, task_id: task.id,
    task_revision: submission.task_revision, submission_id: submission.id, outcome: "accepted", reason,
    reviewed_by: "user", artifact_versions: submission.artifact_versions, created_at: submission.created_at };
  const transport = vi.fn<typeof fetch>(async (input, init) => {
    if (String(input) === postPath && init?.method === "POST") return json(receipt, 201);
    switch (String(input)) {
    case "/api/v1/me/pending-reviews?limit=50": return page([payments, settlement]);
    case `/api/v1/groups/${payments.group.id}/identity`: return identity(payments.group.id, "payments");
    case `/api/v1/groups/${group.id}/identity`: return identity(group.id, "settlement");
    case "/api/v1/projects/payments": return project("payments", "支付项目");
    case "/api/v1/projects/settlement": return proof.promise;
    default: throw new Error(`Unexpected request: ${String(input)}`);
    }
  });
  vi.stubGlobal("crypto", { randomUUID });
  vi.stubGlobal("fetch", transport);
  render(<PendingReviews active />);

  const fallback = `${groupName} · ${title}`, firstName = `支付项目 · ${fallback}`;
  const first = within(await screen.findByRole("article", { name: firstName }));
  expect(first.getByText("来源项目：支付项目")).toBeVisible();
  expect(first.getByText(payments.submission.summary)).toBeVisible();
  const pending = within(screen.getByRole("article", { name: fallback }));
  expect(pending.getByText(submission.summary)).toBeVisible();
  expect(pending.getByRole("button", { name: "接纳" })).toBeEnabled();
  fireEvent.change(pending.getByRole("textbox"), { target: { value: reason } });

  await act(async () => proof.resolve(project("settlement", "结算项目")));
  const chosen = within(await screen.findByRole("article", { name: `结算项目 · ${fallback}` }));
  expect(screen.getByRole("article", { name: firstName })).toBeVisible();
  expect(chosen.getByText("来源项目：结算项目")).toBeVisible();
  expect(chosen.getByText(submission.summary)).toBeVisible();
  expect(chosen.getByRole("textbox")).toHaveValue(reason);
  fireEvent.click(chosen.getByRole("button", { name: "接纳" }));
  const saved = within(await screen.findByRole("region", { name: "验收回执" }));
  expect(saved.getByRole("status")).toHaveTextContent(`已保存：接纳 · 结算项目 / ${groupName} / ${title} · 任务版本 7`);
  const posts = transport.mock.calls.filter(([, init]) => init?.method === "POST");
  expect(posts).toHaveLength(1);
  expect(posts[0][0]).toBe(postPath);
  expect(JSON.parse(String(posts[0][1]?.body))).toEqual({
    expected_revision: submission.task_revision, submission_id: submission.id, outcome: "accepted", reason,
  });
});
