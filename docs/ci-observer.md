# 本地 CI 事件观察器

`.github/scripts/observer/observer.py` 是一个独立 CLI：Python 3.10+ 标准库、SQLite 和部署方已有且已认证的 `gh`，不安装 Python 包。支持 macOS/Linux 的本地文件系统（`flock` + SQLite），不支持 Windows/NFS。一个私有 DB 绑定一个仓库及协调会话；所有 Agent 只访问本地 inbox/ack，不分别请求 GitHub。

事务使用 `journal_mode=DELETE`、`synchronous=FULL` 和有界锁等待。刻意不依赖 WAL：旧版 bundled SQLite 存在官方已确认的 [WAL-reset 损坏风险](https://sqlite.org/wal.html#the_wal_reset_bug)。已有 DB 的权限通过 stat 检查，不能额外 open/close 底层文件，否则可能清除 SQLite 持有的 [POSIX advisory locks](https://sqlite.org/howtocorrupt.html#_posix_advisory_locks_canceled_by_a_separate_thread_doing_close_)。

它只观察、保存和投递，不合并、不重跑 CI、不发布、不运行验收、不执行日志/artifact/PR 提供的命令。**operation owner 来自受信 operator 的本地 assignment，而不是 GitHub 个人账号、PR 正文或旧复审 ledger。后台 run 是经过 authenticated REST 核验的 source，不是可执行输入。**

## 私有部署

下面均为示例变量和虚构身份；实际 session、回执引用和 DB 留在部署目录，不提交到仓库。目录须由当前用户独占，DB 必须为 0600。不要把 token 放入命令、文件或输出；观察器不调用凭据导出接口、不读取凭据内容，认证交给原 `gh`。只需要读 Actions、contents、PR 的现有权限。

```bash
OBSERVER=.github/scripts/observer/observer.py
DB=.local/ci-observer/state.sqlite
python3 -B "$OBSERVER" --db "$DB" init --repo example/project --coordinator coordinator-session

# Only a trusted operator registers the local PR creation/assignment receipt.
python3 -B "$OBSERVER" --db "$DB" assign --repo example/project --pr 12 \
  --author-session author-session --evidence 'local-assignment:receipt-12'

# Choose ONE receiver, not one per Agent.
python3 -B "$OBSERVER" --db "$DB" once
python3 -B "$OBSERVER" --db "$DB" watch --interval 30
```

`reconcile` 是 `once` 的别名。`watch`、`once`、`reconcile` 共用同一个不删除的 DB lock 文件；第二接收进程拒绝启动，进程被杀后内核释放锁。不要删除 lock 文件绕过互斥。`init` 不允许悄悄改绑仓库或协调者。

每轮先不带缓存请求 `/user` 验证认证身份，再分页对账 run，逐一请求当前 run 和每个 attempt 的原生 URI：

- `/repos/{owner}/{repo}/actions/runs/{id}/attempts/{n}`
- `/repos/{owner}/{repo}/actions/runs/{id}/attempts/{n}/jobs`

run 列表完整重扫，不以 `created_at` 截断；DB 已知 run 也持续对账，所以离线期间完成的旧 run rerun 不会被时间游标漏掉。列表 page 1 的 304 仍遍历缓存的后续分页、刷新当前 run 和 attempt jobs，不能当作“整仓库未变”。每个 URL 的 ETag、JSON 和下一页链接持久保存；304 保留它们。只接受同 API 域、相同路径、连续页码的分页；总数变化、分页不齐或达到 500 页上限会报告未知，而不是宣称完成。每个 run 最多接受 1000 attempts，超过同样保留未知。

`gh api --include --method GET --hostname github.com` 负责真实条件请求和 GitHub 的下载重定向；其跨主机重定向认证隔离由原 CLI 实现。子进程 stderr 不转发，关闭 `GH_DEBUG`/交互提示，原始日志不落盘。单次请求最多 20 秒，网络/5xx 立即最多重试一次；403/404/401 不用旧缓存冒充新证据，下一轮重试。失败轮询指数退避，最长 3600 秒；正常默认 30 秒，可调 5–3600 秒。首次启动、重启及转发断线后的下一轮都重新认证和对账。对应 attempt 没有成功对账，其 pending 不投递；原有 confirmed/delivered/acked 事实不被 API 故障抹去。

完整历史条件扫描优先保证不漏 rerun，**没有宣称大仓库的请求规模优化**。观察速率受 GitHub 限流约束，遇限流应增大间隔并查看 `status`，不能给每个 Agent 另开接收器。GitHub 已删除/过期的 run 或 artifact 无法凭空恢复，保留未知及已有本地证据。

## 本地 edge-trigger 原型

可在同一个接收进程中接收转发器的逐行 JSON，不需要用户部署 webhook：

```bash
# A local forwarder supplies lines such as {"event":"workflow_job.completed"}.
# Run this INSTEAD OF the polling-only receiver, with the same DB.
python3 -B "$OBSERVER" --db "$DB" watch --edge-stdin --interval 30
```

识别 `workflow_job.completed`、`workflow_run.completed`、`run.completed`。每行最多 8 KiB；其余字段全部忽略，repo/SHA/收件人/job/命令均不能从转发内容取得。先完成 API 对账，才读取唤醒输入；接受的边沿只缩短下次 authenticated REST 对账等待（最多每秒一轮），不直接创建证据。EOF 自动退回条件轮询；重连时重启同一个接收器即可，仍先对账。通知丢失、重复、乱序不影响事实来源。

## 事件、身份与路由

事件键为 `(repo, sha, run_id, run_attempt, job)`，动作额外区分 `action`。真实 job 只用 API 的稳定数字 job ID；`@terminal`、`@attribution` 是不可能与数字 ID 混淆的独立保留值。每轮共享一次分页 jobs 获取所有 job，包括复用 Full 的内部 jobs，不以 job 名过滤阻断范围。

- `first_failure`：该 attempt 当前完整 API 快照中最早 `completed_at`（同时间按 job ID）的 `failure/timed_out/action_required`，立即持久投递且整轮仅一次，不等日志下载。后来才可见的更早完成 job 仍保存，但不撤回或重发已经发出的首报。
- `terminal`：整轮 API 结论已知且所有返回 jobs 都有已知终态时单独一次。成功还要求非空 jobs 且全部 success/skipped/neutral；冲突、未知 conclusion、未齐 jobs 不会变绿。没有已知阻断 job 的终态明确 `synthetic=true, job_id=null`，不伪造失败 job ID。整轮通知始终用 `@terminal`。
- `job_details`：从该失败 job 的 authenticated 日志解析 `--- FAIL:` 与 Go test JSON，只保存测试名及已知 package；日志失败保持 `logs_pending=true`，首报仍有 job ID 和经过同域校验的 GitHub job HTML URL。日志后补每个 job 单独一次，不重发首阻断。日志最多 8 MiB，超限同样待补；原始输出不入 inbox。
- `attribution`：满足下述约束后的独立 confirmed 补报，一轮一次，不把归因当成失败 job。
- `release_binary_ready`：合法 release 的精确 display name **`Release candidate binary`** 在同 attempt 唯一且 completed success 时，给协调者一份以真实 job ID 去重的待办。无需等 Full 结束；它只允许协调者按本地权威指派启动真实本机 6A/B/C，**不是扫描或通过了验收，也不自动执行任何命令**。

所有 run/attempt/job 都校验 repo、工作流 ID/路径、event、head、run ID/attempt、同域 HTML URL。重复快照不会重复动作，旧 in-progress 快照不覆盖终态，同一个 job/attempt 的终态冲突保持未知。target 的 SHA/tree 一经核实便持久绑定，不允许同 attempt 换目标。

### SHA 不混用

`source_head` 是该 attempt REST 的 `head_sha`；`workflow_sha` 默认等于它，release 回执声明不同控制源码 SHA 时还必须有该 attempt `referenced_workflows` 中精确的 `repo/.github/workflows/release-candidate.yml@refs/heads/master` 和相同 SHA，并验证 master ancestry。

- merge/periodic 的事件键 `sha` 固定为 API source head。merge 的 `tested_sha=null`：没有可信 checkout 回执时，不把 PR head 当成实际 merge checkout。
- periodic 首报也允许 `tested_sha=null`。它可能在旧 event/source 下才解析最新 master 目标，因此 confirmed attribution 中的 `tested_sha/tree` 取核实后的回执，而不是偷偷改动旧事件键。
- release 的事件键 `sha` **就是核实后的 target SHA**，同时保存不同的 source head；目标未绑定时只累计 observations，不发送 release 成功或失败动作，后续重试。

这是必要的早报边界：没有提前 target 回执，无法既立即报告 periodic 的失败 job，又谎称已经知道被测 checkout。需按实际通知的 `sha` 建立等待；使用 `tested_sha` 判断测试目标，不能把两者混为一谈。

### PR 作者

仅 `.github/workflows/merge-gate.yml` 的 `pull_request` 走早期作者 fanout。API run 必须唯一关联该 PR，run 的 source head、嵌入 PR head 和重新读取 PR 的 head SHA 一致，PR base 为同仓库 master，head repo 与 run 一致；然后按 `(repo, pr)` 查询本地 assignment。所有 job 的阻断均适用。共享 GitHub author 账号和旧复审 ledger **不能证明作者 session**。

缺失/冲突指派、PR head 已前移或来源不明，仅交协调者。已核实 PR 但本地 assignment 迟到时，会按原 receipt ID 幂等补投作者；原动作的 PR 来源从未核实则不猜测追溯路由。后来发生 assignment 冲突，会阻止该作者尚未投递的项，保留 pending 供审查；已经投递的事实不撤销。冲突不能靠模型选择一个 session 解决，应交受信 operator 核查。

periodic/release 的失败先交协调者，release 永不凭 artifact/PR 内容指派作者。

### periodic confirmed 归因

仅接受同仓库 master 分支的 schedule/workflow_dispatch/fallback push，source head 必须经 API compare 核实包含于当前 master 历史。只读取唯一的 `periodic-full-<run_id>-<attempt>` artifact，ZIP 根**仅有** `receipt.json`，最大 1 MiB，回执最大 128 KiB；不接受重复成员、路径穿越或其他文件。

必需字段：`schema=1, kind=periodic-full, repo, run_id, run_attempt, sha, tree, status, verified`；run/attempt/schema 为真正整数，不接受字符串或布尔伪装，verified 为布尔，failure 不得 verified=true。artifact API 的 workflow_run ID/source head、回执身份与当前原生 attempt 一致；target SHA/tree 经 commit API 校验，并验证 target 在 master 历史。

仅 `status=failure` 且 `attribution.status=confirmed` 可转发。必须有正整数 `pr`、有效 `good_sha/bad_sha/commit`；认证 compare 证明 `good != commit`、good 包含于 commit、commit 包含于 bad、bad 包含于被测 SHA；认证 `commit/pulls` 完整分页中必须**唯一**存在已合入同仓库 master 且 `PR.merge_commit_sha == commit` 的 PR，并再次读取该 PR 核对。之后才按本地 assignment fanout。只使用核实的 run/attempt/PR 同域只读证据 URL，不转发 artifact 任意链接或文字命令。本观察器验证来源/包含关系，不自行重放或声称重新证明了定位器的测试结论。

### release target 接口

接受 master 的 workflow_dispatch；或 source head 已在 master 历史、同名 `refs/tags/rc/*` 经 API 剥离 annotated/lightweight tag 后精确指向 source head 的 push。不能只相信 run 的分支名；tag 缺失、漂移、权限失败均未知。rc target 还必须与已核实 tag commit 一致；dispatch 的 target 可以不同于 workflow source。

artifact 名精确为 `release-target-<run_id>-<attempt>`，回执的 schema/kind/repo/run/attempt/SHA/tree 与上述同样严格绑定。target commit/tree 和 master ancestry 均经 API 校验。未发布 target 时保留 observations 等待，不猜目标。

实际 release 切片的 target ZIP 包含 `receipt.json` **及 `workspace` 二进制**；因此这里兼容仅回执，或这两个根成员的精确集合。GitHub 的 artifact REST 下载是整个 ZIP，不能像文件 API 一样只取其中一个成员：观察器有界下载最多 128 MiB，在内存中只读取最大 128 KiB 的回执，绝不提取、加载、执行或把二进制写盘。观察器不核验可执行字节，仅发调度信号；协调者的受信机器桥必须独立下载、复核 manifest/hash、设置可执行权限并进行真实验收。超限或未来改变成员名时保留未知，需由集成人员确认接口后适配，不能假装调度成功。尚未接入 Full 结果验收。

## 消费、后台等待和显式 ack

事件/动作/outbox 保存在 SQLite；动作和全部收件人 outbox 在同一事务创建。receipt ID 由动作身份和收件人稳定派生。投递事务同时写入接收人的 durable inbox、将 outbox 从 pending 改为 delivered；进程在事务前后被杀，都不会出现“标 delivered 却没有 inbox”的窗口。每个收件人单独 ack，协调者确认不代替作者确认。

```bash
python3 -B "$OBSERVER" --db "$DB" inbox --recipient author-session
python3 -B "$OBSERVER" --db "$DB" status

# Run as a separate background task, not a blocking wait in the Agent conversation.
# SHA/RUN/ATTEMPT must be taken from the authenticated-source notification.
python3 -B "$OBSERVER" --db "$DB" wait --recipient author-session \
  --repo example/project --sha "$SHA" --run-id "$RUN" --attempt "$ATTEMPT" \
  --action terminal --timeout 300 > .local/ci-observer/continuation.json &
```

`wait` 是 SQLite inbox watch（最多每 250 ms 检查本地 inbox），不访问 GitHub、不占用会话中的同步等待。每个 recipient 只能登记一项 `(repo, sha, run_id, attempt, action)`；不同等待被拒绝，相同登记可重启。命中返回一条 durable receipt，但**不会自动 ack**；超时退出 3，登记仍保留，可重试或 `cancel-wait --recipient ...`。ack 成功才清除与该回执匹配的等待。

```bash
# Only AFTER this recipient explicitly starts processing that receipt:
python3 -B "$OBSERVER" --db "$DB" ack --recipient author-session --receipt "$RECEIPT_ID"
```

重复 ack 保持原 timestamp，错误 recipient/receipt 拒绝确认。inbox/status 保留 pending/delivered/acked、delivered_at/acked_at 的审计链。消费适配器也必须按 receipt ID 幂等，崩溃后先读未 ack 的 delivered；ack 表示开始处理，不表示外部操作已经完成，未知外部执行不能被重试或释放资源的假终态代替。

**信任边界：**这些本地命令面向同一受信 OS 用户，不提供跨用户会话认证。受信 Agent 适配器必须固定 recipient，并保护 DB/CLI 权限，不能让模型或外部通知选择 recipient、执行 `assign` 或代别人 ack。此 CLI 不直接连接真实 Agent 会话，测试只用虚构收件箱。

## 停用与回退

停止接收进程即可；保留 DB、存在的 journal 伴随文件、lock 和本地 assignment，不删除 pending。停止转发器后继续普通 watch，或停掉旧接收器后用相同 DB `once` 对账。取消本地等待不取消 GitHub 执行，也不意味着验收完成。若需备份，在接收器与写入型消费者停止后复制整套 DB 文件；不要在线只复制主 sqlite 文件。新观察器不改原 CI/规则集，可独立撤下。

`once/reconcile` 完整对账退出 0，有未知/身份/互斥/本地错误退出 2；已核实的其他 attempt 可以先投递。日志或可选归因仍待补会记录在 status，不否定已核实的首报。watch 持续重试，Ctrl-C 退出 130。

## 离线验证与本次 TDD 输出

```bash
python3 -B .github/scripts/observer/test_observer.py
```

测试用临时 SQLite、虚构 API 和两收件人 inbox，不调用 `gh`、真实 GitHub 或实际会话。覆盖分页/重启后 304/仍变化的 live jobs、多 attempts/离线完成 rerun、重复乱序、首阻断+独立终态、补日志、伪造/歧义归因拒绝、作者 fallback/迟到指派、release source 与 target 分离、rc 标签、待续办及非对称 ack。子进程真实 SIGKILL 覆盖 pending 后、投递事务中、投递提交后和单接收锁释放，另外用普通 Python 子进程验证真实输出上限与超时。

首次先运行测试，尚无实现，实际退出 1：

```text
from github import API, GhTransport, Reply, Unknown
ModuleNotFoundError: No module named 'github'
```

复审先补回归测试，实际红测摘要：

```text
Ran 29 tests in 2.019s
FAILED (failures=5)
```

五项分别约束 pending 对账失败不可投递、迟到指派、job 终态冲突、未知状态可追溯、release target/tag 一致性。实际 release ZIP 接口补测也先失败，随后实现兼容。

额外验证 Python 3.11.14 / SQLite 3.50.4 时，CLI 持久化测试报告 `sqlite3.DatabaseError: database disk image is malformed`，单项重试仍复现。已有 DB 的额外 raw open/close 经回归测试确认存在；去除此行为并统一改用 rollback journal 后，两个已安装运行时最终并行验证输出：

```text
# Python 3.15.2 / SQLite 3.54.1
Ran 37 tests in 3.744s
OK

# Python 3.11.14 / SQLite 3.50.4
Ran 37 tests in 3.586s
OK
```

CLI `--help`、实际子进程 `wait` 及 inbox/ack 也已离线运行。未声称以上测试耗时是性能基准。

未做 authenticated GitHub 线上集成验证、实际 Agent 投递或真实 6A/B/C；这些由集成方执行。本 checkout 的 `docs/ci-and-merge.md` 和工作流仍是旧 CI，观察器只识别本需求约定的三个新路径，未上线对应工作流时不会把旧 CI 认作新证据。不得把本地绿测当成线上链路验收。

REST 契约核对来源：[workflow runs/attempt](https://docs.github.com/en/rest/actions/workflow-runs?apiVersion=2022-11-28)、[attempt jobs/logs](https://docs.github.com/en/rest/actions/workflow-jobs?apiVersion=2022-11-28)、[artifacts](https://docs.github.com/en/rest/actions/artifacts?apiVersion=2022-11-28)。本次无 Context7 工具，直接核对官方 REST 文档及 `cli/cli` v2.94.0 的 `pkg/cmd/api/api.go`、`api/http_client.go`（`--include` 状态头、304 非零退出码及跨主机认证行为）；其他 CLI 版本须由部署方验证。
