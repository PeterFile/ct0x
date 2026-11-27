#!/usr/bin/env python3
"""Immutable Full trial parameters and deterministic, weight-ordered matrix items."""
from collections import Counter
import hashlib
import importlib.util
import json
import math
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
PRODUCT_SHA = "18d11a3c0176fe98856aeeef6396e9ddfde0cca0"
PRODUCT_TREE = "6df8bf3ccb986f8f4088dc310e0257b3f6285d9a"
WORKSPACE_PACKAGE = "agentworkspace/cmd/workspace"
WORKSPACE_PARTS = 4


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), SCRIPTS / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


shards = load("ci-shards")
verifier = shards.verifier
check = verifier.check


def parameters(scheme, repeat, parallel):
    check(scheme in ("a", "b", "c"), "试验方案必须为 a/b/c")
    check(type(repeat) is int and repeat in (1, 2, 3), "repeat 必须为 1/2/3")
    # Observed account capacity is 20 Linux runners. C adds four workspace jobs
    # and three unchanged sidecars, leaving one runner free even at this cap.
    check(type(parallel) is int and (parallel == 7 if scheme == "a" else 8 <= parallel <= 12),
          "A 必须使用 7 路；B/C 必须使用 8..12 路（保留侧车资源余量）")


def weights():
    source = verifier.decode((SCRIPTS / "full-trial-weights.json").read_text())
    result = source["profiles"]
    check(set(result) == set(verifier.PROFILES), "权重 profile 不完整")
    for profile, (total, *_) in verifier.PROFILES.items():
        check(isinstance(result[profile], list) and len(result[profile]) == total and
              all(type(value) in (int, float) and math.isfinite(value) and value > 0 for value in result[profile]),
              "历史槽权重非法")
    return result


def key(unit):
    return json.dumps(unit, sort_keys=True, separators=(",", ":"))


def unit_slots(scheme, unit):
    profile, worker, part = unit["profile"], unit["worker"], unit["part"]
    if part is not None:
        check(scheme == "c" and profile == "store" and worker in shards.SLOW_STORE_SLOTS,
              "非 C 慢槽子集")
        return (worker,)
    slots = shards.BATCHES[profile][worker]
    return tuple(slot for slot in slots if scheme != "c" or profile != "store" or slot not in shards.SLOW_STORE_SLOTS)


def schedule(scheme, parallel, inventories):
    parameters(scheme, 1, parallel)
    expected = {*verifier.PROFILES, "workspace"}
    check(set(inventories) == expected, "compiled inventory 包不完整")
    for compiled in inventories.values():
        check(compiled and all(value == 1 and type(value) is int for value in verifier.counter(compiled).values()),
              "compiled inventory 为空或重复")
    units = [dict(row, part=None) for row in load("ci-plan").shard_matrix()["include"]]
    if scheme == "a":
        return {"shards": [[unit] for unit in units], "workspace": [0]}
    observed = weights()
    tasks = []
    for unit in units:
        slots = unit_slots(scheme, unit)
        if slots:
            tasks.append((unit, sum(observed[unit["profile"]][slot] for slot in slots)))
    if scheme == "c":
        for slot in sorted(shards.SLOW_STORE_SLOTS):
            selected, _ = verifier.selection(inventories["store"], slot, "store")
            parts = shards.execution_subsets(selected, "store", slot)
            for part in range(len(parts)):
                # New subsets have no historical timing of their own. This is an
                # equal-share scheduling proxy, not a measured speedup claim.
                tasks.append(({"profile": "store", "worker": slot, "part": part}, observed["store"][slot] / len(parts)))
    # The matrix caps concurrency; each item retains the existing unit-list shape.
    jobs = [[unit] for unit, _ in sorted(tasks, key=lambda pair: (-pair[1], key(pair[0])))]
    return {"shards": jobs, "workspace": list(range(WORKSPACE_PARTS)) if scheme == "c" else [0]}


def workspace_selection(compiled, part, total):
    check(type(part) is int and part in range(total) and total in (1, WORKSPACE_PARTS), "workspace 子集编号非法")
    return Counter({name: count for name, count in compiled.items()
                    if int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], "big") % total == part})
