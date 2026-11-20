import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type, type Static } from "typebox";
import { Value } from "typebox/value";
import { INVESTIGATION_ARTIFACT, INVESTIGATION_ASSESSMENT_ACTION, INVESTIGATION_CHANNEL, openBridge, validUnicode } from "./bridge.mjs";

const discussionOnlyExcerptBytes = 4096;
const discussionExpectedReplyJSONBytes = 1024;
const investigationSummaryBytes = 8192;
const investigationResult = Type.Object({ summary: Type.String({ minLength: 1, maxLength: investigationSummaryBytes,
  description: "纯文本调查结论（最多 8192 UTF-8 字节）；说明用了哪些获准资料和未决问题。提交不代表接纳。" }) }, { additionalProperties: false });

const workInputBytes = { reason: 4096, content: 16384, constraint: 2048, handBackReason: 2048 } as const;

const pagination = Type.Object({
  limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 200, description: "每页条数，范围 1 至 200。" })),
  before: Type.Optional(Type.String({ description: "上一页返回的不透明游标。" })),
}, { additionalProperties: false });

const chatAssessmentFields = {
  title: Type.Optional(Type.String({ maxLength: 256, description: "仅request_work填写目标；prepare_work须省略。" })),
  description: Type.Optional(Type.String({ maxLength: 8192, description: "仅request_work填写边界；prepare_work须省略。" })),
  acceptance: Type.Optional(Type.String({ maxLength: 4096, description: "仅request_work填写交付验收；prepare_work须省略，服务器固定目标和验收。" })),
  reason: Type.String({ minLength: 1, maxLength: 4096, description: "结合可信来源、上下文与已启用 Policy 的语义评估依据；不能把 Agent 共识当授权。" }),
  question: Type.Optional(Type.String({ maxLength: 2048, description: "clarify 时一个具体的范围或授权问题。" })),
  repair: Type.Optional(Type.Object({
    review_id: Type.String({ minLength: 1, maxLength: 256, description: "仅引用服务器 repair_source 中的真实本机用户评审；不能选择调用者、目录或授权。" }),
    authorization: Type.Union([Type.Literal("review_request"), Type.Literal("original_agreement"), Type.Literal("chat_request")], { description: "解释实际评审修复请求、原冻结同范围约定或当前真人聊天修复请求；chat_request还须服务器核对当前真实聊天命令和正式Review引用。拒收本身、Agent共识、平台消息标签均不自动授权。服务器重验许可和版本范围。" }),
  }, { additionalProperties: false })),
};

const discussionAction = Type.Object({
  kind: Type.Union([Type.Literal("invite"), Type.Literal("reply"), Type.Literal("resolve")]),
  recipient_id: Type.Optional(Type.String({ minLength: 1, maxLength: 256, description: "仅 invite 必填；选择 list_members 返回的一个现有同群成员 ID。" })),
  expected_reply: Type.Optional(Type.String({ maxLength: discussionExpectedReplyJSONBytes - 2,
    description: `仅 invite 可填；按 Go JSON 字符串编码（含引号、转义）最多 ${discussionExpectedReplyJSONBytes} UTF-8 字节；省略时以邀请正文为准。` })),
}, { additionalProperties: false, description: "仅用于已保存讨论评估后的接力；不得与 recipient_ids 或 intent_assessment 同传。reply 与 resolve 不带 recipient_id 或 expected_reply。" });

function sendMessageParameters(continueWork: boolean, discussionOnly = false, investigation = false, relay = false) {
  const disposition = Type.Union([
    Type.Literal("discuss"), Type.Literal("clarify"), Type.Literal("request_work"), Type.Literal("prepare_work"),
    ...(continueWork ? [Type.Literal("continue_work")] : []),
  ], { description: "仅受信chat_intake/v1解释绑定来源；Generic不能评估授权。讨论/否定/引用选discuss，范围不清选clarify。" +
    (discussionOnly ? "原文明确仅讨论时优先选discuss，逐字保留discussion_only_excerpt，不新建或续办工作。" : "") +
    (continueWork ? `continue_work仅用于当前本机用户明确要求在原授权内续办；有明确Task编号就用followup.task_id引用原文。不填action或新任务字段。续办理由不得为空白，最多 ${workInputBytes.reason} UTF-8 字节。` : "") +
    (investigation ? "prepare_work按已签发的action分流：text_submission仅用于受支持的topic_summary；read_only_investigation仅用于获准范围内明确的新调查，不用于续办。" :
      "prepare_work仅限受支持的topic_summary。") });
  const assessment = Type.Object({
    disposition,
    action: Type.Optional(Type.Union([Type.Literal("text_submission"), Type.Literal("workspace_change"),
      ...(investigation ? [Type.Literal(INVESTIGATION_ASSESSMENT_ACTION)] : []),
    ], { description: investigation ? "仅当本轮已签发该动作、chat_candidate.policy 提供获准调查范围，且绑定的单收件人本机用户原文明确要求另开范围内的调查时，使用 prepare_work、action=read_only_investigation 和 reason。目标、资料和验收由服务器绑定；模型不能授予权限。" :
      "结合可信来源和上下文提出文本交付或目录文件修改；只是一项语义建议，当前 Policy、目录授权和 Task 范围由服务器核定，模型不能授予 IO 权限。" })),
    ...chatAssessmentFields,
    ...(discussionOnly ? { discussion_only_excerpt: Type.Optional(Type.String({ minLength: 1, maxLength: discussionOnlyExcerptBytes,
      description: "仅在 discuss 时逐字引用绑定本机用户消息中明确标示只讨论的连续原文；保留空白、标点和 Unicode 原文，最多 4096 UTF-8 字节。没有则省略。",
    })) } : {}),
    ...(continueWork ? { followup: Type.Optional(Type.Object({
      task_id: Type.Optional(Type.String({ minLength: 1, maxLength: 256, description: "仅填写用户明确引用的任务；实际工作和权限由服务器核定。" })),
      constraint: Type.Optional(Type.String({ maxLength: workInputBytes.constraint,
        description: `仅引用当前用户原文，不授予权限。空字符串等同省略；非空时不得全为空白且最多 ${workInputBytes.constraint} UTF-8 字节。` })),
    }, { additionalProperties: false, description: "仅用于受信 continue_work 评估。" })) } : {}),
  }, { additionalProperties: false });
  return Type.Object({
    content: Type.String({ description: "消息正文。" + (continueWork ? `续办正文不得为空白，最多 ${workInputBytes.content} UTF-8 字节。` : "") }),
    recipient_ids: Type.Optional(Type.Array(Type.String(), { description: "接收成员标识；省略或空数组表示公开消息，不创建投递、不唤醒成员；仅显式列出的成员会进入投递队列。" })),
    reply_to_message_id: Type.Optional(Type.String({ minLength: 1, maxLength: 256,
      description: "仅普通消息使用：当前群组和话题内原消息的 ID（最多 256 UTF-8 字节）；不得与 intent_assessment 或 discussion 同传。" })),
    intent_assessment: Type.Optional(assessment),
    ...(relay ? { discussion: Type.Optional(discussionAction) } : {}),
  }, { additionalProperties: false });
}

const workInputId = Type.String({ minLength: 1, maxLength: 256, description: "服务器返回的本轮工作输入标识。" });

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
    parameters: sendMessageParameters(false),
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
    description: "为绑定尝试提交成果；text_submission禁止产物版本引用，workspace_change必须引用本任务真实发布的精确Artifact版本，不能用完成文字替代文件或版本回执。topic_summary摘要最多8192字节。提交不代表审查通过；结果未知先对账，不自动重试或换幂等键。",
    parameters: Type.Object({
      summary: Type.String({ description: "任务结果摘要。" }),
      artifact_versions: Type.Optional(Type.Array(Type.Object({
        artifact_id: Type.String({ description: "产物标识。" }),
        version_id: Type.String({ description: "产物版本标识。" }),
      }, { additionalProperties: false }), { description: "可选的产物版本引用列表。" })),
    }, { additionalProperties: false }),
  },
  {
    name: "workspace_list",
    label: "查看任务目录",
    description: "列出绑定文件任务的获准目录，非递归且不跟随符号链接；根目录使用相对路径 .。目录和身份由服务器绑定，CWD 不授予权限；结果超限整体拒绝，不截断。",
    parameters: Type.Object({ path: Type.String({ minLength: 1, maxLength: 4096, description: "规范 slash 相对目录路径；根目录为 .，不得包含 ..、反斜杠或控制字符。" }) }, { additionalProperties: false }),
  },
  {
    name: "workspace_read",
    label: "读取任务文件",
    description: "从绑定任务的获准目录读取普通UTF-8文本及实际SHA256，不跟随符号链接。最大64 KiB原文仍须满足完整JSON结果64 KiB预算；超限拒绝，不截断。",
    parameters: Type.Object({ path: Type.String({ minLength: 1, maxLength: 4096, description: "规范 slash 相对文件路径；不能选择根目录或授权目录以外的路径。" }) }, { additionalProperties: false }),
  },
  {
    name: "workspace_write",
    label: "写入任务文件",
    description: "仅在绑定worker文件任务内创建或完整替换UTF-8文本。替换须提供读取到的实际expected_sha256；先保存实际旧字节与持久intent，再写盘并实际读回核验。text最多8192 UTF-8字节，不截断。返回服务器operation_id；未知先对账，不重试、不回滚、不换调用标识。",
    parameters: Type.Object({
      path: Type.String({ minLength: 1, maxLength: 4096, description: "规范 slash 相对文件路径，不自动创建父目录。" }),
      create: Type.Boolean({ description: "true仅新建且禁止expected_sha256；false仅替换且必须提供expected_sha256。" }),
      text: Type.String({ maxLength: 8192, description: "完整新文本，最多8192 UTF-8字节；空文本合法。" }),
      expected_sha256: Type.Optional(Type.String({ pattern: "^[a-f0-9]{64}$", description: "仅替换时提供实际preimage的小写SHA256。" })),
    }, { additionalProperties: false }),
  },
  {
    name: "publish_workspace_artifact",
    label: "发布任务文件版本",
    description: "发布本轮本任务已done的operation文件实际字节，保存不可变Artifact精确版本；旧版本永留。新建提供name，更新提供artifact_id与expected_version_id且禁止name。结果中的精确version_id用于submit_result，发布不代表提交或接纳。",
    parameters: Type.Object({
      operation_id: Type.String({ minLength: 1, maxLength: 256, description: "workspace_write返回的服务器operation_id，不是模型自定幂等键。" }),
      artifact_id: Type.Optional(Type.String({ minLength: 1, maxLength: 256, description: "仅更新现有Artifact时提供。" })),
      name: Type.Optional(Type.String({ minLength: 1, maxLength: 256, description: "仅新建Artifact时提供名称。" })),
      expected_version_id: Type.Optional(Type.String({ minLength: 1, maxLength: 256, description: "更新时必须提供实际基于的精确版本。" })),
    }, { additionalProperties: false }),
  },
  {
    name: "read_work_inputs",
    label: "读取工作输入",
    description: "只读取本轮绑定的工作输入；读取不会确认已处理。",
    defaultActive: false,
    parameters: Type.Object({}, { additionalProperties: false }),
  },
  {
    name: "ack_work_input",
    label: "确认工作输入",
    description: "显式确认本轮已处理指定工作输入；结果未知时保留原调用标识并先对账，不自动重试。",
    defaultActive: false,
    parameters: Type.Object({ input_id: workInputId }, { additionalProperties: false }),
  },
  {
    name: "hand_back_work_input",
    label: "交回工作输入",
    description: "将未处理的工作输入交回并说明原因；结果未知时保留原调用标识并先对账，不自动重试。",
    defaultActive: false,
    parameters: Type.Object({ input_id: workInputId, reason: Type.String({ minLength: 1, maxLength: workInputBytes.handBackReason,
      description: `交回理由不得为空白，最多 ${workInputBytes.handBackReason} UTF-8 字节。` }) }, { additionalProperties: false }),
  },
] as const;

const investigationID = Type.String({ minLength: 1, maxLength: 128 });
const investigationReads = [
  { name: INVESTIGATION_CHANNEL, label: "读取调查话题", description: "只读取本轮获准的绑定话题，不可选择其他话题。", parameters: pagination },
  { name: INVESTIGATION_ARTIFACT, label: "读取调查成果版本", description: "只读取本轮批准的确切版本；offset 和 next_offset 是字节位置。",
    parameters: Type.Object({
      artifact_id: investigationID, version_id: investigationID,
      offset: Type.Optional(Type.Integer({ minimum: 0, maximum: 64 << 20 })),
      max_bytes: Type.Optional(Type.Integer({ minimum: 1, maximum: 16384 })),
    }, { additionalProperties: false }) },
] as const;

function workspacePath(value: string, directory: boolean): boolean {
  return Buffer.byteLength(value, "utf8") <= 4096 && !/[\\\\\u0000-\u001f\u007f-\u009f\ud800-\udfff]/u.test(value) &&
    (directory && value === "." || value.split("/").every(part => part !== "" && part !== "." && part !== ".."));
}

function utf8TextWithinBytes(value: string, limit: number): boolean {
  return validUnicode(value) && Buffer.byteLength(value, "utf8") <= limit;
}

function validNonBlankText(value: string, limit: number): boolean {
  // Go's TrimSpace uses Unicode White_Space, not JavaScript's trim (NEL/BOM).
  return utf8TextWithinBytes(value, limit) && /\P{White_Space}/u.test(value) && !value.includes("\u0000");
}

function validOpaqueIDShape(value: string): boolean {
  return value !== "" && utf8TextWithinBytes(value, 256) && !/[\u0000-\u001f\u007f-\u009f]/u.test(value);
}

function validWorkInputID(value: string): boolean {
  // Match Go's TrimSpace at the boundaries without rewriting the ID.
  return validOpaqueIDShape(value) && !/^\p{White_Space}|\p{White_Space}$/u.test(value);
}

function validPlainQuote(message: { reply_to_message_id?: unknown }): boolean {
  const id = message.reply_to_message_id;
  return typeof id === "string" && id !== "." && id !== ".." && validWorkInputID(id) &&
    !Object.hasOwn(message, "intent_assessment") && !Object.hasOwn(message, "discussion");
}

function validExpectedReply(value: string): boolean {
  if (value === "") return true;
  if (!validNonBlankText(value, discussionExpectedReplyJSONBytes)) return false;
  // Go json.Marshal also escapes HTML-sensitive characters and U+2028/U+2029.
  const encoded = JSON.stringify(value).replace(/[<>&\u2028\u2029]/gu, char => "\\u" + char.charCodeAt(0).toString(16).padStart(4, "0"));
  return Buffer.byteLength(encoded, "utf8") <= discussionExpectedReplyJSONBytes;
}

function validDiscussion(message: { content: string; discussion?: Static<typeof discussionAction> }): boolean {
  const action = message.discussion;
  if (!action || Object.hasOwn(message, "recipient_ids") || Object.hasOwn(message, "intent_assessment") ||
      !validNonBlankText(message.content, 16384)) return false;
  return action.kind === "invite" ?
    typeof action.recipient_id === "string" && validOpaqueIDShape(action.recipient_id) &&
      (action.expected_reply === undefined || validExpectedReply(action.expected_reply)) :
    !Object.hasOwn(action, "recipient_id") && !Object.hasOwn(action, "expected_reply");
}

export default function piBridge(pi: ExtensionAPI) {
  const { invoke, continueWork, discussionOnlyExcerpt, investigationIntake, investigationTools, discussionRelay } = openBridge(
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG,
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256,
  );
  if (investigationTools) {
    // Independent of Pi's CLI allowlist. Do not register unrelated platform
    // tools and fail closed on any unexpected host or other extension call.
    pi.on("tool_call", event => investigationTools.includes(event.toolName) ? undefined :
      { block: true, reason: "工具不在本轮只读调查范围内。" });
  }
  for (const tool of investigationTools ? [...investigationReads, ...tools] : tools) {
    if (investigationTools && !investigationTools.includes(tool.name)) continue;
    let parameters = tool.name === "send_message" && (continueWork || discussionOnlyExcerpt || investigationIntake || discussionRelay) ?
      sendMessageParameters(continueWork, discussionOnlyExcerpt, investigationIntake, discussionRelay) : tool.parameters;
    if (investigationTools && tool.name === "submit_result") parameters = investigationResult;
    const description = discussionRelay && tool.name === "send_message" ?
      "在绑定频道保存消息，不代表投递确认。单独保存 intent_assessment 后可用 discussion 接力，不传 recipient_ids。仅 invite 选择一个成员；reply 由服务器返回给上一位，resolve 只公开收束。not_admitted 不是执行成功。" :
      discussionRelay && tool.name === "read_channel" ? tool.description + " 可能返回 discussion_context；按消息 ID 用 read_history 查阅正文。" : tool.description;
    pi.registerTool<typeof parameters>({
      ...tool,
      description,
      parameters,
      ...(tool.name === "send_message" ? {
        prepareArguments(args: unknown) {
          // Check quote and relay fields before Pi can drop optional nulls or
          // coerce types into an unrelated send or a different handoff.
          const message = args as { content: string; reply_to_message_id?: unknown; discussion?: Static<typeof discussionAction>;
            intent_assessment?: Record<string, unknown> } | null;
          if (message && Object.hasOwn(message, "reply_to_message_id") &&
              (!Value.Check(parameters, args) || !validPlainQuote(message))) {
            throw new Error("成员工具参数无效，未发送请求。");
          }
          if (message && Object.hasOwn(message, "discussion") &&
              (!discussionRelay || !Value.Check(parameters, args) || !validDiscussion(message))) {
            throw new Error("成员工具参数无效，未发送请求。");
          }
          const assessment = message?.intent_assessment;
          if (discussionOnlyExcerpt && message && assessment && Object.hasOwn(assessment, "discussion_only_excerpt") &&
              (typeof assessment.discussion_only_excerpt !== "string" || Object.hasOwn(message, "recipient_ids"))) {
            throw new Error("成员工具参数无效，未发送请求。");
          }
          return args as Static<typeof parameters>;
        },
      } : {}),
      async execute(toolCallId, params, signal, _onUpdate, _ctx) {
        let valid = false;
        let payload: unknown;
        try {
          if (Value.Check(parameters, params)) {
            const encoded = JSON.stringify(params, (_key, value: unknown) => {
              if (value === undefined) throw new Error();
              return value;
            });
            payload = JSON.parse(encoded);
            valid = Value.Check(parameters, payload);
            if (valid && tool.name === "send_message") {
              const message = payload as { content: string; reply_to_message_id?: string; discussion?: Static<typeof discussionAction>;
                recipient_ids?: string[]; intent_assessment?: {
                  disposition: string; reason: string; action?: string; discussion_only_excerpt?: string;
                  repair?: { review_id: string }; followup?: { task_id?: string; constraint?: string };
                } };
              if (Object.hasOwn(message, "reply_to_message_id")) {
                valid = validPlainQuote(message);
              } else if (Object.hasOwn(message, "discussion")) {
                valid = discussionRelay && validDiscussion(message);
              } else if (Object.hasOwn(message, "intent_assessment")) {
                valid = !Object.hasOwn(message, "recipient_ids");
                const assessment = message.intent_assessment!;
                if (Object.hasOwn(assessment, "discussion_only_excerpt")) {
                  valid = valid && assessment.disposition === "discuss" && validUnicode(message.content) && validUnicode(assessment.reason) &&
                    validNonBlankText(assessment.discussion_only_excerpt!, discussionOnlyExcerptBytes);
                }
                if (assessment.action === INVESTIGATION_ASSESSMENT_ACTION) {
                  valid = valid && assessment.disposition === "prepare_work" &&
                    Object.keys(assessment).every(key => ["disposition", "action", "reason"].includes(key));
                }
                if (Object.hasOwn(assessment, "repair")) {
                  const id = assessment.repair!.review_id;
                  valid = valid && assessment.disposition === "request_work" && id.trim() === id && validOpaqueIDShape(id);
                }
                if (assessment.disposition === "continue_work") {
                  const { task_id: reference, constraint = "" } = assessment.followup ?? {};
                  valid = valid && validNonBlankText(assessment.reason, workInputBytes.reason) &&
                    validNonBlankText(message.content, workInputBytes.content) &&
                    (constraint === "" || validNonBlankText(constraint, workInputBytes.constraint)) &&
                    (reference === undefined || validWorkInputID(reference)) &&
                    Object.keys(assessment).every(key => ["disposition", "reason", "followup"].includes(key));
                } else {
                  valid = valid && !Object.hasOwn(assessment, "followup");
                }
              }
            }
            if (valid && (tool.name === "ack_work_input" || tool.name === "hand_back_work_input")) {
              const input = payload as { input_id: string; reason?: string };
              valid = validWorkInputID(input.input_id) &&
                (tool.name !== "hand_back_work_input" || validNonBlankText(input.reason!, workInputBytes.handBackReason));
            }
            if (valid && tool.name === "decide_dispatch") {
              // Keep an object schema for Pi; check the conditional field on the safe snapshot.
              const decision = payload as { kind: string; worker_member_id?: string };
              valid = Object.hasOwn(decision, "worker_member_id") === (decision.kind === "assign");
            }
            if (valid && tool.name === INVESTIGATION_ARTIFACT) {
              const { artifact_id, version_id } = payload as { artifact_id: string; version_id: string };
              valid = [artifact_id, version_id].every(id => validWorkInputID(id) && Buffer.byteLength(id, "utf8") <= 128);
            }
            if (valid && investigationTools && tool.name === "submit_result") {
              valid = validNonBlankText((payload as { summary: string }).summary, investigationSummaryBytes);
            }
            if (valid && (tool.name === "workspace_list" || tool.name === "workspace_read" || tool.name === "workspace_write")) {
              const file = payload as { path: string; create?: boolean; text?: string; expected_sha256?: string };
              valid = workspacePath(file.path, tool.name === "workspace_list");
              if (tool.name === "workspace_write") valid = valid &&
                Object.hasOwn(file, "expected_sha256") === !file.create && utf8TextWithinBytes(file.text!, 8192);
            }
            if (valid && tool.name === "publish_workspace_artifact") {
              const artifact = payload as { operation_id: string; artifact_id?: string; name?: string; expected_version_id?: string };
              const update = Object.hasOwn(artifact, "artifact_id");
              valid = Object.hasOwn(artifact, "expected_version_id") === update && Object.hasOwn(artifact, "name") === !update &&
                Object.values(artifact).every(value => utf8TextWithinBytes(value, 256) && value.trim() !== "" && !/[\u0000-\u001f\u007f-\u009f]/u.test(value));
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
