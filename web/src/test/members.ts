import { group } from "./groups";
export const member = {
  id: "member-1", group_id: group.id, name: "研究员", role: "核对来源", provider_id: "",
  model: "", creator_id: "user", owner_id: "user", status: "active", workspace: "", created_at: group.created_at,
};
export const provider = {
  id: `pi_${"a".repeat(64)}`, name: "pi", path: "/opt/bin/pi", version: "", installation: "installed",
  account: "authenticated", connectivity: "unchecked", capabilities: { one_shot: false, resume: false, tools: false }, evidence: [],
};
