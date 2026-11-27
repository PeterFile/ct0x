# 在线 Pi 自动收件（显式启用）

入口：`.github/scripts/observer/pi-receiver.mjs`。适配 Pi **1.5.2** 的 `sessionManager.getSessionId()/getSessionFile()/getEntries()`、会话生命周期及 `sendMessage`，依据本机随附的 `docs/extensions.md`、`docs/session-format.md` 和 `examples/extensions/file-trigger.ts`。纯 JS，仅用 Node 内置模块；不安装包、不修改 Pi 个人配置或认证。

## 启用范围与默认用法

**启用意味着允许本进程当前模型为本会话的可开工 CI 收据自动续轮，可能付费；`job_details` 补报默认只更新上下文，不启动模型轮。** 仅对已经授权该类续办及预算的在线会话启用；收据不是新增修改、发布或验收授权。适配器不启动其他 Agent，不做模型/认证探针，不访问 GitHub。

1. 按 [观察者说明](ci-observer.md) 初始化本地数据库和真实协调者/作者指派。数据库必须已经存在、由当前用户所有、权限不向组/其他用户开放（通常 `0600`），并位于当前用户所有的独立私有目录（通常 `0700`）。数据库及其直接目录不能是符号链接。
2. 使用**已经落盘的真实 Pi 会话**。RPC 的 `get_state` 返回真实 `sessionId/sessionFile`；本地权威部署据此登记观察者指派。适配器始终从会话管理器取身份，并核对实际 JSONL header；不接受环境变量、消息正文或工具参数提供的 actor。
3. 只在获准的该次 Pi 启动中传入绝对路径：

   ```sh
   CI_OBSERVER_DB=/absolute/private/ci-observer/observer.db \
     pi -e /absolute/repo/.github/scripts/observer/pi-receiver.mjs \
        --session /absolute/sessions/existing-session.jsonl
   ```

   自动化客户端同样可用 `--mode rpc`。保持 stdin 开启并持续消费 stdout JSONL；**无需发送人工 `prompt`**。若启动时已有未处理收据，适配器立即从本地持久 inbox 投递并自行触发续轮。RPC stdout 仅保留 Pi 协议记录，不写子进程日志。

   默认注册 `ci_observer_begin_receipt` 和非阻塞的 `ci_observer_wait_details` 工具。若使用 `--tools` 的显式白名单，须保留 begin；需要主动等待首 job 日志详情时也保留 wait。不要使用禁用全部工具的 `--no-tools`。

未设置 `CI_OBSERVER_DB` 时无后台资源；print/json、`--no-session`、不存在或 header 不匹配的会话文件不接收。启动时尚未落盘的会话需先正常持久化，再以 `--session` 重开。不写全局环境、`settings.json` 或自动发现目录，不扩大其他进程的付费启用范围。切换/fork 后按新的真实 ID 接收，父会话收据不能继承确认权限；新身份若没有本地指派，也不会凭名字得到作者通知。

## 自动投递与确认

- 一个 `fs.watch` 监听数据库独立目录内 db、`-wal`、`-journal`、`-shm` 的事件；100 ms 合并事件，所有本地 CLI 操作串行。忽略空 WAL/SHM 的读者自生事件，避免递归触发。仅有一个 **60 秒本地 reconcile** 补漏，与观察者的 GitHub polling 完全独立。
- 仅使用同目录 `observer.py` 的有界接口，命令均带 `--db <受信绝对路径>`，通过 `python3 -B` 和安全参数数组执行，无 shell：初始化/对账用 `inbox --recipient <实际 session ID> --pending --limit 64 [--after <receipt_id>]`；begin 和确认复查用 `inbox --recipient <同一 ID> --receipt <ID>`；确认用 `ack --recipient <同一 ID> --receipt <ID>`。单条查询返回 0–1 条；不调用 operator 默认的全历史列表。旧 CLI 不支持这些参数、返回越界页/错误收件人或游标不推进时明确报错，**绝不回退全量扫描**。
- 未确认页必须按 `receipt_id` 严格升序；每次最多处理 4 页，然后保留游标让出执行，再合并调度后续页。处理完一页即释放该页，不累计所有页的 payload；会话只保留必要的排队 ID/等待身份。整轮结束对比轮首数据库指纹，补抓扫描中插入到游标之前的收据，避免饥饿。每个命令仍限 15 秒、输出限 4 MiB，但上限不再套用全部历史；单页输出超限仅缩小 `--limit` 后重读同一游标，最小为 1，绝不截断 JSON 或扩大查询范围。单条仍超限则保留未知，不确认。
- 只消费当前收件人的 `delivered` 且未 ACK 收据；`pending` 不唤醒，`acked` 不重放。动作采用严格白名单，未知动作不投递。普通 workflow 收据仍要求正整数 run/attempt；内容只含动作身份、SHA、受限 GitHub 链接及至多 20 个测试名/包名（各 240 字符）。保留 core `TestLog.result()` 的精确 `outcome=failed` 或 `running_at_timeout`：后者只表示超时时仍在运行，不能当成断言失败；缺失/未知 outcome 不补猜值。日志、正文、命令和任意其他字段不进入消息；测试名明确作为数据，绝不作为执行指令。
- 唯一无 run 的例外是 `release_tag_forward`：要求显式 `run_id=null`、`attempt=null`、`tested_sha=null`，合法单层 `tag=refs/tags/rc/<name>`（最多 1024 字符），40 位 `tag_object`，以及完全匹配的 `job=@tag:<fullref>:<tag_object>`。SHA 是本地观察者已去皮并核实的 master 祖先；适配器不自行访问 GitHub 再认证。消息保留精确 tag/object 身份并请**已有授权的 release worker 复核后决定是否启动**；不生成不存在的 run 链接，不声称已经构建，更不会执行 tag 名、JSON 或其中的命令。其它动作不能借此绕过 run/attempt 检查。
- 首报、终态、归因和已知 release 待办调用 `pi.sendMessage(custom, {triggerTurn:true, deliverAs:'followUp'})`：空闲时自行启动；忙时排到当前工作之后，不中断、不 sleep 整个会话。`actionIdentity` 来自 inbox 的 repo/SHA/run/attempt/action/job；tag 待办还携带完整 tag/tag_object，不从会话当前正在做什么推断。已发布的归因 cause key 仅额外接受与 `commit`/正整数 `pr` 完全相符的 `@attribution:<commit>:<pr>`，不放开任意带冒号的 job。
- `release_full_ready` 仅接受当前 core 的 Release Full evidence 成功通知：受信 release-candidate workflow、`provenance=release-intent`、`target_unknown=false`、`tested_sha=sha`、job 与正整数 `job_id` 精确相符；若带 conclusion，必须为 success。当前 RC、master 祖先关系、该 attempt 的真实成功作业及协调者 audience 由本地 core 认证，适配器不另访 GitHub，也不凭消息正文补认证。这是协调者已有授权的续办，不代表 Full/6A/B/C/最终验收。不存在泛化的 `gate_check`、synthetic 或任意新 target-update 动作放行。
- **补报不计作向负责人再次派工。** `job_details`（包括日志和已发布的 `detail_kind=target_binding`）默认用 `triggerTurn:false` 写入上下文，不提示再次 begin，不中断忙碌工作，也不创建新的模型轮。即使同 run 留有十几个旧 job/碎片补报，也只是信息更新。首报已有测试名时直接沿用；首报已排队或落盘后不改写它，后来名字以固定数据补报进入同一上下文，不发送第二份开工请求。
- 会话内排队集合防重复；恢复时读取本会话全部 `custom_message`，按 `details.receiptId + recipient` 去重。该历史是传输事实，`/tree` 不回滚它；fork 复制的其他 recipient 记录无效。已经进入会话但未开始处理的收据不会因观察者/适配器重启再次叫醒；仅入队、尚未持久进入会话的收据可在重启后重抓。
- 模型**主动开始处理**时调用 `ci_observer_begin_receipt({receiptId})`。只有该收据已经出现在当前会话的实际 `custom_message`、动作身份匹配、且仍在该收件人的持久 inbox 中，才执行幂等 ack，并重新读取原始 `acked_at`。普通 I/O/SQLite busy/ACK 输出丢失后，先复查同一单条收据：已 ACK 则沿用原始时间；明确仍未 ACK 才允许最多一次同 key 重试。单条复查本身的普通 I/O 仅再试一次，仍不可知就失败而不再次写入。超时、退出码或排队都不是成功凭据。入队不是 ack，ack 不是完成/验收。工具没有 recipient/actor/path/toolCallId 参数，无法确认别人的收据。
- 仅在**已 begin 的首报动作确实要等首 job 日志详情**时调用 `ci_observer_wait_details({receiptId:<首报 ID>})`。从真实首报绑定 repo、原始 SHA、run、attempt、job，不接受模型指定 scope/actor；不同 run/job/收件人和目标绑定补报都不释放该等待。登记为当前分支的持久 `custom` entry，恢复后可重建；废弃分支的等待不启动模型。匹配的新日志补报最多续办原动作一次，不要求补报再 begin；若详情已在上下文或队列中，工具直接返回受限数据，不另唤醒。登记不阻塞会话或其它精确等待，也不确认任何补报；只有真实 continuation 消息落盘才成为重启后的已续办事实。
- 成功工具结果的 `details` 包含固定 `source=ci-observer-begin-receipt/v1`、`receiptId`、实际 `recipient`、`actionIdentity`、原始 `ackedAt` 和 **Pi 传给 execute 的实际 `toolCallId`**。不会把模型 input、正文或自报 ID 当作调用标识。相同收据的并发调用共用一次 ack，但各自返回自己的真实调用 ID，便于 root 精确复制/关联原始 JSONL 中的 `toolResult.toolCallId`。这些元数据不包含认证信息。
- `session_shutdown` 关闭 watcher/timer 并终止子进程；旧代次异步结果不能投递进替换会话。watcher 错误由唯一低频 reconcile 重建；故障只发固定中文诊断，不输出原始 stderr/凭据。

停止启用：正常关闭该 Pi 进程，下次不传 `-e`/`CI_OBSERVER_DB`。不删除数据库、不自动 ack，不取消 CI，也不改变原指派。

## 验证

```sh
node --test .github/scripts/observer/test-pi-receiver.mjs
```

测试先红后绿；无包安装、无 GitHub、无模型调用。用例覆盖 5000 条已 ACK + 1 条未处理、大批未 ACK 分页/并发低 ID 插入/恢复、旧 ABI 拒绝、跨收件人隔离、单页降限、真实 `TestLog` 的两类 outcome、多 job 补报无新轮、精确等待及有限 ACK 重试。原有 tag 精确形状、调用 ID 来源、恢复去重和伪造身份拒绝仍覆盖。离线集成在真实 SQLite 预存 5000 条已 ACK 历史，通过真实 Python inbox/ack ABI 和 OS watcher 验证后续 workflow/tag 投递、确认、恢复与读者事件静默；Pi API 与 tag 上游证据仍是 fixture，不是实际模型验收。

实际付费 Pi RPC 由获准的主协调器启动。冷启动只需 `--mode rpc --session <已落盘文件>` 与上述显式扩展；客户端可发送只读 `get_state`，**不得用 `prompt`/`steer`/`follow_up` 来冒充自动触发证据**。验收应分别核对：可开工收据 delivered 后出现 `agent_start`；原始 JSONL 中有匹配 recipient/receiptId 的 durable `custom_message`；成功 begin 的 `details.toolCallId` 与实际 `toolResult.toolCallId` 相同且有 durable `ackedAt`；补报只落盘而无新派工/新模型轮；精确主动等待仅续办对应原动作一次；重开同一 session 后相同收据不再唤醒。既有首报自动 ACK 与后续补报 begin I/O 故障记录不能证明本次分页、重试、补报语义已通过真实 Pi 验收，仍须单独核验，不能用 fixture 或旧版记录替代。
