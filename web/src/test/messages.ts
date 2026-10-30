import type { Message } from "../api";
import { group } from "./groups";
export const delivery = { id: "delivery-1", message_id: "message-1", conversation_id: "conversation-1",
  recipient_id: "member-1", state: "queued", turn_id: "", error: "", created_at: group.created_at };
export const message: Message = { id: "message-1", group_id: group.id, topic_id: "", sender_id: "user",
  content: "第一条讨论", created_at: group.created_at, deliveries: [delivery] };
