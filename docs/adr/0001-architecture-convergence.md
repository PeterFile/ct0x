# ADR-0001：架构收敛决策、实证依据与 39 表目标域模型

- **状态**：已冻结（Accepted & Frozen）
- **研究基线提交**：`18d11a3c0176fe98856aeeef6396e9ddfde0cca0`（树 `6df8bf3ccb986f8f4088dc310e0257b3f6285d9a`）
- **实施基点分支**：`origin/master`（`01883613c5bf8e80a5856ca07e1ad455cb2d6bac`；经核查 `18d11a3..0188361` 仅含 `.github/` 与 `docs/` CI 治理提交，Go 后端、SQLite Schema、`extensions/`、`web/` 与 `PRD.md` 变更文件数为 `0`）
- **独立证据核验状态**：全部 8 份实验回执与落盘产物通过交叉核验（`59/59` 项通过，退出码 `0`）

> **诚实双态声明**：本 ADR 冻结架构收敛的全部产品决策、工程偏好、实验依据与 39 表目标域模型契约。截至基线 `origin/master`（`0188361`），主干当前实际运行代码仍为待替换的旧 `Group/Conversation` 实现与 `v48` Schema，直至 [施工路线](../plans/architecture-convergence-roadmap.md) Phase 5 合入 `master`。

---

## 一、背景与 `v48` 结构性负担

基线 `18d11a3` 存在五重结构性负担：

1. **存储与并发瓶颈**：SQLite 使用 Rollback Journal 与 `MaxOpenConns=1` 单连接阻塞读取，读与写互相阻塞。
2. **Schema 与触发器膨胀**：`v48` 累计 `101` 张表（`100` 张用户表）、`298` 条外键、`260` 个索引、`318` 个触发器（`230,374 B`，占 DDL 总字节 `68.09%`，其中 `69` 个跨表 JOIN 验证触发器占 `173,999 B`、最高单个含 `77` 路 JOIN）与 `5` 个视图，并与 Go Store 内重复的 `15-20` 表 provenance mega-SQL 叠加，拉高 `-race` CI 耗时（`2026-11-26` CI 基线中单包 p95 达 `362.65-568.24s`，Full CI 墙钟 `56m09s`）。
3. **双 CLI 协议分裂**：Pi 与 Codex 分别维护 `5,801` 行 TypeScript 扩展（`1,894` LOC 运行时 + `3,907` LOC 测试）与精确补丁号硬锁的 AppServer/旧 HTTP 成员路由两套协议栈。
4. **启动入口膨胀**：`cmd/workspace` 积累 `28` 个启动参数（含 `13` 个 `--enable-*` 实验开关与外部预览旁路），缺乏单一默认启动档案。
5. **崩溃恢复阻塞**：`unknown` 态冻结持有 `resource_claims` 后缺失正式对账 API 与分级确认协议，导致工作区写锁只能等待人工处置或永久滞留。

本次收敛采用**独立集成分支 `arch-convergence` 一步收敛、主干不断链、零双轨长期滞留**路线，直接以 [target_v1 Schema](#四精确-39-表-target_v1user_version100-r2域模型与-100-源表盘点) 替代旧双轨演进。

---

## 二、已冻结的 4 项产品决策与 3 项工程偏好

| 决策项 | 拍板结果 | 冻结契约与验证要点 | 生效阶段 |
| --- | --- | --- | --- |
| **产品决策 1**（死亡进程人工对账） | **选项 C：按写副作用分级确认** | `turns` 同时记录 `external_pid`、`external_pgid`、`external_process_started_at` 与 `external_boot_id`，按进程组与启动身份核验防 OS PID 复用（不匹配返回 `412` 并保持锁）。无未决 `workspace_operations` 的纯文本或只读崩溃允许一键释锁（`one_click_no_write_ops` 返回 `200`）；存在 `pending`、`unknown` 或 `applied_sync_error` 写操作时服务端返回 `428` 拦截一键释锁，强制核对文件预像与磁盘实测 SHA-256 并二次确认（`secondary_write_hash_confirmed`）后再落不可变 `audit_events` 释锁。 | Phase 2, Phase 4 |
| **产品决策 2**（统一策略与影子预览） | **选项 C：项目三档模板 + 频道覆盖 + 越级影子预览放行** | `projects.default_policy_tier` 提供 `chat_only`、`read_only_collab` 与 `authorized_write` 三档模板，默认值强制为 `chat_only`，守住「打开文件夹不等于授权」底线。频道通过 `channel_policies` 继承或覆写策略；触发超出当前档位的只读调查或目录写入时，前端弹出影子预览卡片并支持本次或永久放行。 | Phase 1, Phase 4 |
| **产品决策 3**（临时成员退出交接） | **选项 C：冻结任务进「待我决定」，附一键转协调员建议** | 临时成员退出（`channel_members.status='left'`）时，原 `assignments` 行转为 `cancelled` 并保留历史，任务进入 `tasks.status='handover_pending'` 冻结调度，同时写入 `decisions(kind='handover')` 推入「待我决定」（若频道已绑定协调员，附一键转给协调员建议选项；用户也可改派他人或取消任务至 `tasks.status='cancelled'` 并不可恢复地撤回 `Mandate`）。 | Phase 1, Phase 2, Phase 4 |
| **产品决策 4**（旧群组路由退场） | **选项 A：Phase 4 同波次直接移除** | 前端切至 `/api/v1/projects/{project}/channels/{channel}/*` 的同一波次彻底删除全部旧群组路由、兼容 DTO 与对应旧测试，零双轨滞留。 | Phase 4 |
| **工程偏好 1**（`synchronous` 开关） | **选项 B：默认 `FULL`，保留 `--sqlite-sync=full\|normal`** | 默认启用最强持久性 `synchronous=FULL`；针对外接/机械磁盘卷或 Linux `ext4/xfs` 场景保留单一显式逃生参数 `--sqlite-sync=full\|normal`，并在启动日志与 `/healthz` 元数据中诚实回显当前同步模式。 | Phase 1, Phase 4 |
| **工程偏好 2**（Pi 宿主机 `mcp.json` 隔离） | **选项 B：临时只读隔离 `PI_CODING_AGENT_DIR`** | 每次 Pi 执行前构造临时 `0700` 目录作为 `PI_CODING_AGENT_DIR`，以只读符号链接挂载宿主机 `auth.json`、`models.json` 与 `settings.json` 并写入空 `mcp.json`（`{"mcpServers":{}}`），彻底阻止后台拉起无关宿主机 MCP。Phase 3 落地前必补单测验证符号链接隔离绝不修改原认证文件。 | Phase 3 |
| **工程偏好 3**（实施分支基点） | **选项 A：从最新 `origin/master` 切出** | 基于 `origin/master`（`0188361`）直接继承既有合并门禁、观察器与定期全量 CI。 | Phase 0 |

---

## 三、5 项实验（含 CLI 探针）实证汇总与统计边界

所有数据均基于基线 `18d11a3` 的可重复实测产物：

| 实验单元 | 实测核心数据 | 统计检验、适用边界与诚实 Caveats | 裁决结论 |
| --- | --- | --- | --- |
| **CLI 探针** | `paid_model_calls=0`、`leaked_pids=[]`；确认 `go 1.27.3`、`pi 1.5.2`、`codex-cli 0.172.0`、`modernc.org/sqlite v1.61.0`（SQLite `3.54.1`）；揭露原有精确补丁号白名单已过期 | 仅执行本地 `--help`、RPC 握手与 schema 导出，不探测账号配额 | 解冻至按协议特征探测（支持 `Pi >= 1.5.2` 与 `Codex >= 0.172.0`），移除精确补丁号白名单 |
| **实验 1 + 审计**（SQLite 拓扑） | 8 轮镜像 ABBA：`wal_full_rw_ui250` 相比 `rollback_max1` 读 p95 `210.355ms -> 0.845ms`（`-99.60%`, `p=0.000155`），写吞吐 `184.5 -> 343.0 wOps`（`+85.91%`, `p=0.048174`）；5 拓扑常规与 `-race` 全部 `busy=0, err=0` | **Caveat 1**：写延迟 `w_p50_ms`（`p=0.064957`）与 `w_p95_ms`（`p=0.130381`）未达 `0.05` 显著性。<br>**Caveat 2**：测试盘为内置 APFS SSD（当时余约 `2.9 GiB`），并非机械或外接存储，且 macOS 默认 `fullfsync=0` 仅走 POSIX `fsync`。 | 锁定 `WAL + synchronous=FULL + 写 1 读 4 + _txlock=immediate` + 退出 `TRUNCATE`；同时按偏好 1B 保留 `--sqlite-sync=full\|normal` |
| **实验 2**（触发器 vs 单遍 Go 事务） | `54/54` 非法状态等价拒斥；16 并发 barrier 常规与 `-race` 精确 `1 winner / 15 rejected`，`busy=0`；生命周期写路径 p50 `135.59ms -> 24.23ms`（`-82.13%`, `5.59x`, `p=0.000155`），总耗时 `-80.56%`（`5.14x`, `p=0.000155`）；`-race` 总耗时 `-89.56%`（`9.57x`, `p=0.002165`），workload `-95.07%`（`20.27x`）；消融组 `A_drop2_same_go` 提速为 `0`（`+7.37%, p=0.885714`） | **Caveat**：实测对象仅为 `workspace_operation_lifecycle` 单条写路径，不可直接线性外推全仓所有 Go 包均获得相同降幅；Phase 5 必须以 Phase 0 全包实测基线做逐包校准 | 删除全部 `69` 个跨表 JOIN 触发器并与 Go 侧重复 mega-SQL 同步收敛为单遍 `BEGIN IMMEDIATE` PK 点查事务 |
| **实验 3 + 三边界**（双 CLI 并发） | Pi `1.5.2` 与 Codex `0.172.0` 双话题并行零串台（`leaked_pids=[]`）；实证抓出 3 个静默丢数据边界：同文件无锁并发写 Pi `3/3` 丢失更新（加锁后 `100%` 保留）、Codex 同进程双 `turn/start` 静默合并同 `turnId`（跨进程 `-32600`）、Pi 并发恢复同 `--session` 以 `rc=0` 静默分叉 `parentId` 树 | 高载未影响语义边界结论 | 实施双层锁：数据库部分唯一索引强制 1 会话/话题成员至多 1 活跃轮次 + 可变写获取排他 `resource_claims` 至终态回执（放行同 `cwd` 只读并发） |
| **实验 4**（统一 Loopback MCP） | Pi `1.5.2` 与 Codex `0.172.0` 均 `5/5` 工具类全通落库；纯回环开销 p50 `0.506ms`；断 TCP 换 JSON-RPC `id` 重放 `duplicate_rows=0, receipt_identical=true`；Pi 扩展运行时由 `1,894` LOC（含测试共 `5,801` LOC）压至 `24` LOC（`-98.73%`）；抓出 `--no-mcp` 会阻断 `registerMcpServer` | **Caveat**：`live_codex_forgery_attempt` 中模型自身拒绝发送伪造参数（`tool_calls_attempted=0`），防伪造边界由 `18/18` 直连 HTTP 矩阵与 Pi `3` 次实测拦截联合证明 | 统一收敛至 Loopback Streamable HTTP MCP（`2025-06-18`）+ 24-LOC 内嵌 `pi_thin_mcp.mjs`（`-e builtin:mcp` 去 `--no-mcp`）+ 显式 session 级 `idempotency_key` + 偏好 2B 临时只读 `PI_CODING_AGENT_DIR` |
| **实验 5**（39 表迁移、DDL 实证、隔离与分级对账） | 5/5 样本无损升至 39 表 `user_version=100`（外接存储实测单库 `1.69-1.81s`，37 张业务表 SHA-256 100% 匹配，`unaccounted_source_tables==[]`，原库 SHA-256 零变动）；5/5 DDL 结构约束实证全绿；双项目隔离 `409/404/403` 零泄漏；真实 Pi（`PGID=31431`）与 Codex（`PGID=31437`）进程组 `SIGKILL` 冻结 `unknown` -> 错误 `boot_id` 拒斥 `412` -> Tier 1 一键释锁 `200`；未决写操作 Tier 2 拦截 `428` -> 哈希二次确认 `200` 释锁，`manual_sqlite_edits=0` | `leaked_pids=[]`；全部 59 项独立核验全绿 | 正式采纳 39 表 `target_v1` Schema、全表盘点原子迁移器与决策 1C 分级对账协议 |

---

## 四、精确 39 表 `target_v1`（`user_version=100, r2`）域模型与 100 源表盘点

### 4.1 对象收敛量化对比

| SQLite Schema 对象 | `v48` 基线（`18d11a3`） | `target_v1 r2`（`user_version=100`） | 净删减量 | 降幅 |
| --- | ---: | ---: | ---: | ---: |
| 总表数（含 `sqlite_sequence`） | 101（100 用户表） | 39（39 用户表） | -62 | -61.39% |
| 外键约束（FK） | 298 | 130 | -168 | -56.38% |
| 索引总数（自动 + 显式） | 260（188 + 72） | 85（49 + 36，含 9 个部分唯一守卫） | -175 | -67.31% |
| 触发器总数 | 318（230,374 B，占 DDL 68.09%） | 10（全单表不可变/防反撤权） | -308 | -96.86% |
| 其中：跨表 JOIN 验证触发器 | 69（173,999 B，最高 77 JOIN） | 0 | -69 | -100.00% |
| 视图（View） | 5 | 0 | -5 | -100.00% |

### 4.2 39 张目标表完整清单（6 大域分组）

| 域分组 | 目标表名 | 存储选项 | 核心主键 / 结构约束 | 核心职责与 `PRD.md` 契约映射 |
| --- | --- | --- | --- | --- |
| **1. 元数据与顶层作用域（6）** | `schema_meta` | `STRICT, WITHOUT ROWID` | `PK(key)` | 记录迁移来源与修订元数据（配合 `PRAGMA user_version=100`） |
| | `workspaces` | `STRICT, WITHOUT ROWID` | `PK(id)`, `is_default IN (0,1)` | 顶层工作空间 |
| | `projects` | `STRICT, WITHOUT ROWID` | `PK(id)`, `root_path` 非唯一, `default_policy_tier` 默认 `chat_only` | 项目边界；合并目录配置，承载决策 2C 三档模板 |
| | `channels` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(project_id, name)`, `UNIQUE(id, project_id)` | 项目下频道，替代旧群组与兼容影子表 |
| | `channel_members` | `STRICT, WITHOUT ROWID` | `PK(id)`, `lifecycle IN ('permanent','temporary')`, `status IN ('active','left')` | 频道成员与角色/模型/thinking 配置 |
| | `channel_policies` | `STRICT, WITHOUT ROWID` | `PK(id)`, 4 个部分唯一索引守卫版本与单一 `is_current=1` | 统一版本化频道/话题策略，收敛协调员与准入策略修订表 |
| **2. 独立资源与成果授权（2）** | `workspace_grants` | `STRICT, WITHOUT ROWID` | `PK(id)`, `scope IN ('read_only','read_write')`, 活跃部分唯一索引 | 独立目录授权与不可恢复撤权 |
| | `artifact_read_grants` | `STRICT, WITHOUT ROWID` | `PK(id)`, FK 绑定 `artifact_versions` 与 `deliveries`/`messages` | 显式成果版本读取授权，跨话题/跨投递永不隐式泄露 |
| **3. 话题、消息、技能与执行（10）** | `threads` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(project_id, channel_id, topic_key)`, 延迟 FK `origin_message_id` | 频道内话题实体 |
| | `thread_state_revisions` | `STRICT, WITHOUT ROWID` | `PK(project,channel,thread,task,task_rev,rev)`, `is_head=1` 部分唯一索引 | 版本化话题/任务有效约束、覆盖消息位点与废止链 |
| | `messages` | `STRICT` | `PK(id)`, `UNIQUE(sequence)`, 内联 `reply_to_message_id`/`quote_excerpt`/`quoted_source_kind` | 不可变消息存储与选择性引用回复 |
| | `deliveries` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(message_id, recipient_member_id)` | 投递持久化与 `acked_at` 确认 |
| | `wakeups` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(delivery_id)` | 模型唤醒调度队列，与消息可见性及投递分离 |
| | `sessions` | `STRICT, WITHOUT ROWID` | `PK(id)`, `resumed_from_session_id` 自引用链 | 原生 Pi/Codex 会话绑定 |
| | `skill_snapshots` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(source_kind, source_uri, content_sha256)` | 内容寻址 `SKILL.md` 版本快照目录 |
| | `turns` | `STRICT, WITHOUT ROWID` | `PK(id)`, **2 个活跃轮次部分唯一索引**, `external_pid/pgid/started_at/boot_id` | 轮次生命周期、`unknown` 冻结与进程组防 PID 复用 |
| | `turn_skill_bindings` | `STRICT, WITHOUT ROWID` | `PK(turn_id, skill_snapshot_id)` | 钉选在途轮次实际加载的技能快照版本 |
| | `run_explanations` | `STRICT, WITHOUT ROWID` | `PK(turn_id)`, `UNIQUE(project_id, channel_id, member_id, idempotency_key)` | 不可变公开运行说明（承载 MCP `publish_run_explanation`） |
| **4. 任务、授权、依赖与协作（12）** | `tasks` | `STRICT, WITHOUT ROWID` | `PK(id)`, 5 态 `open/handover_pending/submitted/accepted/cancelled` CHECK | 任务主体，支持决策 3C 交接冻结与撤权取消态 |
| | `task_thread_links` | `STRICT, WITHOUT ROWID` | `PK(task_id, thread_id)`, `link_kind IN ('origin','followup','reference')` | **Thread 与 Task 真实多对多关联** |
| | `mandates` | `STRICT, WITHOUT ROWID` | `PK(id)`, 单表触发器禁止删除、改写核心条款及反撤权 | 不可变版本化工作授权，吸收路由决策与任务协调记录 |
| | `task_dependencies` | `STRICT, WITHOUT ROWID` | `PK(task_id, dependency_task_id)`, 钉选 `submission_artifacts` + `rebound_by='user'` CHECK | 任务依赖 DAG、精确 `ArtifactVersion` 钉选与人类批准改绑 |
| | `task_lineage` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(child_task_id)`, `kind IN ('repair','successor')` | 拒收修复与续办任务谱系 |
| | `assignments` | `STRICT, WITHOUT ROWID` | `PK(id)`, **部分唯一索引 `WHERE status='active'`** | 任务指派历史，支持临时成员退出取消旧指派并保留历史 |
| | `attempts` | `STRICT, WITHOUT ROWID` | `PK(id)`, `CHECK(actor_id='user' OR turn_id IS NOT NULL)` | 执行尝试，绑定 `turn_id` |
| | `task_work_inputs` | `STRICT, WITHOUT ROWID` | `PK(id)`, `status IN ('pending','consumed','returned')` CHECK | 在途补充/约束变更输入及消费/退回回执 |
| | `task_constraint_impacts` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(input_id, kind, task_id, ref_id)` | 约束变更对下游计划、提交与钉选依赖的影响记录 |
| | `submissions` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(attempt_id)`, `notice_message_id` | 任务交付声明 |
| | `verifications` | `STRICT, WITHOUT ROWID` | `PK(id)`, `outcome IN ('pending','passed','failed','inconclusive')` | 独立校验一等实体 |
| | `reviews` | `STRICT, WITHOUT ROWID` | `PK(id)`, `UNIQUE(submission_id)`, `notice_message_id` | 不可变人工终审接纳/拒收 |
| **5. 成果与待决（4）** | `artifacts` | `STRICT, WITHOUT ROWID` | `PK(id)`, 延迟 FK `current_version_id` | 频道成果索引 |
| | `artifact_versions` | `STRICT, WITHOUT ROWID` | `PK(id)`, `blob_hash` 64 位 hex, `source_operation_id` | 成果不可变版本链 |
| | `submission_artifacts` | `STRICT, WITHOUT ROWID` | `PK(submission_id, artifact_id, version_id)` | 提交与成果版本绑定 |
| | `decisions` | `STRICT, WITHOUT ROWID` | `PK(id)`, `kind` 含 `handover`（支持无 `author_turn_id` 系统交接卡片） | 统一待决与交接决策表 |
| **6. 锁、写操作、经验、幂等与审计（5）** | `resource_claims` | `STRICT, WITHOUT ROWID` | `PK(resource_key)`, FK 绑定 `turn_id`/`attempt_id`/`holder_member_id` | 排他资源锁，活跃态与 `unknown` 态持续持有 |
| | `workspace_operations` | `STRICT` | `PK(id)`, `UNIQUE(sequence)`, 内联 `preimage_*` 与 `outcome`/`observed_*` | 工作区写操作与预像/终态/`unknown` 回执 |
| | `project_experiences` | `STRICT, WITHOUT ROWID` | `PK(id)`, 内联 `retirement_*` 与 `retirement_witness_json` CHECK | 项目经验检索与停用出处 |
| | `idempotency_keys` | `STRICT, WITHOUT ROWID` | `PK(operation, actor_id, scope_id, idempotency_key)` | 统一 HTTP/MCP 显式幂等键表 |
| | `audit_events` | `STRICT` | `seq INTEGER PRIMARY KEY`, `UNIQUE(id)`, `json_valid(payload_json)` | 只追加不可变审计日志（含崩溃恢复与分级对账证据） |

### 4.3 9 个部分唯一结构守卫索引与 10 个单表触发器

```sql
CREATE UNIQUE INDEX idx_turns_active_session
    ON turns(session_id)
    WHERE session_id IS NOT NULL AND state IN ('starting', 'running', 'cancel_requested', 'unknown');
CREATE UNIQUE INDEX idx_turns_active_thread_member
    ON turns(project_id, channel_id, thread_id, member_id)
    WHERE state IN ('starting', 'running', 'cancel_requested', 'unknown');
CREATE UNIQUE INDEX idx_assignments_active_task
    ON assignments(task_id) WHERE status = 'active';
CREATE UNIQUE INDEX idx_channel_policies_channel_rev
    ON channel_policies(project_id, channel_id, revision) WHERE thread_id IS NULL;
CREATE UNIQUE INDEX idx_channel_policies_thread_rev
    ON channel_policies(project_id, channel_id, thread_id, revision) WHERE thread_id IS NOT NULL;
CREATE UNIQUE INDEX idx_channel_policies_channel_current
    ON channel_policies(project_id, channel_id) WHERE thread_id IS NULL AND is_current = 1;
CREATE UNIQUE INDEX idx_channel_policies_thread_current
    ON channel_policies(project_id, channel_id, thread_id) WHERE thread_id IS NOT NULL AND is_current = 1;
CREATE UNIQUE INDEX idx_workspace_grants_active_member_scope
    ON workspace_grants(project_id, channel_id, member_id, scope) WHERE status = 'active';
CREATE UNIQUE INDEX idx_thread_state_head
    ON thread_state_revisions(project_id, channel_id, thread_id, task_id, task_revision) WHERE is_head = 1;
```

唯一保留的 **10 个单表不可变触发器**（0 个跨表 JOIN 触发器）：`audit_events_no_update`、`audit_events_no_delete`、`messages_no_update`、`messages_no_delete`、`reviews_no_update`、`reviews_no_delete`、`run_explanations_no_update`、`run_explanations_no_delete`、`mandates_no_delete`、`mandates_no_unrevoke_or_core_mutate`。

### 4.4 100 张 `v48` 源表全量去向盘点与 5 份样本实证（零静默丢行）

实验 5 在 `v48` 规范化后显式登记全部 `100` 张 `v48` 用户表去向（`67` 张迁入目标表 + `33` 张显式退场旧表），并校验 `unaccounted_source_tables == []` 且 `unaccounted_source_rows == 0`：

| 样本库 | `v48` 非空表 / 总行数 | 迁入源表 / 行数（SHA-256 全匹配） | 显式退场源表 / 行数 | 未登记表 / 行 | 目标迁移行数（非空业务表数） | 单库迁移耗时（外接存储实测） |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `v22`（先升 `v48`） | 55 表 / 263 行 | 41 表 / 180 行 | 14 表 / 83 行 | 0 / 0 | 129 行（25 张） | `1,771.71ms` |
| `v33`（先升 `v48`） | 55 表 / 263 行 | 41 表 / 180 行 | 14 表 / 83 行 | 0 / 0 | 129 行（25 张） | `1,799.55ms` |
| `v35`（先升 `v48`） | 57 表 / 297 行 | 43 表 / 214 行 | 14 表 / 83 行 | 0 / 0 | 155 行（25 张） | `1,804.47ms` |
| `v48_successor` | 50 表 / 174 行 | 36 表 / 106 行 | 14 表 / 68 行 | 0 / 0 | 80 行（23 张） | `1,691.32ms` |
| `v48_producer` | 24 表 / 388 行 | 22 表 / 339 行 | 2 表 / 49 行 | 0 / 0 | 209 行（17 张） | `1,811.83ms` |

**18 张在样本中非空但显式退场的 `v48` 内部/桥接表全量枚举**（其余 15 张全样本空表为 `chat_automatic_text_group_imports`、`chat_automatic_text_routes`、`chat_discussions`、`chat_discussion_events`、`chat_discussion_work_returns`、`decision_answer_consumptions`、`decision_thread_answers`、`decision_thread_answer_consumptions`、`group_background_bindings`、`group_background_intents`、`group_background_joins`、`task_work_delegations`、`task_work_handbacks`、`task_work_reports`、`thread_corrections`）：

| 退场分类 | 数量 | 显式退场旧表名 | 废弃依据与耐久事实保留去向 |
| --- | ---: | --- | --- |
| **1. 系统迁移账本与过渡水位** | 2 | `schema_migrations`, `decision_work_input_boundary` | `v1..v48` 版本账本与过渡水位，由 `PRAGMA user_version=100`、`schema_meta` 与 `audit_events(schema_migrated)` 替代 |
| **2. 旧聊天准入评估暂存链** | 6 | `chat_candidates`, `chat_assessments`, `chat_intake_executions`, `chat_followup_routes`, `chat_repair_candidates`, `chat_repair_review_intakes` | 拆散的中间草稿表；原始消息、`turns`/`sessions`、`mandates.routing_reason`、`tasks`、`task_lineage`、`task_work_inputs` 与 `reviews` 已全量迁入 |
| **3. 旧群协调分发队列状态表** | 5 | `coordination_triggers`, `coordination_decisions`, `coordination_dispatch_requests`, `coordination_dispatch_state`, `coordination_assignment_dispatch` | 被 `wakeups + assignments + attempts` 单遍 Go 事务替代；触发源消息与请求/指派号已保留于 `mandates`、`messages` 与 `assignments` |
| **4. 旧跨表 JOIN 触发器桥接表** | 5 | `chat_workspace_scopes`, `chat_workspace_scope_grants`, `task_work_deliveries`, `task_work_continuations`, `decision_answer_wake_replacements` | 专为高路数 JOIN 触发器存在的冗余桥表；目录授权、写操作、投递、轮次、尝试、待决与工作输入事实已全量迁入对应目标表 |

### 4.5 原子升级与回滚协议

1. **前置规范化**：`user_version == 100` 且 `schema_meta.schema_family == 'target_v1'` 直接幂等放行；`v1..v47` 先在私有临时副本经既有 `v48` 路径升级并核验 `integrity_check == ok` 与 `foreign_key_check == 0`。
2. **临时文件单事务构建**：在同挂载点创建 `0600` 临时库 `workspace.db.migrating.<nonce>`，应用 39 表 DDL，在单一 `BEGIN IMMEDIATE` 事务内按拓扑序写入全部 37 张业务表、`schema_meta` 与 `audit_events(schema_migrated)`。
3. **落盘前四重门禁**：`PRAGMA integrity_check == ok`、`PRAGMA foreign_key_check == 0`、全部 37 张业务表规范化内容 SHA-256 100% 匹配、源库非空表全量去向 `unaccounted == 0`。
4. **原子切换与零污染回滚**：执行 `PRAGMA wal_checkpoint(TRUNCATE)`、关闭连接、`fsync(tmp)`、清理旧 `-wal`/`-shm`、`os.Rename(tmp, workspace.db)`、`fsync(data_dir)`。任一步失败立即删除临时文件并报错，原库字节零修改。

相关参考：[PRD.md](../../PRD.md)、[目标架构契约](../architecture.md)、[编排契约](../orchestration.md)、[提供方兼容性契约](../provider-compatibility.md)、[施工路线](../plans/architecture-convergence-roadmap.md)。
