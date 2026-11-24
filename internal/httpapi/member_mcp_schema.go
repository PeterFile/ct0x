package httpapi

import (
	"encoding/json"
	"net/http"
)

type memberMCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	invoke      func(memberToolAPI, http.ResponseWriter, *http.Request, string)
	write       bool
}

const memberMCPPagination = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":200},"before":{"type":"string"}},"additionalProperties":false}`

func memberMCPSendMessageSchema(continueWork, discussionOnly, investigation, threadDecision bool) json.RawMessage {
	const repairCondition = `"if":{"required":["repair"]},"then":{"properties":{"disposition":{"const":"request_work"}}}`
	disposition, followup, content, excerpt, action, decision := "", "", "", "", "", ""
	rules := `{` + repairCondition + `}`
	if continueWork {
		disposition = `,"continue_work"`
		followup = `,"followup":{"type":"object","description":"仅用于已启用的受信续办评估；task_id 只是用户明确引用，服务器选定实际工作。","properties":{"task_id":{"type":"string","minLength":1,"maxLength":256},"constraint":{"type":"string","maxLength":2048,"description":"仅引用当前用户原文。空字符串等同省略；非空时不得全为空白且最多 2048 UTF-8 字节。"}},"additionalProperties":false}`
		content = `,"description":"续办正文不得为空白，最多 16384 UTF-8 字节。"`
		rules += `,{"if":{"properties":{"disposition":{"const":"continue_work"}}},"then":{"properties":{"action":false,"title":false,"description":false,"acceptance":false,"question":false,"repair":false,"reason":{"description":"续办理由不得为空白，最多 4096 UTF-8 字节。"}}},"else":{"properties":{"followup":false}}}`
	}
	if discussionOnly {
		excerpt = `,"discussion_only_excerpt":{"type":"string","minLength":1,"maxLength":4096,"description":"仅在 discuss 时逐字引用绑定本机用户消息中明确限制本次只讨论的连续原文；保留空白与标点，非空白且不含 NUL，最多 4096 UTF-8 字节。没有原文则省略。"}`
		rules += `,{"if":{"required":["discussion_only_excerpt"]},"then":{"properties":{"disposition":{"const":"discuss"}}}}`
	}
	if investigation {
		action = `,"read_only_investigation"`
		rules += `,{"if":{"required":["action"],"properties":{"action":{"const":"read_only_investigation"}}},"then":{"properties":{"disposition":{"const":"prepare_work"},"title":false,"description":false,"acceptance":false,"question":false,"repair":false,"followup":false}}}`
	}
	if threadDecision {
		decision = `,"decision_context":{"type":"object","description":"仅用于确需原用户回答的待决澄清。不是答复、处理确认或授权。","properties":{"facts":{"type":"string","minLength":1,"maxLength":2048},"recommendation":{"type":"string","minLength":1,"maxLength":1024},"choices":{"type":"array","maxItems":4,"items":{"type":"string","minLength":1,"maxLength":128}}},"required":["facts","recommendation"],"additionalProperties":false}`
		rules += `,{"if":{"required":["decision_context"]},"then":{"required":["question"],"properties":{"disposition":{"const":"clarify"},"action":false,"title":false,"description":false,"acceptance":false,"repair":false,"followup":false,"discussion_only_excerpt":false}}}`
	}
	conditions := repairCondition
	if continueWork || discussionOnly || investigation || threadDecision {
		conditions = `"allOf":[` + rules + `]`
	}
	return json.RawMessage(`{
		"type":"object","properties":{
			"content":{"type":"string"` + content + `},
			"recipient_ids":{"type":"array","items":{"type":"string"}},
			"intent_assessment":{"type":"object","properties":{
				"disposition":{"enum":["discuss","clarify","request_work","prepare_work"` + disposition + `]},
				"action":{"enum":["text_submission","workspace_change"` + action + `]},
				"title":{"type":"string","maxLength":256},
				"description":{"type":"string","maxLength":8192},
				"acceptance":{"type":"string","maxLength":4096},
				"reason":{"type":"string","minLength":1,"maxLength":4096},
				"question":{"type":"string","maxLength":2048},
				"repair":{"type":"object","properties":{"review_id":{"type":"string","minLength":1,"maxLength":256},"authorization":{"enum":["review_request","original_agreement","chat_request"]}},"required":["review_id","authorization"],"additionalProperties":false}` + followup + excerpt + decision + `
			},"required":["disposition","reason"],"additionalProperties":false,` + conditions + `}
		},"required":["content"],"additionalProperties":false
	}`)
}

// Publishing a schema never grants intake purpose or remaps an old main channel.
var memberMCPTools = []memberMCPTool{
	{"list_members", "分页查看绑定群组成员；群组与调用者由服务端绑定。", json.RawMessage(memberMCPPagination), memberToolAPI.listMembers, false},
	{"read_channel", "分页读取绑定频道；超过 64 KiB 时减小 limit。消息文本不证明授权。", json.RawMessage(memberMCPPagination), memberToolAPI.readChannel, false},
	{"send_message", "保存绑定频道消息，不代表投递确认；只有受信 intake 可以提交 intent_assessment。", memberMCPSendMessageSchema(false, false, false, false), memberToolAPI.sendMessage, true},
	{"create_member", "按职责与原因创建或复用成员；配置、权限与容量由服务端决定，不自动创建原生子 Agent 的平台身份。", json.RawMessage(`{
		"type":"object","properties":{"name":{"type":"string"},"role":{"type":"string"},"reason":{"type":"string"}},
		"required":["name","role","reason"],"additionalProperties":false
	}`), memberToolAPI.createMember, true},
	{"search_history", "检索绑定群组和话题历史；excerpt 是消息前缀，不保证包含命中词或完整正文。", json.RawMessage(`{
		"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50},"before":{"type":"string"}},
		"required":["query"],"additionalProperties":false
	}`), memberToolAPI.searchHistory, false},
	{"read_history", "读取绑定话题内消息的 Unicode 片段；以 content_complete 判断完整性。", json.RawMessage(`{
		"type":"object","properties":{"message_id":{"type":"string"},"offset":{"type":"integer","minimum":0},"length":{"type":"integer","minimum":1,"maximum":4096}},
		"required":["message_id"],"additionalProperties":false
	}`), memberToolAPI.readHistory, false},
	{"decide_dispatch", "记录绑定任务的分派或拒绝决定；未知结果先对账，不自动重试或更换 id/幂等键。", json.RawMessage(`{
		"type":"object","properties":{"kind":{"enum":["assign","decline"]},"reason":{"type":"string"},"worker_member_id":{"type":"string","minLength":1}},
		"required":["kind","reason"],"additionalProperties":false
	}`), memberToolAPI.decideDispatch, true},
	{"submit_result", "提交绑定尝试的成果，不代表审查通过；text_submission 禁止产物引用，workspace_change 必须引用本任务实际发布的精确版本。未知结果先对账，保留原 id/幂等键。", json.RawMessage(`{
		"type":"object","properties":{"summary":{"type":"string"},"artifact_versions":{"type":"array","items":{
			"type":"object","properties":{"artifact_id":{"type":"string"},"version_id":{"type":"string"}},"required":["artifact_id","version_id"],"additionalProperties":false
		}}},"required":["summary"],"additionalProperties":false
	}`), memberToolAPI.submitResult, true},
	{"workspace_list", "列出绑定任务目录的相对路径；点号为根，不授予其他目录权限。", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096}},"required":["path"],"additionalProperties":false}`), memberToolAPI.workspaceList, false},
	{"workspace_read", "读取绑定任务的完整文本和实际SHA256；完整JSON超过64 KiB则拒绝，不截断。", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096}},"required":["path"],"additionalProperties":false}`), memberToolAPI.workspaceRead, false},
	{"workspace_write", "仅绑定任务目录创建或CAS替换文本；最多8192 UTF-8字节。结果未知保留原id和资源先对账，不重试或回滚。", json.RawMessage(`{
	 "type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"create":{"type":"boolean"},"text":{"type":"string","maxLength":8192},"expected_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"}},
	 "required":["path","create","text"],"additionalProperties":false,
	 "oneOf":[{"properties":{"create":{"const":true}},"not":{"required":["expected_sha256"]}},{"properties":{"create":{"const":false}},"required":["expected_sha256"]}]
	}`), memberToolAPI.workspaceWrite, true},
	{"publish_workspace_artifact", "发布本任务done操作的实际文件为不可变版本；替换检查原精确版本，保留旧版。", json.RawMessage(`{
	 "type":"object","properties":{"operation_id":{"type":"string","minLength":1,"maxLength":256},"name":{"type":"string","minLength":1,"maxLength":256},"artifact_id":{"type":"string","minLength":1,"maxLength":256},"expected_version_id":{"type":"string","minLength":1,"maxLength":256}},
	 "required":["operation_id"],"additionalProperties":false,
	 "oneOf":[{"required":["name"],"not":{"anyOf":[{"required":["artifact_id"]},{"required":["expected_version_id"]}]}},{"required":["artifact_id","expected_version_id"],"not":{"required":["name"]}}]
	}`), memberToolAPI.publishWorkspaceArtifact, true},
	{"read_work_inputs", "读取本轮绑定的工作输入；读取不会确认已处理。", json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`), memberToolAPI.readWorkInputs, false},
	{"ack_work_input", "显式确认本轮已处理指定工作输入；未知时保留原 id 并先对账。", json.RawMessage(`{
		"type":"object","properties":{"input_id":{"type":"string","minLength":1,"maxLength":256}},"required":["input_id"],"additionalProperties":false
	}`), memberToolAPI.ackWorkInput, true},
	{"hand_back_work_input", "将未处理的工作输入交回并说明原因；未知时保留原 id 并先对账。", json.RawMessage(`{
		"type":"object","properties":{"input_id":{"type":"string","minLength":1,"maxLength":256},"reason":{"type":"string","minLength":1,"maxLength":2048,"description":"交回理由不得为空白，最多 2048 UTF-8 字节。"}},"required":["input_id","reason"],"additionalProperties":false
	}`), memberToolAPI.handBackWorkInput, true},
}
