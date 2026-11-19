# 全新 Pi 轮的项目经验与确切成果读取

`--enable-pi-project-experience` 是默认关闭的传输选项。需要 `--enable-execution`、`--enable-member-tools`、明确选定模型的 Pi 1.4.0，以及 `--pi-member-tools-extension` 指向本版确切 `index.ts`/`bridge.mjs` 字节对。它只扩展合格的全新普通历史轮（包括纯聊天 intake）。旧 profile、原生会话续接、Codex/MCP、文件任务、调查角色及提供续办或调查准入的 intake 保持原契约。

## 原生 ABI

| 工具 | 请求参数 | 边界 |
| --- | --- | --- |
| `lookup_project_experiences` | `query?`、`component?`、`version?`、`limit?`、`before?` | 至少一个有效筛选条件。文本上限分别为 512、128、128 UTF-8 字节；`limit` 1–20（默认 10），`before` 为最多 2048 字节的原游标。按 claim 绑定群组和话题。原样返回 E3 的 `{items,next_cursor}`，包括原文 ID、适用性与停用依据；未安装 D2/E3 时返回空页。 |
| `read_artifact` | `artifact_id`、`version_id`、`offset?`、`max_bytes?` | 两个 ID 均必需，各最多 128 UTF-8 字节。`offset` 0–64 MiB，`max_bytes` 1–16384（默认 8192）。仅读取本轮独立获准的确切版本，保持 `ReadArtifact` 原响应，包括 SHA-256、`next_offset`、`content_complete`、`encoding` 和 `content`。`current_version_id` 仅为元数据，不是读取目标。 |

二者是只读 POST，复用回环成员工具 transport、当前 bearer、64 KiB 请求及完整响应预算；无写入幂等键、不截断、不重试、不经 MCP。超预算先减小检索范围、`limit` 或 `max_bytes`。原消息继续按 ID 用 `read_history` 回读。不接受身份、权限、路径或当前版本选择。与 `read_investigation_artifact` 的正式角色、允许版本清单及响应契约独立，不能相互替代。

## 发行与已知阻塞

可信运行时在构造或启动适配器前，用持久 Turn claim 签发新 bearer，同时固定本轮的原生传输投影。已签发的 bearer 不可重新绑定或扩权，即使还没有首个原生事件。私有配置中的 `project_experience_tools` 是已签发权限的有序投影，不是授权。Go 和 Pi 均校验同一份 0600 配置的 SHA-256，并拒绝同时出现 `continue_work`、`intake_assessment_action` 或 `investigation_tools` 的经验配置；保留 `discussion_only_excerpt`。Pi 的原生 `--tools` 与扩展注册使用同一精确名单。不修改历史原文或经验 Store，不增加授权表或迁移。

若启用成果读取，可信进程内 `MemberTools.IssueFreshPiProjectExperience` 要求独立批准的 `ArtifactVersionRef`、`read_artifact` 操作及同一 Store 的真实 `MemberArtifactGrant`。签发时在 claim 群组中核对确切版本的存在性，并将 pin 固定在 grant 副本上。直接成果读取与 E3 的经验验证共用读取边界，在访问 blob 前拒绝不匹配的版本；如果经验页包含未批准的引用，则整页拒绝，不改变筛选或排名。匹配的读取仍须校验现有版本、blob 哈希及授权；撤销 bearer 或持久 claim 失效后拒绝后续读取。命中、评审记录、群组成员资格和用户下载均不授予字节读取权限。

**普通启动没有成果批准来源。** 它只签发历史权限，成果 pin 为空，不向模型注册 `read_artifact`。调查启动借用的 blob 服务有独立的正式调查范围，不能借给普通轮。受控测试在真实 SQLite claim 上签发一个独立批准的固定 tuple；不提供产品批准策略。普通轮的完整原生接手验收仍需可信批准来源接入此签发接缝；不能用 `read_history`、经验命中或当前成果指针补上。

安装态测试依赖 #218 `372cc69e13345e82739a91552286f8b99772afb4` 的 `projectIdentitySchema` 先于 #214 `a091ea68ff93b61c47ba38ba404195148a5c78da` 的 `projectExperienceSchema`。公共登记和编号仍由 B 决定。本片基于 #214 的冻结 `MemberTools.LookupProjectExperiences`，不要求修改 E 或 D 的文件。
