#!/usr/bin/env python3
"""Run the hosted Full A/B/C trial without changing production scheduling."""
import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
from threading import Lock
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

SCRIPTS = Path(__file__).resolve().parent
ROOT = SCRIPTS.parents[1]
_spec = importlib.util.spec_from_file_location("trial_plan", SCRIPTS / "full-trial-plan.py")
planner = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(planner)
shards, verifier = planner.shards, planner.verifier
events = planner.load("full-trial-events")
receipts = planner.load("full-trial-receipts")
workflow = planner.load("full-trial-workflow")
check = verifier.check
prefix, collect, aggregate_canonical = receipts.prefix, receipts.collect, receipts.aggregate_canonical
TIMING_LOCK = Lock()
REQUIRED = {"preflight", "go-shards", "go-workspace", "go-other", "go-vet", "go-benchmarks"}


def check_results(results):
    planner.load("ci-gate").check_results(results, REQUIRED)


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True, text=True).stdout.strip()


def integer(value):
    check(isinstance(value, str) and re.fullmatch(r"[1-9][0-9]*", value), "试验整数输入非法")
    return int(value)


def identity(history=False):
    scheme, repeat, parallel = os.environ["TRIAL_SCHEME"], integer(os.environ["TRIAL_REPEAT"]), integer(os.environ["TRIAL_PARALLEL"])
    planner.parameters(scheme, repeat, parallel)
    check(os.environ["TRIAL_SHA"] == planner.PRODUCT_SHA, "仅接受已冻结的 master 产品 SHA")
    shards.check_identity(planner.PRODUCT_SHA, planner.PRODUCT_TREE)
    workflow.check_legacy(Path.cwd())
    for path, expected in workflow.LEGACY.items():
        # The controller's ci.yml is the disposable dispatch wrapper, owned by Codex.
        if path != ".github/workflows/ci.yml":
            check(hashlib.sha256((ROOT / path).read_bytes()).hexdigest() == expected, "控制器改动了生产调度源码：" + path)
    controller_sha = git(ROOT, "rev-parse", "HEAD")
    check(controller_sha == os.environ["GITHUB_SHA"], "控制器 checkout 与 workflow 提交不符")
    context = {"run_id": os.environ["GITHUB_RUN_ID"], "attempt": os.environ["GITHUB_RUN_ATTEMPT"],
               "controller_sha": controller_sha, "controller_tree": git(ROOT, "rev-parse", "HEAD^{tree}"),
               "checkout_sha": planner.PRODUCT_SHA, "checkout_tree": planner.PRODUCT_TREE,
               "scheme": scheme, "repeat": repeat, "shard_parallel": parallel}
    for field in ("run_id", "attempt"):
        check(re.fullmatch(r"[1-9][0-9]{0,63}", context[field]), "run/attempt 身份非法")
    if history:
        git(Path.cwd(), "merge-base", "--is-ancestor", planner.PRODUCT_SHA, "refs/remotes/origin/master")
    return context


def append_timing(path, context, profile, slot, invocation):
    records = events.compact(path, context, profile, slot, invocation)
    with TIMING_LOCK, (Path(os.environ["RUNNER_TEMP"]) / "trial-timing.jsonl").open("a", encoding="utf-8") as output:
        for record in records:
            output.write(json.dumps(record, sort_keys=True, ensure_ascii=False) + "\n")


@contextmanager
def observe_shards(context):
    original = shards.log_events

    def log(path, stream):
        original(path, stream)
        profile = context["profile"]
        slot = int(path.parent.name.removeprefix("slot-"))
        append_timing(path, context, profile, slot, f"{profile}-{slot}-{path.stem}")

    shards.log_events = log
    try:
        yield
    finally:
        shards.log_events = original


def go_test_args(race):
    return ["go", "test", "-count=1"] + (["-race"] if race else [])


def inventory(profile, temporary, race=True):
    target = "./cmd/workspace" if profile == "workspace" else "./internal/" + profile
    package = planner.WORKSPACE_PACKAGE if profile == "workspace" else verifier.PROFILES[profile][1]
    actual = shards.command(["go", "list", target], capture_output=True, text=True).stdout.strip()
    check(actual == package, "实际 Go 包与固定配置不符")
    path = Path(temporary) / f"{profile}-{'race' if race else 'ordinary'}-list.json"
    with path.open("wb") as output:
        shards.command(go_test_args(race) + ["-json", "-list", "^(Test|Example|Fuzz)", target],
                       stdout=output, env={**os.environ, "TMPDIR": str(temporary)})
    return verifier.inventory(path, package)


def workspace_run(selected, context, temporary, race, part=0, monolith=False):
    check(shutil.which("node"), "workspace 需要 Node；拒绝新增环境条件 skip")
    path = Path(temporary) / f"workspace-{'race' if race else 'ordinary'}-events.json"
    args = go_test_args(race)
    if race:
        args += ["-timeout=20m"]
    args += ["-json"]
    if not monolith:
        args += ["-run", verifier.run_pattern(selected)]
    args += ["./cmd/workspace"]
    try:
        with path.open("wb") as output:
            shards.command(args, stdout=output, env={**os.environ, "TMPDIR": str(temporary)})
    finally:
        if path.exists():
            shards.log_events(path, sys.stdout)
            append_timing(path, context, "workspace", part, "race" if race else "ordinary-monolith")
    return events.workspace(path, selected)


def preflight(context):
    temp = Path(os.environ["RUNNER_TEMP"])
    with tempfile.TemporaryDirectory(prefix="trial-preflight-") as temporary:
        inventories = {profile: inventory(profile, temporary) for profile in (*verifier.PROFILES, "workspace")}
        # Build tags may add or remove tests in either mode; never derive one
        # inventory from the other or compare ordinary outcomes with race runs.
        ordinary = inventory("workspace", temporary, race=False)
        monolith = workspace_run(ordinary, {**context, "job": "preflight", "job_number": 0},
                                 temporary, False, monolith=True)
    plan = {"schema": 2, "identity": context, "base": git(Path.cwd(), "show", "-s", "--format=%P", "HEAD").split()[0],
            "master_oid": git(Path.cwd(), "rev-parse", "refs/remotes/origin/master"), "inventories": inventories,
            "workspace_ordinary": {"compiled": ordinary, "proof": monolith},
            "schedule": planner.schedule(context["scheme"], context["shard_parallel"], inventories)}
    receipts.validate_plan(plan, context)
    directory = temp / "trial-plan"
    directory.mkdir()
    (directory / "plan.json").write_text(json.dumps(plan, sort_keys=True) + "\n")
    (temp / "trial-client.json").write_text('{"mode":"full"}\n')
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for name, value in (("matrix", {"include": [{"job_number": i} for i in range(len(plan["schedule"]["shards"]))]}),
                            ("workspace", {"include": [{"job_number": i} for i in plan["schedule"]["workspace"]]}),
                            ("parallel", context["shard_parallel"]), ("tree", context["checkout_tree"]), ("base", plan["base"])):
            output.write(f"{name}={json.dumps(value) if isinstance(value, dict) else value}\n")


def read_plan(context):
    path = Path(os.environ["RUNNER_TEMP"]) / "trial-plan/plan.json"
    check(path.is_file() and not path.is_symlink(), "预检计划缺失或为符号链接")
    plan = verifier.decode(path.read_text())
    receipts.validate_plan(plan, context)
    return plan


def run_unit(unit, context):
    profile, worker, part = unit["profile"], unit["worker"], unit["part"]
    slots = planner.unit_slots(context["scheme"], unit)
    package = verifier.PROFILES[profile][1]
    expected = shards.identity(context["run_id"], context["checkout_sha"], context["checkout_tree"], package)
    with tempfile.TemporaryDirectory(prefix="trial-unit-") as temporary, observe_shards({**context, "profile": profile}):
        if part is None and slots == shards.BATCHES[profile][worker]:
            directory = Path(temporary) / "canonical"
            shards.run_worker(profile, worker, directory, context["run_id"], context["checkout_sha"], context["checkout_tree"])
            _, _, name, prefix_name = verifier.PROFILES[profile]
            return [verifier.decode((directory / f"{prefix_name}{slot}" / name).read_text()) for slot in slots]
        compiled = inventory(profile, temporary)
        if part is None:
            # Keep the original whole group's slow/two-way rule even after removing
            # slow slots; no two canonical slow slots ever run concurrently here.
            if shards.worker_parallelism(profile, shards.BATCHES[profile][worker]) == 2:
                with ThreadPoolExecutor(max_workers=2) as executor:
                    futures = [executor.submit(shards.run_slot, profile, slot, compiled, package, expected, temporary) for slot in slots]
                    return [future.result() for future in futures]
            return [shards.run_slot(profile, slot, compiled, package, expected, temporary) for slot in slots]
        slot = worker
        selected, _ = verifier.selection(compiled, slot, profile)
        subset = shards.execution_subsets(selected, profile, slot)[part]
        private = Path(temporary) / f"slot-{slot}"
        private.mkdir()
        path = private / f"run-{part}.json"
        try:
            with path.open("wb") as output:
                shards.command(["go", "test", "-count=1", "-race", "-json", "-run", verifier.run_pattern(subset), "./internal/store"],
                               stdout=output, env={**os.environ, "TMPDIR": str(private)})
        finally:
            if path.exists():
                shards.log_events(path, sys.stdout)
        runs, passes = verifier.verify(path, package, subset)
        return [{"schema": 1, "slot": slot, "total": 64, **expected, "compiled": compiled,
                 "selected": subset, "runs": runs, "passes": passes, "skips": {}}]


def write_receipt(receipt):
    directory = Path(os.environ["RUNNER_TEMP"]) / "trial-receipt"
    directory.mkdir()
    (directory / "receipt.json").write_text(json.dumps(receipt, sort_keys=True) + "\n")


def worker(plan, context, kind):
    number = integer(os.environ["TRIAL_JOB_NUMBER"]) if os.environ["TRIAL_JOB_NUMBER"] != "0" else 0
    key = "shards" if kind == "shard" else "workspace"
    check(number in range(len(plan["schedule"][key])), "矩阵作业编号非法")
    job = "go-shards" if kind == "shard" else "go-workspace"
    check(os.environ["GITHUB_JOB"] == job, "实际 execution session 的 job 身份不符")
    context_with_job = {**context, "job": job, "job_number": number}
    receipt = {"schema": 1, "identity": context, "job": job, "job_number": number}
    if kind == "shard":
        receipt["units"] = [{"unit": unit, "summaries": run_unit(unit, context_with_job)}
                            for unit in plan["schedule"]["shards"][number]]
    else:
        with tempfile.TemporaryDirectory(prefix="trial-workspace-") as temporary:
            compiled = inventory("workspace", temporary, race=True)
            check(compiled == plan["inventories"]["workspace"], "workspace 实际 compiled 与预检不符")
            selected = planner.workspace_selection(compiled, number, len(plan["schedule"]["workspace"]))
            proof = workspace_run(selected, context_with_job, temporary, True, number, context["scheme"] != "c")
        receipt["workspace"] = {"compiled": compiled, "proof": proof}
    # The success receipt exists only after every real invocation has passed.
    write_receipt(receipt)


def public_job_numbers(context, plan):
    """Read public metadata with optional aggregate-scoped actions:read auth."""
    repository = os.environ["GITHUB_REPOSITORY"]
    check(repository == "PeterFile/ct0x", "公开回执仓库身份不符")
    url = f"https://api.github.com/repos/{repository}/actions/runs/{context['run_id']}/attempts/{context['attempt']}/jobs?per_page=100"
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "full-scheduling-trial"}
    token = os.environ.get("GH_TOKEN", "").strip()
    if token:
        headers["Authorization"] = "Bearer " + token
    expected = ["Trial preflight", "Go ordinary and remaining race", "Go vet and CI gates", "Go benchmarks", "Trial final receipt"]
    expected += [f"Trial shard {i}" for i in range(len(plan["schedule"]["shards"]))]
    expected += [f"Trial workspace {i}" for i in plan["schedule"]["workspace"]]
    fields = {"id", "name", "run_id", "head_sha", "run_attempt", "status", "conclusion", "started_at", "completed_at"}
    for attempt in range(2):
        try:
            with urlopen(Request(url, headers=headers), timeout=20) as response:
                data = verifier.decode(response.read().decode())
        except (HTTPError, URLError) as error:
            if isinstance(error, HTTPError):
                error.close()
            if attempt or isinstance(error, HTTPError) and error.code < 500:
                detail = f"HTTP {error.code}" if isinstance(error, HTTPError) else "网络错误"
                raise ValueError("读取公开作业编号失败：" + detail) from None
            time.sleep(1)
            continue
        check(isinstance(data, dict) and type(data.get("total_count")) is int and isinstance(data.get("jobs"), list),
              "公开作业编号列表非法")
        jobs = data["jobs"]
        check(data["total_count"] == len(jobs) and len(jobs) <= 100, "公开作业编号列表不完整")
        check(all(isinstance(row, dict) and fields <= row.keys() and isinstance(row["name"], str) and
                  type(row["id"]) is int and row["id"] > 0 and
                  all(row[key] is None or isinstance(row[key], str) for key in ("started_at", "completed_at"))
                  for row in jobs), "公开作业编号字段非法")
        check(len({row["id"] for row in jobs}) == len(jobs), "公开作业编号重复")
        result, pending = {}, False
        for row in jobs:
            matches = [name for name in expected if row["name"] == name or row["name"].endswith(" / " + name)]
            check(len(matches) == 1, "公开作业集合包含非预期作业")
            name = matches[0]
            check(name not in result, "公开作业编号重复：" + name)
            check(str(row["run_id"]) == context["run_id"] and row["head_sha"] == context["controller_sha"] and
                  str(row["run_attempt"]) == context["attempt"], "公开作业编号与本次 run/attempt/控制器不符")
            status = ("in_progress", None) if name == "Trial final receipt" else ("completed", "success")
            if (row["status"], row["conclusion"]) != status:
                check(row["status"] in ("queued", "in_progress", "waiting", "requested", "pending") and
                      row["conclusion"] is None, "公开作业状态不是当前完整成功执行：" + name)
                pending = True
            result[name] = {key: row[key] for key in ("id", "name", "status", "conclusion", "started_at", "completed_at")}
        if len(result) == len(expected) and not pending:
            return [result[name] for name in expected]
        check(not attempt, "公开作业状态未核实：预期作业尚未可见或仍为非终态，请等待 REST 同步后重试")
        time.sleep(10)


def aggregate(plan, context):
    temp = Path(os.environ["RUNNER_TEMP"])
    canonical, workspace = collect(plan, temp / "trial-shards", temp / "trial-workspace", context)
    result = aggregate_canonical(plan, canonical)
    jobs = public_job_numbers(context, plan)
    final = {"schema": 1, "verified": True, **context, "master_oid": plan["master_oid"],
             "created_at": datetime.now(timezone.utc).isoformat(), "jobs": jobs,
             "coverage": {profile: {"slots": row["total"], "top_level": len(row["compiled"]),
                                    "inventory_sha256": hashlib.sha256(planner.key(row["compiled"]).encode()).hexdigest(),
                                    "runs": sum(row["runs"].values()), "passes": sum(row["passes"].values())} for profile, row in result.items()},
             "workspace": workspace, "workspace_ordinary": plan["workspace_ordinary"]["proof"], "schedule": plan["schedule"],
             "weights_sha256": hashlib.sha256((SCRIPTS / "full-trial-weights.json").read_bytes()).hexdigest()}
    (temp / "trial-final.json").write_text(json.dumps(final, sort_keys=True, ensure_ascii=False) + "\n")
    artifact = prefix(context) + "-final"
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"verified=true\nreceipt={artifact}\n")
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
        summary.write(f"## Full 调度试验 {context['scheme'].upper()} / 重复 {context['repeat']}\n\n"
                      "已验证 81 个逻辑槽及全部预检/侧车；workspace 普通单体与 race 实际执行分别精确覆盖各自 compiled inventory。\n\n"
                      "`workspace_ordinary` 保留普通单体证明；`workspace` 保留 race 证明（A/B 单体，C 不重叠子集并集）。\n\n"
                      "C 与跨运行实际 A race 单体的同模式结果等价性由外部试验校验器核实，不以普通单体或清单推断。\n\n"
                      f"产品 `{context['checkout_sha']}` / 树 `{context['checkout_tree']}`\n\n"
                      f"控制器 `{context['controller_sha']}` / 树 `{context['controller_tree']}`\n\n"
                      f"run/attempt `{context['run_id']}/{context['attempt']}`；唯一公开 artifact：`{artifact}`。\n\n"
                      "时间以 run 创建至本聚合/运行终态为准；本次不调整生产调度或 p95 预算。\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("identity", "preflight", "shard", "workspace", "results", "aggregate"))
    args = parser.parse_args()
    context = identity(history=args.command == "preflight")
    if args.command == "identity":
        print(json.dumps(context, sort_keys=True))
    elif args.command == "preflight":
        preflight(context)
    elif args.command == "results":
        check_results(verifier.decode(os.environ["NEEDS_JSON"]))
    else:
        plan = read_plan(context)
        if args.command == "aggregate":
            aggregate(plan, context)
        else:
            worker(plan, context, args.command)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, IndexError, TypeError, OSError, UnicodeError, RecursionError, subprocess.CalledProcessError) as error:
        print(f"Full 调度试验失败：{error}", file=sys.stderr)
        sys.exit(1)
