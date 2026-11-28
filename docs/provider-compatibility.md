# 提供方兼容性与统一接入契约

> **双态边界声明**：本文确立架构收敛后的**唯一目标提供方兼容契约**。截至基线 `origin/master`（`0188361`），主干当前运行代码仍实施旧版精确补丁号白名单并保留实验性 ACP 路径，直至 [施工路线](plans/architecture-convergence-roadmap.md) Phase 0 解冻探测与 Phase 3 统一 MCP 落地合入 `master`。

## 一、目标支持范围与协议基线

架构收敛后的生产执行只面向以下两类本机原生 CLI，**废弃精确补丁号白名单**，改为按**可执行文件路径、安装版本下限与实际协议能力握手**逐项确认：

| 提供方 | 最低支持与已验基线 | 目标协议与执行接入方式 |
| --- | --- | --- |
| **Pi** | `>= 1.5.2`（`1.5.2` 已于实验 3/4/5 实机验真） | 原生 JSON 会话链路 + 内嵌 24-LOC `pi_thin_mcp.mjs`（通过 `-e builtin:mcp` 加载，不传 `--no-mcp`），经由 Session-Bound Loopback Streamable HTTP MCP（`2025-06-18`）调用平台成员工具；每次运行使用临时隔离的 `0700` `PI_CODING_AGENT_DIR` |
| **Codex** | `>= 0.172.0`（`0.172.0` 已于实验 3/4/5 实机验真） | AppServer（`turn/start` / 原生事件流）+ 同一 Session-Bound Loopback Streamable HTTP MCP（`2025-06-18`），无需 TypeScript 桥接扩展 |

- **本阶段明确移除与排除项**：Phase 0 移除实验性 `agy` ACP 链路及对应开关；本收敛阶段不接入 Pi 与 Codex 之外的其他 CLI 或远端执行节点（`node_id = 'local'`）。
- **模型可配置、不硬编码**：成员模型与思考深度由产品配置与 `channel_members.thinking_level` 决定，不绑定任何仓库开发期固定模型。

## 二、发现、认证隔离与能力边界

1. **只读零付费发现**：启动发现与能力探针仅执行本地有界 `--help`、版本检查或协议能力握手，禁止发起任何付费模型请求，禁止自动登录、登出或改写用户全局认证文件。
2. **认证与配置隔离（工程偏好 2B）**：原 CLI 凭据留在原 CLI。每次 Pi 执行前构造临时 `0700` 目录作为 `PI_CODING_AGENT_DIR`，以只读符号链接挂载宿主机 `auth.json`、`models.json` 与 `settings.json`，并写入空 `mcp.json`（`{"mcpServers":{}}`）阻断拉起无关宿主机 MCP。Phase 3 落地前须有单测实证运行前后原宿主机认证文件 `mtime` 与 SHA-256 零变化。
3. **四态分离与能力不外推**：安装、账户、连通性与支持能力四项状态分别保存；能力标记默认不支持，必须绑定实际可执行文件路径、版本与传输协议。配置文件或环境变量存在不证明已登录；进程退出码 `0` 不证明任务交付或验证通过。
4. **服务端硬并发与工具防伪造守卫**：
   - 数据库部分唯一索引 `idx_turns_active_session` 与 `idx_turns_active_thread_member` 强制同一原生会话或同一 `(project_id, channel_id, thread_id, member_id)` 至多存在 1 个活跃轮次，防止并发重入合并同一 `turnId` 或静默分叉 `parentId` 树。
   - 可变目录写操作必须先获取排他 `resource_claims` 租约直至终态回执或正式对账释放；同 `cwd` 只读并发放行。
   - MCP 工具调用由服务端 `claim_token` 硬绑定调用者、项目、频道与授权范围；模型传参包含伪造作用域字段直接以 `-32602` 拒绝；TCP 断开重放受显式 session 级 `idempotency_key` 与 write-unknown 熔断保护。

相关设计依据与实测证据见 [ADR-0001](adr/0001-architecture-convergence.md)，域模型与编排契约见 [目标架构契约](architecture.md) 与 [编排契约](orchestration.md)。
