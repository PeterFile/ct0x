# 原生话题待决接线（C）

`--enable-thread-decision-handoff` 默认关闭。本阶段仅支持已验证的 Codex 0.166.0 AppServer，须同时启用执行、成员工具、协调角色和 AppServer。启动检查需要 F 的请求、Task 答复、Thread 答复及 Thread 处理回执记录都已安装；不登记迁移，不凭版本号、空队列或成员文字判断支持。公共迁移仍由 B 按合入顺序登记。已配置的成员还须选用确切提供方、显式模型和默认私有会话目录。不为此功能配置外部 CWD 或额外工具。

- **提出**：仅服务端签发的全新 `chat_intake/v1` 受信 Codex 轮可在 `send_message.intent_assessment` 的 `clarify` 中显式使用 `decision_context`。普通 `clarify` 仍只走普通评估。候选、来源、提问者和认领世代由服务端绑定；即使只有一个默认收件人，F 的同事务检查也不把它当成明确指定的收件人。MCP 复用能力与 JSON-RPC ID 派生的原键与原参数，无自动重试。未确认写入保留原键及内容用于对账；即使原生轮有成功终态也不把未知写入当成功。获签发这一不同工具 schema 的轮次不进入已有 Codex 文本会话续接。
- **回送与续轮**：只有已有调度器扫描 `PendingThreadDecisions`，按精确投递调用 `ResolveThreadDecisionWake`，认领时携带 `ThreadDecisionAnswerID`。与普通投递共享容量和资源锁；没有就绪的目录或适配器时停留在队列，不改走普通聊天。启动用原会话现有私有目录的物理资源键，固定提供方、模型、目录及完整资源集合；`CommitThreadDecisionLaunch` 在现有 registry 撤销锁内仅花费一次性 owner，成功提交才建立专用内存登记；复查本地目录与活跃 context 后调用全新、只读的 AppServer，不恢复原生会话。原消息、问题和已保存答复在同一话题作为不可信文本数据送入，不是新用户命令或工作授权。
- **显式处理确认**：该续轮的专用 MCP 只提供无参数的 `ack_thread_decision`，无继承的成员、工作区或其他平台工具。服务端使用已受理续轮的 claim、`wake.Receipt.ID` 和由能力及 JSON-RPC ID 派生的原调用键。调度器持久化原生受理事件后方可调用 F 的 `Engine.AcknowledgeThreadDecision`；结束时先撤销并等待在途写入，再结算轮次。不落盘保存该 MCP 凭据。
- **答复不等于处理**：原生受理、回复和成功终止不生成处理确认。F 未确认写入时保留 unknown 和原调用身份，不自动重试；按原 `ThreadRef`、待决编号和版本使用 `GetDecisionAnswerView` 核对。`handling: null` 不等于已处理。现有 Task 的 `ack_work_input`、Task 回送和 Pi 路径不变。

依赖 F #227 `b7a28ed` 的显式确认与历史读取接口；F 持有持久层，公共迁移仍由 B 登记。E 的话题更正组合仍需另验；本阶段不沿话题别名或文本重路由。
