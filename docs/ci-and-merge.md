# PR 检查、合并和发布

## 当前约定

向开放 PR 的来源分支推送会更新检查。普通 PR 仍按路径反馈，`ci:full` 请求完整套件，`ci:refresh` 移除后重新添加可刷新检查。主干推送仍运行 Full。现有 Full 调度、算法、并发和预算不在本次变更中修改。

切换前的 Full 是协调约定，不是必需状态规则。已核对的规则集 `27715395` 只有禁止删除和非快进更新，没有必需 `Full CI`。本草稿仍按旧 Full、独立审查和共享锁约定引导合入。**每次实际合并都须用户单独确认**，CI 成功不代表合并授权。

独立审查批准精确 PR head。CI 和锁检查该 head 在当前 base 上的合并树。head 变化时按现有约定审查新差异；仅 base 前移不强制重新独立审查同一 head。原审查若明确限定 base/tree，则必须继续满足其范围。

## 使用 Merge gate

默认分支部署后，`merge-gate.yml` 原生处理打开、草稿状态变化、推送、重开、retarget 和 `ci:refresh`，无路径或草稿过滤。无关编辑不取消有效运行，全部 skipped 的元数据运行不能取代验证结果。所有目标分支验证自己的实际合并，本地消费才限定 master。

`Merge plan` checkout 原生 `GITHUB_SHA`，fetch-depth 2，实际读取双亲。第二父提交须为事件 head，真实第一父提交是回执 base，不采用可能过期的 payload `base.sha`。包括空差异和纯文档在内，每次都验证身份并执行 `git diff --check`。

- ordinary/race 的 `go list -deps -test -json` 图递归选择受影响包，包括测试专用、外部测试导入。运行完整 ordinary 包套件，不以固定 smoke 替代重包。race 编译输入或边不同的受影响包还运行完整 race，保留 ordinary 的 `!race` 生产调度/liveness 测试。
- 复用现有 `BATCHES`、慢槽隔离和子集边界，最多 7 个并行任务。workspace 完整 race 分为 8 个确定性名称桶，证明实际 run/pass 和完整清单精确并集。其余高风险 race 名称见 `.github/scripts/merge-gate-manifest.json`。必需名称缺失、未匹配或 skip 均失败。普通全包允许非必需 opt-in/subprocess skip，但回执明确记录，不开启真实 provider/凭据。
- 合并为一个 `go vet <affected packages…>` 任务，包含 copylocks。未知、删除、平台/tagged、embed/fixture 归属不明时扩大检查或失败。删除 Markdown 不能因候选 embed 清单忘记旧文件而降为纯文档。Web 用 Node 22，bridge 用 24.13.0，并覆盖 bridge 的 Go 依赖。
- CI 路径从仓库根目录用 Python `-B` 执行全部 `.github/scripts/test-*.py`，包括旧 scheduling/shard、Merge 和已安装的 release/periodic/observer/experiment 入口。必需入口或已登记模块测试缺失即失败。

迁移 SQL、登记和 `store.Open` 触发历史前缀升级及跨群组回归。`merge-gate-migrations.json` 独立固定已发布 18d11a3 的 48 个原始 SQL SHA-256，在构建旧数据库前校验。受信驱动生成独立摘要测试，并用 Go overlay 运行基准升级模板，不能同时修改候选 SQL/摘要/测试来自证兼容。每个前缀创建有 group/member 标记的磁盘 SQLite，升级、比较 ledger/schema、重复 reopen/replay 和跨群组读取。未来从已验证主干独立生成并追加登记，不重写旧前缀。

`Merge tests / <unit>` 和 `Merge evidence` 均只读。汇总明确要求计划及每个任务为 success，核对 repo/PR/来源 fork/head/base/merge/tree/run/attempt 和精确槽覆盖，cancelled/skipped/missing 不算成功。上传 `merge-gate-<run_id>-<run_attempt>`，只含 `merge-gate.json`。

## 唯一发布器和 source-head 检查

唯一人可见必需名为 **Merge gate**，由 `merge-conflict.yml` 的 Checks API 发布到 PR **source head**。不额外发布 synthetic 检查。受控草稿 #270 的[实测运行](https://github.com/PeterFile/ct0x/actions/runs/45866582172/job/140070847775)表明 synthetic 自定义检查不进入 PR/head rollup，不能拿官方文档推断代替这个实测。

只读 `Resolve Merge PR identities` 先解析身份。`Trusted Merge report / PR n` 按生产仓库加 source SHA 串行，所有同 head 目标共用一个 CheckRun。首次成功后若出现第二个开放目标，同一行改为 pending，`reason_code=shared_head_multiple_targets`，`targets` 列出 PR、fork/分支、base ref/SHA；不把不同树的相反结果混成一个绿灯。关闭、retarget、推送会重新核对目标集合。当前只允许唯一明确开放目标通过，多目标须消除歧义，本地消费也重新检查。

两个 job 都只 checkout 经 API 核对主干来源的不可变默认分支 workflow SHA，绝不执行 PR target 代码。仅发布 job 有 `checks:write` 和受信控制器所需 `actions:write`，不运行候选代码、Go、npm 或 artifact。候选测试没有写 token、secret 或共享 Actions cache 写入。

发布器核对工作流 path/id、生产仓库、fork/分支、精确 run/attempt、全部显式 jobs、远端 artifact id/digest、真实双亲/tree 和实时 base/head。原生 run 的 `pull_requests[]` 可为空。确认冲突时 source 行失败，计算中/null、等待或未知为 pending；冲突恢复只有新鲜权威证明才能把同一行改为成功，不遗留第二个同名阻塞行。其他写入者的同名碰撞阻塞。

策略变化先按实际 base/merge blobs 判定，不先解析候选 artifact/jobs。需要时由默认分支 `workflow_dispatch` 固定 PR/来源/base/head/merge/tree/policy SHA，用旧受信驱动在隔离只读 job 验证候选，同时执行候选 CI 自测。因此合法策略升级不要求永远与 master 字节相同。204 只表示派发调用被接受；意图不是已接受 run，未知写入先 API 对账，不直接重试。显式新的 `ci:refresh` 可在完整对账后重新请求。

每次 CheckRun 写入确认后输出一条有界 `MERGE_CHECK` JSON 日志，含 check id/target/status/conclusion、PR/head/base/validated SHA/tree、publisher 和 validation run/attempt，不含凭据或原始 artifact。observer 须同时看 status 和 conclusion，并按 `targets` 路由共享 head 的 pending，不能只读旧 conclusion 或把 pending 当失败告警。

## 收件、审查和锁内消费

普通代理通过 observer 注册、inbox 和 ACK 接收精确回执，只暂停依赖该回执的动作，继续独立工作。作者会话不使用 `gh run watch/list/view` 等 CI 等待循环。审计及事故对账仍允许普通只读 REST API，不等于恢复会话轮询。

配套部署文档为 `docs/ci-observer.md`、`docs/ci-observer-pi.md`，落地前不宣称实时集成已验证。创建 PR 时用 `observer.py assign` 记录可信 creation/repo+PR+head+session 回执，明确来源和作者，不从 Git 分支名推断。Pi 显式加载 `.github/scripts/observer/pi-receiver.mjs` 并用真实 session ID；`ci_observer_begin_receipt({receiptId})` 表示 owner 开始处理，不是送达或 ACK。操作员可用 `observer.py --db "$DB" inbox --recipient …` 和 `ack --recipient … --receipt …` 对账。

默认只读检查外部证明，省略参数失败，不凭本地 JSON 授权绿灯：

```bash
python3 -B .github/scripts/merge-gate-publish.py premerge \
  --repo PeterFile/ct0x --pr "$PR" --reviewed-head "$REVIEWED_HEAD" \
  --run-id "$RUN_ID" --run-attempt "$RUN_ATTEMPT"
```

用户单独确认本次合并后，增加 `--consume --review "$REVIEW_RECEIPT" --outcome "$NEW_OUTCOME" -- gh pr merge "$PR" --repo PeterFile/ct0x --merge --match-head-commit "$REVIEWED_HEAD"`。只接受这个 argv，不接受 shell/admin/额外参数。协调者用 `result: approved`、`independent: true`、实际 reviewer、完整 head、原始 `evidence_path`/`evidence_sha256` 封装已有独立批准，不得自批。base/tree 只在原审查限定时填写。

工具通过 `git rev-parse --git-common-dir` 找到共享 `.local/merge.lock`，使用历史 mkdir 协议；检查与已授权 merge 操作必须同在锁内。关闭/已合入、draft、非 master、head 不符、冲突、来源或目标歧义均阻塞。入口回执 stale 或锁内 base 前移时，消费模式移除/添加 `ci:refresh` 请求同 head 的新原生树，退出且绝不 merge；不调用 update-branch，不重跑旧 `GITHUB_SHA` attempt 冒充刷新。

发送后即使命令非零也只读核对实际 PR、双亲/tree 和主干关系，不自动重试。远端仍未知则持久保存 incident/owner 并保留原 mkdir 锁，进程退出后新旧客户端仍受阻。只读对账无需额外批准：

```bash
python3 -B .github/scripts/merge-gate-publish.py reconcile \
  --root "$WORKTREE" --outcome "$NEW_OUTCOME"
```

仅远端终态证明和自身 token/inode/事故回执全部匹配才释放自己的锁，可重复对账。开放 PR 不证明未知写入没有发生。关闭未合并或精确已合并才是终态，身份/树不符继续阻塞。发送前失败和正常只读检查释放自己的锁。

文件锁仅约束协作客户端。外部网页/工作流可在最后读取与 GitHub head-only CAS 之间推进 master，B 不是原子 head+base CAS。真实反例 [#253](https://github.com/PeterFile/ct0x/pull/253)：[45590602566 attempt 2](https://github.com/PeterFile/ct0x/actions/runs/45590602566/attempts/2) 验证 base `73eff8c`、head `b1165f1`、tree `8e46b61`，实际同 head 合并却使用 base `e9889c4`、tree `1b6633b`。严格 up-to-date 的 A 可作仓库级替代，但属于另需用户批准的设置。

只读计划检查不启动产品测试，`--root` 的 HEAD 必须等于 `--sha`；JSON 分开列路径、包、ordinary/race 名称和任务，缺失名称/不完整图返回非零：

```bash
python3 -B .github/scripts/merge-gate.py inspect \
  --root "$EXACT_TREE_WORKTREE" --base "$BASE_SHA" --sha "$MERGE_SHA"
```

## 切换和回滚

1. 按旧 Full、独立审查和锁约定引导本草稿，每次合并单独取得用户确认。
2. gate 实际合入 master 后，确认它稳定出现在至少 **三个真实 PR** 上，记录 source-head CheckRun、精确树回执和权限隔离证据，不以本地 mock 代替。
3. 执行已授权的 Step6：保存原规则，只添加必需 **Merge gate**，不添加必需 Full 或其他规则。随后用故意失败草稿验证真正红灯及 `isRequired`，保留实测证据。严格 up-to-date A 不包含在这次授权中。
4. 回滚只删除本次新增的必需 Merge gate 规则，恢复已保存的原规则，保留 deletion/non-fast-forward；不虚构此前启用了必需 Full。Full 调度的一行恢复由对应调度切片单独处理。

## 发布

配套 RC 流程部署前，仍按当前约定核对所选 master SHA 的 push Full。部署 `docs/release-candidate.md` 及对应 workflow 后，用户另行批准的发布须消费该 SHA/tree/artifact hash 的权威 `release-evidence.json`，联合核对 Full run/attempt 和真实 6A、6B、6C 回执，缺失/不符即阻塞。届时旧 master push Full 不再单独授权发布。配套文件未落地前不建立悬空链接或宣称已上线。
