# 成员基础工具的可选执行准备

`platform.PrepareOptions` 提供一对可信、仅供本地组合根设置的选项：

- `MemberTools *MemberToolRuntime`
- `PiMemberToolsExtension string`

二者必须同时为空或同时提供。默认关闭；`cmd/workspace` 可显式把同一个 Store 上的唯一 runtime 同时交给 Prepare 和私有 HTTP 路由，不增加队列或调度器。

## 本地进程入口（C2）

```sh
workspace --listen 127.0.0.1:0 --data-dir /absolute/private/data \
  --enable-execution --enable-member-tools \
  --pi-member-tools-extension /absolute/trusted/pi-bridge/index.ts \
  --member-tools-max-members 4
```

- `--enable-member-tools` 默认 `false`。启用必须同时开启 `--enable-execution`，并显式提供绝对、可读的普通扩展文件；不从 CWD、仓库或个人配置自动发现扩展。保留已审查 artifact 中相邻的 `index.ts`、`bridge.mjs`，其代码、依赖和祖先须在整个运行期间可信稳定。入口校验不认证 artifact 内容。
- `--member-tools-max-members` 默认 `4`，显式范围 `1..200`。工具关闭时显式传入扩展或容量参数会报错，不静默忽略。
- 开启工具时监听 IP 只接受 `127.0.0.1` 或 `::1`，规范化为 `127.0.0.1:<port>` / `[::1]:<port>`；显式端口 `0` 使用实际预留监听器的非零端口。配置 endpoint 精确为 `http://<actual-address>/api/v1/member-tools`。关闭工具时保留原有回环地址兼容范围。
- `data/member-tools` 是独立的 `0700` 凭证根，与 `data/runtime`、`data/workspaces` 分离。已有不安全条目被拒绝，不自动 chmod；不预建 RunID 日志目录。关闭工具时不创建凭证根、runtime registry、工具路由或 token。
- 启动顺序仍为独占锁、打开 SQLite、恢复一次、只读 provider catalog 与私有根准备、全部构造/路由验证、启动调度及 HTTP。允许先预留监听器取得端口，但完成组合前不 Serve、不 claim。原有 IM、Task、History 路由保留；不注册 TaskLifecycle、Coordination 或 RequestWork 工具。
- 仅 C1 已验证的 Pi 1.4.0 分支获得四基础平台工具，且成员必须配置显式规范模型；缺模型保持 queued/not_ready。Codex 保持原生只读 baseline，没有平台令牌。提供方凭据仍由原 CLI 持有，子进程仅经专用环境变量收到临时配置路径。
- 关闭先停止 claim 并取消执行，再加入 HTTP handlers、native Execute、runtime 撤销的已授权操作及事件/receipt 排空，最后关闭 DB 和锁。HTTP grace 到期可关闭连接，但不能提前放弃 handler、Revoke 或回执；不增加业务自动重试。unknown 保留资源，native 成功不能证明某个未知 HTTP 操作成功。

回滚接线时去掉三个工具专属参数（或显式设置 `--enable-member-tools=false` 且不传扩展/容量），保留普通执行开关；正常退出负责撤销令牌和清理配置。未知执行仍须单独对账，不能通过回滚或重启释放资源。

## 构造与信任边界

`MemberTools` 必须是通过 `NewMemberToolRuntime` 构造的运行时，并借用与 `NewPreparer` **同一个 `*store.Store` 实例**；打开同一个数据库路径的另一个实例也不符合要求。运行时必须使用两个完整 profile 之一：`list_members/read_channel/send_message/create_member` 原四项，或再加 `search_history/read_history` 的六项；顺序不限，窄集合、只追加一项 history 或未知/重复项仍拒绝。prompt 和 argv 都按原四项后接 search/read history 的规范顺序，只声明实际启用集合；旧 unordered-four 的精确文本和 argv 不变。H2 不修改 cmd，产品入口仍为原四项，六工具产品组合属后续 H3。

扩展入口必须是绝对路径、无 NUL、可读的普通文件；与原生适配器一致，可经受信任的符号链接访问。组合根须保证扩展、相邻依赖和祖先在所有已准备执行结束前可信且稳定。此校验不认证扩展内容，也不是主机文件沙箱。运行时凭证根仍受其原有私有目录及稳定身份约束。

构造只校验配置并保存私有引用/路径，不创建临时凭证或原生运行目录，不调用 factory、Issue 或 CLI。

## Prepare 与执行

Prepare 继续核验数据库中的 queued delivery、群组、活跃成员、会话和触发消息，并按成员选定的精确 catalog ID 经过 `provider.NewExecutor` 的路径、安装及版本校验。消息正文不能选择调用者、群组、模型、扩展或授权。

只有显式配置且选中已验证 Pi 1.4.0 时才包装成员工具执行器。此分支要求成员配置显式非空模型，使用 B 的规范文本校验并保留既有 256 字节限制；无效时返回 `scheduler.ErrNotReady`，保持投递 queued，不猜测 CLI 的默认模型。禁用的 Pi 和配置了此选项的 Codex 均保持原来的普通执行器、模型默认行为和完整提示词，不获得平台 token 或工具。

包装器捕获权威 `GroupID/MemberID/ConversationID/DeliveryID/ProviderID/Model`。Prepare 不 claim、不生成 RunID、不签发 token，也不创建配置或 RunID 日志目录；已有工作区授权、目录身份检查以及私有 CWD 的创建/复用行为不变。

调度器随后真实 `ClaimTurn` 并提供 RunID 与 RuntimeDir。B 在 Execute 内核验全新物理 Turn，签发绑定能力并写入已关闭的 0600 配置文件后，才通过可信 factory 把收到的 `toolNames` 原样交给 `provider.NewPiMemberToolExecutorWithTools`，由构造器验证并冻结规范快照。只有配置路径进入子进程专用环境；token、凭证路径不进入提示词。B 返回前撤销能力、排空已授权调用并清理私有配置；不在此片复制其生命周期实现。

## 能力与结果语义

实际启用的四/六工具仅在服务端绑定的群组、成员、会话、话题和物理 Turn 内执行基础操作。可选 history 搜索返回前缀 excerpt，读取返回 Unicode rune 片段；不是默认全文读取，也不扩大范围。创建成员继承可信 provider/model，不授予外部 CWD。姓名、职责文本及创建成员不授予 Coordinator、Assignment、Mandate、RequestWork、工作准入、任务提交/接纳或评审权限；也不会自动调度授权工作。

文件授权和 CWD 行为不变：未授予文件写入权限，CWD 不提供主机文件读取隔离。这里只支持全新单轮及已验证的原生能力，不新增恢复或运行中追加输入。

工具原生 JSONL 帧的 **1 MiB** 上限与 Store 最终助手文本上限不同。最终文本必须是有效 UTF-8，最多 **65536 字节**；65537 字节被事件回调拒绝，结果保持 unknown，不截断发布成功回复，也不提前释放未知执行的资源。

某次 HTTP 工具结果 unknown 与物理 Turn succeeded 可以并存。模型最终文本或 Turn 成功不证明该 HTTP 操作成功，不生成虚假的业务成功凭据，也不触发自动业务重试；原有幂等/对账语义保持权威。

C1 的临时假 CLI 测试验证准备、真实 claim 后包装、原生事件、能力清理及最终文本边界。C2 的 cmd 测试用临时 Go HTTP 服务、SQLite 与假 native CLI（调用 Go test helper，无 Node/Python 运行依赖），验证真实四工具路由、身份/话题隔离、幂等小收据、容量、继承模型、撤销、unknown/cancel 及关闭排空。fixture 主动丢弃一次 HTTP 响应并保留 unknown 工具结果，即使之后 native final 成功也不生成工具成功证据。

上述 Go 测试是本地组合及合成协议证据；H2 另用真实 Pi 1.4 loader 与无 prompt 的 SDK 内存会话验证四/六 active/callable allowlist，不能以 session API 扩权，见 [bridge 验证](../extensions/pi-bridge/README.md)。均不是模型验收或 H3 产品六工具接通证据；不探测真实提供方、不读取认证、不宣称 Coordinator/Assignment/RequestWork 已接通。既有桥加载验证独立维护，真实验收仍需冻结产物经独立审核后另行批准。相关领域边界见 [架构](architecture.md) 与 [编排契约](orchestration.md)。
