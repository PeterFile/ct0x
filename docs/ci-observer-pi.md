# 在线 Pi 自动收件（显式启用）

入口：`.github/scripts/observer/pi-receiver.mjs`。适配 Pi **1.5.2** 的 `sessionManager.getSessionId()/getSessionFile()/getEntries()`、会话生命周期及 `sendMessage`，依据本机随附的 `docs/extensions.md`、`docs/session-format.md` 和 `examples/extensions/file-trigger.ts`。纯 JS，仅用 Node 内置模块；不安装包、不修改 Pi 个人配置或认证。

## 启用范围与默认用法

**启用意味着允许本进程当前模型为本会话的已投递 CI 收据自动续轮，可能付费。** 仅对已经授权该类续办及预算的在线会话启用；收据不是新增修改、发布或验收授权。适配器不启动其他 Agent，不做模型/认证探针，不访问 GitHub。

1. 按 [观察者说明](ci-observer.md) 初始化本地数据库和真实协调者/作者指派。数据库必须已经存在、由当前用户所有、权限不向组/其他用户开放（通常 `0600`），并位于当前用户所有的独立私有目录（通常 `0700`）。数据库及其直接目录不能是符号链接。
2. 使用**已经落盘的真实 Pi 会话**。RPC 的 `get_state` 返回真实 `sessionId/sessionFile`；本地权威部署据此登记观察者指派。适配器始终从会话管理器取身份，并核对实际 JSONL header；不接受环境变量、消息正文或工具参数提供的 actor。
3. 只在获准的该次 Pi 启动中传入绝对路径：

   ```sh
   CI_OBSERVER_DB=/absolute/private/ci-observer/observer.db \
     pi -e /absolute/repo/.github/scripts/observer/pi-receiver.mjs \
        --session /absolute/sessions/existing-session.jsonl
   ```

   自动化客户端同样可用 `--mode rpc`。保持 stdin 开启并持续消费 stdout JSONL；**无需发送人工 `prompt`**。若启动时已有未处理收据，适配器立即从本地持久 inbox 投递并自行触发续轮。RPC stdout 仅保留 Pi 协议记录，不写子进程日志。

   默认注册 `ci_observer_begin_receipt` 工具。若使用 `--tools` 的显式白名单，须保留该工具；不要使用禁用全部工具的 `--no-tools`。

未设置 `CI_OBSERVER_DB` 时无后台资源；print/json、`--no-session`、不存在或 header 不匹配的会话文件不接收。启动时尚未落盘的会话需先正常持久化，再以 `--session` 重开。不写全局环境、`settings.json` 或自动发现目录，不扩大其他进程的付费启用范围。切换/fork 后按新的真实 ID 接收，父会话收据不能继承确认权限；新身份若没有本地指派，也不会凭名字得到作者通知。

## 自动投递与确认

- 一个 `fs.watch` 监听数据库独立目录内 db、`-wal`、`-journal`、`-shm` 的事件；100 ms 合并事件，所有本地 CLI 操作串行。忽略空 WAL/SHM 的读者自生事件，避免递归触发。仅有一个 **60 秒本地 reconcile** 补漏，与观察者的 GitHub polling 完全独立。
- 只调用同目录 CLI 的 `inbox --recipient <实际 session ID>`、`ack --recipient <同一实际 ID> --receipt <ID>`，使用 `python3 -B`、安全参数数组，无 shell。每个命令限 15 秒、输出限 4 MiB；超限/失败不确认，保留持久记录，低频本地 reconcile 可重试。大量历史导致超限时需维护本地 inbox 的容量，适配器不会截断 JSON 后猜测状态。
- 只消费当前收件人的 `delivered` 收据；`pending` 不唤醒，`acked` 不重放。动作采用严格白名单，未知动作不投递。普通 workflow 收据仍要求正整数 run/attempt；内容只含动作身份、SHA、受限 GitHub 链接及至多 20 个测试名/包名（各 240 字符）。日志、正文、命令和任意其他字段不进入消息；测试名明确作为数据，绝不作为执行指令。
- 唯一无 run 的例外是 `release_tag_forward`：要求显式 `run_id=null`、`attempt=null`、`tested_sha=null`，合法单层 `tag=refs/tags/rc/<name>`（最多 1024 字符），40 位 `tag_object`，以及完全匹配的 `job=@tag:<fullref>:<tag_object>`。SHA 是本地观察者已去皮并核实的 master 祖先；适配器不自行访问 GitHub 再认证。消息保留精确 tag/object 身份并请**已有授权的 release worker 复核后决定是否启动**；不生成不存在的 run 链接，不声称已经构建，更不会执行 tag 名、JSON 或其中的命令。其它动作不能借此绕过 run/attempt 检查。
- 投递调用 `pi.sendMessage(custom, {triggerTurn:true, deliverAs:'followUp'})`：空闲时自行启动；忙时排到当前工作之后，不中断、不 sleep 整个会话。`actionIdentity` 来自 inbox 的 repo/SHA/run/attempt/action/job；tag 待办还携带完整 tag/tag_object，不从会话当前正在做什么推断。
- 会话内排队集合防重复；恢复时读取本会话全部 `custom_message`，按 `details.receiptId + recipient` 去重。该历史是传输事实，`/tree` 不回滚它；fork 复制的其他 recipient 记录无效。已经进入会话但未开始处理的收据不会因观察者/适配器重启再次叫醒；仅入队、尚未持久进入会话的收据可在重启后重抓。
- 模型**主动开始处理**时调用 `ci_observer_begin_receipt({receiptId})`。只有该收据已经出现在当前会话的实际 `custom_message`、动作身份匹配、且仍在该收件人的持久 inbox 中，才执行幂等 ack，并重新读取原始 `acked_at`。入队不是 ack，ack 不是完成/验收。工具没有 recipient/actor/path/toolCallId 参数，无法确认别人的收据。
- 成功工具结果的 `details` 包含固定 `source=ci-observer-begin-receipt/v1`、`receiptId`、实际 `recipient`、`actionIdentity`、原始 `ackedAt` 和 **Pi 传给 execute 的实际 `toolCallId`**。不会把模型 input、正文或自报 ID 当作调用标识。相同收据的并发调用共用一次 ack，但各自返回自己的真实调用 ID，便于 root 精确复制/关联原始 JSONL 中的 `toolResult.toolCallId`。这些元数据不包含认证信息。
- `session_shutdown` 关闭 watcher/timer 并终止子进程；旧代次异步结果不能投递进替换会话。watcher 错误由唯一低频 reconcile 重建；故障只发固定中文诊断，不输出原始 stderr/凭据。

停止启用：正常关闭该 Pi 进程，下次不传 `-e`/`CI_OBSERVER_DB`。不删除数据库、不自动 ack，不取消 CI，也不改变原指派。

## 验证

```sh
node --test .github/scripts/observer/test-pi-receiver.mjs
```

测试先红后绿；无包安装、无 GitHub、无模型调用。`fixture:` 用例及 fake Pi API 验证 tag 精确形状、无人工 input 的启动钩子、恢复去重、调用 ID 来源、并发确认和伪造身份拒绝；它们不是实际 Pi 模型运行。离线集成用真实 Python inbox/ack ABI、真实 SQLite 和 OS watcher 验证 workflow/tag 的持久投递及读者自生事件静默，Pi API 与 tag 上游证据仍是 fixture。

实际付费 Pi RPC 由获准的主协调器启动。冷启动只需 `--mode rpc --session <已落盘文件>` 与上述显式扩展；客户端可发送只读 `get_state`，**不得用 `prompt`/`steer`/`follow_up` 来冒充自动触发证据**。验收应分别核对：本地 inbox 变为 delivered 后出现 `agent_start`；原始 JSONL 中存在匹配 recipient/receiptId 的 `custom_message`；成功 begin 的 `details.toolCallId` 与实际 `toolResult.toolCallId` 相同，且有 durable `ackedAt`；重开同一 session 后相同收据没有第二次唤醒。旧版工具结果没有 trace 字段不能冒充新版 trace 验收。已检查协调器保存的补充前真实冷启动记录：请求仅有 `get_state`，之后观察到 `agent_start`、成功 begin/持久确认，以及 recipient 与会话 header 相符的 durable `custom_message`。该记录没有新版 trace 字段；新版 trace 与真实 resume 去重仍须单独验收，不能用 fixture 或旧版运行替代。
