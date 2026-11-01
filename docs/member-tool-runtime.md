# 成员工具 postclaim 运行时（slice B）

本 slice 只提供适配器无关的凭证生命周期，不接入 Prepare、HTTP/cmd 路由，不启用工具，也不宣称 Pi/Codex 真实工具验收通过。

## API 与组合

```go
type MemberToolRuntimeOptions struct {
    ConfigRoot, Endpoint string
    Operations []string
    MaxGroupMembers int
}
func NewMemberToolRuntime(*store.Store, MemberToolRuntimeOptions) (*MemberToolRuntime, error)
func (*MemberToolRuntime) Tools() *MemberTools

type MemberToolRunBinding struct {
    GroupID, MemberID, ConversationID, DeliveryID, ProviderID, Model string
}
type MemberToolExecutorFactory func(configPath, runID string, toolNames []string) (provider.Executor, error)
func (*MemberToolRuntime) Wrap(MemberToolRunBinding, MemberToolExecutorFactory) (provider.Executor, error)
```

- 构造函数复制策略；`Tools()` 始终返回绑定同一个 Store 的同一个 registry，供后续私有 HTTP 组合复用。不能另建 registry 接收这份 token。
- `ConfigRoot` 必须已存在、绝对路径、末级非符号链接且权限严格为 `0700`；记录并复查目录身份及解析路径。祖先必须可信、稳定。
- `Endpoint` 仅允许 `http://127.0.0.1:<port>/api/v1/member-tools` 或 `http://[::1]:<port>/api/v1/member-tools`；端口须为 1..5 位十进制数字，数值 `1..65535`。地址/端口形式与冻结的 [bridge](../extensions/pi-bridge/bridge.mjs) 对齐，不接受其他 loopback、展开/映射 IPv6、六位零填充端口、DNS、userinfo、query、fragment、编码路径、尾斜线或任何 CR/LF；合法输入不做 authority 规范化。
- `Operations` 为 `list_members/read_channel/send_message/create_member` 的非空、无重复子集。容量默认 `4`，显式范围 `1..200`。
- `Wrap` 复制规范化身份，验证 factory；不读 claim、不分配 token、不建文件、不调用 factory。授予 `create_member` 时，ProviderID/Model 必须非空且规范化；创建策略继承这两个可信值及容量，没有外部 cwd。Model 长度沿用原生执行契约的 256 字节上限；不授予创建时可使用空 provider/model。
- 后续 C 从权威 Prepare 记录建立 binding，并从已验证目录选择可信 adapter factory。模型参数不能选择身份、群组、权限或 adapter。原生 Pi constructor 是独立接入，不是 B 的依赖；factory 是正式组合接口，不是全局测试 hook。

## 执行顺序与结果

调度器先 ClaimTurn，再设置 `request.RunID/RuntimeDir`。每个 wrapper 只能调用一次 Execute，包括首次准备失败的情形。

1. 用捕获的 GroupID 与请求 RunID 读取 Turn；精确核对 member/conversation/delivery、非空 claim token 和可信 Model。只允许无原生 session、acceptance、候选输出/终态、取消记录的 fresh `starting`；已有 `running` 也不能作为新启动。
2. 只检查日志路径，不创建、删除或打开 `RuntimeDir/RunID`。复用现有目录身份重叠检查，拒绝配置根与 RuntimeDir 相同、祖孙重叠或别名；已有 RunID 文件、目录或符号链接都视为历史未知。
3. 用读取到的精确 `store.TurnClaim` 调用 Issue，由 registry 复查成员、状态、取消和 generation。成功后，在固定目录句柄下以独立随机文件名、`O_EXCL/0600` 写入且只写入 `{"endpoint":...,"token":...}`；写入句柄完全关闭后才允许调用 factory。
4. factory 只收到绝对配置路径、可信 RunID 和复制的工具名子集；必须只构造 executor，即使返回错误也不得启动执行。factory 前后复查当前 claim、registry 权限、日志缺席和文件/根身份。每次 preflight 用 `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)` 创建独立的 5 秒复核 context，并在返回时 cancel；GetTurn 与 withAuthorized 共用这份期限，包括 database/sql 连接池等待。原 context 取消不妨碍复核 generation，但未能在期限内确认 fresh authority 一律为 `unknown`，不能降为 `not_started`。这是每次复核的期限，不是 native 执行或整个 Execute 的返回期限。
5. 调用 `native.Execute(ctx, request, onEvent)`，三个参数原样传入；调用后原样返回全部 native receipt 字段，包括 Outcome、Terminal、OutputComplete、Reason 等，不编造成功、回滚或替换已知终态。
6. 所有 Issue 成功的退出路径都先无条件 Revoke，等待已授权在途操作结束，再清理本次拥有的配置条目并关闭目录句柄；取消和超时不能跳过 drain。不结算 Turn、不释放 claim、不关闭 Store、不重试。

| 证据 | wrapper 的结果 |
| --- | --- |
| 重复 Execute、已有日志、Issue 失败、复核超时或读取/身份不符、generation 改变、Turn 取消/恢复/unknown/终态或已有原生执行证据 | `unknown`，固定中文原因要求对账，不调用 native |
| 本次独占 Issue 成功后，仍可复核 fresh claim、权限和日志缺席，但配置/factory 准备失败或启动前 context 取消 | `not_started`，固定脱敏中文原因 |
| 已调用 native Execute | 原生 receipt 完整保留，native 自己判定未启动、未知或终态 |

尤其是 factory 报错也必须先复核 unknown/日志证据，不能一概降为 `not_started`。未知 receipt 交给调度器应用后，Store 保留资源与容量占用。wrapper 的一次性 guard 不是跨 wrapper/跨进程的执行历史缓存：仍依赖唯一调度认领、Store 恢复 fencing，以及 adapter 原有的独占创建 RunID 日志目录；不能以新 wrapper 盲目重试。

## 清理与信任边界

token 不进入 request、factory 参数明文、prompt、事件或 wrapper 原因；可信原生 adapter 也必须保持这一边界，不能把配置内容复制到 argv、日志、事件或 receipt。wrapper 原样转发 native 输出，不是通用脱敏器。

配置清理使用固定的 `os.Root` 目录句柄及创建时文件身份：目录被移动时只在原目录清理；文件名已被替换成另一文件、目录或符号链接则不删除、不跟随。原文件只是改权限仍可按身份清理。文件被移走、删除失败或身份无法确认时不扫描、不删除替代条目；可能遗留已撤销的配置文件，也不因此改写原生结果。

这是可信本机文件系统中的生命周期管理，不是对同 UID 恶意程序的文件系统沙箱。目录身份复核与后续使用、文件身份检查与 unlink 并非原子 compare-and-delete，不能抵抗持续并发篡改、特殊挂载或宿主攻击。受支持宿主上的目录句柄防止路径替换将清理引向新目录；最终授权始终由 registry 的撤销和每次操作的 Store fencing 决定。若这些信任前提失效，应停止新启动并独立对账/清理，而不是宣称撤销能终止外部进程。

## 验证范围

`TestMemberToolRuntime*` 使用现有小型平台 fixture、临时 SQLite 和确定性进程内 factory/executor，覆盖真实 claim、策略复制、跨身份隔离、生命周期/generation、凭证边界、在途 drain、文件替换/重叠、失败分类和 unknown 资源保留；不启动真实 CLI。取消后的 gated operation 保持超过复核窗口（6 秒），验证 Revoke 仍等待、凭证文件保留、Turn/资源不提前释放，随后原样返回 native receipt。

连接池测试边界：Store 未公开 sql.DB、连接借用或持连接回调；fixture 的 raw DB 是另一连接，不能可靠构造 Store 单连接池的无限占用。此处未用反射、全局 driver/timing hook 或新增 Store 接口伪造池耗尽测试；现有真实 SQLite 的取消、恢复、factory-error/unknown/log-history 回归验证分类，但不等同于直接验证连接池超时。5 秒 context 约束可取消的复核工作，不为 registry 锁等待、Revoke 在途 drain 或文件系统调用提供强制中断；Revoke 必须无条件 join，不能提前关闭 Store、结算 Turn 或释放 claim。

产品实现约 300 行，较多测试用于这些安全/失败矩阵，不引入大型 store fixture 或全局注入点。实际 native 协议、CLI 参数与 live 工具接入由后续独立 slice 验证。
