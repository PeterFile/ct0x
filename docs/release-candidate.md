# 发布候选操作手册

每次实际发布前，用 `finalize` 消费权威 `release-evidence.json`，核对明确选定的 SHA 和待发布文件的实际字节，再由用户逐次批准发布。普通 master Full 和 raw intake 不能替代此门禁。

本草稿未登记真实机器签名者，生产联合 E2E 仍待验收。已完成的 [跨机构建试验 45858696170](https://github.com/PeterFile/ct0x/actions/runs/45858696170) 与 [字节负例试验 45868496329](https://github.com/PeterFile/ct0x/actions/runs/45868496329) 各自仅证明其试验范围。

## 1. 选择入口并固定目标

在 **Release candidate**（`.github/workflows/release-candidate.yml`）选择 `master`，向 `sha` 提交完整 40 位 commit SHA。目标归一化为小写，并证明可从当前远端 master 到达。

标签入口为新建单段 `rc/*`，支持合法 Unicode、`+`、lightweight 和 annotated 标签。流程核对创建事件、原始 ref/object、剥离结果和提交，拒绝更新、强推及删除。

历史目标缺少候选 workflow 时，由唯一观察器依据权威本地 SQLite 的基线、ref 快照和已投递 receipt 交给 `release-tag-forward.py`，再从 master 转发固定 SHA。创建依据取自本地持久记录，公共 Events 不作凭据。消费者使用现有 `--repo`、`--observer-db`、`--coordinator`、`--receipt`、`--state`，观察器库只读，转发状态独立持久保存。

## 2. 核对策略、owner 和 attempt

`sha` / `tree` 固定产品代码。`workflow_sha` / `policy_sha` 固定可信控制策略，入口先认证获取 master，再证明调用提交的祖先关系并精确 detach。`Release Full` 向 reusable `ci-full.yml` 传入调用者策略 SHA 和实际产品身份，核对实际调用 source identity。

`Release admission <sha>` 按目标 SHA 串行认领，只有 claim 拥有 `statuses: write`。canonical `release-candidate/first-owner-v1` status 以 `success` 记录登记完成，描述为“首个 owner 已登记；尚未验证候选”，绑定经认证的首 owner job ID、时间及 run URL。候选结论由最终 verdict 另行产生。

每个 SHA 正常只执行一轮 canonical Full 与真实机器验收。`Release duplicate verdict` 独立核验原 owner 的原生成功及权威证据，验证通过才重定向；等待、失败或证据过期均拒绝引用，不启动第二轮 Full。

结清未知执行后，真正重试在同一 owner 选择原生 **Re-run all jobs**，生成新 attempt。局部重跑、旧 job 重新标注 attempt、跨次拼接成功均拒绝；核对实际开始时间及证据生成窗口。

## 3. 按两个完成信号交接

| 到达的信号 | 协调器动作与门禁 |
| --- | --- |
| `Release candidate binary` 原生成功完成 | 唯一观察器交给授权本机执行 `release-machine.py run` |
| 本地原生完成，同次 `Release Full evidence` 成功 | 协调器执行 `submit`，强制重审原始证据、二进制并联合 Full |
| `Release receipt intake` 成功 | `Release machine evidence` 收集联合回执；`Release candidate evidence` 暂存，`Release candidate verdict` 独立裁决 |

Full 与机器工作并行。唯一观察器分别交付 binary 和外层 Full evidence 信号，无第二个 watcher。任一 lane 失败时，另一条已运行的 lane 继续收集终态。

`Release Full` 保留原有 81 个逻辑槽和 `fail-fast: false`。`Release clients` 覆盖普通 `cmd/workspace`、Web 测试/typecheck/build、Pi bridge 测试/typecheck，Full 保留 vet、race 和 benchmark。

## 4. 在授权机器执行原始验收

准备已授权的 Darwin x86_64 机器、AGY、匹配目标的 Go 和原始 HDD harness。注册表固定在操作系统用户的应用数据锚点；scratch 使用已有的非符号链接 0700 HDD 目录，所有候选共用机器锁。私有 manifest 固定真实原始 harness 路径、源码摘要及 HDD 卷身份。

从 `workflow_sha` 对应的可信控制检出执行。先确认 `--help` 已列出 `run/submit/finalize`、`--expected-sha` 和 `--artifact`；缺项时停止并部署匹配版本。实际运营者获准后执行一次 `bind`，本地权威路径无需新增全局签名凭据。

`repo/run_id/attempt` 来自观察器认证的事件，`sha` 是明确选定的完整小写 SHA，`schema_version` 来自该目标迁移回执。运营者预先配置私有 `host_manifest`、`harness_root`、`work_dir`、`go_path`、`agy_path`、`publish_artifact`；禁止事件指定这些路径或程序。`run --execute-native` 需本次真实模型调用授权。

```bash
python3 -B .github/scripts/release-machine.py --help
python3 -B .github/scripts/release-host.py bind --manifest "$host_manifest"
python3 -B .github/scripts/release-machine.py run \
  --repo "$repo" --run-id "$run_id" --attempt "$attempt" \
  --harness-root "$harness_root" --work-dir "$work_dir" \
  --go "$go_path" --agy "$agy_path" --schema-version "$schema_version" --execute-native

# Wait for native completion and same-attempt Release Full evidence success.
python3 -B .github/scripts/release-machine.py submit \
  --repo "$repo" --run-id "$run_id" --attempt "$attempt" \
  --work-dir "$work_dir" --harness-root "$harness_root" --go "$go_path"
```

唯一 target 二进制按 `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 -trimpath -buildvcs=true` 构建，下载后重哈希，核对 Go revision、`vcs.modified=false` 和准确目标 tree。原始 6A/6C 并行，停止后 6B 独占；取消未经过 unknown 时实际执行同提交四项 unknown race 回归。原始协议、数据库、transcript、日志和配置留在私有 HDD。

## 5. 区分暂存和权威结果，再核对待发布文件

**Release machine receipt** 从 master 接收联合回执及独立 `machine_signature`，原生标题为 `RC receipt <run_id> attempt <run_attempt>`。owner 身份认证保障传输；远端机器证明须真实验签，本地路径须独立重审原始证据。

成功的 **Release candidate evidence** 只输出 `release-materials-<run_id>-<attempt>`，ZIP 根含 `receipt.json` 和 `decision.json`；材料为 `collected`、`verified=false`、`authoritative=false`。`release-host-proof.py assess` 绑定不可变 `workflow_sha`、同次 Full、完整机器回执及材料摘要。

远端权威路径须先经独立审阅在不可变 `.github/release-machine.pub` 固定获准公钥，匹配密钥已加载在授权机器 agent。`submit` 在强制 Full 联合及私有重审后签署完整绑定数据，使用 SSHSIG namespace `agent-workspace-release-machine-v1`。远端真实验签通过才允许 **Release candidate verdict** 的 **Enforce remote machine proof** 成功，生成权威 `release-evidence-<sha>-<run>-<attempt>/evidence.json`（`verified=true`、`authoritative=true`）。

未登记远端锚点时，staging 可成功，verdict 拒绝。同一授权 HDD 工作站仅在精确 `no_host_anchor` 路径可独立出具本地权威 `release-evidence.json`：可信控制树确认公钥确实缺失，成功 staging 的 `decision.json` 为 `no_host_anchor`；候选 run 已完成为 failure；同 run/attempt 的 normalize、admission、Full、clients、binary、Full evidence、machine、staging 全部原生成功；唯一失败步骤为 verdict 的 **Enforce remote machine proof**。

本地再次重审原始 6A/B/C、Full、来源摘要、实际选定 SHA/tree 及制品字节；发布文件须与原始验收文件及同次 `release-target-<run_id>-<attempt>` 绑定一致。

已登记锚点下缺失或无效签名、raw/hash 不符、其他作业失败/缺失/取消、部分 attempt 拼接，两条正式路径均拒绝。

在每次发布审批前执行，`publish_artifact` 必须是实际准备发布的文件：

```bash
python3 -B .github/scripts/release-machine.py finalize \
  --repo "$repo" --run-id "$run_id" --attempt "$attempt" \
  --expected-sha "$sha" --artifact "$publish_artifact" \
  --work-dir "$work_dir" --harness-root "$harness_root" --go "$go_path"
```

`finalize` 消费并复核权威证据，保存本地 `release-evidence.json`，仅完成证据保全；`publishable=false`，实际发布每次仍需用户明确批准。远端成功路径独立核对已完成 owner 与权威 artifact，本地缺锚路径保留严格 staging 判定。

POST 未知、机器中断或取消时保留注册表、锁、scratch 和原认领，只读对账，结清真实终态及资源后再恢复；禁止删状态重放。回滚时停止新候选与观察器交接，经审查恢复策略并保留证据；发布/部署回滚分别审批。新候选 artifact 保留 90 天，提前归档原始私有审计及 Full 证明。

## 6. 运行离线检查（Python、Git、actionlint 在 PATH）

```bash
for test in .github/scripts/test-release-*.py .github/scripts/release-test-*.py
do
  python3 -B "$test" -v || exit 1
done
python3 -B .github/scripts/test-ci-scheduling.py -v
python3 -B .github/scripts/test-store-shard.py -v
actionlint .github/workflows/release-candidate.yml \
  .github/workflows/release-receipt.yml .github/workflows/ci-full.yml
```
