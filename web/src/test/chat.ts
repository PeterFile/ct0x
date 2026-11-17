import { group } from "./groups";
import { message } from "./messages";
import { task } from "./tasks";

export const chatAdmission = {
  id: "chat-assessment-1", candidate_id: "candidate-1", group_id: group.id, topic_id: "",
  message_id: message.id, source_sender_id: "user", assessed_by: "member-assessor", turn_id: "assessor-turn",
  policy_revision: 3, candidate_generation: 12, disposition: "request_work", authority: "explicit_request",
  authorization_message_id: message.id, action: "text_submission", task_id: task.id, mandate_id: "mandate-auto",
  dispatch_request_id: "automatic-request-1", notice_message_id: "notice-auto", created_at: task.created_at,
};
export const autoDispatch = {
  request: { id: chatAdmission.dispatch_request_id, group_id: group.id, topic_id: "", task_id: task.id,
    task_revision: 1, authority_generation: 1, mandate_id: chatAdmission.mandate_id, coordinator_member_id: "coordinator-1",
    binding_revision: 2, policy_revision: 4, requested_by: "user", reason: "本群用户明确要求文本成果", created_at: task.created_at },
  state: { request_id: chatAdmission.dispatch_request_id, generation: 1, status: "pending", attempt_count: 0,
    next_attempt_at: task.created_at, last_error_code: "", held_reason: "", delivery: null },
};
export const autoExecution = { group_id: group.id, task_id: task.id, request_id: chatAdmission.dispatch_request_id,
  dispatch_status: "pending", coordinator_turn: null, decision: null, assignment: null,
  assignment_dispatch_status: null, binding: null, worker_turn: null };
