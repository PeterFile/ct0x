#!/usr/bin/env python3
"""Select, verify and aggregate four Store race shards from Go JSON evidence."""
import argparse
from collections import Counter
import hashlib
import json
import math
from pathlib import Path
import re
import sys
import unicodedata


TOTAL = 4
SCHEMA = 1
MAX_NAME_BYTES = 4096
# Stay below Linux's per-argument limit, including for multibyte identifiers.
MAX_PATTERN_BYTES = 60000
SUMMARY = "store-summary.json"
ARTIFACT_PREFIX = "store-shard-v1-"


def check(condition, message):
    if not condition:
        raise ValueError(message)


def object_fields(pairs):
    result = dict(pairs)
    check(len(result) == len(pairs), "JSON 字段重复")
    return result


def finite_float(text):
    value = float(text)
    check(math.isfinite(value), "JSON 数字必须有限")
    return value


def check_utf8(value):
    if isinstance(value, str):
        value.encode("utf-8", errors="strict")
    elif isinstance(value, dict):
        for key, item in value.items():
            check_utf8(key)
            check_utf8(item)
    elif isinstance(value, list):
        for item in value:
            check_utf8(item)


def decode(text):
    value = json.loads(text, object_pairs_hook=object_fields, parse_float=finite_float,
                       parse_constant=lambda _: check(False, "JSON 常量非法"))
    check_utf8(value)
    return value


def events(path, package):
    started, finished = False, False
    with open(path, encoding="utf-8", newline="") as source:
        for line in source:
            check(line.endswith("\n") and not finished, "JSON 事件行截断或包完成后仍有事件")
            row = decode(line)
            check(isinstance(row, dict) and row.get("Package") == package, "JSON 对象或包身份错误")
            action, name = row.get("Action"), row.get("Test", "")
            check(isinstance(name, str) and action in ("start", "run", "pause", "cont", "pass", "output", "attr", "artifacts"),
                  "事件非法或包含失败/跳过")
            if "Elapsed" in row:
                elapsed = row["Elapsed"]
                check(type(elapsed) in (int, float) and elapsed >= 0, "Elapsed 必须为非负有限数字")
            for field in ("Time", "Output", "Key", "Value"):
                check(field not in row or isinstance(row[field], str), f"{field} 必须为字符串")
            if action == "output":
                check("Output" in row, "output 缺少 Output")
            if action == "attr":
                check("Key" in row and "Value" in row, "attr 缺少 Key/Value")
            if not started:
                check(action == "start" and not name, "必须以包 start 开始")
                started = True
            elif not name:
                check(action in ("output", "pass"), "包事件不完整或重复")
                if action == "pass":
                    finished = True
                else:
                    yield row
            else:
                check(action != "start", "测试不能包含 start 事件")
                yield row
    check(started and finished, "缺少包 start/pass 事件")


def valid_name(name):
    check(isinstance(name, str) and 0 < len(name.encode("utf-8")) <= MAX_NAME_BYTES, "顶层名称为空或过长")
    prefix = next((prefix for prefix in ("Test", "Example", "Fuzz") if name.startswith(prefix)), None)
    check(prefix is not None and (len(name) == len(prefix) or unicodedata.category(name[len(prefix)]) != "Ll"),
          f"Go 测试名称非法：{name!r}")
    check(all(c == "_" or unicodedata.category(c) in ("Lu", "Ll", "Lt", "Lm", "Lo", "Nd") for c in name),
          f"Go 标识非法：{name!r}")


def inventory(path, package):
    output = []
    for row in events(path, package):
        check(row["Action"] == "output" and not row.get("Test"), "compiled-list 只能包含包 Output 事件")
        output.append(row["Output"])
    text = "".join(output)
    check(text.endswith("\n"), "compiled-list 标识行不完整")
    compiled = Counter()
    lines = text[:-1].split("\n")
    for index, name in enumerate(lines):
        if name.startswith(("Test", "Example", "Fuzz")):
            valid_name(name)
            compiled[name] += 1
        else:
            status = rf"ok[ \t]+{re.escape(package)}[ \t]+[0-9]+(?:\.[0-9]+)?s(?:[ \t]+\[no tests to run\])?"
            check(index == len(lines) - 1 and re.fullmatch(status, name), f"compiled-list 含未知或错误格式行：{name!r}")
    check(compiled and all(count == 1 for count in compiled.values()), "compiled 不能为空或包含重复名称")
    return compiled


def selection(compiled, slot):
    selected = Counter({name: count for name, count in compiled.items()
                        if int.from_bytes(hashlib.sha256(name.encode("utf-8")).digest()[:8], "big") % TOTAL == slot})
    pattern = "^(" + "|".join(re.escape(name) for name in sorted(selected)) + ")$" if selected else "^$"
    check("/" not in pattern and len(pattern.encode("utf-8")) <= MAX_PATTERN_BYTES, "分片表达式过长或包含 slash；拒绝截断")
    return selected, pattern


def verify(path, package, selected):
    states, active, runs, passes = {}, set(), Counter(), Counter()
    for row in events(path, package):
        action, name = row["Action"], row.get("Test", "")
        if not name:
            continue
        check(len(name.encode("utf-8")) <= 65536 and not any(c.isspace() or unicodedata.category(c).startswith("C") for c in name),
              "测试事件名称过长或含非法字符")
        root = name.split("/", 1)[0]
        check(root in selected, f"未列出或不属于此分片的顶层项：{name}")
        # Slash can occur inside a subtest label; only recorded prefixes are parents.
        parents = [name[:index] for index, char in enumerate(name) if char == "/" and name[:index] in states]
        check(name == root or parents, f"子项缺少父项 run：{name}")
        check(all(states[parent] != "done" for parent in parents), f"子项属于已结束父项：{name}")
        if action in ("run", "pause", "cont", "pass"):
            check(all(states[parent] == "running" for parent in parents), f"子项的父项未活动：{name}")
        if action == "run":
            check(name not in states, f"重复 run：{name}")
            states[name] = "running"
            active.add(name)
            if name == root:
                runs[name] += 1
        elif action == "pause":
            check(states.get(name) == "running", f"pause 缺少活动 run 或重复：{name}")
            states[name] = "paused"
        elif action == "cont":
            check(states.get(name) == "paused", f"cont 缺少 pause 或重复：{name}")
            states[name] = "running"
        elif action == "pass":
            check(states.get(name) == "running" and not any(child.startswith(name + "/") for child in active),
                  f"pass 缺少活动 run/cont、重复或仍有未完成子项：{name}")
            states[name] = "done"
            active.remove(name)
            if name == root:
                passes[name] += 1
        else:
            check(action in ("output", "attr", "artifacts") and states.get(name) in ("running", "paused"),
                  f"事件无法归属活动条目：{name}")
    check(not active and runs == selected and passes == selected,
          f"顶层 run/pass 覆盖计数不符或含未完成项：selected={dict(selected)}, run={dict(runs)}, pass={dict(passes)}")
    return runs, passes


def identity(args):
    check(re.fullmatch(r"[1-9][0-9]{0,63}", args.run_id), "run 身份非法")
    check(re.fullmatch(r"[0-9a-f]{40}", args.sha) and re.fullmatch(r"[0-9a-f]{40}", args.tree), "checkout SHA/tree 身份非法")
    return {"run_id": args.run_id, "checkout_sha": args.sha, "checkout_tree": args.tree, "package": args.package}


def counter(value):
    check(isinstance(value, dict), "summary Counter 必须为对象")
    for name, count in value.items():
        valid_name(name)
        check(type(count) is int and count > 0, "summary Counter 必须为正整数（不含 bool）")
    return Counter(value)


def aggregate(directory, expected):
    directory = Path(directory)
    names = {f"{ARTIFACT_PREFIX}{slot}" for slot in range(TOTAL)}
    check(directory.is_dir() and not directory.is_symlink(), "summary 下载目录不存在或为符号链接")
    check({path.name for path in directory.iterdir()} == names, "summary 必须恰有槽 0..3 的四个 artifact 目录")
    common, combined, all_runs, all_passes = None, Counter(), Counter(), Counter()
    for slot in range(TOTAL):
        artifact = directory / f"{ARTIFACT_PREFIX}{slot}"
        check(artifact.is_dir() and not artifact.is_symlink(), "artifact 目录非法")
        files = list(artifact.iterdir())
        check(len(files) == 1 and files[0].name == SUMMARY and files[0].is_file() and not files[0].is_symlink(),
              "每槽必须恰有一份 summary，不允许额外文件或符号链接")
        summary = decode(files[0].read_text(encoding="utf-8"))
        fields = {"schema", "slot", "total", *expected, "compiled", "selected", "runs", "passes", "skips"}
        check(isinstance(summary, dict) and set(summary) == fields, "summary 字段不完整或多余")
        for field, value in (("schema", SCHEMA), ("total", TOTAL), ("slot", slot)):
            check(type(summary[field]) is int and summary[field] == value, f"summary {field} 非法或槽重复")
        check(all(summary[field] == value for field, value in expected.items()), "summary 与本次 run/实际 checkout/包身份不符")
        compiled, selected, runs, passes, skips = (counter(summary[field]) for field in ("compiled", "selected", "runs", "passes", "skips"))
        check(compiled and all(count == 1 for count in compiled.values()), "compiled 不能为空或包含重复名称")
        if common is None:
            common = compiled
        check(compiled == common, "跨片 compiled 不一致")
        check(selected == selection(compiled, slot)[0] and runs == selected and passes == selected and not skips,
              "summary 哈希选片或 run/pass/skip 覆盖不符")
        check(not (combined.keys() & selected.keys()), "分片重复覆盖")
        combined.update(selected)
        all_runs.update(runs)
        all_passes.update(passes)
    check(combined == all_runs == all_passes == common, "四片并集与 compiled 多重计数不符")
    return {"schema": SCHEMA, "total": TOTAL, **expected, "compiled": common, "runs": all_runs, "passes": all_passes, "skips": {}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for command in ("select", "verify", "aggregate"):
        subparser = commands.add_parser(command)
        if command != "aggregate":
            subparser.add_argument("slot", type=int, choices=range(TOTAL))
        subparser.add_argument("package")
        subparser.add_argument("directory" if command == "aggregate" else "inventory")
        if command == "verify":
            subparser.add_argument("events")
        if command != "select":
            for flag in ("run-id", "sha", "tree"):
                subparser.add_argument("--" + flag, required=True)
    args = parser.parse_args()
    check(len(args.package) <= 4096 and re.fullmatch(r"[A-Za-z0-9._~+-]+(?:/[A-Za-z0-9._~+-]+)*", args.package)
          and not any(part in (".", "..") for part in args.package.split("/")), "预期包身份非法")
    if args.command == "aggregate":
        result = aggregate(args.directory, identity(args))
    else:
        compiled = inventory(args.inventory, args.package)
        selected, pattern = selection(compiled, args.slot)
        if args.command == "select":
            print(pattern)
            return
        runs, passes = verify(args.events, args.package, selected)
        result = {"schema": SCHEMA, "slot": args.slot, "total": TOTAL, **identity(args),
                  "compiled": compiled, "selected": selected, "runs": runs, "passes": passes, "skips": {}}
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, UnicodeError, RecursionError) as error:
        print(f"Store 分片校验失败：{error}", file=sys.stderr)
        sys.exit(1)
