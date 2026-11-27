# PR 检查、合并和发布

## 当前约定

向开放 PR 的来源分支推送会更新检查，`ci:refresh` 移除后重新添加可刷新检查。旧 PR 反馈、`ci:full` 和 master push 是否启用，须查看默认分支实际生效的 `.github/workflows/ci.yml`，包括 push 开关。每次 master push Full 是 #267 切换前的引导行为，不是持续保证。

稳态由 `.github/workflows/periodic-full.yml` 在获准启动后固定最新 master 执行周期 Full，操作及 push 恢复见 `docs/periodic-full.md`；发布候选通过 `.github/workflows/release-candidate.yml` dispatch 固定所选 SHA，配套 `.github/workflows/release-receipt.yml` 和 `docs/release-candidate.md`。这些配套流程以各自实际部署状态为准，不依赖兄弟草稿的合入顺序。本切片不修改 Full 的分片、算法、并发或预算。

切换前的 Full 是协调约定，不是必需状态规则。已核对的规则集 `27715395` 只有禁止删除和非快进更新，没有必需 `Full CI`。本草稿仍按旧 Full、独立审查和共享锁约定引导合入。**本次升级授权范围内，每个 PR 的实际合并须用户单独确认**；这不是永久仓库规则。长期技术准入是精确树 Merge gate 与独立审查，CI 成功本身不代表合并授权。

独立审查批准精确 PR head。CI 和锁检查该 head 在当前 base 上的合并树。head 变化时按现有约定审查新差异；仅 base 前移不强制重新独立审查同一 head。原审查若明确限定 base/tree，则必须继续满足其范围。

## 使用 Merge gate

默认分支部署后，`merge-gate.yml` 原生处理打开、草稿状态变化、推送、重开、retarget 和 `ci:refresh`，无路径或草稿过滤。只有受信 workflow 明确标记的纯标题/正文编辑或无关标签才忽略，包括仍在排队的元数据运行。sync、retarget、refresh 或未知事件即使全部 skipped 也阻塞旧绿灯。所有目标分支验证自己的实际合并，本地消费才限定 master。

`Merge plan` checkout 原生 `GITHUB_SHA`，fetch-depth 2，实际读取双亲。第二父提交须为事件 head，真实第一父提交是回执 base，不采用可能过期的 payload `base.sha`。仓库、来源、双亲/tree、run/attempt 和控制器策略身份通过后，先上传不可变 `merge-identity-<run>-<attempt>/merge-identity.json`，再执行 diff 格式、工具链安装和 Go 发现。当前精确树的已知规划失败因此仍能报红；无身份证明保持 pending，旧树身份请求刷新。包括空差异和纯文档在内仍须 `git diff --check`。

- ordinary/race 的 `go list -deps -test -json` 保留原始编译节点及 ImportMap 边，反向遍历到真实 test main。源文件归属按 Dir/包确定，`ForTest` 是重编译目标，不是源包。仅改 `_test.go` 运行拥有者完整 ordinary，跨包必需项只运行登记名称，不扩大为完整消费者。真实 race 编译差异仍运行完整 race，保留 ordinary 的 `!race` 调度/liveness 测试。
- 完整套件复用现有 `BATCHES`、慢槽隔离和子集边界，最多 7 个并行任务；workspace 完整 race 保留 8 个确定性名称桶。命名子集按 `merge-gate-weights.json` 的固定产品逐项实测权重，大项优先贪心平衡，每包最多 7 组、目标估算 300s；已有该包测量时，未知项保守取其最大观测值并列为 `unmeasured`；整包无测量则不估算。该文件逐项给出公开 run/attempt、timing artifact/digest、job、invocation 和 pass 时间；从 `trial-timing.jsonl` 按包名/测试名/调用筛选 `action=pass`，读取 `elapsed` 即可复算。来源运行 [45873235028/1](https://github.com/PeterFile/ct0x/actions/runs/45873235028/attempts/1) 整体失败，不是完整 Full 成功样本；38 个权重仅代表各自通过的顶层项。不改变任何超时预算，不把估算当端到端或 p95；单项负载过高和完整 race 仍可能超过十分钟。汇总要求每模式精确并集、无重复及全部必需项实际 pass，不能用拆分少测。普通全包允许非必需 opt-in/subprocess skip，但明确记录，不开启真实 provider/凭据。
- 合并为一个 `go vet <affected packages…>` 任务，包含 copylocks。未知、删除、平台/tagged、embed/fixture 归属不明时扩大检查或失败。删除 Markdown 不能因候选 embed 清单忘记旧文件而降为纯文档。Web 用 Node 22，bridge 用 24.13.0，并覆盖 bridge 的 Go 依赖。
- CI 路径从仓库根目录用 Python `-B` 执行全部 `.github/scripts/test-*.py`。安装 observer/observer.py 时必须有 `test-ci-observer.py`；安装 release-core.py 时还执行并要求三个 `release-test-{evidence,machine,workflow}.py`。安装 periodic-full.py 时先验证 clients/diagnostics/full/history 四个独立套件齐全，才仅给 `test-ci-scheduling.py` 的私有环境副本设置 `CI_TESTS_DISCOVERED=1`，防止 #267 嵌套重复；其他脚本及未安装 periodic 时均清除此继承标记。独立 #271 不要求未安装的兄弟模块。

迁移 SQL、登记、`store.Open` 或受保护策略变化触发独立 ordinary 迁移任务。模板 `.github/scripts/merge-gate-migration_test.go.template` 不进入普通 Go 自动发现或 Full race；受信驱动以独占临时文件注入，候选同名碰撞失败，完成后只清理自己的文件。原有 Go/race 测试保持覆盖。

`merge-gate-migrations.json` 独立固定已发布 18d11a3 的 48 个原始 SQL SHA-256，构建旧数据库前校验。候选登记必须精确覆盖运行时全部迁移的长度、顺序和摘要，同时保留受信主干全部旧前缀；追加 v49 后，下一轮主干的 v49 也被冻结，不能同步改 SQL/摘要/测试来自证。每个前缀创建有 group/member 标记的磁盘 SQLite，升级、比较 ledger/schema、重复 reopen/replay 和跨群组读取。

高风险路径及原执行模式登记见 `merge-gate-manifest.json`。历史缺少现代测试、旧 schema/方法数前置失败或未到业务断言，不算已捕获安全缺陷；纯 docs/Web 的 CLI 时序故障保留给相应 Full，不按时间相关性扩大合并门禁。

`Merge tests / <unit>` 和 `Merge evidence` 均只读。汇总明确要求计划及每个任务为 success，核对 repo/PR/来源 fork/head/base/merge/tree/run/attempt 和精确槽覆盖，cancelled/skipped/missing 不算成功。上传 `merge-gate-<run_id>-<run_attempt>`，只含 `merge-gate.json`。

## 唯一发布器和 source-head 检查

唯一人可见必需名为 **Merge gate**，由 `merge-conflict.yml` 的 Checks API 发布到 PR **source head**。不额外发布 synthetic 检查。受控草稿 #270 的[实测运行](https://github.com/PeterFile/ct0x/actions/runs/45866582172/job/140070847775)表明 synthetic 自定义检查不进入 PR/head rollup，不能拿官方文档推断代替这个实测。

只读 `Resolve Merge PR identities` 先解析身份。`Trusted Merge report / PR n` 按生产仓库加 source SHA 串行，所有同 head 目标共用一个 CheckRun。目标集合变化先撤回旧绿灯。`reason_code=source_head_intersection` 的 `targets` 逐 PR 记录 fork/分支、当前 base/ref/tree、最新 authority run/attempt 和 verification。只有所有开放目标分别有新鲜成功证明才共同绿色；任一失败、未知、缺失或 stale 都阻塞。关闭、retarget、推送重新计算交集，消费时也逐目标重新核对。

两个 job 都只 checkout 经 API 核对主干来源的不可变默认分支 workflow SHA，绝不执行 PR target 代码。仅发布 job 有 `checks:write` 和受信控制器所需 `actions:write`，不运行候选代码、Go、npm 或 artifact。候选测试没有写 token、secret 或共享 Actions cache 写入。

发布器核对工作流 path/id、生产仓库、fork/分支、精确 run/attempt、全部显式 jobs、远端 artifact id/digest、真实双亲/tree 和实时 base/head。原生 run 的 `pull_requests[]` 可为空。确认冲突时 source 行失败，计算中/null、等待或未知为 pending；冲突恢复只有新鲜权威证明才能把同一行改为成功，不遗留第二个同名阻塞行。其他写入者的同名碰撞阻塞。

策略变化先按实际 base/merge blobs 判定，不先解析候选 artifact/jobs。需要时由默认分支 `workflow_dispatch` 固定 PR/来源/base/head/merge/tree/policy SHA，用旧受信驱动在隔离只读 job 验证候选，同时执行候选 CI 自测。因此合法策略升级不要求永远与 master 字节相同。同一发布进程仅对受保护路径按精确提交 SHA 缓存已校验 Git blob/大小的文件内容，最多 256 项、4MiB 解码数据；错误、未知或不匹配响应不缓存，超限恢复普通读取。PR、refs、选中 run/attempt、检查/成员集合及写入后 guard 均保持新鲜读取，不作速度保证。204 只表示派发调用被接受；意图不是已接受 run，未知写入先 API 对账，不直接重试。显式新的 `ci:refresh` 可在完整对账后重新请求。

每次 CheckRun 写入确认后输出一条有界 `MERGE_CHECK` JSON 日志，含 check id/target/status/conclusion、PR/head/base/validated SHA/tree、publisher 和 validation run/attempt，不含凭据或原始 artifact。observer 须同时看 status 和 conclusion，按 `targets`、`failed_prs`、`pending_prs` 路由具体目标，不把兄弟 PR 的失败报成自身失败，也不把 pending 当失败告警。

## 收件、审查和锁内消费

普通代理通过 observer 注册、inbox 和 ACK 接收精确回执，只暂停依赖该回执的动作，继续独立工作。作者会话不使用 `gh run watch/list/view` 等 CI 等待循环。审计及事故对账仍允许普通只读 REST API，不等于恢复会话轮询。

配套部署文档为 `docs/ci-observer.md`、`docs/ci-observer-pi.md`，落地前不宣称实时集成已验证。创建 PR 时用 `observer.py assign` 记录可信 creation/repo+PR+head+session 回执，明确来源和作者，不从 Git 分支名推断。Pi 显式加载 `.github/scripts/observer/pi-receiver.mjs` 并用真实 session ID；发送方 delivered 时间与接收方 begin/ACK 调用时间分开记录。`ci_observer_begin_receipt({receiptId})` 成功会把该 recipient 行标为 acked，但不证明之后已经完成调查或实际工作，须另有工作证据。操作员可用 `observer.py --db "$DB" inbox --recipient …` 和 `ack --recipient … --receipt …` 对账。

默认只读检查外部证明，省略参数失败，不凭本地 JSON 授权绿灯：

```bash
python3 -B .github/scripts/merge-gate-publish.py premerge \
  --repo PeterFile/ct0x --pr "$PR" --reviewed-head "$REVIEWED_HEAD" \
  --run-id "$RUN_ID" --run-attempt "$RUN_ATTEMPT"
```

取得本次合并授权后，增加 `--consume --review "$REVIEW_RECEIPT" --outcome "$NEW_OUTCOME" -- gh pr merge "$PR" --repo PeterFile/ct0x --merge --match-head-commit "$REVIEWED_HEAD"`。只接受这个 argv，不接受 shell/admin/额外参数。协调者用 `result: approved`、`independent: true`、实际 reviewer、完整 head、原始 `evidence_path`/`evidence_sha256` 封装已有独立批准，不得自批。base/tree 只在原审查限定时填写。

工具通过 `git rev-parse --git-common-dir` 找到共享 `.local/merge.lock`，使用历史 mkdir 协议；检查与已授权 merge 操作必须同在锁内。关闭/已合入、draft、非 master、head 不符、冲突、来源不明或交集未验证均阻塞。锁内在消费前再次要求每个目标的最新相关 run/attempt 正是回执所列且明确 completed/success；发布器仍排队时也不能消费被较新失败取代的旧绿灯。入口回执 stale 或锁内 base 前移时，消费模式移除/添加 `ci:refresh` 请求同 head 的新原生树，退出且绝不 merge；不调用 update-branch，不重跑旧 `GITHUB_SHA` attempt 冒充刷新。

macOS/Linux 新协议还持有目录内的内核租约，不能把活动验证误当死锁。进程死亡后，只在 owner/token/inode/租约一致且无发送意图时自动回收；已持久化 `submitted=false` 的事故可记为 `not_submitted` 后回收，不必查询 GitHub。遗留空锁/schema 1、身份不符或其他文件一律不自动删除；未完成持久化的身份仍阻塞。文件及目录同步后才越过发送边界。

发送后即使命令非零也只读核对实际 PR、双亲/tree 和主干关系，不自动重试。远端仍未知则持久保存 incident/owner 并保留原 mkdir 锁，进程退出后新旧客户端仍受阻。只读对账无需额外批准：

```bash
python3 -B .github/scripts/merge-gate-publish.py reconcile \
  --root "$WORKTREE" --outcome "$NEW_OUTCOME"
```

已可能发送的操作即使内核租约随进程退出释放，也必须保留目录锁；仅远端终态及自身 token/inode/事故回执匹配才释放，可重复只读对账。开放 PR 不证明未知写入没有发生。关闭未合并或精确已合并才是远端终态，身份/树不符继续阻塞。已证实未发送的本地记录按上述新协议恢复，正常只读检查释放自己的锁。

文件锁仅约束协作客户端。外部网页/工作流可在最后读取与 GitHub head-only CAS 之间推进 master，B 不是原子 head+base CAS。真实反例 [#253](https://github.com/PeterFile/ct0x/pull/253)：[45590602566 attempt 2](https://github.com/PeterFile/ct0x/actions/runs/45590602566/attempts/2) 验证 base `73eff8c`、head `b1165f1`、tree `8e46b61`，实际同 head 合并却使用 base `e9889c4`、tree `1b6633b`。严格 up-to-date 的 A 可作仓库级替代，但属于另需用户批准的设置。

只读计划检查不启动产品测试，`--root` 的 HEAD 必须等于 `--sha`；JSON 分开列路径、包、ordinary/race 名称和任务，缺失名称/不完整图返回非零：

```bash
python3 -B .github/scripts/merge-gate.py inspect \
  --root "$EXACT_TREE_WORKTREE" --base "$BASE_SHA" --sha "$MERGE_SHA"
```

## 切换和回滚

1. 按旧 Full、独立审查和锁约定引导本草稿；本次升级授权中的每个 PR 合并单独取得用户确认。
2. gate 实际合入 master 后，确认它稳定出现在至少 **三个真实 PR** 上，记录 source-head CheckRun、精确树回执和权限隔离证据，不以本地 mock 代替。
3. 执行已授权的 Step6：保存原规则，只添加必需 **Merge gate**，不添加必需 Full 或其他规则。随后用故意失败草稿验证真正红灯及 `isRequired`，保留实测证据。严格 up-to-date A 不包含在这次授权中。
4. 回滚以实时规则集为基准，只删除本次新增且绑定 Actions app `15368` 的必需 **Merge gate** 条目；若该项是规则中唯一条目，只移除空规则。保留并发变更、deletion/non-fast-forward 及其他检查；原始快照仅作比较证据，不整份覆盖实时规则。不虚构此前启用了必需 Full；Full 调度的一行恢复由对应调度切片单独处理。

## 发布

配套 RC 流程部署前，按引导约定核对所选 master SHA 的可核验 Full；先查生效 `ci.yml` 的 push 开关，不假定仍有每次 push Full。部署 `docs/release-candidate.md`、`release-candidate.yml` 和 `release-receipt.yml` 后，用户另行批准的发布须消费该 SHA/tree/artifact hash 的权威 `release-evidence.json`，联合核对 Full run/attempt 和真实 6A、6B、6C 回执，缺失/不符即阻塞。旧 master push 或周期 Full 均不能单独替代 RC 发布证明。配套文件未落地前不建立悬空链接或宣称已上线。
