#!/usr/bin/env python3
"""Compact Go timings and exact workspace evidence within each compiled build mode."""
from collections import Counter
import importlib.util
import json
from pathlib import Path
import tempfile

_spec = importlib.util.spec_from_file_location("trial_verifier", Path(__file__).with_name("verify-store-shard.py"))
verifier = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(verifier)
check = verifier.check
WORKSPACE = "agentworkspace/cmd/workspace"
# Proven at the immutable product SHA, cmd/workspace/artifacts_test.go:676-680.
# Node-dependent tests are NOT exceptions: the driver requires Node to exist.
BASELINE_SKIPS = {"TestArtifactReadMarkerWriteFailureHelper"}
WORKSPACE_FIELDS = {"selected", "runs", "passes", "skips"}
CONTEXT_FIELDS = {"run_id", "attempt", "controller_sha", "controller_tree", "checkout_sha", "checkout_tree",
                  "scheme", "repeat", "shard_parallel", "job", "job_number"}


def compact(path, context, profile, slot, invocation):
    """Keep lifecycle/durations, including failures; never copy Output or env."""
    result = []
    with Path(path).open(encoding="utf-8", errors="strict") as source:
        for line in source:
            row = verifier.decode(line)
            action = row.get("Action")
            if action not in ("run", "pass", "fail", "skip"):
                continue
            name, package, elapsed = row.get("Test", ""), row.get("Package"), row.get("Elapsed")
            check(isinstance(name, str) and len(name.encode()) <= 65536 and isinstance(package, str), "计时事件身份非法")
            check(elapsed is None or type(elapsed) in (int, float) and elapsed >= 0, "计时 Elapsed 非法")
            result.append({**{key: value for key, value in context.items() if key in CONTEXT_FIELDS},
                           "package": package, "name": name, "profile": profile, "slot": slot,
                           "invocation": invocation, "action": action, "elapsed": elapsed,
                           "time": row.get("Time")})
    return result


def workspace(path, selected):
    """Reuse the strict lifecycle verifier, admitting only the proven fixture skip."""
    skips = Counter()
    with tempfile.TemporaryDirectory(prefix="trial-workspace-") as temporary:
        normalized = Path(temporary) / "events.json"
        with normalized.open("w", encoding="utf-8") as output, Path(path).open(encoding="utf-8") as source:
            for line in source:
                check(line.endswith("\n"), "workspace JSON 事件行截断")
                row = verifier.decode(line)
                check(isinstance(row, dict), "workspace JSON 事件必须为对象")
                if row.get("Action") == "skip":
                    name = row.get("Test")
                    check(row.get("Package") == WORKSPACE and name in BASELINE_SKIPS and name in selected,
                          "workspace 出现固定源码基线以外的 skip")
                    skips[name] += 1
                    row["Action"] = "pass"
                output.write(json.dumps(row) + "\n")
        runs, normalized_passes = verifier.verify(normalized, WORKSPACE, selected)
    expected_skips = Counter({name: 1 for name in BASELINE_SKIPS if name in selected})
    check(skips == expected_skips, "workspace fixture 的 skip 结果偏离固定源码基线")
    return {"selected": dict(selected), "runs": dict(runs), "passes": dict(normalized_passes - skips), "skips": dict(skips)}


def workspace_coverage(parts, compiled):
    """Validate actual per-mode proofs, not outcomes inferred from an inventory."""
    compiled = verifier.counter(compiled)
    check(compiled and all(count == 1 for count in compiled.values()), "workspace compiled 为空或重复")
    check(isinstance(parts, list) and parts, "workspace 实际执行证明缺失")
    combined = {field: Counter() for field in WORKSPACE_FIELDS}
    for part in parts:
        check(isinstance(part, dict) and set(part) == WORKSPACE_FIELDS, "workspace 回执字段不完整或多余")
        counters = {field: verifier.counter(part[field]) for field in WORKSPACE_FIELDS}
        check(all(count == 1 for count in counters["selected"].values()) and
              counters["runs"] == counters["selected"] == counters["passes"] + counters["skips"] and
              not (counters["passes"].keys() & counters["skips"].keys()), "workspace run/pass/skip 计数不符")
        expected_skips = Counter({name: 1 for name in BASELINE_SKIPS if name in counters["selected"]})
        check(counters["skips"] == expected_skips, "workspace skip 偏离固定源码基线")
        check(not (combined["selected"].keys() & counters["selected"].keys()), "workspace 子集重复")
        for field in WORKSPACE_FIELDS:
            combined[field].update(counters[field])
    check(combined["selected"] == combined["runs"] == compiled, "workspace 实际名称/结果并集与同模式 compiled 不符")
    return {field: dict(value) for field, value in combined.items()}


def compare_workspace(parts, monolith):
    """Compare actual executions of the same mode, never an inferred baseline."""
    check(isinstance(monolith, dict) and set(monolith) == WORKSPACE_FIELDS, "workspace 单体回执字段非法")
    reference = workspace_coverage([monolith], monolith["selected"])
    combined = workspace_coverage(parts, reference["selected"])
    check(combined == reference, "workspace 子集名称/结果与同模式实际单体不符")
    return combined
