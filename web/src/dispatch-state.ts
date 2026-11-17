import type { DispatchRequest, DispatchState } from "./dispatch-request-api";

export const sameDispatchRequest = (left: DispatchRequest, right: DispatchRequest) =>
  (Object.keys(left) as (keyof DispatchRequest)[]).every(field => left[field] === right[field]);
function stateSignature(state: DispatchState) {
  const d = state.delivery;
  return JSON.stringify([state.request_id, state.generation, state.status, state.attempt_count, state.next_attempt_at, state.last_error_code,
    state.held_reason, d && [d.message_id, d.delivery_id, d.conversation_id, d.coordinator_member_id, d.materialized_from_generation, d.created_at]]);
}
export function followsDispatchState(old: DispatchState, incoming: DispatchState) {
  if (incoming.generation < old.generation) return false;
  if (incoming.generation === old.generation) return stateSignature(old) === stateSignature(incoming);
  // Reads may skip retries, materialization and a decision; held/decided states cannot reopen.
  if (old.status === "pending") return incoming.attempt_count >= old.attempt_count
    && (incoming.attempt_count > old.attempt_count || incoming.next_attempt_at === old.next_attempt_at);
  // The only update after enqueued is one decision, preserving every receipt/retry field.
  return old.status === "enqueued" && (incoming.status === "assigned" || incoming.status === "declined")
    && incoming.generation - old.generation === 1
    && stateSignature(old) === stateSignature({ ...incoming, generation: old.generation, status: old.status });
}
