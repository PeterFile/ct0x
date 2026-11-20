# 全新 Pi 轮的项目经验与确切成果读取

`--enable-pi-project-experience` 是默认关闭的传输选项。需要 `--enable-execution`、`--enable-member-tools`、明确选定模型的 Pi 1.4.0，以及 `--pi-member-tools-extension` 指向本版确切 `index.ts`/`bridge.mjs` 字节对。它只扩展合格的全新普通历史轮（包括纯聊天 intake）。旧 profile、原生会话续接、Codex/MCP、文件任务、调查角色及提供续办或调查准入的 intake 保持原契约。

## 原生 ABI

| 工具 | 请求参数 | 边界 |
| --- | --- | --- |
| `lookup_project_experiences` | `query?`、`component?`、`version?`、`limit?`、`before?` | 至少一个有效筛选条件。文本上限分别为 512、128、128 UTF-8 字节；`limit` 1–20（默认 10），`before` 为最多 2048 字节的原游标。按 claim 绑定群组和话题。原样返回 E3 的 `{items,next_cursor}`，包括原文 ID、适用性与停用依据；未安装 D2/E3 时返回空页。 |
| `read_artifact` | `artifact_id`、`version_id`、`offset?`、`max_bytes?` | 两个 ID 均必需，各最多 128 UTF-8 字节。`offset` 0–64 MiB，`max_bytes` 1–16384（默认 8192）。仅读取本轮独立获准的确切版本，保持 `ReadArtifact` 原响应，包括 SHA-256、`next_offset`、`content_complete`、`encoding` 和 `content`。`current_version_id` 仅为元数据，不是读取目标。 |

二者是只读 POST，复用回环成员工具 transport、当前 bearer、64 KiB 请求及完整响应预算；无写入幂等键、不截断、不重试、不经 MCP。超预算先减小检索范围、`limit` 或 `max_bytes`。原消息继续按 ID 用 `read_history` 回读。不接受身份、权限、路径或当前版本选择。与 `read_investigation_artifact` 的正式角色、允许版本清单及响应契约独立，不能相互替代。

## 签发与本机用户批准

可信运行时在构造或启动适配器前，用持久 Turn claim 签发新 bearer，同时固定本轮的原生传输投影。已签发的 bearer 不可重新绑定或扩权，即使还没有首个原生事件。私有配置中的 `project_experience_tools` 是已签发权限的有序投影，不是授权。Go 和 Pi 均校验同一份 0600 配置的 SHA-256，并拒绝同时出现 `continue_work`、`intake_assessment_action` 或 `investigation_tools` 的经验配置；保留 `discussion_only_excerpt`。Pi 的原生 `--tools` 与扩展注册使用同一精确名单。不修改历史原文、经验 Store 或既有工具 ABI。

若启用成果读取，可信进程内 `MemberTools.IssueFreshPiProjectExperience` 要求独立批准的 `ArtifactVersionRef`、`read_artifact` 操作及同一 Store 的真实 `MemberArtifactGrant`。签发时在 claim 群组中核对确切版本的存在性，并将 pin 固定在 grant 副本上。直接成果读取与 E3 的经验验证共用读取边界，在访问 blob 前拒绝不匹配的版本；如果经验页包含未批准的引用，则整页拒绝，不改变筛选或排名。匹配的读取仍须校验现有版本、blob 哈希及授权；撤销 bearer 或持久 claim 失效后拒绝后续读取。命中、评审记录、群组成员资格和用户下载均不授予字节读取权限。

本机用户可在 `POST /api/v1/groups/{group}/messages` 的普通消息中附上独立批准。`recipient_ids` 必须明确列出恰好一个本群成员；不接受默认接收者、公开消息或群发的批准。请求示例：

```json
{"content":"跟进那个导出 BUG","topic_id":"export-bug","recipient_ids":["member-id"],"artifact_read_grant":{"artifact_id":"artifact-id","version_id":"version-id"}}
```

群组由 URL 确定，话题以实际消息为准，发送者由 LocalUser 绑定。带许可的正文拒绝无效 UTF-8 及未配对的 Unicode 代理项；有效内容按原字节保留，无许可的解码不变。许可与实际消息、投递和幂等回执在同一事务提交；同一幂等键更换或省略许可会冲突，匹配的重放仍须验证原持久许可。准备时仅查看本次待认领的投递；签发前及实际读取时，在当前 claim 下重验投递及确切版本。已授权的投递不得改绑，也不能为同一消息追加接收者。模型只在符合现有全新 Pi profile 且实际签发读取权时得到该工具。不附批准仍保持原来的检索权限；同话题的其他投递、成员消息、成果当前版本及调查授权都不能补上批准。

B 须将 `internal/store/delivery_artifact_read_grant.sql` 作为独立追加迁移登记。它依赖 v3 的消息/投递、v4 的 `deliveries_turn_identity`、v6 的 `artifact_versions`、v8 的不可变消息及 v17 的 `message_command_sources`；无需 D2 或 E3 表。确实未安装时，旧消息和经验检索保持原状，新的批准请求返回 `503 artifact_read_grant_unavailable`，不写任何消息或投递。若安装不完整或已有批准却缺少持久证据，则拒绝使用，不降级为无批准消息。测试仅在私有 SQLite 夹具安装该片段，不改公共登记。若另行登记项目经验，仍由 B 在 #218 的 `projectIdentitySchema` 之后登记 #214 的 `projectExperienceSchema`。
