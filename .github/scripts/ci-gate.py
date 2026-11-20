#!/usr/bin/env python3
"""Fail closed unless every required full-suite job succeeded."""
import json
import os
import sys


REQUIRED = {"go-shards", "go-other", "go-vet", "go-benchmarks", "web", "pi-bridge"}


def check_results(results):
    if not isinstance(results, dict) or set(results) != REQUIRED:
        raise ValueError("完整 CI 的必需结果缺失或多余")
    failed = [name for name, value in results.items() if not isinstance(value, dict) or value.get("result") != "success"]
    if failed:
        raise ValueError("完整 CI 未成功：" + ", ".join(sorted(failed)))


if __name__ == "__main__":
    try:
        check_results(json.loads(os.environ["NEEDS_JSON"]))
    except (ValueError, KeyError) as error:
        print(f"CI 合并门禁失败：{error}", file=sys.stderr)
        sys.exit(1)
