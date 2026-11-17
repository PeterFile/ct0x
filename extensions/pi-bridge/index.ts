import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { Value } from "typebox/value";
import { createClient } from "./bridge.mjs";

const pagination = Type.Object({
  limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 200, description: "每页条数，范围 1 至 200。" })),
  before: Type.Optional(Type.String({ description: "上一页返回的不透明游标。" })),
}, { additionalProperties: false });

const chatAssessment = Type.Object({
  disposition: Type.Union([Type.Literal("discuss"), Type.Literal("clarify"), Type.Literal("request_work"), Type.Literal("prepare_work")], { description: "仅受信chat_intake/v1解释绑定来源；Generic不能评估授权。讨论/否定/引用选discuss，范围不清选clarify。prepare_work仅限受支持的topic_summary。" }),
  action: Type.Optional(Type.Literal("text_submission", { description: "本片只允许文本 Submission，不授文件或外部动作。" })),
  title: Type.Optional(Type.String({ maxLength: 256, description: "仅request_work填写目标；prepare_work须省略。" })),
  description: Type.Optional(Type.String({ maxLength: 8192, description: "仅request_work填写边界；prepare_work须省略。" })),
  acceptance: Type.Optional(Type.String({ maxLength: 4096, description: "仅request_work填写文本验收；prepare_work须省略，服务器固定目标和验收。" })),
  reason: Type.String({ minLength: 1, maxLength: 4096, description: "结合可信来源、上下文与已启用 Policy 的语义评估依据；不能把 Agent 共识当授权。" }),
  question: Type.Optional(Type.String({ maxLength: 2048, description: "clarify 时一个具体的范围或授权问题。" })),
}, { additionalProperties: false });

const tools = [
  {
    name: "list_members",
    label: "查看群组成员",
    description: "分页查看当前执行所属群组的成员；群组与调用者由服务端绑定。",
    parameters: pagination,
  },
  {
    name: "read_channel",
    label: "读取频道消息",
    description: "分页读取绑定频道；source_kind/source_id为只读服务端事实，sender_id=user不证明真人授权。超过64 KiB时缩小页数。",
    parameters: pagination,
  },
  {
    name: "send_message",
    label: "发送群组消息",
    description: "在绑定频道保存消息，不代表投递确认。只有受信intake可intent_assessment；not_admitted是持久未准入，不是执行成功。intake须先评估，随后可公开讨论；真人触发轮最多邀请一个同群固定ID一次，成员轮不能递归邀请。",
    parameters: Type.Object({
      content: Type.String({ description: "消息正文。" }),
      recipient_ids: Type.Optional(Type.Array(Type.String(), { description: "接收成员标识；省略或空数组表示公开消息，不创建投递、不唤醒成员；仅显式列出的成员会进入投递队列。" })),
      intent_assessment: Type.Optional(chatAssessment),
    }, { additionalProperties: false }),
  },
  {
    name: "create_member",
    label: "创建协作成员",
    description: "按职责与加入原因创建或复用群内成员；运行配置、权限与容量由服务端决定。",
    parameters: Type.Object({
      name: Type.String({ description: "成员名称。" }),
      role: Type.String({ description: "成员职责。" }),
      reason: Type.String({ description: "需要该成员加入的原因。" }),
    }, { additionalProperties: false }),
  },
  {
    name: "search_history",
    label: "搜索历史消息",
    description: "按关键词检索当前执行的群组和话题历史；身份与话题由服务端绑定，excerpt 仅为消息前缀，不保证包含命中词或完整正文。",
    parameters: Type.Object({
      query: Type.String({ description: "搜索关键词。" }),
      limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 50, description: "每页条数，范围 1 至 50；省略为 20。" })),
      before: Type.Optional(Type.String({ description: "上一页返回的不透明历史游标；省略或空字符串表示起始页。" })),
    }, { additionalProperties: false }),
  },
  {
    name: "read_history",
    label: "读取历史片段",
    description: "读取当前执行绑定话题内指定消息的 Unicode 片段 [offset,next_offset)；身份与话题由服务端绑定，片段不保证是完整正文；依据 content_complete 判断是否覆盖完整消息。",
    parameters: Type.Object({
      message_id: Type.String({ description: "当前绑定群组和话题内的消息标识。" }),
      offset: Type.Optional(Type.Integer({ minimum: 0, description: "起始 Unicode rune 偏移；省略为 0。" })),
      length: Type.Optional(Type.Integer({ minimum: 1, maximum: 4096, description: "读取的 Unicode rune 数，范围 1 至 4096；省略为 1024。" })),
    }, { additionalProperties: false }),
  },
  {
    name: "decide_dispatch",
    label: "决定任务分派",
    description: "为当前绑定任务记录分派或拒绝决定；调用者、群组、任务及权限由服务端绑定。结果未知时须先对账，不要自动重试或更换幂等键。",
    parameters: Type.Object({
      kind: Type.Union([Type.Literal("assign"), Type.Literal("decline")], { description: "assign 表示分派，decline 表示拒绝。" }),
      reason: Type.String({ description: "分派或拒绝的理由。" }),
      worker_member_id: Type.Optional(Type.String({ minLength: 1, description: "assign 时必须提供非空的工作成员标识；decline 时禁止提供此字段。" })),
    }, { additionalProperties: false }),
  },
  {
    name: "submit_result",
    label: "提交任务结果",
    description: "为绑定尝试提交成果；聊天text_submission禁止产物版本引用，topic_summary摘要须非空有效UTF-8且最多8192字节，其他任务按原约定。提交不代表审查通过；结果未知时先对账，不要自动重试或换幂等键。",
    parameters: Type.Object({
      summary: Type.String({ description: "任务结果摘要。" }),
      artifact_versions: Type.Optional(Type.Array(Type.Object({
        artifact_id: Type.String({ description: "产物标识。" }),
        version_id: Type.String({ description: "产物版本标识。" }),
      }, { additionalProperties: false }), { description: "可选的产物版本引用列表。" })),
    }, { additionalProperties: false }),
  },
] as const;

export default function piBridge(pi: ExtensionAPI) {
  const invoke = createClient(process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG);
  for (const tool of tools) {
    pi.registerTool<typeof tool.parameters>({
      ...tool,
      async execute(toolCallId, params, signal, _onUpdate, _ctx) {
        let valid = false;
        let payload: unknown;
        try {
          if (Value.Check(tool.parameters, params)) {
            const encoded = JSON.stringify(params, (_key, value: unknown) => {
              if (value === undefined) throw new Error();
              return value;
            });
            payload = JSON.parse(encoded);
            valid = Value.Check(tool.parameters, payload);
            if (valid && tool.name === "send_message") {
              const message = payload as { recipient_ids?: string[]; intent_assessment?: unknown };
              if (Object.hasOwn(message, "intent_assessment")) valid = !Object.hasOwn(message, "recipient_ids");
            }
            if (valid && tool.name === "decide_dispatch") {
              // Keep an object schema for Pi; check the conditional field on the safe snapshot.
              const decision = payload as { kind: string; worker_member_id?: string };
              valid = Object.hasOwn(decision, "worker_member_id") === (decision.kind === "assign");
            }
          }
        } catch { /* Reject unsafe runtime values without exposing their errors. */ }
        if (!valid) throw new Error("成员工具参数无效，未发送请求。");
        const result = await invoke(tool.name, payload, toolCallId, signal);
        return { content: [{ type: "text", text: result.text }], details: undefined };
      },
    });
  }
}
