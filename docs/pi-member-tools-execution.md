# Pi 成员工具执行器（slice A）

## 显式启用与信任边界

```go
provider.NewPiMemberToolExecutor(selected Provider, extensionPath, configPath string, timeout time.Duration) (Executor, error)
```

仅接受已确认安装、版本严格为 `1.4.0` 的 Pi；复用 `NewExecutor` 的 ID、可执行路径、版本和超时校验，不启动探针，不修改所选 Provider 元数据或能力声明。Codex、未知提供方及其他版本均拒绝。超时为零仍默认十分钟，负数拒绝。

调用方必须提供并维护可信文件：

- `extensionPath`：非空绝对本地文件路径，目标是可读普通文件；应指向已审查、仅注册四项成员工具的入口。
- `configPath`：绝对路径、非符号链接的普通文件，权限严格为 `0600`，JSON 对象恰有 `endpoint`、`token` 两个字段。调用方负责正确内容、与 CLI 一致的文件所有者，以及文件、扩展依赖和所有祖先路径在执行器生命周期内的可信性与不可变性。这不是针对恶意调用方的路径竞态防护。
- provider 只检查文件元数据，不读取或解析配置内容；bridge 在子进程内读取、校验 JSON 一次。构造器只保存路径字符串；失败返回固定中文错误，不回显路径、底层错误或配置内容。

仅把配置的绝对路径注入子进程的 `AGENT_WORKSPACE_MEMBER_TOOL_CONFIG`；移除继承环境中该键的所有重复值后追加唯一值，不修改父进程环境。token 不进入 argv、提示词、日志、模型可见 schema、响应或回执。其保密还依赖可信入口及服务端拒绝回显敏感数据；provider 不读 token，不能替不可信扩展做内容脱敏。

本 slice **不签发 token，不做 postclaim 授权，不接调度或运行时路由**；调用方的执行身份、群组、权限和生命周期绑定仍由服务端负责。四项工具只能执行服务端授权的平台操作，但扩展代码与 Pi 拥有相同的宿主机权限；禁用内置工具不是文件系统沙箱，也不意味着平台操作只读。

## 启动参数与默认兼容

工具模式保留原有 `--offline --mode json -p --no-extensions --no-mcp --no-skills --no-prompt-templates --no-context-files --no-approve`，以及 `--session-dir`、`--session-id`、可选 model/thinking 行为。仅将 `--no-tools` 替换为 `--no-builtin-tools`，并增加：

```text
--tools list_members,read_channel,send_message,create_member
--extension <trusted absolute entrypoint>
```

Pi 1.4.0 的 `--no-extensions` 仍允许显式 `--extension`。`ExecuteRequest`、现有构造器和 factory 不变；普通 Pi 保持禁用工具，普通 Pi/Codex 的 argv 和原始行预算不变。`--offline` 禁止启动时的自动联网，不禁止显式模型请求或这些工具的授权 HTTP 请求。

## 独立的帧、队列与日志预算

| 边界 | 上限 | 计数规则 |
| --- | ---: | --- |
| 普通 Pi/Codex JSONL 原始行 | `64 << 10`（65536） | 不含 LF，包含去除前的可选 CR |
| 成员工具模式 JSONL 原始行 | `1 << 20`（1048576） | 同上，所有事件一视同仁 |
| bridge 的 HTTP 工具 JSON 请求/结果 | 65536 | HTTP 正文的全部字节，存在末尾换行时也计入 |
| 工具模式待解析原始帧队列 | 64 帧、`8 << 20` 原始字节 | 两项都包含正在解析的帧；原始字节含 CR、不含 LF；有序损坏标记占一帧、零字节 |
| 工具模式额外组帧缓冲 | `1 << 20` | 固定容量，独立于队列；复制前先预留队列计数和字节预算，不复用消费者持有的数据 |
| 产品事件回调队列 | 64 事件、`1 << 20` 字符串字节 | 包含正在执行的回调；不同于原始帧预算，原有逻辑不变 |
| stdout/stderr 原始日志（各自） | `1 << 20` | 独立截断，不停止解析和排空 |

工具参数在原生事件中是对象，不是二次编码的 JSON 字符串。工具结果则把完整 HTTP JSON（可含末尾换行）作为 `content[].text` 字符串嵌入原生 JSON；引号和反斜线再次转义后，原始行可接近两倍大小。HTTP 合法不代表所有累计事件都能装入原生帧预算。

`turn_end` 包含整批工具结果；`agent_end.messages` 累计本次低层运行的初始消息、助手消息和工具结果。即便这些事件不产生产品响应，也必须经过同一行预算及 JSON/UTF-8 校验。**1 MiB 是有界运行聚合预算，不是无限工具轮次支持**：累计事件可以合法超过此限制，此时执行结果必须保持 `unknown`，不能忽略它而报告成功。

仅工具模式启用原始帧流水线：stdout copier 只写有界原始日志、组帧并以非阻塞方式入队；单一消费者串行解码 JSON，独占解析器及原生回执状态。锁只保护计数，不包围复制或解析。队列最多保留 8 MiB 原始数据（包含在途解析帧），另有一个固定 1 MiB 组帧缓冲和固定队列元数据；消费者的解码临时对象仍受单帧上限约束。普通 Pi/Codex 不创建该队列或缓冲，维持同步解析和原有分配路径。

原始行超限时入队有序损坏标记，丢弃该行至 LF/EOF，继续排空；无效 JSON/UTF-8 由消费者标记损坏。未溢出时，后续有效帧仍可观察到。原始队列计数或字节溢出则请求取消，停止接纳新帧，但继续排空并写有界日志；消费者处理完已接纳的前缀，再有序标记协议损坏，不能从丢失的后缀推断终态。此类溢出不是 `DeliveryFailed`；回调队列的独立失败仍按原规则处理。

`WaitDelay` 保持 100 ms，不再覆盖工具模式的 JSON 解码工作。子进程 `Wait` 加入所有管道读线程后，先关闭并加入原始帧消费者，再关闭并加入事件交付，最后读取回执；`Start` 失败也加入所有已创建的工作线程，不以超时遗弃解析器或回调。后续原生终态可保留为 `Terminal` 证据，但不能修复受损的 `Outcome`/`OutputComplete`。取消、管道不完整、队列溢出和未知结果不构成提前释放外部资源的依据；回收直接子进程也不证明后代或外部操作已终止。

每条完整助手 `message_end` 都产生替换式文本快照，包括纯工具调用助手消息对应的空字符串；空快照必须清除之前的候选文本，避免终态回执发布旧回答。仅提取助手文本，工具参数和 `toolResult` 文本不进入响应；混合消息中的助手文本仍保留。最终 stop reason 和 `agent_settled` 保持权威。原始日志截断可与 `OutputComplete=true`、执行成功同时成立；日志写入失败则仍按原有规则取消并保留未知结果。串行交付队列仍受原有 64 事件、1 MiB 字符串载荷预算限制。

## 源码依据与本地夹具

只读核对安装版 Pi 1.4.0：`pi-coding-agent/dist/modes/print-mode.js:84–86` 对事件直接 `JSON.stringify` 加 LF；`json-event.js:16–18` 只转换 `message_update`。`pi-agent-core/dist/types.d.ts:452–497`、`agent-loop.js:48–55,170–185,654+`，以及 `pi-ai/dist/types.d.ts:271,368,420` 定义对象参数、结果文本及累计事件；`agent-session.js:753` 添加 `willRetry`。bridge `ee1ee4f` 的 `details: undefined` 在 JSON 中省略。显式扩展加载的既有 help 证据位于本地 `.local/evidence/live-loop-20261101/logs/pi-help.txt`。

新增测试前缀为 `TestPiMemberTools`、`TestExecutionFrames`，仅使用临时假 CLI、合成凭据及程序生成的原生 JSONL，无模型、认证、服务或真实 HTTP 调用。测试分别覆盖构造/argv/env、原生协议和帧队列；额外篇幅用于完整原生重复事件，而非提交大数据文件：

- 65536 字节紧凑请求对象、65536 字节含 LF 的结果 JSON，四种工具的完整多轮序列；生成约 1.58 MB 输出、最大帧 330026 字节，证明日志截断与完整成功互不混淆。
- 八项合法 HTTP 结果构成整批 `turn_end` 和累计 `agent_end`，仅调节最后一项结果正文，使原始帧精确命中 1048576 / 1048577 字节及 CRLF 边界。约 5.24 MB 的临时流来自原生事件对结果的多次重复，不是任意巨大填充；覆盖后续响应、EOF、未知结果和直接子进程回收。
- 最大助手工具调用 `message_end` 使用十六项各不超过 65536 字节的合法参数对象，精确达到 1 MiB，覆盖 `RawMessage` 内容被再次解析的路径及 LF/CRLF 边界。
- 确定性阻塞解析消费者，在子进程写完全部尾部并退出后继续阻塞 250 ms：同步对照返回 `ErrWaitDelay`、EOF 不完整、unknown；队列路径在解析仍被阻塞时已完成 `Wait` 和 EOF，等待解析释放后才返回成功。100 ms `WaitDelay`、子进程输出和回执断言均未放宽。
- 64 帧及 8 MiB 精确边界包含在途帧；再多一帧/一字节触发非阻塞溢出、持续排空和有序损坏。既有约 5.24 MB / 54 帧的完整原生累计夹具可在解析暂停时装入该预算；这不是无限突发或无限工具轮次保证。
- 普通 Pi/Codex 的 65536 / 65537 字节与 CRLF 边界仍按原语义执行。Go 1.26.0 的 `BenchmarkExecutionFramesPlainParser` 变更前后均为：Pi 2752 B/op、51 allocs/op；Codex 1896 B/op、41 allocs/op。这是解析器分配对照，不是吞吐量保证。

早前 `race-snapshot-first.log` 的首次失败历史保留不变；受控诊断证实同步解析本身足以耗尽 copier 的 `WaitDelay`，不是以一次成功重试消除疑问。本次使用 `GOTOOLCHAIN=go1.26.0` 的 parser RED/GREEN、定向 race、分配基线、provider vet 和差异检查的精确命令及输出保存在本地 `.local/pi/pi-member-tool-executor/`，不提交运行证据或凭据。未进行真实提供方验证，也不据夹具宣称已经完成运行时集成或真实工具获准执行。
