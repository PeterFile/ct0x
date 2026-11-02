#!/usr/bin/env python3
"""Compare one Store shard's JSON execution multiset with its compiled list."""
import json
import os
import re
import sys
import unicodedata
from collections import Counter


def check(condition, message):
    if not condition:
        raise ValueError(message)


def object_fields(pairs):
    result = dict(pairs)
    check(len(result) == len(pairs), "JSON 字段重复")
    return result


def events(path):
    with open(path, encoding="utf-8") as source:
        rows = [json.loads(line, object_pairs_hook=object_fields,
                           parse_constant=lambda _: check(False, "JSON 常量非法")) for line in source]
    check(len(rows) >= 2, f"{path} 缺少开始或完成事件")
    for index, row in enumerate(rows):
        check(isinstance(row, dict) and row.get("Package") == package, "JSON 对象或包身份错误")
        action, name = row.get("Action"), row.get("Test", "")
        check(isinstance(name, str) and action in ("start", "run", "pause", "cont", "pass", "skip", "output", "attr", "artifacts"), "事件非法或包含失败")
        if action == "output":
            check(isinstance(row.get("Output"), str), "Output 必须为字符串")
        elif not name:
            check((index == 0 and action == "start") or (index == len(rows) - 1 and action == "pass"), "包事件不完整或重复")
    check(rows[0]["Action"] == "start" and not rows[0].get("Test") and
          rows[-1]["Action"] == "pass" and not rows[-1].get("Test"), "必须以包 start 开始、包 pass 完成")
    return rows[1:-1]


try:
    mode, package, list_path, run_path = sys.argv[1:]
    match = re.fullmatch(r"\^Test\(([A-Za-z]+(?:\|[A-Za-z]+)*)\)", os.environ.get("STORE_RACE_GROUP", ""))
    check(mode in ("run", "skip") and match is not None, "仅支持 run/skip 和无 slash 的顶层字面前缀表达式")
    check(package and not any(c.isspace() for c in package), "预期包身份非法")
    prefixes = tuple("Test" + part for part in match.group(1).split("|"))
    compiled = Counter()
    listed = events(list_path)
    check(all(row["Action"] == "output" and not row.get("Test") for row in listed), "compiled-list 只能包含包 Output 事件")
    text = "".join(row["Output"] for row in listed)
    check(not text or text.endswith("\n"), "compiled-list 标识行不完整")
    # Only documented identifier lines in decoded Output are inventory entries.
    for name in text.split("\n"):
        if name.startswith(("Test", "Example", "Fuzz")):
            check(all(c == "_" or unicodedata.category(c) in ("Lu", "Ll", "Lt", "Lm", "Lo", "Nd") for c in name), f"Go 标识非法：{name!r}")
            compiled[name] += 1
    selected = Counter({name: count for name, count in compiled.items() if name.startswith(prefixes) == (mode == "run")})
    check(selected, "所选顶层条目不能为空")
    runs, skipped, active = Counter(), Counter(), set()
    for row in events(run_path):
        action, name = row["Action"], row.get("Test", "")
        if not name:
            continue
        root = name.split("/", 1)[0]
        check(root in selected, f"未列出或不属于此分片的顶层项（不猜测重名后缀）：{name}")
        if action == "run":
            check(name not in active and (name == root or root in active), f"重叠重名或子项缺少活动父项：{name}")
            active.add(name)
            runs[name] += 1
        elif action in ("pass", "skip"):
            check(name in active and not any(child.startswith(name + "/") for child in active), f"完成事件缺失 run 或仍有活动子项：{name}")
            active.remove(name)
            if action == "skip":
                skipped[name] += 1
        else:
            check(action in ("output", "pause", "cont", "attr", "artifacts") and name in active, f"事件无法归属活动条目：{name}")
    actual = Counter({name: count for name, count in runs.items() if "/" not in name})
    check(not active and actual == selected, f"覆盖多重计数不符：预期={dict(selected)}，实际={dict(actual)}，未完成={sorted(active)}")
    print(json.dumps({"compiled": compiled, "selected": selected, "runs": runs, "existing_skips": skipped}, ensure_ascii=False))
except (ValueError, OSError) as error:
    print(f"Store 分片校验失败：{error}", file=sys.stderr)
    sys.exit(1)
