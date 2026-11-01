import type { Task, TaskInput } from "../task-api";
import { group } from "./groups";

export const task: Task = {
  id: "task-1", group_id: group.id, title: "核对来源", description: "逐项核对原始资料。",
  acceptance: "每项结论附上可验证的来源。", assignee_id: "", dependency_ids: [],
  status: "open", revision: 1, active_attempt_id: "", current_submission_id: "",
  created_at: "2026-10-30T12:00:00Z",
};
export const taskInput: TaskInput = { title: task.title, description: task.description, acceptance: task.acceptance };
export const secondTask: Task = { ...task, id: "task-2", title: "整理证据" };
export const submittedTask: Task = { ...task, status: "submitted", active_attempt_id: "attempt-1", current_submission_id: "submission-1" };
export const acceptedTask: Task = { ...submittedTask, status: "accepted", active_attempt_id: "" };
