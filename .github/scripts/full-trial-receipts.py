#!/usr/bin/env python3
"""Fail-closed receipt validation and reconstruction of original BATCHES evidence."""
from collections import Counter
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

_spec = importlib.util.spec_from_file_location("trial_plan", Path(__file__).with_name("full-trial-plan.py"))
plan_module = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(plan_module)
shards, verifier = plan_module.shards, plan_module.verifier
events = plan_module.load("full-trial-events")
check = verifier.check
IDENTITY_FIELDS = {"run_id", "attempt", "controller_sha", "controller_tree", "checkout_sha", "checkout_tree",
                   "scheme", "repeat", "shard_parallel"}


def prefix(context):
    return f"full-trial-{context['scheme']}-r{context['repeat']}-{context['run_id']}-{context['attempt']}"


def validate_plan(plan, expected):
    check(isinstance(plan, dict) and set(plan) == {"schema", "identity", "base", "master_oid", "inventories", "workspace_ordinary", "schedule"},
          "试验计划字段不完整或多余")
    check(type(plan["schema"]) is int and plan["schema"] == 2 and plan_module.key(plan["identity"]) == plan_module.key(expected) and
          set(expected) == IDENTITY_FIELDS, "计划与实际 run/attempt/控制器/产品/方案身份不符")
    plan_module.parameters(expected["scheme"], expected["repeat"], expected["shard_parallel"])
    for field in ("run_id", "attempt"):
        check(isinstance(expected[field], str) and re.fullmatch(r"[1-9][0-9]{0,63}", expected[field]), "run/attempt 非法")
    for field in ("controller_sha", "controller_tree", "checkout_sha", "checkout_tree"):
        check(isinstance(expected[field], str) and re.fullmatch(r"[0-9a-f]{40}", expected[field]), "OID 身份非法")
    check(expected["checkout_sha"] == plan_module.PRODUCT_SHA and expected["checkout_tree"] == plan_module.PRODUCT_TREE,
          "产品不是本次冻结提交/树")
    for field in ("base", "master_oid"):
        check(isinstance(plan[field], str) and re.fullmatch(r"[0-9a-f]{40}", plan[field]), "master 历史身份非法")
    calculated = plan_module.schedule(expected["scheme"], expected["shard_parallel"], plan["inventories"])
    check(plan_module.key(plan["schedule"]) == plan_module.key(calculated), "计划调度偏离固定基线/LPT 算法")
    ordinary = plan["workspace_ordinary"]
    check(isinstance(ordinary, dict) and set(ordinary) == {"compiled", "proof"}, "workspace 普通模式证明字段非法")
    events.workspace_coverage([ordinary["proof"]], ordinary["compiled"])


def read_receipt(directory, context, job, number, payload):
    shards.directory_entries(directory, ["receipt.json"])
    path = directory / "receipt.json"
    check(path.is_file() and not path.is_symlink(), "回执不是普通文件")
    receipt = verifier.decode(path.read_text())
    check(isinstance(receipt, dict) and set(receipt) == {"schema", "identity", "job", "job_number", payload}, "作业回执字段非法")
    check(type(receipt["schema"]) is int and receipt["schema"] == 1 and plan_module.key(receipt["identity"]) == plan_module.key(context) and
          receipt["job"] == job and type(receipt["job_number"]) is int and receipt["job_number"] == number,
          "作业回执混入其他 run/attempt/方案/runner 身份")
    return receipt[payload]


def collect(plan, shard_directory, workspace_directory, expected):
    validate_plan(plan, expected)
    shard_directory, workspace_directory = Path(shard_directory), Path(workspace_directory)
    lanes = plan["schedule"]["shards"]
    shards.directory_entries(shard_directory, [f"{prefix(expected)}-shard-{i}" for i in range(len(lanes))])
    combined = {}
    for number, units in enumerate(lanes):
        payload = read_receipt(shard_directory / f"{prefix(expected)}-shard-{number}", expected, "go-shards", number, "units")
        check(isinstance(payload, list) and len(payload) == len(units), "runner 单元回执缺失/重复")
        for result, unit in zip(payload, units):
            check(isinstance(result, dict) and set(result) == {"unit", "summaries"} and
                  plan_module.key(result["unit"]) == plan_module.key(unit), "runner 执行单元错配")
            profile = unit["profile"]
            total, package, _, _ = verifier.PROFILES[profile]
            slots = plan_module.unit_slots(expected["scheme"], unit)
            check(isinstance(result["summaries"], list) and len(result["summaries"]) == len(slots), "逻辑槽回执缺失/重复")
            for summary, slot in zip(result["summaries"], slots):
                identity = shards.identity(expected["run_id"], expected["checkout_sha"], expected["checkout_tree"], package)
                fields = {"schema", "slot", "total", *identity, "compiled", "selected", "runs", "passes", "skips"}
                check(isinstance(summary, dict) and set(summary) == fields and
                      all(summary.get(field) == value for field, value in identity.items()), "逻辑槽身份/字段非法")
                for field, value in (("schema", 1), ("slot", slot), ("total", total)):
                    check(type(summary[field]) is int and summary[field] == value, "逻辑槽 schema/编号/模数非法")
                compiled = verifier.counter(summary["compiled"])
                check(compiled == plan["inventories"][profile], "实际 compiled 与预检包 inventory 不符")
                selected, _ = verifier.selection(compiled, slot, profile)
                if unit["part"] is not None:
                    selected = shards.execution_subsets(selected, profile, slot)[unit["part"]]
                counters = {field: verifier.counter(summary[field]) for field in ("selected", "runs", "passes", "skips")}
                check(counters["selected"] == counters["runs"] == counters["passes"] == selected and not counters["skips"],
                      "逻辑槽子集哈希/run/pass/skip 不符")
                slot_key = (profile, slot)
                if slot_key not in combined:
                    combined[slot_key] = {**summary, **{field: Counter() for field in counters}}
                destination = combined[slot_key]
                check(not (destination["selected"].keys() & selected.keys()), "子集重复覆盖")
                for field in counters:
                    destination[field].update(counters[field])
    expected_slots = {(profile, slot) for profile, (total, *_) in verifier.PROFILES.items() for slot in range(total)}
    check(set(combined) == expected_slots, "81 个规范逻辑槽缺失/多余")
    for (profile, slot), summary in combined.items():
        selected, _ = verifier.selection(plan["inventories"][profile], slot, profile)
        check(summary["selected"] == summary["runs"] == summary["passes"] == selected and not summary["skips"],
              "重建规范槽的完整并集不符")
    workspace_jobs = plan["schedule"]["workspace"]
    # A single matching artifact is extracted directly by download-artifact.
    flat_workspace = len(workspace_jobs) == 1 and (workspace_directory / "receipt.json").is_file()
    if not flat_workspace:
        shards.directory_entries(workspace_directory, [f"{prefix(expected)}-workspace-{i}" for i in workspace_jobs])
    proofs = []
    for number in workspace_jobs:
        directory = workspace_directory if flat_workspace else workspace_directory / f"{prefix(expected)}-workspace-{number}"
        payload = read_receipt(directory, expected, "go-workspace", number, "workspace")
        check(isinstance(payload, dict) and set(payload) == {"compiled", "proof"} and
              verifier.counter(payload["compiled"]) == plan["inventories"]["workspace"], "workspace compiled 不符")
        selected = plan_module.workspace_selection(plan["inventories"]["workspace"], number, len(workspace_jobs))
        proof = payload["proof"]
        check(isinstance(proof, dict) and set(proof) == events.WORKSPACE_FIELDS, "workspace 回执字段不完整或多余")
        check(verifier.counter(proof["selected"]) == selected, "workspace 分片哈希/顶层名称不符")
        proofs.append(proof)
    # A/B provide actual race monoliths; C provides only actual disjoint race
    # subsets. Cross-run equivalence to A's race monolith is an external gate.
    return combined, events.workspace_coverage(proofs, plan["inventories"]["workspace"])


def aggregate_canonical(plan, canonical):
    context = plan["identity"]
    with tempfile.TemporaryDirectory(prefix="trial-canonical-") as temporary:
        root = Path(temporary)
        for profile, workers in shards.BATCHES.items():
            _, _, name, prefix_name = verifier.PROFILES[profile]
            for worker, slots in enumerate(workers):
                for slot in slots:
                    path = root / f"{profile}-worker-v1-{worker}" / f"{prefix_name}{slot}" / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(json.dumps(canonical[profile, slot], sort_keys=True) + "\n")
        # Execute the immutable product checkout's real CLI, not a replacement gate.
        process = subprocess.run([sys.executable, ".github/scripts/ci-shards.py", "aggregate", str(root),
                                  "--run-id", context["run_id"], "--sha", context["checkout_sha"],
                                  "--tree", context["checkout_tree"]], check=True, capture_output=True, text=True)
        # The original CLI prints two harmless git command records before its JSON.
        result = verifier.decode(process.stdout.splitlines()[-1])
        check(set(result) == set(verifier.PROFILES) and
              all(result[profile]["compiled"] == plan["inventories"][profile] for profile in result),
              "原 aggregate 与实际预检 inventory 不符")
        return result
