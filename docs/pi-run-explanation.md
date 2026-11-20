# 普通 Pi 成员公开运行说明

`--enable-run-explanation` 默认关闭。须同时使用 `--enable-execution`、`--enable-member-tools` 和 `--pi-member-tools-extension`。仅当普通成员的全新文本轮次使用 Pi 1.4.0、扩展及相邻 `bridge.mjs` 精确匹配本版本、且存储确认支持时，才签发 `publish_run_explanation`。没有 F 表的 v30 库继续执行原工具配置；此选项不安装迁移，也不影响 Codex、原生会话续接、chat intake 或协调/任务角色。

模型只提供 `{ "text": "…" }`：一条专门写给用户看的、不全为空白的 UTF-8 说明，最多 1024 字节，不含 NUL。不要提交私密信息、内部推理或原始日志。运行身份取自服务端已接受且仍有效的认领；桥接器从工具名、签发的运行 ID 和原生调用 ID 派生幂等键。不发送聊天消息，也不创建唤醒投递。

公开说明以 `GET /api/v1/me/agent-runs/{run}?group_id={group}` 的 `public_explanation` 项为准。它不是运行回执或操作证明。如果存储事务未确认，或服务端写入但 Pi 未确认收到对应结果，保留私有运行目录中的 `run-explanation-attempt.json`（原幂等键和原文，非回执）。桥接器不自动重发，也不允许在未知结果后继续写入。即使原生终止显示成功，轮次仍保留未知状态用于对账。
