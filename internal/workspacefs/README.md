# workspacefs（Unix）

仅提供真实目录的有界文本 IO，不是用户授权、执行器或 Artifact 版本库。

- 可信 server 调用 `Open(absolutePath, expectedFileInfo)`；物理身份取自可信 caller 的实际目录观察，不能由模型选择根、用户、群或 grants。每次操作核对当前根路径与持有句柄的物理身份；句柄由本包持有，`Close` 等待同实例操作完成。
- `List(ctx, relativeDir)` 非递归、按名称排序，最多 200 个公开项（含 symlink/特殊项），超限整体失败；保留暂存目录隐藏且不计入上限，最多读取 202 个目录项，仍有界。根目录用 `.`，列表不是一致性快照。
- `Read(ctx, relativeFile)` 返回 `Snapshot{Text, SHA256, Mode}`；只读取普通文件、有效 UTF-8，最多 64 KiB（按字节，包含边界），SHA256 为实际字节的小写十六进制。Snapshot 是观察，不是授权或 OS snapshot。
- `Replace(ctx, relativeFile, expectedSHA256, text)` 检查实际 preimage，staging 后重新检查字节、inode、大小、mtime、mode，再以同一父目录下私有暂存区的 `Root.Rename` 发布完整的新 inode。保留普通权限位 `0777`；不承诺保留 owner/group、ACL、xattr、特殊权限位、时间或 hardlink 关系。
- `Create(ctx, relativeFile, text)` 以私有暂存区内独占 staging + `Root.Link` 原子发布完整文本，目标存在则不覆盖（包括 symlink/目录），新文件权限为 `0600`。不递归建目标目录，不提供任意删除、重命名或命令功能。发布前观察到已存在返回 unchanged；发布尝试后任何错误（包括 `EEXIST`）均保守 unknown。
- 所有输入路径必须是规范的、UTF-8 的 slash 相对路径；拒绝空路径、绝对路径、`.` 文件名、`..`、重复/尾部分隔符、反斜杠、NUL，以及所有 symlink 路径组件（包括内部 symlink）。列表只描述最终 symlink，不跟随它。

## 强制的暂存协议边界

每个目标父目录保留固定 `.workspacefs-staging` 命名空间，首次写入时创建，要求是当前有效 UID 所有的 `0700` 普通目录；已有非私有目录/文件/symlink 拒绝，不改其权限。`Open` 拒绝根路径包含该保留组件，公共 `List/Read/Replace/Create` 拒绝任何包含该组件的相对路径，列表隐藏该项。因此另一 `Root` 即使知道随机 payload 名，也不能通过这些 API 观察或替换活动暂存。

写入持有私有目录的独立 `os.Root` 句柄、独占随机文件及其 inode 身份；发布前复核实际请求字节、inode 和暂存目录身份。清理只相对此私有句柄、对仍匹配本次 inode 的 payload 执行，**从不按公共目录名删除暂存目录**；该空目录保留复用，失败时不匹配的文件留下而不删除。成功时同步源私有目录及目标父目录。

这些强制措施隔离遵守本包路径协议的 Root，不是针对同 UID 任意进程的隔离。可信 caller 必须确保整个写入期间，保留目录项及内部文件仅由本协议写入；若非合作进程有能力改它，需先用真正的 OS 权限/进程隔离或物理独占排除它，仅持平台合作锁不够。当前 UID/`0700` 检查不证明 ACL、挂载或外进程已排除。发布仍使用父目录相对的源路径；身份复核和清理前检查**都不是原子 CAS**，不能据此宣称防御最后窗口内恶意同 UID 替换。无法满足此前提时不可调用写入 API。

## 写回执与取消

`WriteReceipt` 即使返回 error 也有效；hash 描述已核验 preimage / 请求的新字节，不证明最终路径未被别人再改。

| 回执 | 可确认的事实 |
| --- | --- |
| `EffectUnchanged` + error | 本次没有发布目标；外部写者可能已改目标，不能解释为“全局原文未变” |
| `StagePublish / EffectUnknown` + error | 已尝试发布，效果未知，包括 `EEXIST`；不自动重试或回滚 |
| `StageSync / EffectApplied` + error | 发布已成功；后续清理/目录同步/根复核未完整确认，不能说成“未执行” |
| `StageDone / EffectApplied` + nil | 文件数据、权限和父目录同步及根复核均成功，不代表业务接受或 SQLite 提交 |

发布前的取消返回 unchanged；发布一经尝试便完成必要收尾，不用取消掩盖真实结果。取消不是可中断 syscall、回滚或资源释放协议。异常退出可能在保留命名空间中留下随机 payload；本包只清理本次仍匹配身份的私有 payload，不按公共名称删除目录或目标、不建恢复队列。

## 必须由上层维持的边界

根及其目录祖先必须可信且稳定，文件系统须支持这里使用的 Unix rename/link/sync 语义。`os.Root` 不是 OS sandbox：不隔离挂载、设备、hardlink 或恶意外部写者。拒绝特殊目标、非阻塞读取和复核不能消除敌对目录替换竞态。

同一 `Root` 的 mutex 只串行本实例。平台必须按真实节点/目录/资源身份跨句柄、跨项目持有资源锁，并在同一锁内读取及保存实际 preimage blob、持久 intent，再调用写入、保存结果。最终复核与 rename 之间没有 inode/hash CAS；外进程仍可能在该窗口写入，禁止声称对外原子 CAS 或 filesystem+SQLite exactly-once。

业务权限、Engine/SQLite/run 绑定、持久幂等与未知门禁均由紧接着的可信封装负责。任何 `EffectUnknown` 或带 error 的 `EffectApplied` 都须保留原 Turn/resource gate 并对账，不能通过重试、补偿队列或另起执行绕过。本包未接 Model/MemberTools/Artifact/UI，不构成已有用户目录的产品验收。
