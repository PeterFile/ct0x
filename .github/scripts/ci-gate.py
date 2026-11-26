#!/usr/bin/env python3
"""Fail closed unless every required full-suite job succeeded."""
import importlib.util
import json
import os
from pathlib import Path
import re
import sys


REQUIRED = {"go-shards", "go-other", "go-workspace", "go-vet", "go-benchmarks"}


def check_results(results, required=REQUIRED):
    if not isinstance(results, dict) or set(results) != required:
        raise ValueError("完整 CI 的必需结果缺失或多余")
    failed = [name for name, value in results.items() if not isinstance(value, dict) or value.get("result") != "success"]
    if failed:
        raise ValueError("完整 CI 未成功：" + ", ".join(sorted(failed)))


def check_outer(results, run_id, sha):
    check_results(results, {"feedback", "full"})
    preflight = results["feedback"].get("outputs")
    full = results["full"].get("outputs")
    if not isinstance(preflight, dict) or not isinstance(full, dict):
        raise ValueError("预检或完整证据输出缺失")
    if preflight.get("mode") != "full" or full.get("verified") != "true":
        raise ValueError("不是完整预检或缺少聚合证明")
    identity = {field: preflight.get(field) for field in ("run_id", "sha", "tree", "head", "base")}
    if identity["run_id"] != run_id or identity["sha"] != sha or not re.fullmatch(r"[1-9][0-9]{0,63}", run_id):
        raise ValueError("预检与当前 run/触发 SHA 不符")
    for field in ("sha", "tree", "head", "base"):
        if not isinstance(identity[field], str) or not re.fullmatch(r"[0-9a-f]{40}", identity[field]):
            raise ValueError("预检提交身份非法")
    if any(full.get(field) != value for field, value in identity.items()):
        raise ValueError("聚合证据与同一次预检身份不符")
    print(json.dumps({"verified": True, **identity}, sort_keys=True))


if __name__ == "__main__":
    try:
        results = json.loads(os.environ["NEEDS_JSON"])
        if sys.argv[1:] == ["outer"]:
            spec = importlib.util.spec_from_file_location("ci_plan", Path(__file__).with_name("ci-plan.py"))
            plan = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(plan)
            event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
            if plan.mode(os.environ["GITHUB_EVENT_NAME"], event) != "full":
                raise ValueError("反馈或无关事件不能通过完整合并门禁")
            check_outer(results, os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_SHA"])
        elif not sys.argv[1:]:
            check_results(results)
        else:
            raise ValueError("未知门禁类型")
    except (ValueError, KeyError, OSError) as error:
        print(f"CI 合并门禁失败：{error}", file=sys.stderr)
        sys.exit(1)
