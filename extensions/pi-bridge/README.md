# 原生 Pi 成员工具扩展

Pi 1.4.0 扩展，注册 `list_members`、`read_channel`、`send_message`、`create_member`、`search_history`、`read_history` 六项 schema。注册不代表启用：可信启动器的精确 `--tools` allowlist 决定实际可用项。**H2 只提供可选 profile，不包含 H3 cmd/HTTP 组合，不代表产品六工具或真实模型验收。**

## 加载与配置

可信启动器取得真实执行绑定的服务端 capability 后，创建当前用户拥有的普通 `0600` 文件（建议置于私有 `0700` 目录），内容仅为：

```json
{"endpoint":"http://127.0.0.1:7310/api/v1/member-tools","token":"SERVER_ISSUED_CAPABILITY"}
```

环境变量只保存该文件的**绝对路径**，不是 JSON 或 token。不要把 token 放入参数、提示词、命令行或日志。启动器负责文件生命周期；扩展初始化读取一次，之后不重读、不删除。文件超过 4096 字节、末级符号链接、权限/所有者不符或配置无效时，加载失败且不注册工具。

从仓库根目录显式加载（仅说明独立扩展调用方式，不代表已有启动器）：

```sh
AGENT_WORKSPACE_MEMBER_TOOL_CONFIG=/absolute/private/run.json \
  pi --no-extensions --no-mcp -e ./extensions/pi-bridge/index.ts \
  --no-builtin-tools --tools list_members,read_channel,send_message,create_member
```

旧四工具 allowlist 不激活 history；完整六工具 profile 使用 `list_members,read_channel,send_message,create_member,search_history,read_history`。profile 仅由可信运行时提供，不新增模型可选参数；配置文件仍只有 endpoint/token。Prepare 只接受完整四项或完整六项，不接受窄集合或只追加一项 history；产品 cmd 仍维持旧四项，六工具组合另属 H3。

Pi 提供 `typebox` 和 ExtensionAPI，无需安装开发依赖即可加载。正常宿主要求 Node >=22.19。endpoint 必须精确为 `http://127.0.0.1:<port>/api/v1/member-tools` 或 `http://[::1]:<port>/api/v1/member-tools`，端口为 1–65535；不接受 DNS、其他地址、用户信息、查询、片段或末尾斜线。每次 POST 追加 `/<toolname>`。

## 边界与错误

- 参数只有冻结的业务字段；执行时用同一原生 schema 校验原始值及 JSON 快照，拒绝额外字段、错误类型和显式 `undefined`。调用者、群组、话题、运行配置与 grants 均不能由参数提供。`recipient_ids` 省略或为空数组时是公开消息，不创建投递、不唤醒成员；仅显式列出的成员进入投递队列。
- `search_history` 必需 `query`，可选整数 `limit:1..50`（默认 20）和原样 `before`（省略/空字符串均为起始页）；结果 excerpt 是最多 256 Unicode rune 的消息前缀，不保证包含命中词。`read_history` 必需 `message_id`，可选整数 `offset>=0`（默认 0）、`length:1..4096`（默认 1024）；返回显式 `[offset,next_offset)` 片段及完整性信息，不能冒充全文。两项均受服务端绑定身份和精确话题限制，原样转发结果；都是读操作，没有写入 key/fingerprint，不触发或解除既有 write fuse。
- 使用 `node:http.request`，没有 Origin/Fetch Metadata，也不继承代理；token 只进入 `Authorization: Bearer`。写操作的 `Idempotency-Key` 固定为 `sha256(toolName + toolCallId)` 小写十六进制；空调用 ID 被拒绝。
- 请求在联网前按 **JSON 编码后的 UTF-8 字节**检查；请求及响应上限均为 **65536 字节（含边界）**。响应不解压、不截断；超限明确报错 `tool_result_too_large`。服务端读/list 超限应返回 HTTP 413，可缩小 `limit` 后重新读取。
- 完整成功 JSON 原文进入文本 content，不投影、不静默裁剪；`details: undefined`，不重复写入完整载荷。服务端负责返回小型写收据：send 201 至少为 `{message_id,created_at,delivery_count,content_omitted:true}`；create 201 创建 / 200 复用，收据为 `{member_id,reused,join_message_id}`，复用的 `join_message_id` 为 `""`。扩展不额外限制成功收据字段，仍原样转发服务端 JSON。
- 只接受严格 UTF-8、安全 JSON 与预期状态；拒绝非法编码/BOM、危险属性名、非有限数值、压缩、HTML 和敏感数据回显。错误 JSON 仅保留最多 64 字符的稳定代码，并使用本地中文安全消息；从不透传服务端 message、响应头、原始网络错误或取消原因。`request_canceled` / `deadline_exceeded` 明确表示结果未知：先对账，保留原工具调用标识及幂等键，不要自动重试。`member_capacity` 提示容量已满，仅在有空余容量后重试；`tool_result_too_large` 提示减小 `limit` 重新读取，不截断结果。
- 总超时为 25 秒（不是空闲超时）；取消、超限及时关闭请求/响应。发送后超时、断线、取消或无可信收据属于**结果未知**，不能解释为成功或未执行；先由可信协调方对账。无自动重试、重定向、模型调用或备用 provider。显式重放同一调用 ID 使用同一写入键；扩展不伪造持久化或投递确认。

写工具仅在上述必要收据字段及状态一致时视为成功，允许安全的增量字段；完整 4xx 拒绝须匹配既有状态与稳定代码白名单。5xx、发送后的取消/超时、断链或不可信收据均保留未知：首次写入未知在交给模型前永久锁住当前 client 实例的后续写入。同工具、原生调用 ID 与精确序列化 payload 的重放只返回本地缓存的原未知，不再发送 HTTP；其他写入在传输前拒绝。读工具仍可用，读失败不锁写；后续读写成功或拒绝均不解锁，没有自动或模型可调用的解锁入口。此前已发送的并发写可各自结束，此保护不跨 client、进程或新轮次持久化，也不替代可信对账。

## 聚焦验证

仅在本包安装固定版本开发依赖：

```sh
npm --prefix extensions/pi-bridge ci --ignore-scripts --no-audit --no-fund
npm --prefix extensions/pi-bridge test
npm --prefix extensions/pi-bridge run typecheck
npm --prefix extensions/pi-bridge run smoke
git diff --check
```

测试采用 `node:test`、真实 IPv4/IPv6 loopback HTTP 和 Pi 1.4.0 的实际 `loadExtensions`。loader-only 测试不创建会话；新增 active-profile 测试使用真实 SDK/AgentSession、显式内存 settings/session、受控 resourceLoader、合成模型及拒绝调用的 auth/model/network 方法，不使用默认 AuthStorage/ModelRuntime，不读个人 models.json，绝不调用 prompt/stream，并 dispose 会话。实际 active/callable API 验证旧四项与完整六项，尝试通过 session API 激活未授权项也不能扩大 allowlist。测试包含真实 25 秒滴流超时，运行约需 26 秒；完整 `test` 已包含 `smoke` 的原生加载及六 schema 检查，无需随后重复运行 smoke。可用 `PI_BRIDGE_PI_PACKAGE=/absolute/path/to/@earendil-works/pi-coding-agent` 指向另一份已安装的 **1.4.0** 包进行验证。该变量只用于测试，不是扩展配置。

假服务测试只是传输证据，**不是真实 provider 或产品集成验收**。扩展拥有 Pi 进程的操作系统权限，并非 OS 沙箱；同权限恶意代码可以读取环境、文件或进程内存。服务端 capability 才负责真实身份、claim、群组隔离、grants、幂等与生命周期授权；可信启动器和私有父目录仍是前提。回滚时撤下本扩展的显式 `-e` 与工具 allowlist；服务端撤销 capability，启动器清理配置文件。本包不修改任何个人设置或凭据。
