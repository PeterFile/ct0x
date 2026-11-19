# 原生话题待决接线（C）

`--enable-thread-decision-handoff` 默认关闭。本阶段仅支持已验证的 Codex 0.166.0 AppServer，须同时启用执行、成员工具、协调角色和 AppServer。启动检查需要 F 的请求、Task 答复和 Thread 答复记录都已安装；不登记迁移，不凭版本号、空队列或成员文字判断支持。公共迁移仍由 B 按合入顺序登记。已配置的成员还须选用确切提供方、显式模型和默认私有会话目录。不为此功能配置外部 CWD 或额外工具。

- **提出**：仅服务端签发的全新 `chat_intake/v1` 受信 Codex 轮可在 `send_message.intent_assessment` 的 `clarify` 中显式使用 `decision_context`。普通 `clarify` 仍只走普通评估。候选、来源、提问者和认领世代由服务端绑定；即使只有一个默认收件人，F 的同事务检查也不把它当成明确指定的收件人。MCP 复用能力与 JSON-RPC ID 派生的原键与原参数，无自动重试。未确认写入保留原键及内容用于对账；即使原生轮有成功终态也不把未知写入当成功。获签发这一不同工具 schema 的轮次不进入已有 Codex 文本会话续接。
- **回送与续轮**：只有已有调度器扫描 `PendingThreadDecisions`，按精确投递调用 `ResolveThreadDecisionWake`，认领时携带 `ThreadDecisionAnswerID`。与普通投递共享容量和资源锁；没有就绪的目录或适配器时停留在队列，不改走普通聊天。启动用原会话现有私有目录的物理资源键，固定提供方、模型、目录及完整资源集合；`CommitThreadDecisionLaunch` 在现有 registry 撤销锁内仅花费一次性 owner，成功提交后复查本地目录与活跃 context，再调用全新、不签发成员工具的只读 AppServer。原消息、问题和已保存答复在同一话题作为不可信文本数据送入，不是新用户命令或工作授权。
- **答复不等于处理**：原 F inbox 成功返回已保存答复和投递的回执。原生接收、产出文本或成功终止都不生成处理确认。Thread 不调用 Task 的 `ack_work_input`；现有 Task 决定回送与处理不变。本阶段未加入 Task 提出工具或 Pi 提出方。

## 保留的依赖

#227 的 `GetDecisionAnswerView` 只在有 `TaskID` 时读取 `decision_answer_consumptions`。Thread ledger 没有认领绑定的显式处理命令，也没有对应的持久读回。启用确认前，F 需要一个等价于 `Engine.AcknowledgeThreadDecisionAnswer(ctx, claim store.TurnClaim, answerID, idempotencyKey string) (domain.ThreadDecisionHandlingReceipt, error)` 的正式接口（这里是待提供契约，不是当前 API）：同一事务按实际活跃续轮认领核对原 Group/Topic、Conversation、asker、精确 request-version/answer/wake 及幂等身份，保留未知写入；并从 Thread 的 `GetDecisionAnswerView` 投影独立的持久处理回执。C 不补写 F ledger，也不从 Turn 终态推断该回执。E 的话题更正组合仍需另验；本阶段不沿话题别名或文本重路由。
