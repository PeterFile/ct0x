# 受限 ACP v1（实验性、默认关闭）

本适配器仅实现 stable v1 JSON-RPC stdio 的单新进程、单新 session、单 prompt。不注册到默认 registry，不启用自动派发。当前能力报告仍为 unknown；fixture 不是 Gemini/OpenCode 真实模型、MCP 或业务验收。

## 接线接口

可信宿主显式提供 `ACPConfig`：`AgentPath`、精确 `Version`、`VendorProfile`、非空 `Model`、当前 0600 `MemberToolConfigPath` 与本 purpose 的 `ToolNames`。`NewACPAdapter` 满足现有 `Adapter`；`NewACPExecutor` 满足现有 `Executor.Execute`。不运行发现探针，不认证、不下载、不自动登录。每次 Execute 复验成员工具配置，不接受请求改选模型或 thinking。

平台仍通过同一 MemberTools 校验身份、claim、purpose 和 grants；ACP session、模型参数、工具 title 不提供身份或新授权。每次 run 创建随机 MCP server 名，bearer 仅经私有 stdio HTTP headers 发送。适配器日志只存白名单计数、哈希、关联及权限摘要，不存原始帧、stderr、回答或 bearer；不宣称能控制 CLI 自己的内部存储。

### OpenCode 启动前期限与取证

实验接线显式使用 `MemberToolRuntime.WrapWithContext`，在 factory 内把收到的同一个 `ctx` 传给 `NewACPExecutorContext(ctx, config, executeTimeout)`。宿主若另有 MCP `tools/list` 等 factory 工作，也应使用这个 `ctx`、单独计时。未改变旧 `Wrap`、`Adapter` 和 `NewACPExecutor` 签名；旧构造入口没有调用者期限。新入口不刷新外层期限，`executeTimeout` 仍只配置 Execute 的协作期限。

- `preflight.WithObserver` 可选地在进程内观察固定阶段名、`begin/ok/rejected/canceled/deadline` 和单调耗时；不传路径、配置、token、原始错误或事件。Observer 应只记内存、不阻塞。`begin` 表示到达阶段门禁，可以早于取消检查，并不证明已经开始 I/O。阶段包括 runtime 的配置创建及 factory 前后复核、构造的成员配置/文件身份/镜像哈希/依赖 walk，以及 Execute 启动门禁和 `acp_os_start`。
- 构造与启动复验同步执行；镜像/依赖读取在每块、依赖 walk 在每项检查取消。期限不能中断已阻塞的文件系统或 OS 启动调用，也不能中断 registry 锁等待或撤销时的在途操作排空，因此不保证按墙钟返回。不后台遗留检查或在取消后的预检中推进启动。已经进入 OS Start 的竞态按实际启动结果处理，不能把已启动子进程写成 `not_started`。启动前后原有来源 pins/复核与进程回收规则仍适用。
- ACP 只有独占创建受信运行根下的 `RuntimeDir/RunID` 后，才能报告 `not_started`；此前取消、校验失败或目录冲突保持 `unknown`。普通 runtime 的两种 factory 路径仍复核当前 claim、授权和目录；目录不存在的快照不是预留。若在交接前必须停止，只有复核通过且独占创建同一个运行目录后才报告未启动，保留空目录阻止相同 RunID 再次取得执行权，否则保持 `unknown`。正常交接仍由适配器预留；同一 RunID 的运行根及其祖先必须保持稳定，不移除或复用仍待对账的目录。
- 通过预检或 `acp_os_start=ok` 都不是 CLI 接受。`Started` 来自成功的 `Cmd.Start`。此 OpenCode profile 在匹配的 prompt 终态报告 `Accepted`。Store 根据事件和回执记录受理与资源占用。

显式启用的离线计时测试 `TestACPOpenCodePinnedConstructorPhases` 使用 `-v` 实时输出脱敏阶段；首次构造和后续复验各有独立的 12 分钟协作期限，外层测试超时须覆盖两段（例如 `-timeout=26m`）。不启动 CLI，不跳过镜像或依赖校验。

## 厂商 profile

- **Gemini 0.68.0**：固定 `--acp --extensions none --allowed-mcp-server-names <unique> --approval-mode default --model <selected>`。私有系统设置只允许本 purpose 精确 MCP 规范名；不使用空 core 或 `mcp_*`。0.68 的 `generateValidName` 会补 `mcp_`；本实现拒绝需要过滤或截断的名称及碰撞。精确 core ALLOW 4.25 高于 wildcard DENY 4.24，空 core 会错误拒绝 MCP。禁 builtin 注册、hooks、extensions、agents、auto-memory、ripgrep/IDE 准备与自动更新；skills 使用真正独立的 `skills.enabled=false`。本地 `admin` 会被 `computeMergedSettings` 重建，不再声称本地 admin.skills 是有效闸。既有管理策略、未核政策/工具设置、workspace/dotenv、额外 context roots 或启动覆盖存在则前置拒绝，绝不替换组织策略。原 HOME/GEMINI_CLI_HOME 和认证保持不变。
  - 安装版 `gemini-Q2FNZWQG.js:8311–8321,8366–8385,18920–18951` 显示，`GEMINI_SANDBOX` 覆盖私有 `tools.sandbox=false`，启动 sandbox 会在 ACP 前读取非 TTY stdin。只允许未设置、经 JS 小写/trim 后为空（回退到 `false`）、`0` 或 `false`；拒绝 `1`、`true`、命令和未知值，不清除或覆盖用户显式环境策略。原 `SANDBOX` 与 `GEMINI_CLI_*` 守卫不变。
- **OpenCode 1.19.6**：匿名 `opencode/big-pickle` 合成文本 profile，支持首次及已运行但账户为空的重复执行。须显式提供 `ACPOpenCodeProfile{ConfigDir, LockSHA256}`，目录 basename 为 `opencode`，由可信宿主预装 pinned `@opencode-ai/plugin@1.19.6`，并预置精确 `.gitignore`：`node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore`（无末尾换行）。否则 vendor `ensureGitignore` 会写入准备目录，必须在启动前拒绝，而不是放过 posthash 漂移。构造时冻结物化依赖树，执行前后复验；根 package/lock/installed version、npm no-op 条件、目录内容或哈希不符均拒绝。仅子进程的 `XDG_CONFIG_HOME` 指向 D 的父目录，配置覆盖只在子进程内生效；HOME/XDG_DATA_HOME 不变。原 auth 文件、HOME/.opencode、managed 配置或继承 OPENCODE_* 环境存在仍拒绝。

Gemini 可选可信宿主字段（不是模型参数）：

```go
type ACPGeminiContextFile struct { Path, SHA256 string }
type ACPGeminiContextManifest struct {
    Version string
    CWD string
    Files []ACPGeminiContextFile
}
// ACPConfig.GeminiContext *ACPGeminiContextManifest
```

Manifest 绑定 0.68.0、精确 canonical CWD 和完整发现的指令闭包；构造时深拷贝，执行前后复验。文件必须 owned、regular、无 symlink、不可被 group/world 写入、有界 UTF-8、哈希及读前后 metadata 稳定。nil manifest 只接受没有 ambient 指令的闭包，不静默忽略用户文件。闭包包含默认 GEMINI.md **及** user context.fileName 的 global 文件、受信 CWD 到最近 `.git` 的祖先（无 `.git` 则仅 CWD）；extensions none 禁 extension context，精确本 run MCP instructions 仍来自既有可信 MemberTools。按安装版 `findImports` 和 backtick 规则拒绝所有活跃 import，不读取未列入 manifest 的指令正文。Manifest 不是 native file、工具、caller 或 grants 授权；native file tools 关闭，所以不开放文件访问触发的 JIT context。

`autoMemory=false` 不阻止读取 userProjectMemory。预检只读有界 projects.json/ownership metadata：已登记 CWD 检查其短 ID 和 legacy CWD-hash 迁移源；未登记时因 vendor 可选择/认领 tmp slug，要求该 HOME 下有界 tmp 候选的 MEMORY.md 和 fallback names 均不存在。不读 memory 正文或 native session；不把别的已登记项目的 memory 误当当前上下文。未知 registry、ownership、链接、过大 metadata 或新出现的相关 memory 均拒绝。

OpenCode 现有原 SQLite 仅用于启动配置/匿名账单来源 guard，不用于 prompt 终态推断。使用 `database/sql` 和已有 sqlite driver 的专用只读连接及 authorizer：核对安装版 account/account_state/control_account schema，仅 COUNT(account/control_account) 和 active account/org 非 secret 状态；禁止 token/整行/session 读取、SQL 写入、迁移或全局连接 hook。任何账号、活动状态、未知 schema、busy/corrupt、override 或来源漂移都拒绝。仅接受静止且无 WAL/SHM/journal 的 namespace；检查 20-byte SQLite 格式 header，已 checkpoint 的 WAL-mode main DB 用 immutable+ro 避免只读打开创建 sidecar，绝不忽略现存 WAL。新建空 DB 可在真实 Wait 后复核，已有 DB 不得换 inode；前后来源必须仍匿名且稳定。并发 native writer、遗留 sidecar 和未知结果需先对账，本 profile 不清理它们。

### OpenCode 模型目录与 SDK 来源

- 只接受已核对的 `opencode-darwin-x64-baseline@1.19.6` 中 `package/bin/opencode`（195440480 bytes，SHA-256 `7bbfae1858ea68bd3a6b9331c5b601e22f1a5a69be6324e6845bae276fe0ad1f`）。已安装文件与包内字节哈希一致；构造、启动前和真实 Wait 后校验所解析可执行文件。相同版本标签的其他构建也不能沿用此证明。
- [v1.19.6 ModelsDev](https://github.com/anomalyco/opencode/blob/v1.19.6/packages/core/src/models-dev.ts#L184-L223) 优先读磁盘，再读编译 snapshot；禁 fetch 并不禁磁盘读取。每次独占创建私有 0700 `opencode-cache/opencode`，只给 child 设置 `XDG_CACHE_HOME` 和 `OPENCODE_MODELS_PATH`，后者指定该目录内必须不存在的 `models.json`。显式 path 又跳过默认 cache 的解析失败删除分支；原 cache 不读、不改。启动前、发 prompt 前、真实 Wait 后校验目录身份与该文件仍不存在。已启动后出现漂移仍是 unknown；vendor 在私有 cache 创建 `bin` 不算目录漂移。
- 从上述包的编译 snapshot 静态核得：`opencode/big-pickle` 的 provider/model API 为 `https://opencode.ai/inference/openai/v1`，model 无 npm override，继承 `@ai-sdk/openai-compatible`。按 [provider 模型映射](https://github.com/anomalyco/opencode/blob/v1.19.6/packages/opencode/src/provider/provider.ts#L1316-L1330) 和 [SDK 分支](https://github.com/anomalyco/opencode/blob/v1.19.6/packages/opencode/src/provider/provider.ts#L1813-L1879)，此键命中打包的 `createOpenAICompatible`，不走 `Npm.add`/`file://`。child 禁用实验性 native LLM，并固定 `model`/`small_model`、`enabled_providers` 和该 provider 的模型 allowlist；不从配置注入 API、SDK 或 key。版本/模型名称握手本身不足以证明路由。snapshot 中价格字段为零、源码有无 key 的 `public` 路由，仅是本地证据；不证明当前服务可用、当前价格/额度、登录状态或实际计费。

OpenCode 固定 `--pure acp`、受控空 cwd、loopback/随机内部端口、关闭 mDNS；禁 project config、更新、模型目录 fetch、LSP 下载、外部 skills/Claude context、实验 code-mode/background。唯一的受限 primary agent 最终 permission 为 `*:deny` 加本 run 精确 MCP keys 的 allow；不使用通配 allow。session/new 必须回报精确 model 和 mode，否则不发 prompt。`--pure`、空 mcp/plugin 配置本身不是隔离；D 的 no-op 检查保证不进入运行时 reify。

这不是主机沙箱。native 文件/命令/subagent/background、Artifact、生产仓库任务均未开放。OpenCode 自建内部 listener 是原 CLI 行为，不是平台新增服务。

## 权限与回执

| 合同字段 | 受限实现要求与证据 |
| --- | --- |
| `initialize` | 匹配请求的 `protocolVersion=1`、`agentCapabilities.mcpCapabilities.http=true`、`agentInfo.name` 与精确 `agentInfo.version`。脱敏日志只记录握手结果和 agentInfo 哈希，不据厂商自报升级能力。 |
| `session/new` | 当前绝对 CWD、唯一 HTTP MCP；绑定返回的 `sessionId`。Gemini 核 `models.currentModelId` / `modes.currentModeId=default`；OpenCode 核 `configOptions` 中 `model` / `mode` 的 `currentValue`，可选 `effort` 只能保持初始实值。 |
| `session/prompt` / `session/update` | 一个文本 prompt；绑定 `sessionId`/`toolCallId`，匹配原请求的 `stopReason` 才有 native prompt 终态证据。 |
| 回收及交付 | `DirectChildReaped`、`ExitCode` 来自真实 Wait；`OutputComplete` 还要求 clean drain/EOF/close、协议、来源完整；`Outcome` 进一步要求工具结束、无未知工作及交付成功。已观测 `Terminal` 可以和 `Outcome=unknown` 并存。 |

不发送 Codex `initialized` 或 ACP `authenticate`；未实现 steer、load/resume、会话复用，也不按厂商自报开放这些能力。JSONL 有界，拒绝重复键、无效 UTF-8/surrogate、模糊 request ID、跨 session 或无关联结果。session/new 的提前 updates 有数量/字节上限。

只有 `agent_message_chunk` 累计为非空的完成回答；thought、plan、tool 输出不成为回答。工具按 toolCallId 合并部分状态。Gemini 只有精确本 run MCP metadata 可选择本次真实 `allow_once`；不选择 `allow_always`。OpenCode 缺少可靠 server/tool metadata，所有 ACP permission 请求均拒绝，不猜 title。fs/terminal 请求一律拒绝。

两种已核源码的普通自动允许工具更新均可能没有结构化 server/tool。仅在本 run 受限配置已实际通过 preflight、固定版本/path/config 与精确工具全集封存，且 session 回报实际 model/agent（Gemini default mode；OpenCode 唯一 primary agent）后，内部 sealed-universe attestation 可证明动作只来自当前 run MCP 全集。全部 matching toolCallId 已结束、匹配 end_turn、drain 和真实 Wait 完整时可结算原生交互；`tool.identity` 仍为空，不从 title 猜、不编造 identity，不用于 permission 批准。未封存 profile 缺 meta 仍 unknown。显式矛盾 meta、cross-session、孤儿/重复/未结束 ID、native kind/locations/diff/fs/terminal、config/mode/effort 漂移、未知后台、输出或交付失败均保持 unknown。

ACP v1 在 prompt 期间通过 `session/update` 发送正文，并通过原 `session/prompt` 请求的结果报告终态。Gemini 和 OpenCode 在匹配的终态报告平台 `Accepted`。封存的 AGY 进程只运行一个新会话和一个 prompt。`execution_acp_protocol.go` 在 prompt 发出后看到同一会话首条有效的非空 `agent_message_chunk` 时报告 `Accepted`。没有提前正文时，匹配的终态仍可确认受理。受理记录仅证明 CLI 开始处理 prompt，平台在结算时独立检查终态与成果。 [ACP v1 Prompt Turn](https://agentclientprotocol.com/protocol/v1/prompt-turn)

匹配的 `end_turn` 和全部已结束工具可结算 `succeeded`。空回答也可结算成功，平台只发布非空回答。`refusal`、达到限制与 `cancelled` 分别映射 `failed`、`truncated` 和 `aborted`。Task Submission 和 Review 以各自的正式记录为准。

ACP v1 用无 `id` 的 `session/cancel` 请求中止。Agent 结束在途操作后，以原 prompt 的 `cancelled` 结果回复。 [ACP v1 Cancellation](https://agentclientprotocol.com/protocol/v1/prompt-turn#cancellation)

`scheduler/loop.go` 读到匹配 claim 的持久 `CancelRequestedAt` 后，把明确的取消原因传给执行器。执行上下文取消后，`execution_acp.go` 凭该原因以及原 AGY prompt 的匹配 `cancelled` 结算 `aborted`。结算还要求工具全部结束、协议与来源有效、stdout 和 stderr 洁净排空、真实 `Cmd.Wait` 和交付完整性。`DirectChildReaped` 只证明直接子进程已回收。Store 在应用同一 claim 的可信回执时释放资源。超时、普通关机、kill、协议损坏、未知后台、未结束工具、投递失败或不完整输出仍保持 `unknown`。上述检查依赖受信安装与稳定祖先，不是敌对 writer/ABA 隔离或主机沙箱；仅允许 owned mount boundary 的非 world-writable group-writable 祖先，不放过指令/配置文件的可写或链接检查。

## 后续实机验收（此候选未运行）

独立源码复审后，由已获授权的操作者把现有私有 ACP 单轮 harness（`acp-native-live-harness-20261118`）按本次冻结源码另建、重编译并复核哈希；不得运行旧候选二进制、复用旧试用库或改正在运行的 A/B。两种实现分开准备全新的私有 data/runtime/cwd/out，只用合成文本和一次 `read_channel`，不启用生产 registry。

- Gemini：核精确 0.68.0 安装、当前 CWD 的 context manifest、继承 sandbox/管理/策略/memory 前提。原 CLI 自有认证和显式获授权模型须可用；无证据不猜测、不探针登录、不复制认证。
- OpenCode：核唯一受审二进制、可信 D/lock/no-op、受控空 cwd、私有空 cache、无未核配置/管理层、静止无 sidecar 的匿名账户来源。当前模型服务能否接受这一匿名选择及账单条件须另证；不能改用其他模型、账号或付费 fallback。
- 每次记录实际 v1/HTTP MCP 与 session 选择；要求真实 MCP `read_channel` 成功并回复仅在合成频道消息里的 nonce，匹配终态、已结束工具、完整输出/真实 Wait/exit0、任务外文本回执、资源归零与旧 token HTTP 403。无实际工具回执、断线、未知或前置拒绝就保留证据并停止，不重发。该 harness 不验收 Task Submission/Review、取消能力升级、PTY 或原生恢复。

AGY 离线 fixture 在流出正文后等待 `session/cancel`，覆盖受理、取消通知和匹配的 `cancelled` 结果。调度测试核对取消请求已落库、终态回执前仍有 claim、完整回执后才释放。真实 AGY 是否在业务运行中留下足够的取消时间，须从同一次活模型执行的 Turn、Attempt、终态和资源记录核对。

## 固定依据

wire 使用官方 [v1 initialization](https://agentclientprotocol.com/protocol/v1/initialization)、[session setup](https://agentclientprotocol.com/protocol/v1/session-setup)、[prompt turn](https://agentclientprotocol.com/protocol/v1/prompt-turn)、[tool calls](https://agentclientprotocol.com/protocol/v1/tool-calls)，以及 Gemini 0.68.0 installed schema/source、[OpenCode v1.19.6 source](https://github.com/anomalyco/opencode/tree/v1.19.6)。未采用 ACP v2/RFD、HTTPConnectionId 或新的 provider 表。
