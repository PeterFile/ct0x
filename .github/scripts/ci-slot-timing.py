#!/usr/bin/env python3
"""Bounded same-runner Store 44/45 ABBA probe, not a full-CI benchmark."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import json
import os
from pathlib import Path
import resource
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace


_spec = importlib.util.spec_from_file_location("store_shard_verifier", Path(__file__).with_name("verify-store-shard.py"))
verifier = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(verifier)
SLOTS = (44, 45)
# Reverse admission order in the second half as well as balancing modes.
ROUNDS = (("serial", SLOTS), ("parallel", SLOTS),
          ("parallel", SLOTS[::-1]), ("serial", SLOTS[::-1]))
BUDGET_SECONDS = 22 * 60
TARGET = "./internal/store"


def command(args, deadline, label, output=None, env=None):
    remaining = deadline - time.monotonic()
    verifier.check(remaining > 0, f"{label}：探测总时间预算已耗尽")
    with subprocess.Popen(args, stdout=output if output is not None else subprocess.PIPE,
                          stderr=subprocess.PIPE, start_new_session=True, env=env) as process:
        try:
            stdout, stderr = process.communicate(timeout=remaining)
        except BaseException:
            # Kill the whole Go/test process group before waiting or reporting failure.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()
            raise
        verifier.check(process.returncode == 0,
                       f"{label}：命令退出 {process.returncode}；{stderr[-8192:].decode('utf-8', errors='replace')}")
    return stdout.decode("utf-8").strip() if stdout is not None else None


def cpu_usage():
    parent = resource.getrusage(resource.RUSAGE_SELF)
    children = resource.getrusage(resource.RUSAGE_CHILDREN)
    return {"parent_cpu_seconds": parent.ru_utime + parent.ru_stime,
            "child_cpu_seconds": children.ru_utime + children.ru_stime}


def elapsed_cpu(before):
    usage = {key: value - before[key] for key, value in cpu_usage().items()}
    return {**usage, "cpu_seconds": sum(usage.values())}


def optional_text(path):
    try:
        return Path(path).read_text(encoding="utf-8").strip()
    except FileNotFoundError:
        return None


def runner_info(deadline):
    cpuinfo = optional_text("/proc/cpuinfo") or ""
    model = next((line.split(":", 1)[1].strip() for line in cpuinfo.splitlines()
                  if line.startswith("model name") and ":" in line), None)
    # Record both cgroup generations when exposed; do not tune CPU limits or Go flags.
    return {"os": os.environ.get("RUNNER_OS"), "arch": os.environ.get("RUNNER_ARCH"),
            "image_os": os.environ.get("ImageOS"), "image_version": os.environ.get("ImageVersion"),
            "nproc": int(command(["nproc"], deadline, "读取 CPU 数")),
            "logical_cpus": os.cpu_count(), "cpu_model": model,
            "cgroup_cpu_max": optional_text("/sys/fs/cgroup/cpu.max"),
            "cgroup_cpu_quota_us": optional_text("/sys/fs/cgroup/cpu/cpu.cfs_quota_us"),
            "cgroup_cpu_period_us": optional_text("/sys/fs/cgroup/cpu/cpu.cfs_period_us"),
            "go_environment": {key: os.environ.get(key) for key in ("GOMAXPROCS", "GORACE", "GOFLAGS")}}


def checkout_identity(deadline):
    sha = command(["git", "rev-parse", "HEAD"], deadline, "读取 checkout SHA")
    tree = command(["git", "rev-parse", "HEAD^{tree}"], deadline, "读取 checkout tree")
    verifier.check(sha == os.environ["GITHUB_SHA"], "实际 checkout SHA 与 GITHUB_SHA 不符")
    return sha, tree


def run_slot(slot, selected, pattern, package, directory, deadline, label):
    path = directory / f"slot-{slot}.jsonl"
    tmpdir = directory / f"slot-{slot}-tmp"
    tmpdir.mkdir(mode=0o700)
    environment = {**os.environ, "TMPDIR": str(tmpdir)}
    started = time.monotonic()
    with path.open("wb") as output:
        command(["go", "test", "-count=1", "-race", "-json", "-run", pattern, TARGET],
                deadline, f"{label} 槽 {slot}", output, env=environment)
    test_wall = time.monotonic() - started
    try:
        runs, passes = verifier.verify(path, package, selected)
    except (ValueError, OSError, UnicodeError, RecursionError) as error:
        raise ValueError(f"{label} 槽 {slot} 覆盖校验失败：{error}") from error
    return {"slot": slot, "test_wall_seconds": test_wall, "selected": sum(selected.values()),
            "runs": sum(runs.values()), "passes": sum(passes.values()), "skips": 0}


def run_round(index, mode, order, selections, package, directory, deadline):
    label = f"第 {index} 轮 {mode}"
    workers = 1 if mode == "serial" else 2
    directory.mkdir()
    before = cpu_usage()
    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(run_slot, slot, *selections[slot], package, directory, deadline, label)
                   for slot in order]
        # A failed command or any missing/duplicate/skipped test invalidates the entire probe.
        results = [future.result() for future in futures]
    wall = time.monotonic() - started
    usage = elapsed_cpu(before)
    print(f"{label}：{wall:.3f}s，CPU {usage['cpu_seconds']:.3f}s，全部覆盖校验通过", flush=True)
    return {"round": index, "mode": mode, "workers": workers, "slot_order": list(order),
            "wall_seconds": wall, **usage, "slots": results}


def comparison(rounds):
    means = {mode: {field: sum(row[field] for row in rounds if row["mode"] == mode) / 2
                    for field in ("wall_seconds", "cpu_seconds")}
             for mode in ("serial", "parallel")}
    serial, parallel = means["serial"]["wall_seconds"], means["parallel"]["wall_seconds"]
    verifier.check(serial > 0 and parallel > 0, "计时必须为正数")
    return {"serial_mean_wall_seconds": serial, "parallel_mean_wall_seconds": parallel,
            "serial_mean_cpu_seconds": means["serial"]["cpu_seconds"],
            "parallel_mean_cpu_seconds": means["parallel"]["cpu_seconds"],
            "parallel_to_serial_wall_ratio": parallel / serial,
            "runner_seconds_saved_per_pair": serial - parallel,
            "runner_time_saved_percent": 100 * (1 - parallel / serial),
            "parallel_observed_faster": parallel < serial}


def run_probe(output):
    started = time.monotonic()
    deadline = started + BUDGET_SECONDS
    verifier.check(not output.exists() and not output.is_symlink(), "指标输出已存在或为符号链接，拒绝覆盖")
    sha, tree = checkout_identity(deadline)
    package = command(["go", "list", TARGET], deadline, "解析 Store 包")
    verifier.check(package == verifier.PROFILES["store"][1], "实际 Store 包与固定分片配置不符")
    identity = verifier.identity(SimpleNamespace(run_id=os.environ["GITHUB_RUN_ID"], sha=sha, tree=tree, package=package))
    go_version = command(["go", "version"], deadline, "读取 Go 版本")
    runner = runner_info(deadline)
    with tempfile.TemporaryDirectory(prefix="ci-slot-timing-") as temporary:
        directory = Path(temporary)
        inventory_path = directory / "inventory.jsonl"
        before = cpu_usage()
        warm_started = time.monotonic()
        with inventory_path.open("wb") as listed:
            command(["go", "test", "-count=1", "-race", "-json", "-list", "^(Test|Example|Fuzz)", TARGET],
                    deadline, "一次性编译预热及 compiled-list", listed)
        compiled = verifier.inventory(inventory_path, package)
        warmup = {"wall_seconds": time.monotonic() - warm_started, **elapsed_cpu(before)}
        selections = {slot: verifier.selection(compiled, slot, "store") for slot in SLOTS}
        verifier.check(all(selected for selected, _ in selections.values()), "固定槽不能为空，拒绝无效对比")
        rounds = [run_round(index, mode, order, selections, package, directory / f"round-{index}", deadline)
                  for index, (mode, order) in enumerate(ROUNDS, 1)]
    verifier.check(checkout_identity(deadline) == (sha, tree), "探测期间 checkout 身份发生变化")
    verifier.check(command(["go", "version"], deadline, "复核 Go 版本") == go_version, "探测期间 Go 版本发生变化")
    result = {"schema": 1, "status": "complete", **identity, "go_version": go_version, "runner": runner,
              "slots": list(SLOTS), "order": "ABBA", "compiled_count": sum(compiled.values()),
              "measurement": "每轮包含两个 go test 调用及覆盖校验；不含一次性准备和编译预热",
              "scope": "仅此 runner 和两个槽的方向性样本，不能外推完整 CI 或统计显著性",
              "budget_seconds": BUDGET_SECONDS, "warmup": warmup, "rounds": rounds,
              "comparison": comparison(rounds), "probe_wall_seconds": time.monotonic() - started}
    verifier.check(result["probe_wall_seconds"] < BUDGET_SECONDS, "探测总时间预算已耗尽")
    # Raw inventories and test events are private temporary files, never artifacts.
    with output.open("x", encoding="utf-8") as destination:
        destination.write(json.dumps(result, ensure_ascii=False, sort_keys=True, indent=2) + "\n")
    summary = result["comparison"]
    print(f"有效 ABBA：串行均值 {summary['serial_mean_wall_seconds']:.3f}s，"
          f"并行均值 {summary['parallel_mean_wall_seconds']:.3f}s，"
          f"并行/串行 {summary['parallel_to_serial_wall_ratio']:.3f}；仅代表本 runner 和这两个槽", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    run_probe(args.output)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, UnicodeError, RecursionError, KeyError, subprocess.TimeoutExpired) as error:
        print(f"Store 双槽计时失败（不生成有效指标）：{error}", file=sys.stderr)
        sys.exit(1)
