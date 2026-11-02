import type { ReceptionConfiguration } from "../reception-defaults-api";
import { json } from "./groups";
import { provider } from "./members";

export const endpoint = "/api/v1/settings/reception-defaults";
export const saved: ReceptionConfiguration = {
  id: "defaults-1", revision: 1, provider_id: provider.id, model: "user-chosen-model",
  role: "区分建议与已确认决定。", created_at: "2026-11-01T06:00:00Z",
};
export function settings(configuration: ReceptionConfiguration | null = null, execution_enabled = false) {
  return json({ configuration, availability: {
    code: configuration ? "configured" : "not_configured", message: "服务提供的状态说明。",
  }, execution_enabled });
}
