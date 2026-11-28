#!/usr/bin/env python3
"""Batch fixed logical race shards without weakening their existing evidence contract."""
import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from threading import Lock
from types import SimpleNamespace


_spec = importlib.util.spec_from_file_location("store_shard_verifier", Path(__file__).with_name("verify-store-shard.py"))
verifier = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(verifier)
# Admit the historically long Platform slots first; isolate slots 1 and 2.
BATCHES = {
    "platform": ((4,), (8, 0), (1,), (3, 5), (6, 7), (2,)),
    "store": tuple(tuple(range(start, start + 4)) for start in range(0, 64, 4)),
    "httpapi": ((0, 1, 2, 3), (4, 5, 6, 7)),
}
PLATFORM_RUN_LIMIT = 24
STORE_SLOW_RUN_LIMIT = 5
# Pairing A/B: run 44712299768. Preserve isolation for #225 slots and #240 package times >=330s.
SLOW_STORE_SLOTS = {6, 8, 38, 50, 51, 57, 62}
# Refine observed package-alarm slots only after their original partitioning.
PACKAGE_ALARM_SLOTS = {("platform", 3), ("store", 21), ("store", 40), ("store", 55)}
LOG_LOCK = Lock()


def worker_parallelism(profile, slots):
    if profile != "store" or SLOW_STORE_SLOTS.intersection(slots) or sys.platform != "linux":
        return 1
    try:
        return 2 if len(os.sched_getaffinity(0)) >= 4 else 1
    except (AttributeError, NotImplementedError, OSError):
        return 1


def execution_subsets(selected, profile, slot=None):
    if profile == "platform":
        limit = PLATFORM_RUN_LIMIT
    elif profile == "store" and slot in SLOW_STORE_SLOTS:
        limit = STORE_SLOW_RUN_LIMIT
    else:
        limit = None
    subsets = [selected]
    if limit is not None and len(selected) > limit:
        names = sorted(selected)
        groups = (len(names) + limit - 1) // limit
        subsets = [Counter({name: selected[name] for name in names[index::groups]}) for index in range(groups)]
    if (profile, slot) not in PACKAGE_ALARM_SLOTS:
        return subsets
    refined = []
    for subset in subsets:
        if len(subset) <= 1:
            refined.append(subset)
        else:
            names = sorted(subset)
            refined.extend(Counter({name: subset[name] for name in names[index::2]}) for index in range(2))
    return refined


def command(args, **kwargs):
    with LOG_LOCK:
        print("CI:", json.dumps(args), flush=True)
    return subprocess.run(args, check=True, **kwargs)


def log_events(path, stream):
    with LOG_LOCK:
        with path.open(encoding="utf-8", errors="replace") as source:
            shutil.copyfileobj(source, stream, length=64 * 1024)
        stream.flush()


def check_identity(sha, tree):
    actual = command(["git", "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
    actual_tree = command(["git", "rev-parse", "HEAD^{tree}"], capture_output=True, text=True).stdout.strip()
    verifier.check(actual == sha and actual_tree == tree, "实际 checkout SHA/tree 与不可变输入不符")


def identity(run_id, sha, tree, package):
    return verifier.identity(SimpleNamespace(run_id=run_id, sha=sha, tree=tree, package=package))


def run_slot(profile, slot, compiled, package, expected, temporary):
    total = verifier.PROFILES[profile][0]
    target = "./internal/" + profile
    private = Path(temporary) / f"slot-{slot}"
    private.mkdir()
    scratch = private / "tmp"
    scratch.mkdir()
    env = {**os.environ, "TMPDIR": str(scratch)}
    selected, _ = verifier.selection(compiled, slot, profile)
    with LOG_LOCK:
        print(f"CI: {profile} 槽 {slot}/{total}，{len(selected)} 个顶层项", flush=True)
    runs, passes = Counter(), Counter()
    for index, subset in enumerate(execution_subsets(selected, profile, slot)):
        pattern = verifier.run_pattern(subset)
        events = private / f"run-{index}.json"
        try:
            with events.open("wb") as output:
                command(["go", "test", "-count=1", "-race", "-json", "-run", pattern, target], stdout=output, env=env)
        except subprocess.CalledProcessError:
            # Keep failure diagnostics in the job log, never in evidence artifacts.
            log_events(events, sys.stderr)
            raise
        log_events(events, sys.stdout)
        subset_runs, subset_passes = verifier.verify(events, package, subset)
        verifier.check(not (runs.keys() & subset_runs.keys()) and not (passes.keys() & subset_passes.keys()),
                       "逻辑槽子集重复覆盖")
        runs.update(subset_runs)
        passes.update(subset_passes)
    verifier.check(runs == passes == selected, "逻辑槽子集 run/pass 覆盖计数不符")
    return {"schema": verifier.SCHEMA, "slot": slot, "total": total, **expected,
            "compiled": compiled, "selected": selected, "runs": runs, "passes": passes, "skips": {}}


def run_worker(profile, worker, directory, run_id, sha, tree):
    _, expected_package, summary_name, prefix = verifier.PROFILES[profile]
    verifier.check(type(worker) is int and worker in range(len(BATCHES[profile])), "worker 编号非法")
    expected = identity(run_id, sha, tree, expected_package)
    check_identity(sha, tree)
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    verifier.check(not directory.is_symlink() and not list(directory.iterdir()), "worker 输出目录必须为空且不是符号链接")
    target = "./internal/" + profile
    package = command(["go", "list", target], capture_output=True, text=True).stdout.strip()
    verifier.check(package == expected_package, "实际 Go 包与固定分片配置不符")
    with tempfile.TemporaryDirectory(prefix="ci-shard-") as temporary:
        listed = Path(temporary) / "list.json"
        with listed.open("wb") as output:
            command(["go", "test", "-count=1", "-race", "-json", "-list", "^(Test|Example|Fuzz)", target],
                    stdout=output, env={**os.environ, "TMPDIR": temporary})
        compiled = verifier.inventory(listed, package)
        slots = BATCHES[profile][worker]
        if worker_parallelism(profile, slots) == 2:
            with ThreadPoolExecutor(max_workers=2) as executor:
                futures = [executor.submit(run_slot, profile, slot, compiled, package, expected, temporary) for slot in slots]
                summaries = [future.result() for future in futures]
        else:
            summaries = [run_slot(profile, slot, compiled, package, expected, temporary) for slot in slots]
    # Publish nothing until every subprocess and coverage check has finished successfully.
    for summary in summaries:
        artifact = directory / f"{prefix}{summary['slot']}"
        artifact.mkdir()
        (artifact / summary_name).write_text(json.dumps(summary, ensure_ascii=False, sort_keys=True) + "\n", encoding="utf-8")


def directory_entries(directory, names):
    verifier.check(directory.is_dir() and not directory.is_symlink(), "证据目录缺失或为符号链接")
    verifier.check({path.name for path in directory.iterdir()} == set(names), "worker/逻辑槽证据缺失、重复或额外")


def aggregate_workers(directory, run_id, sha, tree):
    directory = Path(directory)
    names = {f"{profile}-worker-v1-{worker}" for profile, workers in BATCHES.items() for worker in range(len(workers))}
    directory_entries(directory, names)
    result = {}
    with tempfile.TemporaryDirectory(prefix="ci-aggregate-") as temporary:
        for profile, workers in BATCHES.items():
            _, package, summary_name, prefix = verifier.PROFILES[profile]
            expected = identity(run_id, sha, tree, package)
            flattened = Path(temporary) / profile
            flattened.mkdir()
            for worker, slots in enumerate(workers):
                artifact = directory / f"{profile}-worker-v1-{worker}"
                directory_entries(artifact, [f"{prefix}{slot}" for slot in slots])
                for slot in slots:
                    logical = artifact / f"{prefix}{slot}"
                    directory_entries(logical, [summary_name])
                    source = logical / summary_name
                    verifier.check(source.is_file() and not source.is_symlink(), "summary 缺失或为符号链接")
                    target = flattened / logical.name
                    target.mkdir()
                    shutil.copyfile(source, target / summary_name)
            # Preserve all existing identity, hash, lifecycle and exact-union checks.
            result[profile] = verifier.aggregate(flattened, expected, profile)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("run", "aggregate"):
        subparser = commands.add_parser(name)
        if name == "run":
            subparser.add_argument("profile", choices=BATCHES)
            subparser.add_argument("worker", type=int)
        subparser.add_argument("directory", type=Path)
        for flag in ("run-id", "sha", "tree"):
            subparser.add_argument("--" + flag, required=True)
    args = parser.parse_args()
    if args.command == "run":
        run_worker(args.profile, args.worker, args.directory, args.run_id, args.sha, args.tree)
    else:
        check_identity(args.sha, args.tree)
        result = aggregate_workers(args.directory, args.run_id, args.sha, args.tree)
        print(json.dumps(result, ensure_ascii=False, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, UnicodeError, RecursionError, subprocess.CalledProcessError) as error:
        print(f"CI 分片校验失败：{error}", file=sys.stderr)
        sys.exit(1)
