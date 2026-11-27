# 本地 CI 事件观察器

`.github/scripts/observer/observer.py` 使用 Python 3.10+ 标准库、SQLite 和部署方已有认证的 `gh`，不安装 Python 包。支持 macOS/Linux 的本地文件系统，不支持 Windows/NFS。一个私有数据库绑定一个仓库和协调会话，只有一个观察接收器；Agent 通过本地 inbox 消费，不分别轮询 GitHub。

观察器只读取、记录和投递：不合并、不重跑、不发布、不执行日志、PR、通知或 artifact 中的命令。通知是既有任务的调度信息，不是新的执行授权、Full 验证或真机验收结论。

## 部署与权限

所有示例身份均为占位符。实际数据库、assignment 和会话配置留在私有部署目录，不提交到仓库。目录须仅当前用户可访问，数据库须为 0600；不要把 token 放进命令、文档或输出。认证由原 `gh` 处理，观察器不导出或另存凭据；只需要已有的 Actions、contents、PR、Checks、commit statuses 读取权限。

```bash
OBSERVER=.github/scripts/observer/observer.py
DB=.local/ci-observer/state.sqlite

python3 -B "$OBSERVER" --db "$DB" init \
  --repo example/project --coordinator coordinator-session

# Only a trusted operator registers an actual local assignment receipt.
python3 -B "$OBSERVER" --db "$DB" assign --repo example/project --pr 12 \
  --author-session author-session --evidence 'local-assignment:receipt-12'

# Choose ONE receiver mode, not one process per Agent.
python3 -B "$OBSERVER" --db "$DB" once
# For continuous operation, run this instead of once:
python3 -B "$OBSERVER" --db "$DB" watch --interval 30
```

`reconcile` 是 `once` 的别名。`watch/once/reconcile`、标签观察和可信 Merge 发布者 intake 共用同一个接收器锁。初始化和升级也须取得该锁。**不能删除 lock 文件绕过互斥**；进程退出或被杀后内核释放锁。`init` 不会改绑已存在的仓库或协调者。

数据库采用 `journal_mode=DELETE`、`synchronous=FULL`。不要在 SQLite 连接存活期间额外打开再关闭同一个数据库的底层文件描述符：这可能清除 SQLite 的 POSIX 锁。`inbox/status` 使用 SQLite `mode=ro`，不建表、不迁移、不切换 journal；`ack/wait` 只进行各自的事务，不承担升级。

## 来源发现、对账和未知状态

每轮先无缓存请求 `/user`，确认当前 CLI 认证有效，再处理以下固定路径：

| 工作流 | 用途 |
| --- | --- |
| `.github/workflows/merge-gate.yml` | 原生 Merge 作业、失败早报及被测合并树 |
| `.github/workflows/periodic-full.yml` | Periodic 目标及逐项确认归因 |
| `.github/workflows/release-candidate.yml` | Release 目标和阶段就绪信号 |
| `.github/workflows/merge-conflict.yml` | 默认分支代码发布的最终 `Merge gate` Check |

这些是新协议路径；部署方必须确认实际工作流和产物协议已投产。旧 CI 不会自动成为这些工作流的证据。单一路径的元数据 404 不阻止其他路径；已发现工作流的 runs/jobs 查询失败则保持未知。

发现按工作流 ID 查询**不带 branch/status/created 过滤**的原生 runs，避免依赖过滤查询的 1,000 条上限。数据库持久记录 run 摘要、历史收集页、已对账 attempt 和待补状态。活跃及未完成对账来源优先；已稳定历史不逐轮重取所有 jobs，而是轮转审计旧 run，发现离线期间增加的 attempt。历史登记、审计和未知重试是多轮收敛，不承诺一次轮询完成整个历史。

每轮最多处理 20 个 run、每 run 最多推进 4 个 attempt，同时进行有界历史收集。分页最多 500 页，每 run 最多接受 1,000 个 attempt；达到安全边界报未知，不假装历史完整。原生请求使用 `/actions/runs/{id}/attempts/{n}` 和对应 `/jobs`。独立已核实的 jobs 页可以先投递失败；后续页或其他 run 出错，不回滚已经核实的通知，也不会顺便释放未核实来源的 pending。

ETag、JSON 和 Link 元数据保存在本地；304 复用相应响应体，**不代表 live jobs 或整仓库未变**。分页只接受同域、同仓库、相同过滤和连续页码。GitHub 的 `/repositories/<numeric-id>/...` Link 先通过认证的仓库元数据核对 ID，再重建 `/repos/<owner>/<repo>/...` 请求；不会直接跟随通知或任意 Link URL。分页不齐、身份矛盾、401/403/404、未知 conclusion 均不能让旧缓存变成新的成功证据。

JSON 请求预算为 5 秒，明确的传输失败或 5xx 最多立即重试一次；stderr 不作为通知输出。API 限流的 `Retry-After`/reset 持久保存，重启和 webhook 都不能绕过。已结束但待补的来源使用有界重试退避；正常轮询默认 30 秒，可设置 5–3600 秒。日志读取另有短预算，见下文；这些不是端到端通知延迟保证。

已删除或过期的 run/artifact、权限不足、来源不完整的旧待办只能保持未知。记录在 `status` 的 problems 中；不重建数据库、不猜身份，也不阻止独立合法来源。

### 可选 webhook 唤醒

```bash
# A local forwarder supplies newline-delimited JSON; use this INSTEAD of polling-only watch.
python3 -B "$OBSERVER" --db "$DB" watch --edge-stdin --interval 30
```

接受原生 `action=completed` 且含 `workflow_job` 或 `workflow_run` 的 payload，也兼容 `{"event":"workflow_job.completed"}`、`workflow_run.completed`、`run.completed` 提示。支持分片输入，每行最多 256 KiB，超长行丢弃。

stdin 只缩短下一次 REST 对账等待，最多每秒触发一轮；repo、actor、SHA、收件人和命令字段没有授权作用。先完成初始认证对账，EOF 自动回到轮询；丢失、重复、乱序或重连不改变事实来源。

## 事件与目标身份

事件键为 `(repo, sha, run_id, run_attempt, job)`，动作另含 `action`。真实作业使用原生数字 ID；`@terminal`、`@target`、`@attribution:<commit>:<pr>` 等是独立保留键。

- `source_head`：该原生 attempt 的 REST `head_sha`，可能只是控制工作流的源码。
- `workflow_sha`：已核实的控制源码；Release 目标尚未知时可以为空。
- `tested_sha/tested_tree`：通过轻量证据和 REST 校验后的实际测试目标；未知时为空，`target_unknown=true`。
- `sha`：不可变的动作来源键。已发首报不会因稍后得到 target 而换键或换 receipt。

首次失败可只有 `provenance=source_only`，之后用 `job_details/detail_kind=target_binding` 补充目标，不重发或改写原首报。已有目标 SHA/tree、控制源码和已确认 PR 来源不能被同一 attempt 的另一份证据替换。**成功终态、Release 就绪等正向依赖只能满足实际 tested SHA 的订阅，不能拿 source-key SHA 冒充被测目标。**

同一次部分重跑的 jobs 可能包含继承作业，甚至全部被 API 标成最新 `run_attempt`。观察器同时核对 `run_started_at`、job `started_at/completed_at` 和原 attempt 标记。继承的失败不能成为新首阻断，继承的 binary/Full/证据 producer 成功不能成为新就绪信号；时间不足时保持未知。原 attempt 的记录独立保留。

### 原生动作

| 动作 | 含义 |
| --- | --- |
| `first_failure` | 最早可见的本 attempt `failure/timed_out/action_required`，按完成时间、job ID 排序，一轮仅一次；不等所有 jobs 或大产物 |
| `terminal` | 原生整轮结论已知、所有已返回 jobs 终态已知时的独立通知；成功还要求目标已绑定、非空 jobs、全部为本次执行且 success/skipped/neutral |
| `job_details` | 日志或目标补充，不是第二次首报；没有实际阻断 job 的整轮通知使用 `synthetic=true, job_id=null` |
| `attribution` | 经独立来源与包含关系验证的逐项归因，不是失败 job |
| `release_binary_ready` | 当前 attempt 唯一的 `Release candidate binary` 明确成功 |
| `release_full_ready` | 当前 attempt 唯一的 `Release Full evidence` 明确成功 |

Release 的 ancillary duplicate 是一个严格例外。观察器同时核实以下证据：

- 轻量 intent 的 SHA、tree、原生 master 控制源码及其 source blobs。
- GitHub Actions creator 的 first-owner status，同仓库同 workflow 的首轮 admission 作业，以及 status 创建窗口。
- 当前 attempt 的 normalize 和 admission 成功，所有产品、Full、机器作业均 skipped，旧 attempt 也未执行过产品作业。
- 完整日志中明确的 canonical 未成功 fail-closed 原因，没有其他异常错误。

证据齐全时只保留一个 `terminal`，保留原始 `conclusion=failure`。`investigation=false` 和 `release_duplicate` 记录 owner URL、ID、状态及证据来源。canonical 自己的失败仍走普通首报。未知、来源冲突、异常日志或独立执行过产品作业都不能被当作 duplicate 隐藏。

信息性 duplicate 不触发新调查、修复或 begin。Pi 可见但不唤醒模型，Codex 保存可查阅的信息投影但不主动入队。迟到的已核实证据沿用原 terminal receipt，旧本地首报和详情保留审计记录、退出可消费待办，不伪造 ACK。已返回的原生作业事实仍匹配已核实快照，而 API 暂时不可用时，保留信息终态并等待补证，不降级成新调查。owner 后来成功也不会重建 duplicate 调查。返回的证据冲突或异常仍保留普通失败。已经实际送出的历史消息不能撤回。

核实原生作业身份和当前首败赢家后，先提交稳定的 `first_failure` 动作与来源绑定，再读取网络日志。此时不建立 outbox，不能被其他来源的投递顺便发出；在此处崩溃，恢复仍沿用同一赢家、动作 ID 和创建时间。只有重新核实的合法收件人才能进入后续投递。

首次仅给该赢家一次最多 **5 秒**的流式日志窗口。完整 HTTP 200 响应头之后解析到 Go `--- FAIL:`/JSON 具名失败或 timeout running 名称，即可停止慢下载，将已知名称、准确 outcome、job ID/URL 放入同一首次实际投递。不等整轮、完整慢日志或 binary archive；无真实具名证据的环境故障不编造测试名。停止、超时、404 或未知时保留已知字段及 `logs_pending=true`，后续读取预算仍为 **15 秒**，详情关联原动作，默认不另派调查。每次后续对账还可推进一个其他失败 job 的日志。

读取前会推进已有的原生投递；外部 I/O 不持有 SQLite 写事务，其他已投递收件及其独立 ACK 可继续。当前赢家的读取最多占用上述窗口，不新增日志线程池，也不逐个等待所有失败 job。

日志最多读取 64 MiB，单行最多 64 KiB，最多保留 1,000 个测试项，不保留原始输出。结果是有界提取，不保证完整测试清单。`outcome=failed` 才是明确失败；Go timeout 的 running tests 标成 `running_at_timeout`，不是断言失败。测试名、package 和日志文字始终是非可信数据，不执行其中的字符串。

## 轻量产物协议

只接受唯一的精确名称产物、当前 attempt 实际执行并成功的命名 producer、执行区间内的 artifact 创建时间、认证 REST 的 run/source 身份和 SHA-256 digest。ZIP 拒绝重复成员、路径穿越和危险条目；只读取指定 JSON，不执行或提取其他成员。

| 工作流 | producer | artifact / JSON |
| --- | --- | --- |
| Merge | `Merge plan` | `merge-plan-<run>-<attempt>/merge-plan.json` |
| Periodic | `Resolve trusted master` | `periodic-full-plan-<run>-<attempt>/plan.json` |
| Periodic | `Periodic Full evidence` | `periodic-full-initial-<run>-<attempt>/receipt.json` |
| Periodic | `Periodic attribution evidence` | `periodic-full-<run>-<attempt>/receipt.json` |
| Release | `Release normalize` | `release-intent-<run>-<attempt>/receipt.json` |

轻量 plan/intent ZIP 最多 2 MiB，Periodic 证据 ZIP 最多 32 MiB，所读 JSON 最多 2 MiB。Periodic 包可包含真实 producer 附带的安全证据文件，不要求 ZIP 只有一个成员。

Merge 核对 `repo/pr/head/base/sha/tree/run_id/run_attempt`、最新 PR、真实合并 SHA/tree 和顺序为 `[base, head]` 的两个父提交；仅其旧协议允许规范十进制字符串 run 字段。master 控制的 `workflow_dispatch` 还须核对 `controller_sha` 和 PR 的 repo ID/name/ref/base_ref，不能从 run title 猜 PR。控制器目标未核实时先报协调者；核实计划后，按独立绑定补投作者并保留原始首报内容及 ID。

Periodic plan 使用嵌套 `identity`；initial/final 回执为 schema 1、kind `periodic-full`。Release 只读取 schema 1、kind `release-intent` 的轻量回执，包含 `workflow_sha`。这些协议的 schema/run/attempt 使用真正整数，不接受字符串或布尔伪装。target 的 commit tree 和相关 master ancestry 均通过 API 核验。

Release dispatch 的来源须为同仓库 master；`rc/*` push 还须剥离 annotated/lightweight tag，核对其精确 commit，目标不得漂移。不同于 run source 的 Release 控制 SHA 还要求该 attempt 的精确 master `referenced_workflows` 条目及 ancestry。

**不再读取 `release-target-*`，也不下载、解压、执行 `workspace` binary archive。** 两种 Release ready 只投协调者，保存原 job ID 和 `completed_at`。协调者的受信机器桥必须独立下载并复核 manifest/hash、执行真正的 Full/本机 6A/B/C 和最终验收；收到 ready 不等于完成这些步骤。

## 作者与最终 Merge Check

作者来自受信 operator 的 `(repo, pr)` 本地 assignment receipt，不来自 GitHub 共享账号、PR 正文、旧 ledger 或模型参数。

原生 Merge PR 关联为空时，使用认证的 commit→PR API，要求唯一匹配 source head、来源仓库和同仓库 master base，再重新读取 PR。指派缺失/冲突、PR 前移或关联歧义只交协调者。已经投递的历史不会因后来的指派冲突被撤回；尚未投递的作者项保持 pending。相同已核实 PR 的迟到指派可幂等补投。

Periodic/Release 首先交协调者。confirmed 归因支持单项旧格式和 `attribution.items`；每项独立验证 `good → commit → bad → tested` 的包含关系、唯一合入同仓库 master 的 PR 及 fresh PR，再按本地 assignment 投递。未知项不阻止已确认项；归因证据、cause owner 和 outbox 原子保存，崩溃或重启不重复派同一作者调查。Release 复用同 SHA 的 Periodic 归因前，还须重新认证原 run/attempt/jobs/receipt；只有 SHA 相同不够。

最终 `Merge gate` 走独立的 `merge_reports.py` 协议：验证默认分支发布者、原生 reporter 执行、GitHub Actions Check issuer/external ID、结构化 summary、当前 PR/head/base 和成功验证运行的真实合并树。每个矩阵 PR 独立处理；不能把 PR 自报绿色或普通原生 CI 失败当合并冲突。

`merge_gate_failure` 表示新的合并检查调查；`merge_gate_result` 是检查状态更新。已有原生失败调查不重复派工。发布者成功本身不是被测合并树成功，最终 Merge Check 通过也不是 Release/验收通过。协议未匹配或某个 PR 证据异常时，该来源保持未知，不回滚合法兄弟来源。

同一进程也调用 `rc_refs.py` 的标签兜底观察：首次完整快照建立不重放历史的 baseline；后续精确 tag/ref/object 经 API 核验后才可能产生协调者专用 `release_tag_forward`。标签事务使用独立 tag key，不借另一个 workflow run 的成功刷新来释放待办。

## 消费、订阅和显式 ACK

通常动作和各收件人的 outbox 在同一事务创建；首败在日志读取前先提交无收件人的保留动作。首次建立 outbox 前可填入已核实的日志，但不改变动作 ID、来源键、已知身份绑定或创建时间；已有 outbox 后不改写原动作，仅可为尚未实际投递的收件人保存已核实的 payload 投影。已投递 payload、投递时间和 ACK 时间不回写。receipt ID 仍由动作与收件人稳定派生；投递事务同时写 durable inbox 和 delivered 状态。每个收件人独立 ACK，协调者 ACK 不代替作者。

```bash
python3 -B "$OBSERVER" --db "$DB" inbox --recipient author-session --pending --limit 32
python3 -B "$OBSERVER" --db "$DB" inbox --recipient author-session --pending \
  --limit 32 --after "$LAST_RECEIPT_ID"
python3 -B "$OBSERVER" --db "$DB" inbox --recipient author-session --receipt "$RECEIPT_ID"
python3 -B "$OBSERVER" --db "$DB" status
```

pending 页只含 delivered 且未 ACK 项，按 receipt ID 排序；默认每页 32，允许 1–256。`--after` 为该顺序的游标；跨轮扫描应重新从第一页开始，不能用旧尾游标永久跳过后来插入的较小 ID。`--receipt` 精确查询本收件人的一项，含 ACK 状态，不能混用分页参数。不加 `--pending` 的默认 inbox 保留历史兼容行为。

```bash
# Run in a separate background task; do not block the Agent conversation.
python3 -B "$OBSERVER" --db "$DB" wait --recipient author-session \
  --repo example/project --sha "$SHA" --run-id "$RUN" --attempt "$ATTEMPT" \
  --action first_failure --job "$JOB_ID" --timeout 300 \
  > .local/ci-observer/continuation.json &
```

订阅的 repo/SHA/run/attempt/action 和可选 job 必须来自确切的受信任务来源；不能只按 branch 或“最近一次”匹配。一个 recipient 可以有多项独立订阅；相同选择器幂等登记，命中只返回 receipt，**不会自动 ACK**。等待只查询本地 SQLite，最多每 250 ms 一次，不请求 GitHub；超时退出 3，订阅保留。

```bash
python3 -B "$OBSERVER" --db "$DB" cancel-wait --recipient author-session \
  --repo example/project --sha "$SHA" --run-id "$RUN" --attempt "$ATTEMPT" \
  --action first_failure --job "$JOB_ID"

# Only AFTER this recipient actually starts processing this receipt:
python3 -B "$OBSERVER" --db "$DB" ack --recipient author-session --receipt "$RECEIPT_ID"
```

多项等待时，省略选择器的取消会拒绝，不会清空其他任务。ACK 只清除匹配订阅，重复 ACK 保留原时间；错误 recipient/receipt 拒绝确认。标签兜底订阅不用 run/attempt，但必须指定精确 `@tag:<fullref>:<object>` job。

**本地 CLI 的 `--recipient` 不是原生会话认证。** 这些命令面向同一受信 OS 用户；受信适配器必须由服务器执行会话固定 recipient 和 DB，不能让模型或外部消息选择 caller、assignment、其他人的 ACK 或权限。

Pi 原生会话适配见 [CI 观察器的 Pi 收件适配](ci-observer-pi.md)。接收可见性、模型唤醒、durable delivery 和“已开始处理”ACK 是不同状态；消费适配器也须按 receipt ID 幂等。ACK 不代表外部动作完成，取消请求不构成终态回执，未知外部执行不得自动重试或释放资源。

## 运维、升级与回退

- `status` 展示投递/ACK 统计、问题、来源登记和收集进度；零条通知或退出 0 都不是 CI 绿色的证明。
- `once/reconcile` 有界本轮无对账错误退出 0；未知、身份、互斥或本地错误退出 2。其他已核实来源仍可先投递，历史收集可能尚未完成。`watch` 持续重试，Ctrl-C 退出 130。
- 升级前停止接收器和所有写入型消费者，备份完整数据库及可能存在的 journal。新版本在原 DB 上事务迁移，保留旧 action/receipt/ACK 和订阅；不能删除 pending 或换空 DB 来冒充恢复。损坏或缺少身份字段的旧项保留并报告，不猜测修复。
- 停用只需停止接收器/转发器，保留 DB、assignment 和原 lock。取消本地等待不取消 GitHub 执行，也不等于验收完成。
- 不做 schema 原地降级。需要程序回退时先保留升级后的完整副本，再停写恢复升级前备份；备份后的接收/ACK 事实会回退，须核对 receipt 防止重复处理。不要在线只复制主 SQLite 文件，也不要替换仍被进程持有的 lock。

## 离线验证与部署验收

标准入口自动发现观察器目录所有 `test_*.py`，再执行 Pi 的 Node receiver 测试；任一套失败均非零退出。需要本地 Python 和 Node，不需要私有部署、凭据、GitHub 或真实 Agent 会话：

```bash
python3 -B .github/scripts/test-ci-observer.py
# Add -v for individual Python cases.
```

离线测试覆盖分页/304/认证失败/限流、历史和离线重跑、原生部分重跑继承、来源/目标及产物伪造、逐项归因、旧 DB 原样恢复、SIGKILL 投递/迁移、并发锁、只读 inbox、精确多订阅、非对称 ACK、实际本地 CLI/Pi ABI 和 watcher。运行结果以标准入口的当次输出为准，不能仅跑四个核心文件后宣称整体通过。

部署验收仍须由授权集成方进行：实际 GitHub 权限、`gh` 版本的 ETag/304/下载重定向、真实 workflow/artifact 协议、原生会话送达与显式开始处理、机器桥字节复核、真实 Full 和本机 6A/B/C。离线绿测不替代这些验收，也不证明其他并行切片已经投产。

REST 契约参考：[runs/attempt](https://docs.github.com/en/rest/actions/workflow-runs?apiVersion=2022-11-28)、[jobs/logs](https://docs.github.com/en/rest/actions/workflow-jobs?apiVersion=2022-11-28)、[artifacts](https://docs.github.com/en/rest/actions/artifacts?apiVersion=2022-11-28)。各 producer 的本地工作流和脚本是轻量证据接口的直接定义；接口变化须重新验证，不能以猜测兼容放宽身份校验。
