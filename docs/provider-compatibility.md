# 提供方版本兼容性

`NewExecutor` 仅支持以下规范化后的精确版本，执行基线均为新回合（fresh-turn），不恢复历史会话。

| 提供方 | 精确版本 | 执行基线 |
| --- | --- | --- |
| Pi | `1.4.0` | JSON 单轮，`--no-tools` 禁用工具 |
| Codex | `0.162.1`、`0.166.0` | `exec --json` 单轮，`--sandbox read-only`，可使用原生只读工具 |

其他版本（包括相邻版本、预发布版本、带 `v` 前缀的版本字符串）仍拒绝，不使用版本范围。工厂不探测 CLI、凭据或账户，不修改提供方元数据；账户状态和发现能力标记不能提升此基线。凭据继续由原 CLI 管理。

## Codex 0.166.0 的验证来源

2026-11-01 已核对已安装 `0.166.0` 的 help，并用适配器的完整参数加 `--help` 验证参数可接受（退出码为 0）。这只证明参数契约，不证明账户或模型可用。

同日，协调者在基线提交 `a9cd6e4d3c99acbe8fae11b67ef8bb131e7a0bd6` 显式运行一次 `TestProviderLiveCodex`，使用模型 `gpt-6.1-sol`，结果通过：

- `Started`、`Accepted` 为真，响应与期望值精确匹配。
- `session → accepted → response → terminal` 顺序正确，均绑定同一原生会话。
- `Outcome`、`Terminal` 均为 `succeeded`，退出码为 0；`OutputComplete`（EOF）、`DirectChildReaped` 为真，无事件或日志截断、无投递失败。

此真实执行证据与工厂的临时哨兵测试分开记录；本次版本放行未再次调用真实提供方。原始证据仅保留在本地审计中，不纳入仓库。

[官方 `codex exec` 文档](https://learn.chatgpt.com/docs/developer-commands#codex-exec) 仅用于说明非交互执行及 `--json` 的 JSONL 语义，不证明精确版本兼容、账户能力或未来版本可用。

## 支持边界

以上仅是提供方适配器的新回合兼容性，不是应用层 Codex 验收，也不代表支持 resume、steer、平台工具或 compaction。原生只读工具不等于平台工具支持；不承诺任意版本、账户或模型均可运行。
