# 成员基础工具的可选执行准备

`platform.PrepareOptions` 提供一对可信、仅供本地组合根设置的选项：

- `MemberTools *MemberToolRuntime`
- `PiMemberToolsExtension string`

二者必须同时为空或同时提供。默认关闭；本片不增加命令行开关、HTTP 路由、队列或运行时启用接线。

## 构造与信任边界

`MemberTools` 必须是通过 `NewMemberToolRuntime` 构造的运行时，并借用与 `NewPreparer` **同一个 `*store.Store` 实例**；打开同一个数据库路径的另一个实例也不符合要求。运行时必须使用完整的 `list_members/read_channel/send_message/create_member` 四工具集合，顺序不限。原生适配器固定暴露四项，不能将较窄授权误报为完整工具能力。

扩展入口必须是绝对路径、无 NUL、可读的普通文件；与原生适配器一致，可经受信任的符号链接访问。组合根须保证扩展、相邻依赖和祖先在所有已准备执行结束前可信且稳定。此校验不认证扩展内容，也不是主机文件沙箱。运行时凭证根仍受其原有私有目录及稳定身份约束。

构造只校验配置并保存私有引用/路径，不创建临时凭证或原生运行目录，不调用 factory、Issue 或 CLI。

## Prepare 与执行

Prepare 继续核验数据库中的 queued delivery、群组、活跃成员、会话和触发消息，并按成员选定的精确 catalog ID 经过 `provider.NewExecutor` 的路径、安装及版本校验。消息正文不能选择调用者、群组、模型、扩展或授权。

只有显式配置且选中已验证 Pi 1.4.0 时才包装成员工具执行器。此分支要求成员配置显式非空模型，使用 B 的规范文本校验并保留既有 256 字节限制；无效时返回 `scheduler.ErrNotReady`，保持投递 queued，不猜测 CLI 的默认模型。禁用的 Pi 和配置了此选项的 Codex 均保持原来的普通执行器、模型默认行为和完整提示词，不获得平台 token 或工具。

包装器捕获权威 `GroupID/MemberID/ConversationID/DeliveryID/ProviderID/Model`。Prepare 不 claim、不生成 RunID、不签发 token，也不创建配置或 RunID 日志目录；已有工作区授权、目录身份检查以及私有 CWD 的创建/复用行为不变。

调度器随后真实 `ClaimTurn` 并提供 RunID 与 RuntimeDir。B 在 Execute 内核验全新物理 Turn，签发绑定能力并写入已关闭的 0600 配置文件后，才通过可信 factory 调用 `provider.NewPiMemberToolExecutor`。只有配置路径进入子进程专用环境；token、凭证路径不进入提示词。B 返回前撤销能力、排空已授权调用并清理私有配置；不在此片复制其生命周期实现。

## 能力与结果语义

四工具仅在服务端绑定的群组、成员、会话、话题和物理 Turn 内执行基础操作。创建成员继承可信 provider/model，不授予外部 CWD。姓名、职责文本及创建成员不授予 Coordinator、Assignment、Mandate、RequestWork、工作准入、任务提交/接纳或评审权限；也不会自动调度授权工作。

文件授权和 CWD 行为不变：未授予文件写入权限，CWD 不提供主机文件读取隔离。这里只支持全新单轮及已验证的原生能力，不新增恢复或运行中追加输入。

工具原生 JSONL 帧的 **1 MiB** 上限与 Store 最终助手文本上限不同。最终文本必须是有效 UTF-8，最多 **65536 字节**；65537 字节被事件回调拒绝，结果保持 unknown，不截断发布成功回复，也不提前释放未知执行的资源。

某次 HTTP 工具结果 unknown 与物理 Turn succeeded 可以并存。模型最终文本或 Turn 成功不证明该 HTTP 操作成功，不生成虚假的业务成功凭据，也不触发自动业务重试；原有幂等/对账语义保持权威。

本片的临时假 CLI 测试验证准备、真实 claim 后包装、原生事件、能力清理及最终文本边界；不证明实际 HTTP/bridge 组合、真实 CLI 或模型已经验收。相关领域边界见 [架构](architecture.md) 与 [编排契约](orchestration.md)。
