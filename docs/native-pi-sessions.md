# Pi 受限文本原生会话（实验性）

本适配器默认关闭、未接入工厂或能力目录。仅提供 `NewPiSessionExecutor` 显式入口；离线子进程 fixture 不代表真实 Pi 续接、权限重绑或服务重启已经通过验收。

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

仅在原执行真实 succeeded、native terminal/`agent_settled`、输出排空、真实 `Cmd.Wait`、parser 与 callback 全部 join 后捕获：再次严格读取同一 inode，核旧完整字节前缀、新单轮消息与 stdout 完成消息一致、末叶和末次完成 assistant，然后同步自有文件及直到 RuntimeDir 的受控父目录。任何失败均只取消引用，不伪造或修改真实 outcome、OutputComplete、DirectChildReaped 或业务接纳。

引用是不可变的 server-only `ExecutionReceipt.NativeRef json:"-"`，不会进入旧 canonical receipt JSON。Binding 仅有界严格 canonical 验证和深拷贝，provider 不解释业务身份。失败时可只返回当前 Binding 屏障；复制 receipt 后不得原地修改共享指针。

## 不属于本片的保证

Store/Runtime 必须先选最新同作用域记录，再核 succeeded、引用和当前兼容 profile；unknown、失败或无 Ref 均是屏障，不能过滤后找旧成功。引用与绑定必须随原 Apply 事务保存，新 claim/token 仍是执行授权。此 provider 不实现数据库查询、授权、迁移、新队列或原生 writer 锁。

引用不保存 transcript、token、raw claim、配置或 argv。代码不主动把它们加入会话，但普通 prompt、工具参数/结果、模型输出或 CLI 错误仍可能包含敏感内容，不能承诺任意正文绝无秘密；验收只扫描当前已知 token 和自有受控文件，不读取个人会话。

真实 Pi 文件写入及新进程同文件/ID 续接、权限撤销后的旧 token/claim 拒绝、重启和同事务绑定仍需 root 独立验收。在此之前不能据 fixture 或版本声明开启产品 resume capability。
