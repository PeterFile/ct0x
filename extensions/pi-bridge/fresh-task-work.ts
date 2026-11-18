import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { Value } from "typebox/value";
import { createFreshTaskWorkClient, validWorkInputCursor, validWorkInputID } from "./fresh-task-work.mjs";

const inputID = Type.String({ minLength: 1, maxLength: 256, description: "本轮服务端给出的工作输入 ID，不是 Task 或 Assignment ID。" });
const pageFields = {
  limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 20, description: "目录每页条数，默认 10。" })),
  before: Type.Optional(Type.String({ minLength: 1, maxLength: 512, description: "上一页的 next_cursor，原样使用；第一页省略。" })),
};
const segmentFields = {
  offset: Type.Optional(Type.Integer({ minimum: 0, maximum: Number.MAX_SAFE_INTEGER, description: "规范 JSON 的 UTF-8 字节偏移，默认 0；续读使用 next_offset。" })),
  max_bytes: Type.Optional(Type.Integer({ minimum: 4, maximum: 8192, description: "片段最多字节，默认 4096；服务端不会切开 UTF-8 字符。" })),
};
const closed = { additionalProperties: false } as const;
const page = Type.Object(pageFields, closed);
const segment = Type.Object({ input_id: inputID, ...segmentFields }, closed);
const tools = [
  {
    name: "read_work_inputs",
    label: "读取工作输入",
    description: "仅列出服务端绑定的工作输入目录（limit/before），或按 input_id 分段读取 {content,constraint,disposition}。constraint 只可能是本条原文的补充；returned 不返回正文或约束。complete 只表示末尾；处理状态变化后从 offset 0 重读。读取不是确认，不扩大原授权。",
    // Pi expects top-level object properties; B accepts exactly one of these two shapes.
    parameters: Type.Object({ ...pageFields, input_id: Type.Optional(inputID), ...segmentFields }, { ...closed, oneOf: [page, segment] }),
  },
  {
    name: "ack_work_input",
    label: "确认单条工作输入",
    description: "实际处理后显式确认该输入，不提交成果。结果未知须保留原调用标识和参数先对账，不自动重试。",
    parameters: Type.Object({ input_id: inputID }, closed),
  },
  {
    name: "hand_back_work_input",
    label: "交回单条工作输入",
    description: "交回未处理的单条输入并说明原因，不交回整项 Task，不撤销原授权或自动重派。结果未知须保留原调用标识和参数先对账，不自动重试。",
    parameters: Type.Object({ input_id: inputID, reason: Type.String({ minLength: 1, maxLength: 2048, description: "非空交回原因，最多 2048 UTF-8 字节。" }) }, closed),
  },
  {
    name: "submit_result",
    label: "提交文本成果",
    description: "为当前绑定尝试提交纯文本，不接受产物版本。未处理的输入会阻止提交；提交不等于验收。结果未知须保留原调用标识和参数先对账，不自动重试。",
    parameters: Type.Object({ summary: Type.String({ minLength: 1, maxLength: 8192, description: "非空文本成果，最多 8192 UTF-8 字节。" }) }, closed),
  },
] as const;

type Tool = typeof tools[number];

function validText(value: unknown, byteLimit: number): boolean {
  return typeof value === "string" && /\P{White_Space}/u.test(value) &&
    !/[\u0000\ud800-\udfff]/u.test(value) && Buffer.byteLength(value, "utf8") <= byteLimit;
}

function validArgs(tool: Tool, value: unknown): boolean {
  if (!Value.Check(tool.parameters, value)) return false;
  const args = value as Record<string, unknown>;
  switch (tool.name) {
    case "read_work_inputs":
      return Object.hasOwn(args, "input_id") ? Value.Check(segment, args) && validWorkInputID(args.input_id) :
        Value.Check(page, args) && (!Object.hasOwn(args, "before") || validWorkInputCursor(args.before));
    case "ack_work_input": return validWorkInputID(args.input_id);
    case "hand_back_work_input": return validWorkInputID(args.input_id) && validText(args.reason, 2048);
    case "submit_result": return validText(args.summary, 8192);
  }
}

function checkedArgs(tool: Tool, raw: unknown): Record<string, unknown> {
  try {
    if (validArgs(tool, raw)) {
      const bytes = JSON.stringify(raw, (_key, value: unknown) => {
        if (value === undefined) throw new Error();
        return value;
      });
      const payload = JSON.parse(bytes);
      if (validArgs(tool, payload)) return payload;
    }
  } catch { /* Do not expose argument access or serialization errors. */ }
  throw new Error("成员工具参数无效，未发送请求。");
}

export default function freshTaskWork(pi: ExtensionAPI) {
  const invoke = createFreshTaskWorkClient(process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG,
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256);
  for (const tool of tools) {
    pi.registerTool<typeof tool.parameters>({
      ...tool,
      // Check original values before Pi can coerce or drop optional fields.
      prepareArguments(raw) { return checkedArgs(tool, raw); },
      async execute(toolCallId, params, signal, _onUpdate, _ctx) {
        const result = await invoke(tool.name, checkedArgs(tool, params), toolCallId, signal);
        return { content: [{ type: "text", text: result.text }], details: undefined };
      },
    });
  }
}
