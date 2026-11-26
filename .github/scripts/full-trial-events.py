#!/usr/bin/env python3
"""Compact, output-free Go timing exports and baseline-equivalent workspace proof."""
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
                row = verifier.decode(line)
                if row.get("Action") == "skip":
                    name = row.get("Test")
                    check(row.get("Package") == WORKSPACE and name in BASELINE_SKIPS and name in selected,
                          "workspace 出现未经单体基线证明的 skip")
                    skips[name] += 1
                    row["Action"] = "pass"
                output.write(json.dumps(row) + "\n")
        runs, normalized_passes = verifier.verify(normalized, WORKSPACE, selected)
    expected_skips = Counter({name: 1 for name in BASELINE_SKIPS if name in selected})
    check(skips == expected_skips, "workspace fixture 的 skip 结果偏离固定源码基线")
    return {"selected": dict(selected), "runs": dict(runs), "passes": dict(normalized_passes - skips), "skips": dict(skips)}


def compare_workspace(parts, monolith):
    fields = ("selected", "runs", "passes", "skips")
    combined = {field: Counter() for field in fields}
    for part in parts:
        check(set(part) == set(fields), "workspace 回执字段不完整或多余")
        counters = {field: verifier.counter(part[field]) for field in fields}
        check(counters["runs"] == counters["selected"] == counters["passes"] + counters["skips"] and
              not (counters["passes"].keys() & counters["skips"].keys()), "workspace run/pass/skip 计数不符")
        check(set(counters["skips"]) <= BASELINE_SKIPS, "workspace 出现新 skip")
        check(not (combined["selected"].keys() & counters["selected"].keys()), "workspace 子集重复")
        for field in fields:
            combined[field].update(counters[field])
    check(parts and all(combined[field] == verifier.counter(monolith[field]) for field in fields),
          "workspace 子集名称/结果与同次预检单体不符")
    return {field: dict(value) for field, value in combined.items()}
