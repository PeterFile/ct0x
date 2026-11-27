# 周期 Full CI

合并后的完整检查由 `.github/workflows/periodic-full.yml` 执行，默认每六小时一次（UTC `17 */6 * * *`）。本切片仅关闭 `ci.yml` 的 master push 触发，保留基线普通 PR 的 targeted Go/cmd/Web/bridge、仅文档输出及 `ci:full` 完整预检（含 draft、有效 refresh/base-edit）；`ci-full.yml` 不变。普通 PR 的后续默认策略由 #271 独立处理，本实现不依赖 Merge gate，也不修改仓库设置。

## 调整周期、跳过和回滚

- 修改 `periodic-full.yml` 的 cron 行即可调整周期。GitHub schedule 可能延迟，不保证准点执行。
- 仓库变量 `PERIODIC_FULL_ENABLED=false` 可暂停自动周期；未设置时默认启用。手动 dispatch 不受此变量影响。
- 同 SHA 已有可信成功 Full 证明时跳过，生产手动 dispatch 也不绕过去重；固定 SHA 故障演练使用独立实验入口。
- 恢复每次 master push 全量检查：把 `ci.yml` 中的 `# push: {branches: [master]}` 取消注释即可。内部 push 路由、预检和外层 `Full CI` 门禁仍保留。需要停止周期运行时，再暂停周期策略。

只有启用的 master schedule 和 master 手动 dispatch 共用 `main-master-periodic`，`cancel-in-progress: false`：一个 running、一个 pending，新 pending 替换旧 pending。非 master、无关事件和暂停的 schedule 使用 run/attempt 独立组，不启动 resolve，也不替换可信 pending。

## 固定实际被测提交

排队结束、运行获准启动后，bootstrap 使用只读认证 API **读取一次** `refs/heads/master`。事件的 `github.sha` 不决定被测目标。后续 checkout 固定这个 SHA，并核对 API commit 的 tree、head 和第一父提交 base。

控制脚本和预检共用固定 master checkout，执行普通 cmd、Web test/typecheck/build、Pi bridge test/typecheck，然后调用未修改的 `ci-full.yml`；不重复 checkout 同一份源码。预检保留 setup-go/npm 缓存但清空测试凭据；历史回放只用私有缓存。

静态 reusable workflow 跟随触发源。事件提交与最新 master 的 `ci-full.yml` blob 不同时立即失败，不执行旧 Full，也不出具成功证明；下一次可信、最新 master schedule/手动请求使用新触发源刷新。控制脚本必须先进入 master；普通分支手动请求不执行 Full。

API 权限仅 `contents/actions/pull-requests: read`，所有请求为 `gh api --method GET`。不调用 `gh run`，不改设置、不部署。取证作业不执行历史测试；回放在另一台 runner 上进行，不注入 API token，checkout 不保留凭据。回放子进程只继承白名单环境，使用临时 HOME、固定 module/package/test 和 argv；不执行 shell 拼接。客户端和普通 cmd 预检也清空继承环境。

## 只复用可核验的 Full 证明

查找不会按 `head_sha` 过滤 schedule 或 release 候选运行，因为触发 SHA 可以不同于被测 SHA。所有来源都核对同仓库、非 fork、master 祖先、工作流路径/事件、精确 run ID 和 attempt、同 attempt 成功的 Full 聚合作业，以及 API commit 的 SHA/tree/head/base。

支持以下来源：

- 旧 `.github/workflows/ci.yml` master push：成功的 `CI preflight and feedback`、`Full suite evidence` 和精确命名的 `Full CI` 作业。只接受后者当前 attempt 日志中唯一的 `ci-gate.py outer` 规范 JSON；兼容带 Actions 时间戳的原始行。部分重跑可继承原生成功预检，但必须认证其真实旧 attempt、同仓库/HEAD/控制源、原生时间窗、runner、完整步骤及逐字节相同的原始日志。`preflight_provenance` 分别保存 reported/actual job 与 attempt、控制 blob 和内容指纹；不按 job ID 推断来源，也不宣称全部作业重新执行。
- master 控制的 `.github/workflows/periodic-full.yml` schedule/dispatch/fallback push：成功的 `Periodic Full evidence`、预检和 Full 聚合；规范日志必须与 attempt 限定 artifact 的 `receipt.json` 严格同类型、同值。
- `.github/workflows/release-candidate.yml`：成功的 `Release Full evidence` 和 Full 聚合，`release-full-<run_id>-<run_attempt>` 根目录的成功、verified `release-full` receipt。核对 schema、repo、整数 run/attempt、SHA/tree、产物摘要格式及 API artifact 的来源和 ZIP digest。后续硬件验收失败不撤销已经成功的 Full。整个 release run 仍在等待机器时，只要当前 attempt 的 `Release Full evidence` 和实际 Full 聚合均已完成成功，且最终 artifact 可下载并通过全部身份/摘要检查，也可复用这份 Full；这不是机器验收通过或已经发布的证明。此时 `updated_at` 可能早于下游完成时间，故以读取认证作业后的本地 UTC 观察为上界，artifact 创建时间必须落在真实 producer 的原生执行窗口内；仍运行、失败或继承的 Full producer 不可使用。legacy/periodic 仍要求整体 run 完成。dispatch 必须来自 master，目标必须是 master 祖先。

`rc/*` push 还须原生同仓库、tag 逐层 peel 后等于 run.head_sha 和被测 SHA，并位于当前 master 历史。消费者对有效 Git tag 名进行 URL 编码，读取并校验该提交的顶层 release 工作流 blob、原生 producer/Full 调用图和实际被调用的 `ci-full.yml` 来源。顶层 release 工作流不需要出现在自己的 `referenced_workflows`，最小 receipt 不要求 `workflow_ref/workflow_sha/head/base` 或另一条打印日志。移动、游离、不可信 tag 或证据缺失均重新运行。`artifact_sha256` 是生产者的产物摘要，不是 ZIP 自摘要。

先给各来源目标证明机会，再按实际首亲链从近到远查找已验证绿点。旧 master push 支持精确 `head_sha` 查询，避免近期 PR 运行挤掉历史记录；周期和 release 不按触发 SHA 过滤。运行、作业、artifact 和关联 PR 均按 API 分页读取，不设“两页 / 三次 attempt / 40 候选 / 33 提交”截断。每进程仍限制 240 个不同 GET、实际取证时间 600 秒、单响应 32 MiB、磁盘缓存合计 512 MiB（不把全部原始响应常驻内存）。分页变化、过期 artifact、缺日志、未知格式、限流或真实预算耗尽均保留 unknown 原因；没有同 SHA 证明就运行 Full，已核验祖先仅用于回放，不能声称遗漏历史已执行。目标身份不明则启动失败。

去重还核对实际调用的 Full 工作流 SHA/blob、原生调用者和精确聚合作业名。Full 聚合及外层 producer 必须有当前 attempt 内的新鲜执行证明；带时间戳的规范成功日志也必须新鲜。jobs API 即使把旧作业标成当前 attempt 并重分配 ID，也不能把旧执行或无关同名叶子当成当前成功。仅旧 legacy 必需预检可按上述完整来源证明继承；来源缺失或不唯一仍为 unknown。

同 SHA 长期不变时，最新认证的周期 skip receipt 可提供一次非递归 `skip_proof` 定位提示，避免 100 个六小时 skip 耗尽查询预算。skip 自身绝不算 Full；消费者重新读取并验证所指真实 Full 的 repo/SHA/tree/run/attempt、原生 producer、source、artifact/digest。每次查找最多使用一个同目标认证提示，单个提示最多 24 个不同 GET，仍受全局真实时间/API 预算约束。跨仓库、目标错误、自引用、指向另一份 skip、不可验证或缺失的提示均为 unknown，正常证据查找可继续。

原始 job logs 先用普通 `gh api --method GET`。仅当本机 CLI 明确拒绝 terminal escape sequences 时重试一次 `--allow-escape-sequences`，只写有界文件、从不回显原始转义；旧 CLI 正常路径不带此旗标。其他 403 不重试、不扩大权限；历史去重和失败收集共用此路径。

## 读取不可变 receipt 和证据

观察者接口固定为 artifact **`periodic-full-<run_id>-<run_attempt>`**，ZIP 根成员 **`receipt.json`**。核心形状：

```json
{
  "schema": 1,
  "kind": "periodic-full",
  "repo": "owner/repo",
  "run_id": 123,
  "run_attempt": 1,
  "sha": "<40 lowercase hex>",
  "tree": "<40 lowercase hex>",
  "head": "<tested SHA>",
  "base": "<first parent SHA>",
  "status": "success",
  "verified": true,
  "components": { "resolve": "success", "preflight": "success", "full": "success" },
  "attribution": {
    "status": "unknown",
    "good_sha": null,
    "bad_sha": "<tested SHA>",
    "commit": null,
    "reason": "完整套件成功"
  }
}
```

`status` 仅为 `success/failure/skipped`。`verified=true` 只在预期 needs 全部成功、实际预检 checkout 和 Full 聚合身份完全一致时产生。`full` 保存被测身份和整数 run/attempt；`checkout` 保存实际预检身份。跳过时 `verified=false`，`skip_proof` 引用独立核验的成功来源。

失败使用 `attribution.items`：每项独立保存来源作业、精确测试/文件及签名摘要、good/bad SHA、转换 commit 和原始证据引用；仅该项完成复现及唯一精确合并 PR 核验后，才有 `status=confirmed` 和正整数 `pr`。未知项绝无 `pr`，不能借其他项的证明。只要有未分类阻塞，整体仍为 unknown，但已确认项仍可独立路由；全部项指向同一引入合并时才保留兼容的顶层 confirmed/pr，`coverage` 单独表示覆盖程度。

`ancestor` 保存可信首亲绿点。初始 `failure_candidates` 保留完整签名；`failure_jobs/first_blocking_event` 保存真实叶作业的完成时间、原生首失败步骤和日志摘要，有叶子时不让聚合/证据作业抢首阻塞；完成元数据不足则 `blocking_order_status=unknown`。`api-*/index.jsonl` 绑定原始 GET endpoint、文件和 SHA-256，不保存认证头或 token。

定位前上传 `periodic-full-initial-<run_id>-<run_attempt>`，发布失败作业链接和首阻塞。无凭据回放另存 `periodic-full-localization-<run_id>-<run_attempt>`，含 `localization.json`、原始执行/源码/工具链记录；认证复核的 API 原始响应另存 `periodic-full-verification-<run_id>-<run_attempt>`。

最终 canonical ZIP **只有根 receipt.json，最大 2 MiB**，通过 `evidence_artifacts` 及逐项 `evidence` 保存独立 artifact 名、run/attempt、路径和 SHA-256；不复制 API/历史源码/探测目录。初始证据和旧 attempt 不覆盖，均保留 90 天，到期需重新验证或维护者归档。归因不会把原始 Full/预检失败改绿。

## 保守定位，不从提交标题猜 PR

来源必须绑定认证原生 job/failed step 和该 SHA 的完整 workflow/helper recipe，不从日志里打印的命令或今天的显示名推断历史控制。支持独立命名 Go 失败、Web/bridge 原生断言，以及 `periodic-diagnostics.py` 的 Go 编译/vet 和两份 CI Python unittest 断言；共享 CI Python 步骤依据真实 traceback 选择脚本。依赖编译错误同时保留触发 job/原始包选择与实际失败 package，不要求编造 Go 测试名。同编译输入的 `-list` / `-run` 和重复分片合并为一项，包选择、race/build flags 或诊断身份不同则分开；所有原生 job、日志摘要及源码 recipe 均保留。无法证明一步内实际到达哪条命令时显式记录 `source-alternatives`，不把源码中的每条命令说成已执行。已完成的命名失败不会被其他包/作业的超时擦除；不支持或基础设施错误作为独立 unknown 保留。

`periodic-history.py` 已接入正式回放和最终原始日志复核。范围来自完整 Git 首亲链，不截断提交或候选数量；先验证原始 good/bad，再稀疏探测边界，并从末端逐提交筛查当前连续失败后缀，相邻 good/bad 和末端 bad 各确认三次。不以二分样本假设单调性：修复后重新引入只能归因于最近连续失败后缀的相邻边界，`scope=latest-continuous-failing-episode` 不代表首次出现或更早历史。`original_good_sha/original_bad_sha` 固定原始端点，`continuation.intervals` 记录未采样的零基闭区间，`next` 记录被预算或未知证据阻塞的下一次查询；原始逐次 probe 和中断不丢弃。最终复核在共享的真实时间预算内重新核验原始绿点的 Full run/attempt，并按同一确定性顺序消费全部已验证原始观察，拒绝遗漏后缀、额外观察、复用日志及伪造范围/覆盖摘要。`evidence_status=reconstructed` 才表示原始观察已重建；中断或尚未重建项标为 `unverified`，其续查提示不是可信结论。

回放总预算仍为 210 分钟，hosted replay job 上限仍为 240 分钟，均不改变 Full 预算。命名 Go 保留认证原始 race/ordinary 和 timeout literal（默认 10m，支持如 15m、12m30s，须正有限且不超过原始作业/6 小时）；编译准备另限 300 秒。Go 编译诊断不运行测试二进制，保留构建 flags/依赖包图，使用原始 job 预算，并单独记录原 test timeout。客户端原生命令限 300 秒，Web Node 22、bridge 24.13.0，Web `npm ci` 启用生命周期、bridge 保留 `--ignore-scripts --no-audit --no-fund`；仅私有 npm tarball 缓存可跨探测复用，HOME/node_modules/源码不复用。

达到真实时间/日志上限、测试缺失/skip、动态或不支持的原始模式、工具链失败、签名不同、重复观察不稳定均为 unknown，保留实际耗时、逐次探测和续查位置；未采样历史始终独立于已验证的当前后缀。没有完整相邻边界及连续失败后缀时不能指定 PR。

“新测试首次失败”单独标为 `transition: new-test`，除了重复编译清单缺失/出现，还要求范围内每个提交的源码证明：引入前一直无同名项，引入后均有明确同名测试声明；源码摘要保留原始字节。仅 build tag 或工具链导致的清单变化不算新测试。最终取证 runner 重建 Git 首亲范围、源码身份、每次执行日志和签名，再通过认证 `commit/pulls` 核对唯一已合并 PR 的 `merge_commit_sha` 和 base master。没有精确唯一匹配就保持 unknown，绝不从 subject、作者或相似测试名称猜测。

## 本地验证

```bash
python3 .github/scripts/test-ci-scheduling.py -v
python3 .github/scripts/test-store-shard.py -v
```

普通 CI 策略反馈及 Full 的现有 `test-ci-scheduling.py` 入口默认通过单层 `load_tests` 加载 full/history/clients/diagnostics 四个 periodic 模块一次；不在 opt-in 或周期预检重复运行。#271 只有在调用者已经逐项列出全部四个 periodic 套件时，才对那一个 scheduling 测试进程设置 `CI_TESTS_DISCOVERED=1`，跳过嵌套加载；未设置及任何其他值均保留默认行为。精确路径 allowlist 覆盖周期工作流及所有相关脚本，普通 PR 修改这些文件会触发控制测试，文档变更仍仅作差异检查。调试可直接运行 `test-periodic-full.py`、`test-periodic-history.py`、`test-periodic-clients.py` 或 `test-periodic-diagnostics.py`。使用默认系统 `TMPDIR`，不要把 Go 编译 scratch 放到 HDD checkout。测试仅用标准库、假 API、临时 Git 对象及真实本地 Go/Python/Node 进程；Go 夹具需要本地工具链，原生客户端测试可配置 `PERIODIC_WEB_NODE/NPM`、`PERIODIC_BRIDGE_NODE/NPM` 和 `PERIODIC_TEST_VITEST`。可选只读历史 archive 目录通过 `PERIODIC_ARCHIVE_DIR`、`PERIODIC_DIAGNOSTIC_ARCHIVES` 提供，不随源码提交。

历史日志解析、固定 SHA 真实进程复现、receipt 重建/认证和生产周期调度是四层不同证据；本地测试或受控故障演练不能宣称真实产品回归或生产 schedule 已验收。部署后的原生 Actions 权限、触发与生产 receipt 由协调者另行核验。
