# Pi 受限文本原生会话（实验性）

Pi 原生会话续接默认关闭。要在 `workspace` 中为符合条件的 Pi 1.4.0 文本投递启用续接，启动时同时传入 `--enable-execution`、`--enable-member-tools` 和 `--enable-native-session-resume`。用 `--pi-member-tools-extension` 指定受信且可读的 `extensions/pi-bridge/index.ts` 的绝对路径，保留相邻的 `bridge.mjs` 和加载依赖。原生续接会校验 `index.ts` 和 `bridge.mjs` 的文件内容。

为当前成员选择目录中的 Pi 1.4.0，并显式填写精确的 `provider/model-id`。普通文本和未启用工作输入的 `chat_intake` 用 `--native-pi-thinking` 显式指定 thinking，可选 `off`、`minimal`、`low`、`medium`、`high`、`xhigh` 或 `max`。不指定时仍走普通 Pi 执行，`--role-thinking` 不替代它。能力目录只报告适配器支持，不保证当前投递或历史引用符合续接条件。

如果已用 `--enable-coordination-roles` 和 `--enable-codex-app-server` 开启 Codex AppServer，可以保留这两个参数。Codex AppServer 仍走原有的全新单轮，不随 Pi 启用续接。

## 使用边界

每 Turn 启动一个新的 Pi 1.4.0 JSON 进程，复用现有 member-tools 构造器、受信 extension、精确 `--tools` 名单和仅传给子进程的本轮配置路径。认证仍由原 CLI 管理，不复制或更改 CLI HOME、个人配置及凭据。调用方必须每次重新 claim、发证、建立配置，并在服务器端校验当前 purpose/grants；历史工具声明和引用均不授予权限。

```go
executor, err := provider.NewPiSessionExecutor(
    selected, extensionPath, currentConfigPath, issuedTools,
    provider.PiSessionOptions{Resume: qualifiedReference, Binding: currentBinding},
    timeout,
)
```

- `Resume == nil` 明确选择新建并尝试捕获引用。非 nil 只接受本适配器的完整、已由 Store 选定和审核的唯一 Pi payload；绑定快照、session ID 或任意文件路径都不足够。预检失败不发送提示词，不修复文件、不扫描目录，不 fresh fallback、continue、fork 或接管个人历史。
- `Model` 必须显式为精确 `provider/model-id`，不接受模糊选择、glob 或 thinking 后缀；`Thinking` 必须显式指定。模型、思考级别、CWD、工具集合、CLI 路径/版本必须与引用和实际持久记录匹配。不支持虚拟模型路由。
- `CWD`、`RuntimeDir` 必须为绝对、clean、无 symlink 的实际路径。调用方继续负责信任 RuntimeDir 的上层祖先；受控 RuntimeDir、run 与 native 目录必须 owned 0700，session 必须 owned、单链接、普通 0600 文件。适配器额外核对祖先非 symlink、owner 与稳定物理身份，但不据此承诺防恶意同用户并发修改。正式执行串行化仍只有原平台资源 claim。

新建时在新独占 runDir 下排他预置 `native/session.jsonl` 空文件，使用 `--session <absolute-file> --session-dir <native-parent>`，省略新分支的 `--session-id`，由 Pi 产生原生 UUIDv7。续接只打开同 RuntimeDir 中精确的旧受控文件。**旧构造器仍使用 `--session-dir/--session-id` 且要求 native ID 等于 RunID；Pi 支持合法自定义 ID，这些历史不需要修复或迁移。**

## 可恢复引用的证明

启动前完整读取并验证精确文件：device/inode/size/mtimeNS/SHA-256、非空、完整尾 LF、严格 Unicode/逐行 JSON（递归拒绝重复 key）、唯一首行 v3 header、原生 UUIDv7/CWD、唯一 entry ID、parent 拓扑和当前末叶。首版只接受本适配器产生的线性 append-only 文本历史；branch、compaction、context edit、自定义 entry、图片和未知格式均不具备恢复资格。文件最多 32 MiB、16384 条记录，每行最多 1 MiB，JSON 深度最多 64；超限拒绝，不截断或迁移。

完整历史必须证明最后 assistant 正常 `stop`、没有等待中的用户输入或工具调用；实际 model/thinking 和 system 工具声明须与当前 profile 一致。工具 digest 与 Store 一致：对发行名单的副本 sort(unique)，canonical JSON array 的 lowercase SHA-256，不改变真实发行顺序。

本轮同时观察 assistant 的实际 toolCall、toolResult 与 `tool_execution_start/end`，核对 ID、名称、参数和结果。所有调用都必须来自本轮名单并完整匹配；任何缺结束、`isError`、未知或不一致均永久取消本轮 capture。尤其不能以最终 assistant 正常 stop 清掉 send_message/create_member/decide_dispatch/submit_result 的未知写风险。首版对只读工具错误也保守不给引用。

仅在观察到 `agent_start` 且完整用户事件对与本轮提示词匹配后记录接纳。原有工具按当前 claim 授权，`starting` 和 `running` 均可使用，不依赖接纳事件先落库。

收到成功的原生 `agent_settled` 后，须等 `Cmd.Wait`、输出排空、解析与回调全部结束，再验证同一 inode 和完整旧字节前缀，并按规范 JSON 摘要核对新追加的消息序列与 stdout 中的完整消息一致。首轮持久化的 system 消息也必须在 stdout 中有对应的事件。既有获批的 Pi 1.4.0 合成试验中，首轮两侧都有该消息，续接轮两侧都没有新增 system 消息；不能用文档示例补齐缺失帧。验证末叶并同步会话文件及直到 RuntimeDir 的受控父目录后，才返回成功和原生引用。

缺少成功证明时，或观察到 `aborted`、`failed`、`truncated` 时，opt-in 路径的 `Outcome` 保持 `unknown`，等待对账。非成功终态目前没有独立的本轮持久化证明，不使用成功专用的引用捕获。此时不发布候选回复、不释放 claim；已观察到的 `Terminal`、`Accepted`、直接子进程回收及输出排空证据照常保留。未启用原生会话的执行不受此限制。

引用是不可变的 server-only `ExecutionReceipt.NativeRef json:"-"`，不会进入旧 canonical receipt JSON。Binding 仅有界严格 canonical 验证和深拷贝，provider 不解释业务身份。失败时可只返回当前 Binding 屏障；复制 receipt 后不得原地修改共享指针。

## 不属于本片的保证

Store/Runtime 必须先选最新同作用域记录，再核 succeeded、引用和当前兼容 profile；unknown、失败或无 Ref 均是屏障，不能过滤后找旧成功。引用与绑定必须随原 Apply 事务保存，新 claim/token 仍是执行授权。此 provider 不实现数据库查询、授权、迁移、新队列或原生 writer 锁。

引用不保存 transcript、token、raw claim、配置或 argv。代码不主动把它们加入会话，但普通 prompt、工具参数/结果、模型输出或 CLI 错误仍可能包含敏感内容，不能承诺任意正文绝无秘密；验收只扫描当前已知 token 和自有受控文件，不读取个人会话。

Pi 1.4.0 在 v26 数据库上的实测只覆盖纯讨论的两轮投递。首轮新建会话，服务冷重启后第二轮用同一文件和原生会话 ID 续接。两轮都走原有资源 claim 关口，结算后均无残留资源 claim。第二轮运行时，旧 token 的工具请求被拒绝（403），新签发的 token 仍对当前运行的 claim 有效。Task 和 Mandate 相关的续接边界只有离线验证，不在这次实测范围内。
