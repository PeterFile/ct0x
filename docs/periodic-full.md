# 周期 Full CI

合并后的完整检查由 `.github/workflows/periodic-full.yml` 执行，默认每六小时一次（UTC `17 */6 * * *`）。普通 `ci.yml` 默认不接收 master push；PR 的 `ci:full`、原有预检和 `ci-full.yml` 不变。本文描述新的周期策略；旧的 `ci-and-merge.md` 中“每次 master push 全量检查”的说明不再适用。

## 调整周期、跳过和回滚

- 修改 `periodic-full.yml` 的 cron 行即可调整周期。GitHub schedule 可能延迟，不保证准点执行。
- 仓库变量 `PERIODIC_FULL_ENABLED=false` 可暂停自动周期；未设置时默认启用。手动 dispatch 不受此变量影响。
- 同 SHA 已有可信成功 Full 证明时跳过。手动 dispatch 的 `force=true` 忽略去重，重新执行 Full。
- 恢复每次 master push 全量检查：把 `ci.yml` 中的 `# push: {branches: [master]}` 取消注释即可。内部 push 路由、预检和外层 `Full CI` 门禁仍保留。需要停止周期运行时，再暂停周期策略。

工作流固定使用 `main-master-periodic` 并发组，`cancel-in-progress: false`。运行中的任务不会被新事件取消；Actions 保留一个 pending，新 pending 替换旧 pending。

## 固定实际被测提交

排队结束、运行获准启动后，bootstrap 使用只读认证 API **读取一次** `refs/heads/master`。事件的 `github.sha` 不决定被测目标。后续 checkout 固定这个 SHA，并核对 API commit 的 tree、head 和第一父提交 base。

控制脚本从这个可信 master checkout 加载；预检在独立 target checkout 执行普通 cmd、Web test/typecheck/build、Pi bridge test/typecheck，然后调用未修改的 `ci-full.yml`。静态 reusable workflow 跟随触发源，因此启动前还核对事件提交与可信 master 的 `ci-full.yml` blob 一致。手动分支测试也测试最新解析的 master；分支结果不能成为后续去重或确认 PR 的可信来源。初次部署需先让这些控制脚本进入 master。

API 权限仅 `contents/actions/pull-requests: read`，所有请求为 `gh api --method GET`。不调用 `gh run`，不改设置、不部署。取证作业不执行历史测试；回放在另一台 runner 上进行，不注入 API token，checkout 不保留凭据。回放子进程只继承白名单环境，使用临时 HOME、固定 module/package/test 和 argv；不执行 shell 拼接。客户端和普通 cmd 预检也清空继承环境。

## 只复用可核验的 Full 证明

查找不会按 `head_sha` 过滤 schedule 或 release 候选运行，因为触发 SHA 可以不同于被测 SHA。所有来源都核对同仓库、非 fork、master 祖先、工作流路径/事件、精确 run ID 和 attempt、同 attempt 成功的 Full 聚合作业，以及 API commit 的 SHA/tree/head/base。

支持以下来源：

- 旧 `.github/workflows/ci.yml` master push：成功的 `CI preflight and feedback`、`Full suite evidence` 和精确命名的 `Full CI` 作业。只接受后者该 attempt 日志中唯一的 `ci-gate.py outer` 规范 JSON；兼容带 Actions 时间戳的原始行。
- master 控制的 `.github/workflows/periodic-full.yml` schedule/dispatch/fallback push：成功的 `Periodic Full evidence`、预检和 Full 聚合；规范日志必须与 attempt 限定 artifact 的 `receipt.json` 严格同类型、同值。
- `.github/workflows/release-candidate.yml`：成功的 `Release Full evidence` 和 Full 聚合，`release-full-<run_id>-<run_attempt>` 根目录的成功、verified `release-full` receipt。核对 schema、repo、整数 run/attempt、SHA/tree、产物摘要格式及 API artifact 的来源和 ZIP digest。后续硬件验收失败不撤销已经成功的 Full。dispatch 必须来自 master，目标必须是 master 祖先。

`rc/*` push 额外要求 API tag 精确指向运行的 master 祖先，以及 receipt 的 `workflow_ref`/`workflow_sha` 与 API `referenced_workflows` 中固定的 master 控制源一致。缺少这种兼容来源证明就重新运行，不从 tag 名或 artifact 自述推断可信性。`release-full.artifact_sha256` 是生产者的产物摘要，不是包含自身 receipt 的 ZIP 自摘要。

每来源最多查看两页运行、每 run 最近三个 attempt，总共最多验证 40 个历史候选；取证进程最多 240 次不同 API 请求。过期 artifact、缺日志、未知格式、分页/查询上限或不可核验来源都视为 unknown；找不到可用证明便运行 Full。目标身份本身无法核验时则启动失败，不能用未知身份出具成功证明。

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

失败的 `attribution.status` 初始为 `unknown`；只有复现、首亲转换和精确合并 PR 都核验后才为 `confirmed`，此时才出现正整数 `pr`。`ancestor` 保存最近可取得可信成功证据的首亲绿点；`failure_jobs`、`first_blocking_event`、`diagnostic_status` 和 `failure_candidates` 保留原始作业 ID/名称/URL、首个失败步骤、日志摘要和签名。API 原始响应及日志在 `api-*` 下，`index.jsonl` 将 GET endpoint 绑定到文件和 SHA-256；不保存认证头或 token。

定位前先上传 `periodic-full-initial-<run_id>-<run_attempt>` 并发布失败作业链接和首阻塞事件。之后无凭据回放上传 `periodic-full-localization-<run_id>-<run_attempt>`，包括 `localization.json`、每次编译清单/执行的原始 Go JSON、argv、状态、checkout 和日志摘要。最终 artifact 合并这些证据，不覆盖初始 artifact，也不覆盖旧 attempt。证据保留 90 天；到期不能继续依赖，需重新验证或由维护者另行归档。Full 或预检失败仍使整个运行失败，归因作业不会把它改绿。

## 保守定位，不从提交标题猜 PR

只对已知 Go 测试作业/步骤中、已识别 package 的精确命名失败定位。支持 Go JSON 和 `--- FAIL: Test...` 加相应 `FAIL\t<package>`；作业 ID 和原始日志必须可取得。非 Go、基础设施失败、缺证据或多个签名直接保留范围和 unknown。

从最近可核验的首亲绿点到失败点，最多回放 32 个提交，每个提交重复三次；每个命令最多 90 秒，总回放预算 20 分钟，命名测试另有 60 秒 Go timeout。不是假设单调的二分搜索：整条首亲链都要筛查，局部相邻 good/bad、两端和失败签名都需一致。测试缺失、skip、泛化超时、工具链失败、不稳定结果或多个转换均不确认 PR。

“新测试首次失败”单独标为 `transition: new-test`，除了重复编译清单缺失/出现，还要求前一点源码无同名项、后一处有明确同名测试声明；仅 build tag 或工具链导致的清单变化不算新测试。最终取证 runner 重建 Git 首亲范围、源码身份、每次执行日志和签名，再通过认证 `commit/pulls` 核对唯一已合并 PR 的 `merge_commit_sha` 和 base master。没有精确唯一匹配就保持 unknown，绝不从 subject、作者或相似测试名称猜测。

## 本地验证

```bash
python3 .github/scripts/test-ci-scheduling.py -v
python3 .github/scripts/test-store-shard.py -v
python3 .github/scripts/test-periodic-full.py -v
```

新套件也接入 `test-ci-scheduling.py` 的 unittest loader，因此现有 CI 预检和 Full CI gates 作业都会执行它。测试使用 Python 标准库、假 API、本地临时 Git 对象，以及无外部依赖的真实 Go 失败夹具；真实夹具需要本地 Go 工具链。远端 Actions 调度、权限和实际生产 receipt 仍需部署后独立验证。
