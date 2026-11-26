#!/usr/bin/env python3
"""Credential-free, bounded client replay; no API calls and no repository mutations.

Candidates use repository-relative files and native full names (suite/leaf names
joined by spaces, not the Vitest display separator " > "). Only comparable
assertions are supported. Discovery is per-test: unrelated infrastructure errors
must not erase a named assertion. Replay is deliberately stricter than discovery.

Set PERIODIC_WEB_NODE / PERIODIC_WEB_NPM or PERIODIC_BRIDGE_NODE /
PERIODIC_BRIDGE_NPM to absolute Node and npm-cli.js paths (an npm symlink is OK).
Web requires Node 22.x; bridge requires exactly 24.13.0. There is no PATH fallback.
Each probe exports a read-only Git archive, checks its blobs against ls-tree,
installs the exact public-registry lockfile without lifecycle scripts, and runs
one anchored native test. Existing node_modules and user npm configuration are
never reused. A missing leaf in an existing file is unknown, not absent.

verify_probe reconstructs state from external artifact bytes, not claimed state.
The authenticated caller must bind sha/tree to its trusted first-parent chain;
pass root= to additionally recheck tree and inventory against that repository.
Pass one shared seen=set() across finalization calls to reject reused artifacts.
Environment isolation is not an OS sandbox for hostile same-user source code.
"""
from collections import Counter
import fcntl
import hashlib
import io
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import tarfile
import tempfile
import textwrap
import threading
import time
from urllib.parse import urlsplit
import uuid


MAX_LOG, MAX_ARCHIVE, MAX_FILES, MAX_TESTS = 4 * 1024 * 1024, 64 * 1024 * 1024, 20000, 10000
MAX_TOTAL_OUTPUT = 512 * 1024 * 1024
SHA_RE = r"[0-9a-f]{40}"
PROJECTS = {"web": "web", "bridge": "extensions/pi-bridge"}
NAMES = {"web": "agent-workspace-web", "bridge": "agent-workspace-pi-bridge"}
SCRIPTS = {"web": "vitest run", "bridge": "node --test tests/*.test.mjs"}
ANSI = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))")
VOLATILE = re.compile(r"timed?\s*out|timeout|deadline|ECONN|ENOTFOUND|EAI_AGAIN|ENOMEM|ENOSPC|"
                      r"out of memory|worker.*(?:exit|kill)|signal:|https?://|file://|"
                      r"/(?:tmp|private|home|Users|var)/|\b0x[0-9a-f]+\b|\b\d{4}-\d\d-\d\dT", re.I)


def check(condition, message):
    if not condition:
        raise ValueError(message)


def unknown(reason):
    return {"state": "unknown", "reason": reason}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def strict_json(text):
    def pairs(items):
        result = {}
        for key, value in items:
            check(key not in result, "JSON 包含重复字段")
            result[key] = value
        return result
    return json.loads(text, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("JSON 包含非有限值")))


def cleaned(text):
    check(isinstance(text, str) and len(text) <= MAX_LOG and len(text.encode()) <= MAX_LOG, "客户端日志超过上限或类型非法")
    return "\n".join(re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", ANSI.sub("", line)).rstrip()
                     for line in text.splitlines())


def safe_path(value):
    return (isinstance(value, str) and 0 < len(value) <= 512 and
            re.fullmatch(r"[A-Za-z0-9_./@+-]+", value) is not None and not value.startswith("/") and
            all(part not in ("", ".", "..", ".git", "node_modules") for part in value.split("/")))


def client_file(client, value):
    check(client in PROJECTS and isinstance(value, str), "客户端或文件身份非法")
    project = PROJECTS[client]
    if value.startswith("./"):
        value = value[2:]
    if value.startswith("/"):
        # Only an absolute path containing the complete known package prefix is usable.
        check(re.fullmatch(r"/[A-Za-z0-9_./@+-]+", value) and "//" not in value and
              not any(part in (".", "..") for part in value.split("/")) and
              value.count("/" + project + "/") == 1, "测试绝对路径不安全或有歧义")
        value = project + "/" + value.split("/" + project + "/", 1)[1]
    if not value.startswith(project + "/"):
        value = project + "/" + value
    check(safe_path(value), "测试文件路径不安全")
    relative = value[len(project) + 1:]
    pattern = r"src/(?:[A-Za-z0-9_@+-]+/)*[A-Za-z0-9_@+-]+\.(?:test|spec)\.[cm]?[jt]sx?" if client == "web" else r"tests/(?:[A-Za-z0-9_@+-]+/)*[A-Za-z0-9_@+-]+\.test\.mjs"
    check(re.fullmatch(pattern, relative), "不是已知客户端测试文件")
    return value


def valid_title(value):
    return (isinstance(value, str) and 0 < len(value) <= 2048 and value.strip() == value and
            not re.search(r"[\x00-\x1f\x7f\x85\u2028\u2029]", value))


def js_escape(value):
    # Python re.escape also escapes spaces; use the ECMAScript metacharacters only.
    return re.sub(r"([\\^$.*+?()[\]{}|/])", r"\\\1", value)


def signature(client, name, message):
    check(name in (("AssertionError", "TestingLibraryElementError") if client == "web" else ("AssertionError",)), "错误类型不可比较")
    check(isinstance(message, str), "断言消息类型非法")
    message = textwrap.dedent(ANSI.sub("", message)).strip()
    if client == "web":
        message = message.splitlines()[0] if message else ""
    message = "\n".join(line.rstrip() for line in message.splitlines()).strip()
    check(0 < len(message) <= 8192 and not VOLATILE.search(message) and
          not re.search(r"[\x00-\x08\x0b-\x1f\x7f]", message), "断言含超时、基础设施或不可比较数据")
    return {"name": name, "message": message}


def web_signature(message):
    check(isinstance(message, str), "断言消息缺失")
    match = re.match(r"^(AssertionError|TestingLibraryElementError): ([^\n]+)", message.strip())
    check(match, "缺少可比较的命名断言")
    return signature("web", match[1], match[2])


def validate_candidate(candidate):
    check(isinstance(candidate, dict) and candidate.get("kind") == "client", "客户端候选类型非法")
    client = candidate.get("client")
    check(client in PROJECTS and candidate.get("file") == client_file(client, candidate.get("file")), "客户端候选文件非法")
    check(valid_title(candidate.get("test")), "精确测试名非法")
    value = candidate.get("signature")
    check(isinstance(value, dict) and value == signature(client, value.get("name"), value.get("message")), "候选断言签名非法")
    return client


def named(client, file, test, value):
    check(valid_title(test), "缺少精确叶测试名")
    return {"kind": "client", "client": client, "file": client_file(client, file), "test": test, "signature": value}


def unique_candidates(rows, counts):
    return [row for row in rows if counts[(row["file"], row["test"])] == 1]


def web_rows(document):
    check(isinstance(document, dict) and isinstance(document.get("testResults"), list) and
          len(document["testResults"]) <= MAX_FILES, "Vitest JSON 文件清单非法")
    result = []
    for file in document["testResults"]:
        check(isinstance(file, dict) and isinstance(file.get("assertionResults"), list), "Vitest 文件结果非法")
        path = client_file("web", file.get("name"))
        for row in file["assertionResults"]:
            check(isinstance(row, dict) and isinstance(row.get("ancestorTitles"), list) and
                  all(valid_title(name) for name in row["ancestorTitles"]) and valid_title(row.get("title")), "Vitest 叶测试身份非法")
            title = " ".join([*row["ancestorTitles"], row["title"]])
            check(valid_title(title) and row.get("fullName") == title, "Vitest 完整测试名不符")
            check(isinstance(row.get("failureMessages"), list) and all(isinstance(value, str) for value in row["failureMessages"]), "Vitest 断言消息非法")
            result.append((path, title, row, file))
            check(len(result) <= MAX_TESTS, "Vitest 测试数量超过上限")
    return result


def web_failures(text):
    if text.lstrip().startswith("{"):
        document = strict_json(text)
        check(isinstance(document, dict) and isinstance(document.get("testResults"), list) and
              len(document["testResults"]) <= MAX_FILES, "Vitest JSON 文件清单非法")
        rows = []
        # Discovery tolerates an unrelated malformed row; proof checking does not.
        for file in document["testResults"]:
            if not isinstance(file, dict) or not isinstance(file.get("assertionResults"), list):
                continue
            check(len(file["assertionResults"]) + len(rows) <= MAX_TESTS, "Vitest 测试数量超过上限")
            for row in file["assertionResults"]:
                try:
                    rows.extend(web_rows({"testResults": [{**file, "assertionResults": [row]}]}))
                except (ValueError, TypeError, KeyError):
                    continue
        counts = Counter((path, title) for path, title, _, _ in rows)
        found = []
        for path, title, row, file in rows:
            try:
                if row.get("status") == "failed" and file.get("status") == "failed" and len(row["failureMessages"]) == 1:
                    found.append(named("web", path, title, web_signature(row["failureMessages"][0])))
            except ValueError:
                continue
        return unique_candidates(found, counts)
    headers = list(re.finditer(r"(?m)^\s*FAIL\s+(\S+) > (.+)$", text))
    found, counts = [], Counter()
    for index, header in enumerate(headers):
        try:
            path = client_file("web", header[1])
            title = " ".join(header[2].split(" > "))
            check(valid_title(title), "Vitest 完整测试名非法")
            counts[(path, title)] += 1
            block = text[header.end():headers[index + 1].start() if index + 1 < len(headers) else len(text)]
            diagnostic = next((line.strip() for line in block.splitlines() if line.strip()), "")
            found.append(named("web", path, title, web_signature(diagnostic)))
        except ValueError:
            continue
    return unique_candidates(found, counts)


def tap_name(value):
    # Node escapes TAP syntax characters; do not interpret arbitrary JS/YAML escapes.
    value = re.sub(r"\\([#\\])", r"\1", value)
    check(valid_title(value), "TAP 测试名非法")
    return value


def yaml_scalar(value):
    if value.startswith("'") and value.endswith("'"):
        return value[1:-1].replace("''", "'")
    if value.startswith('"'):
        result = strict_json(value)
        check(isinstance(result, str), "TAP 字符串非法")
        return result
    if re.fullmatch(r"-?\d+(?:\.\d+)?", value):
        return float(value) if "." in value else int(value)
    if value in ("true", "false", "null"):
        return {"true": True, "false": False, "null": None}[value]
    check(value and not re.search(r"[{}\[\]&*!]", value), "TAP 标量格式不受支持")
    return value


def tap_diagnostic(lines, start, indent):
    fields, index = {}, start + 1
    prefix = " " * indent
    while index < len(lines) and lines[index] != prefix + "...":
        match = re.fullmatch(re.escape(prefix) + r"([A-Za-z_][A-Za-z0-9_]*):(?: (.*))?", lines[index])
        check(match and match[1] not in fields, "TAP 诊断字段非法或重复")
        key, value = match[1], match[2] or ""
        index += 1
        if value in ("|-", "|"):
            content = []
            while index < len(lines) and (lines[index].startswith(prefix + "  ") or not lines[index]):
                content.append(lines[index][indent + 2:] if lines[index] else "")
                index += 1
            fields[key] = "\n".join(content).rstrip("\n")
        elif value == "":
            # Object expected/actual/stack details are not used as a signature.
            while index < len(lines) and lines[index].startswith(prefix + "  "):
                index += 1
            fields[key] = None
        else:
            try:
                fields[key] = yaml_scalar(value)
            except ValueError:
                # Native reporters can emit arbitrary expected/actual YAML objects.
                # They cannot establish identity or the assertion message.
                check(key not in ("type", "location", "failureType", "error", "code", "name", "duration_ms"), "TAP 身份字段格式不受支持")
                fields[key] = None
    check(index < len(lines), "TAP 诊断未终止")
    return fields, index + 1


def tap_records(text):
    """Retain completed leaves on a later malformed/unfinished unrelated record."""
    lines = text.splitlines()
    records, pending, ordinals, plans, summary = [], {}, Counter(), {}, {}
    complete, error, index = True, "", 0
    try:
        starts = [i for i, line in enumerate(lines) if line == "TAP version 13"]
        check(len(starts) == 1, "缺少唯一 TAP 版本头")
        # npm banners before TAP are allowed for discovery, never for replay.
        index = starts[0] + 1
        while index < len(lines):
            line = lines[index]
            index += 1
            if not line:
                continue
            subtest = re.fullmatch(r"( *)# Subtest: (.+)", line)
            if subtest:
                level = len(subtest[1])
                check(level % 4 == 0 and level not in pending and level not in plans and len(records) < MAX_TESTS, "TAP start 重复或层级非法")
                check(level == 0 or level - 4 in pending, "TAP 子测试缺少活动父级")
                ancestors = pending[level - 4]["names"] if level else []
                pending[level] = {"name": tap_name(subtest[2]), "names": [*ancestors, tap_name(subtest[2])], "children": False}
                if level:
                    pending[level - 4]["children"] = True
                continue
            terminal = re.fullmatch(r"( *)(not ok|ok) ([1-9][0-9]*) - (.+?)(?: # (SKIP|TODO)(?: .*)?)?", line)
            if terminal:
                level = len(terminal[1])
                check(level in pending and not any(depth > level for depth in pending), "TAP 终止事件缺少对应 start")
                item = pending.pop(level)
                check(tap_name(terminal[4]) == item["name"] and int(terminal[3]) == ordinals[level] + 1 and level not in plans, "TAP 结果名、序号或 plan 不符")
                ordinals[level] += 1
                fields = {}
                if index < len(lines) and lines[index] == " " * (level + 2) + "---":
                    fields, index = tap_diagnostic(lines, index, level + 2)
                if item["children"]:
                    check(plans.get(level + 4) == ordinals[level + 4], "TAP 子级 plan 缺失")
                    plans.pop(level + 4)
                    ordinals.pop(level + 4)
                records.append({**item, "test": " ".join(item["names"]), "status": terminal[2],
                                "directive": terminal[5], "fields": fields})
                continue
            plan = re.fullmatch(r"( *)1\.\.([0-9]+)(?: # .*)?", line)
            if plan:
                level = len(plan[1])
                check(level not in plans and level not in pending and int(plan[2]) == ordinals[level], "TAP plan 缺失结果或重复")
                plans[level] = int(plan[2])
                continue
            total = re.fullmatch(r"# (tests|suites|pass|fail|cancelled|skipped|todo|duration_ms) ([0-9]+(?:\.[0-9]+)?)", line)
            if total:
                check(total[1] not in summary, "TAP 汇总字段重复")
                summary[total[1]] = float(total[2]) if "." in total[2] else int(total[2])
                continue
            check(re.match(r" *#(?: |$)", line) and not re.search(r"uncaughtException|unhandledRejection|asynchronous activity|Warning:.*(?:Error|failed)", line, re.I), "TAP 含无法关联的输出或异步错误")
        check(not pending and plans == {0: ordinals[0]}, "TAP 生命周期或根 plan 不完整")
    except (ValueError, TypeError, KeyError) as failure:
        complete, error = False, str(failure)
    return records, summary, complete, error


def node_signature(record):
    fields = record["fields"]
    check(not record["children"] and record["status"] == "not ok" and not record["directive"] and
          fields.get("type", "test") == "test" and fields.get("failureType") == "testCodeFailure" and
          fields.get("code") == "ERR_ASSERTION" and fields.get("name") == "AssertionError", "不是精确叶测试的可比较断言")
    return signature("bridge", "AssertionError", fields.get("error"))


def location_file(value):
    check(isinstance(value, str), "Node 缺少可验证的源码位置")
    match = re.fullmatch(r"(.+):[1-9][0-9]*:[1-9][0-9]*", value)
    check(match, "Node 源码位置非法")
    return client_file("bridge", match[1])


def node_failures(text):
    if re.search(r"(?m)^TAP version 13$", text):
        records, _, _, _ = tap_records(text)
        counts, found = Counter(), []
        for record in records:
            try:
                file = location_file(record["fields"].get("location"))
                counts[(file, record["test"])] += 1
                found.append(named("bridge", file, record["test"], node_signature(record)))
            except ValueError:
                continue
        # Duplicate passes have no location in native TAP: name duplication is also unsafe.
        names = Counter(record["test"] for record in records if not record["children"])
        return [row for row in unique_candidates(found, counts) if names[row["test"]] == 1]
    marker = re.search(r"(?m)^[✖×] failing tests:$", text)
    if not marker:
        return []
    lifecycle_rows = list(re.finditer(r"(?m)^( *)(?:✖|×) (.+) \([0-9.]+m?s\)$", text[:marker.start()]))
    lifecycle = Counter(match[2] for match in lifecycle_rows if not match[1])
    all_names = Counter(match[2] for match in lifecycle_rows)
    groups = set(re.findall(r"(?m)^▶ (.+)$", text[:marker.start()]))
    blocks = list(re.finditer(r"(?m)^test at (.+:[0-9]+:[0-9]+)\n[✖×] (.+) \([0-9.]+m?s\)\n", text[marker.end():]))
    suffix, found, counts = text[marker.end():], [], Counter()
    for index, block in enumerate(blocks):
        try:
            file, title = location_file(block[1]), block[2]
            counts[(file, title)] += 1
            check(lifecycle[title] == 1 and all_names[title] == 1 and title not in groups, "Node 默认报告缺少无歧义的叶测试生命周期")
            body = textwrap.dedent(suffix[block.end():blocks[index + 1].start() if index + 1 < len(blocks) else len(suffix)]).strip()
            match = re.match(r"AssertionError \[ERR_ASSERTION\]: ([\s\S]*?)(?:\n\s+at |$)", body)
            check(match, "Node 默认报告缺少可比较断言")
            found.append(named("bridge", file, title, signature("bridge", "AssertionError", match[1])))
        except ValueError:
            continue
    return unique_candidates(found, counts)


def failures(client, log):
    """Parse only authenticated failed-step stdout supplied by the caller."""
    try:
        text = cleaned(log)
        return web_failures(text) if client == "web" else node_failures(text) if client == "bridge" else []
    except (ValueError, TypeError, KeyError, RecursionError):
        return []


def assess_web(report, code, candidate, stdout="", expected_file=None):
    try:
        check(validate_candidate(candidate) == "web", "不是 Web 候选")
        document = strict_json(cleaned(report))
        rows = web_rows(document)
        check(type(code) is int and code in (0, 1) and len(document["testResults"]) == 1, "Vitest 执行异常或匹配多个文件")
        check(not re.search(r"Unhandled (?:Error|Rejection)|unhandled errors|still running|worker.*(?:exit|kill)|build failed", cleaned(stdout), re.I), "Vitest 有未关联错误")
        check(all(type(document.get(key)) is int and document[key] >= 0 for key in
                  ("numPendingTestSuites", "numTotalTestSuites", "numFailedTestSuites", "numPassedTestSuites")) and
              document["numPendingTestSuites"] == 0 and document["numTotalTestSuites"] > 0 and
              document["numFailedTestSuites"] + document["numPassedTestSuites"] == document["numTotalTestSuites"], "Vitest suite 汇总不完整")
        matches = [row for path, title, row, _ in rows if path == candidate["file"] and title == candidate["test"]]
        check(len(matches) == 1 and all(path == candidate["file"] for path, _, _, _ in rows), "Vitest 没有唯一精确叶测试")
        target = matches[0]
        check(all(row is target or row.get("status") in ("skipped", "pending", "todo") for _, _, row, _ in rows), "Vitest 执行了其他测试或有其他失败")
        states = Counter(row.get("status") for _, _, row, _ in rows)
        check(not set(states) - {"passed", "failed", "skipped", "pending", "todo"}, "Vitest 测试状态非法")
        expected = {"numTotalTests": len(rows), "numPassedTests": states["passed"], "numFailedTests": states["failed"],
                    "numPendingTests": states["pending"] + states["skipped"], "numTodoTests": states["todo"]}
        check(all(type(document.get(key)) is int and document[key] == value for key, value in expected.items()), "Vitest 测试汇总不符")
        file = document["testResults"][0]
        check(not file.get("message") and (expected_file is None or file.get("name") == expected_file), "Vitest 文件有编译错误或身份不符")
        check(all(isinstance(file.get(key), (int, float)) and not isinstance(file[key], bool) and math.isfinite(file[key]) for key in ("startTime", "endTime")) and file["endTime"] >= file["startTime"], "Vitest 文件生命周期不完整")
        check(isinstance(target.get("duration"), (int, float)) and not isinstance(target["duration"], bool) and
              math.isfinite(target["duration"]) and target["duration"] >= 0, "Vitest 叶测试没有执行时长")
        snapshot = document.get("snapshot")
        check(isinstance(snapshot, dict) and not snapshot.get("failure") and not snapshot.get("unmatched") and not snapshot.get("unchecked"), "Vitest snapshot 证据不可比较")
        if code == 0:
            check(target.get("status") == "passed" and not target["failureMessages"] and document.get("success") is True and
                  file.get("status") == "passed" and document["numFailedTestSuites"] == 0, "Vitest 通过证据不完整或测试跳过")
            return {"state": "pass"}
        check(target.get("status") == "failed" and document.get("success") is False and file.get("status") == "failed" and
              document["numFailedTestSuites"] > 0 and len(target["failureMessages"]) == 1, "Vitest 命名失败证据不完整")
        return {"state": "fail", "signature": web_signature(target["failureMessages"][0])}
    except (ValueError, TypeError, KeyError, RecursionError) as error:
        return unknown("Web 执行证据不足：" + str(error))


def assess_tap(text, code, candidate):
    try:
        check(validate_candidate(candidate) == "bridge", "不是 bridge 候选")
        text = cleaned(text)
        check(text.startswith("TAP version 13\n") and type(code) is int and code in (0, 1), "Node 执行异常或不是原生 TAP")
        records, summary, complete, error = tap_records(text)
        check(complete, "Node TAP 不完整：" + error)
        leaves = [row for row in records if not row["children"] and row["fields"].get("type", "test") == "test"]
        check(len(leaves) == 1 and leaves[0]["test"] == candidate["test"], "Node 没有唯一精确叶测试或执行了其他测试")
        target = leaves[0]
        check(all(not row["directive"] for row in records), "Node 测试跳过或 todo")
        check(all(row is target or row["children"] and row["fields"].get("type") == "suite" and
                  candidate["test"].startswith(row["test"] + " ") for row in records), "Node 执行了其他匹配项")
        tests = [row for row in records if row["fields"].get("type", "test") == "test"]
        expected = {"tests": len(tests), "suites": len(records) - len(tests),
                    "pass": sum(row["status"] == "ok" for row in tests), "fail": sum(row["status"] == "not ok" for row in tests),
                    "cancelled": 0, "skipped": 0, "todo": 0}
        check(all(type(summary.get(key)) is int and summary[key] == value for key, value in expected.items()) and
              isinstance(summary.get("duration_ms"), (int, float)), "Node 汇总与命名生命周期不符")
        if code == 0:
            check(all(row["status"] == "ok" and not row["fields"].get("error") and not row["fields"].get("failureType") for row in records), "Node 通过证据不完整")
            return {"state": "pass"}
        check(location_file(target["fields"].get("location")) == candidate["file"], "Node 失败文件不符")
        check(all(row is target or row["status"] == "not ok" and row["fields"].get("failureType") == "subtestsFailed" for row in records), "Node suite 有其他错误")
        return {"state": "fail", "signature": node_signature(target)}
    except (ValueError, TypeError, KeyError, RecursionError) as error:
        return unknown("Node 执行证据不足：" + str(error))


def child_env(home):
    home = Path(home)
    return {"PATH": str(home / "bin") + ":/usr/bin:/bin", "HOME": str(home), "TMPDIR": str(home),
            "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "CI": "true", "NO_COLOR": "1", "TZ": "UTC",
            "XDG_CONFIG_HOME": str(home / "config"), "XDG_CACHE_HOME": str(home / "cache"),
            "npm_config_cache": str(home / "npm-cache"), "npm_config_userconfig": str(home / "npmrc"),
            "npm_config_globalconfig": str(home / "global-npmrc"), "npm_config_registry": "https://registry.npmjs.org/",
            "npm_config_ignore_scripts": "true", "npm_config_engine_strict": "true", "npm_config_update_notifier": "false",
            "npm_config_fetch_retries": "0", "npm_config_fetch_timeout": "30000", "npm_config_audit": "false",
            "npm_config_fund": "false", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_TERMINAL_PROMPT": "0"}


def validate_lock(package, lock, client):
    check(isinstance(package, dict) and package.get("name") == NAMES[client] and
          isinstance(package.get("scripts"), dict) and package["scripts"].get("test") == SCRIPTS[client], "客户端 package.json 身份或测试命令不符")
    check(isinstance(lock, dict) and lock.get("name") == package["name"] and type(lock.get("lockfileVersion")) is int and
          lock["lockfileVersion"] in (2, 3) and isinstance(lock.get("packages"), dict) and len(lock["packages"]) <= MAX_FILES, "缺少精确 npm lockfile")
    root = lock["packages"].get("")
    check(isinstance(root, dict) and root.get("name") == package["name"] and not package.get("workspaces"), "lockfile 根包身份非法")
    for key in ("dependencies", "devDependencies", "optionalDependencies", "peerDependencies"):
        check(package.get(key, {}) == root.get(key, {}), "lockfile 与 package.json 依赖不符")
    for path, entry in lock["packages"].items():
        if not path:
            continue
        check(isinstance(entry, dict) and not entry.get("link") and isinstance(path, str) and
              re.fullmatch(r"node_modules/(?:[A-Za-z0-9_@.+-]+/)*[A-Za-z0-9_@.+-]+", path) and ".." not in path.split("/"), "lockfile 包路径或链接不安全")
        check(isinstance(entry.get("resolved"), str), "lockfile resolved 类型非法")
        url = urlsplit(entry["resolved"])
        check(url.scheme == "https" and url.netloc == "registry.npmjs.org" and not url.username and
              not url.password and not url.query and not url.fragment and url.path.endswith(".tgz") and
              not re.search(r"[\s\\]", url.path) and ".." not in url.path.split("/"), "lockfile 不是无凭据的公开 npm 源")
        check(isinstance(entry.get("integrity"), str) and re.fullmatch(r"sha(?:256|384|512)-[A-Za-z0-9+/]+=*", entry["integrity"]), "lockfile 缺少完整性校验")


def archive_files(data, project):
    check(isinstance(data, bytes) and len(data) <= MAX_ARCHIVE, "源码归档超过上限")
    files, seen = {}, set()
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        for member in archive:
            path = member.name.rstrip("/")
            check(len(seen) < MAX_FILES and path not in seen and safe_path(path), "归档路径不安全、重复或超过数量上限")
            seen.add(path)
            check(member.isdir() or member.isfile() and not member.sparse, "归档包含链接或非普通文件")
            if member.isdir():
                check(path == project or path.startswith(project + "/") or project.startswith(path + "/"), "归档目录不在已知包内")
                continue
            check(path.startswith(project + "/") and member.size <= MAX_LOG, "归档文件不在已知包内或过大")
            check(not any(part in (".npmrc", ".yarnrc", ".yarnrc.yml", ".pnpmfile.cjs") or part.startswith(".env") for part in path.split("/")), "源码包含可能携带凭据的本地配置")
            files[path] = archive.extractfile(member).read(MAX_LOG + 1)
    return files


def inventory_files(data, project):
    check(len(data) <= MAX_LOG, "Git 文件清单超过上限")
    records = data.decode("utf-8").split("\0")
    check(records[-1] == "" and len(records) <= MAX_FILES + 1, "Git 文件清单不完整")
    result = {}
    for record in records[:-1]:
        match = re.fullmatch(r"(100(?:644|755)) blob (" + SHA_RE + r")\t(.+)", record)
        check(match and safe_path(match[3]) and match[3].startswith(project + "/") and match[3] not in result, "Git 清单含链接、子模块或非法路径")
        result[match[3]] = (match[1], match[2])
    return result


def source_definition(archive, inventory, candidate):
    project = PROJECTS[candidate["client"]]
    files, entries = archive_files(archive, project), inventory_files(inventory, project)
    check(files.keys() == entries.keys(), "Git 归档与 tree 文件清单不符（可能有 export-ignore）")
    for path, data in files.items():
        check(hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest() == entries[path][1], "归档源码与 Git blob 不符")
    paths = [project + "/package.json", project + "/package-lock.json"]
    check(all(path in files for path in paths), "缺少客户端 package.json 或精确 package-lock.json")
    validate_lock(strict_json(files[paths[0]]), strict_json(files[paths[1]]), candidate["client"])
    if candidate["file"] in files:
        paths.append(candidate["file"])
    return {"state": "present" if candidate["file"] in files else "absent", "files": {path: digest(files[path]) for path in paths}}, files


def git_argv(binary, root, *args):
    return [binary, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", str(root), *args]


def source_commands(binary, root, sha, client):
    return {"tree": git_argv(binary, root, "rev-parse", "--verify", sha + "^{tree}"),
            "inventory": git_argv(binary, root, "ls-tree", "-rz", "--full-tree", sha, "--", PROJECTS[client]),
            "archive": git_argv(binary, root, "archive", "--format=tar", sha, "--", PROJECTS[client])}


def install_argv(node, npm):
    return [node, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--include=dev",
            "--package-lock=true", "--registry=https://registry.npmjs.org/"]


def run_argv(node, candidate, report=None):
    project = PROJECTS[candidate["client"]]
    relative = candidate["file"][len(project) + 1:]
    pattern = "^" + js_escape(candidate["test"]) + "$"
    if candidate["client"] == "bridge":
        return [node, "--test", "--test-reporter=tap", "--test-name-pattern=" + pattern, relative]
    return [node, "node_modules/vitest/vitest.mjs", "run", relative, "--testNamePattern=" + pattern,
            "--reporter=json", "--outputFile=" + str(report), "--retry=0", "--no-file-parallelism", "--no-color"]


def version_ok(client, text):
    return bool(re.fullmatch(r"v22\.\d+\.\d+\n?", text) if client == "web" else re.fullmatch(r"v24\.13\.0\n?", text))


def read_bounded(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        check(stat.S_ISREG(os.fstat(stream.fileno()).st_mode), "证据不是普通文件")
        data = stream.read(limit + 1)
    check(len(data) <= limit, "证据文件超过上限")
    return data


def process_to_file(argv, cwd, env, path, timeout, limit, watches=()):
    """No shell, bounded disk output, deadline, and process-group cleanup."""
    record = {"argv": list(argv), "cwd": str(cwd), "log": path.name}
    check(timeout > 0, "回放时间预算耗尽")
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        process = subprocess.Popen(argv, cwd=cwd, env=env, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        deadline = time.monotonic() + timeout
        try:
            while True:
                exceeded = path.stat().st_size > limit or any(item.exists() and (item.is_symlink() or item.stat().st_size > MAX_LOG) for item in watches)
                timed_out = time.monotonic() >= deadline
                ended = process.poll() is not None
                if exceeded or timed_out or ended:
                    if exceeded or timed_out:
                        record.update(state="unknown", reason="客户端回放超时或输出超过上限", timed_out=timed_out, oversized=exceeded)
                    break
                time.sleep(0.02)
        finally:
            # Clean up descendants even on interruption, an I/O error, or parent exit.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
    if path.stat().st_size > limit:
        record.update(state="unknown", reason="客户端输出超过上限", oversized=True)
        with path.open("r+b") as output:
            output.truncate(limit + 1)
    data = read_bounded(path, limit + 1)
    record.update(returncode=process.returncode, sha256=digest(data), bytes=len(data))
    return record, data


class ClientProbe:
    def __init__(self, root, output, timeout=90, budget=1200):
        check(timeout > 0 and budget > 0, "回放时间上限非法")
        self.root, self.output = Path(root).resolve(), Path(output).absolute()
        check(not self.output.is_symlink(), "证据目录不能是符号链接")
        self.output.mkdir(parents=True, mode=0o700, exist_ok=True)
        self.timeout, self.deadline = timeout, time.monotonic() + budget
        self.session, self.index, self.serial = uuid.uuid4().hex[:12], 0, 0
        self.output_bytes = 0
        self.home = tempfile.TemporaryDirectory(prefix="periodic-clients-")
        self.env = child_env(Path(self.home.name))
        self.mutex = threading.Lock()
        self.hashes = {}
        self.closed = False

    def close(self):
        with self.mutex:
            self.closed = True
            self.home.cleanup()

    @property
    def probe_id(self):
        return f"{self.session}-{self.index:04d}"

    def artifact(self, label, data):
        check(self.output_bytes + len(data) <= MAX_TOTAL_OUTPUT, "客户端证据总大小达到上限")
        self.serial += 1
        name = f"client-{self.probe_id}-{self.serial:04d}-{label}.bin"
        path = self.output / name
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
        self.output_bytes += len(data)
        return {"log": name, "sha256": digest(data), "bytes": len(data)}

    def artifact_command(self, argv, cwd, label, data, code):
        # Also useful for small mocked native-reporter/source fixtures.
        return {"argv": list(argv), "cwd": str(cwd), "returncode": code, **self.artifact(label, data)}

    def execute(self, argv, cwd, label, limit=None, watches=()):
        self.serial += 1
        path = self.output / f"client-{self.probe_id}-{self.serial:04d}-{label}.bin"
        remaining = min(self.timeout, self.deadline - time.monotonic())
        if remaining <= 0 or self.output_bytes >= MAX_TOTAL_OUTPUT:
            return {"argv": list(argv), "cwd": str(cwd), "state": "unknown", "reason": "回放总时间或证据总大小达到上限"}, b""
        limit = min(MAX_LOG if limit is None else limit, MAX_TOTAL_OUTPUT - self.output_bytes)
        record, data = process_to_file(argv, cwd, self.env, path, remaining, limit, watches)
        self.output_bytes += len(data)
        return record, data

    def tools(self, client, home, result):
        prefix = "PERIODIC_" + client.upper()
        requirement = "22.x" if client == "web" else "24.13.0"
        node, npm = os.environ.get(prefix + "_NODE", ""), os.environ.get(prefix + "_NPM", "")
        check(node and npm and Path(node).is_absolute() and Path(npm).is_absolute(),
              f"需要显式提供 {prefix}_NODE 和 {prefix}_NPM 绝对路径；Node 必须为 {requirement}")
        node, npm = Path(node).resolve(strict=True), Path(npm).resolve(strict=True)
        check(node.is_file() and os.access(node, os.X_OK) and npm.is_file() and npm.name == "npm-cli.js", "Node/npm 工具链路径非法（npm 必须指向 npm-cli.js）")
        (home / "bin").mkdir(mode=0o700)
        (home / "bin/node").symlink_to(node)
        (home / "bin/npm").symlink_to(npm)
        for config in ("npmrc", "global-npmrc"):
            (home / config).touch(mode=0o600)
        tools = {"node": str(node), "npm": str(npm)}
        result["toolchain"] = tools
        for key, path in (("node_sha256", node), ("npm_sha256", npm)):
            stat = path.stat()
            cache_key = (str(path), stat.st_size, stat.st_mtime_ns)
            if cache_key not in self.hashes:
                with path.open("rb") as stream:
                    self.hashes[cache_key] = hashlib.file_digest(stream, "sha256").hexdigest()
            tools[key] = self.hashes[cache_key]
        for key, argv in (("node_version", [str(node), "--version"]), ("npm_version", [str(node), str(npm), "--version"])):
            tools[key], data = self.execute(argv, home, key.replace("_", "-"))
            check(not tools[key].get("state") and tools[key].get("returncode") == 0, "Node/npm 版本探测失败")
            text = data.decode("utf-8")
            check(version_ok(client, text) if key == "node_version" else re.fullmatch(r"\d+\.\d+\.\d+\n?", text), "Node/npm 版本不符合 CI；要求 Node " + requirement)
        return str(node), str(npm)

    def __call__(self, sha, candidate):
        with self.mutex:
            return self.probe(sha, candidate)

    def probe(self, sha, candidate):
        result = {"sha": sha}
        lock = None
        try:
            check(not self.closed, "客户端回放器已经关闭")
            client = validate_candidate(candidate)
            check(isinstance(sha, str) and re.fullmatch(SHA_RE, sha), "回放 SHA 非法")
            check(time.monotonic() < self.deadline, "回放总时间达到上限")
            self.index += 1
            result.update(probe_id=self.probe_id, client=client, repository=str(self.root))
            lock = os.open(self.output / ".periodic-clients.lock", os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
            check(stat.S_ISREG(os.fstat(lock).st_mode), "回放锁不是普通文件")
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ValueError("同一证据目录已有客户端回放，不能共享安装或执行证据")
            with tempfile.TemporaryDirectory(prefix="probe-", dir=self.home.name) as temporary:
                home = Path(temporary).resolve()
                self.env = child_env(home)
                git = shutil.which("git", path=os.defpath)
                check(git, "缺少只读 Git 工具")
                result["git"] = str(Path(git).resolve())
                source = result["source"] = {}
                data = {}
                for label, argv in source_commands(result["git"], self.root, sha, client).items():
                    source[label], data[label] = self.execute(argv, self.root, label, limit=MAX_ARCHIVE if label == "archive" else MAX_LOG)
                    check(not source[label].get("state") and source[label].get("returncode") == 0, "只读 Git 源码取证失败")
                tree = data["tree"].decode().strip()
                check(re.fullmatch(SHA_RE, tree), "回放 tree 身份非法")
                result["tree"] = tree
                result["definition"], files = source_definition(data["archive"], data["inventory"], candidate)
                if result["definition"]["state"] == "absent":
                    return {**result, "state": "absent"}
                checkout = home / "source"
                checkout.mkdir(mode=0o700)
                entries = inventory_files(data["inventory"], PROJECTS[client])
                for path, content in files.items():
                    target = checkout / path
                    target.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
                    target.write_bytes(content)
                    target.chmod(int(entries[path][0][-3:], 8))
                result["checkout"] = str(checkout)
                node, npm = self.tools(client, home, result)
                cwd = checkout / PROJECTS[client]
                result["install"], _ = self.execute(install_argv(node, npm), cwd, "install")
                check(not result["install"].get("state") and result["install"].get("returncode") == 0, "精确 lockfile 安装失败；不能使用已有依赖或其他工具链")
                report = home / "vitest-report.json"
                result["report_path"] = str(report)
                result["run"], text = self.execute(run_argv(node, candidate, report), cwd, "run", watches=(report,) if client == "web" else ())
                check(not result["run"].get("state"), "客户端测试超时或输出超过上限")
                for path, value in result["definition"]["files"].items():
                    check(digest(read_bounded(checkout / path, MAX_LOG)) == value, "安装或测试修改了输入源码/lockfile")
                if client == "web":
                    raw = read_bounded(report, MAX_LOG)
                    result["report"] = self.artifact("report", raw)
                    assessed = assess_web(raw.decode(), result["run"]["returncode"], candidate, text.decode(), str(checkout / candidate["file"]))
                else:
                    assessed = assess_tap(text.decode(), result["run"]["returncode"], candidate)
                return {**result, **assessed}
        except (ValueError, OSError, subprocess.SubprocessError, TypeError, KeyError, tarfile.TarError, RecursionError) as error:
            return {**result, **unknown("客户端回放无法完成：" + str(error))}
        finally:
            if lock is not None:
                os.close(lock)
            self.env = child_env(Path(self.home.name))

    def verify_probe(self, result, candidate, directory, seen=None):
        return verify_probe(result, candidate, directory, root=self.root, seen=seen)


def verify_probe(result, candidate, directory, root=None, seen=None):
    """Return reconstructed state/signature, or unknown on any proof mismatch."""
    try:
        client = validate_candidate(candidate)
        check(isinstance(result, dict) and result.get("client") == client and result.get("state") in ("pass", "fail", "absent"), "probe 状态或客户端身份不可核验")
        check(re.fullmatch(SHA_RE, result.get("sha", "")) and re.fullmatch(SHA_RE, result.get("tree", "")), "probe SHA/tree 非法")
        probe_id = result.get("probe_id", "")
        check(re.fullmatch(r"[0-9a-f]{12}-[0-9]{4}", probe_id), "probe 执行身份非法")
        seen = set() if seen is None else seen
        check(isinstance(seen, set), "重复执行证据集合非法")

        def artifact(evidence, label, limit=MAX_LOG, argv=None, cwd=None):
            check(isinstance(evidence, dict) and not evidence.get("state") and not evidence.get("timed_out") and not evidence.get("oversized"), "执行证据状态异常")
            name = evidence.get("log", "")
            check(isinstance(name, str) and re.fullmatch("client-" + probe_id + r"-[0-9]{4}-" + label + r"\.bin", name) and name not in seen, "执行证据路径非法或重复")
            seen.add(name)
            raw = read_bounded(Path(directory) / name, limit)
            check(evidence.get("sha256") == digest(raw) and type(evidence.get("bytes")) is int and evidence["bytes"] == len(raw), "执行证据大小或摘要不符")
            if argv is not None:
                check(evidence.get("argv") == argv and evidence.get("cwd") == str(cwd) and type(evidence.get("returncode")) is int, "执行 argv、工作目录或退出状态不符")
            return raw

        repository, binary = result.get("repository"), result.get("git")
        check(isinstance(repository, str) and Path(repository).is_absolute() and isinstance(binary, str) and Path(binary).is_absolute(), "probe 仓库或 Git 路径非法")
        data = {}
        for label, argv in source_commands(binary, repository, result["sha"], client).items():
            evidence = result["source"][label]
            data[label] = artifact(evidence, label, MAX_ARCHIVE if label == "archive" else MAX_LOG, argv, repository)
            check(evidence["returncode"] == 0, "只读源码命令失败")
        check(data["tree"] == (result["tree"] + "\n").encode(), "tree 命令证据不符")
        definition, _ = source_definition(data["archive"], data["inventory"], candidate)
        check(result.get("definition") == definition, "源码定义证明不能重建")
        if root is not None:
            check(Path(repository).resolve() == Path(root).resolve(), "probe 不属于可信仓库")
            with tempfile.TemporaryDirectory(prefix="verify-clients-") as temporary:
                home = Path(temporary)
                trusted = shutil.which("git", path=os.defpath)
                check(trusted, "缺少可信只读 Git")
                for label, args in (("tree", ("rev-parse", "--verify", result["sha"] + "^{tree}")),
                                    ("inventory", ("ls-tree", "-rz", "--full-tree", result["sha"], "--", PROJECTS[client]))):
                    proof, raw = process_to_file(git_argv(trusted, root, *args), root, child_env(home), home / (label + ".bin"), 30, MAX_LOG)
                    check(not proof.get("state") and proof["returncode"] == 0 and raw == data[label], "probe tree/源码清单与可信 Git 不符")
        if definition["state"] == "absent":
            check(result["state"] == "absent" and "signature" not in result and "run" not in result, "不存在的测试被声明为已执行")
            return {"state": "absent"}
        checkout = result.get("checkout")
        check(isinstance(checkout, str) and Path(checkout).is_absolute(), "回放 checkout 路径非法")
        cwd = Path(checkout) / PROJECTS[client]
        tools = result["toolchain"]
        node, npm = tools["node"], tools["npm"]
        check(all(isinstance(path, str) and Path(path).is_absolute() for path in (node, npm)) and Path(npm).name == "npm-cli.js", "工具链绝对路径非法")
        check(all(isinstance(tools.get(key), str) and re.fullmatch(r"[0-9a-f]{64}", tools[key]) for key in ("node_sha256", "npm_sha256")), "缺少工具链摘要")
        home = Path(checkout).parent
        for key, argv in (("node_version", [node, "--version"]), ("npm_version", [node, npm, "--version"])):
            evidence = tools[key]
            raw = artifact(evidence, key.replace("_", "-"), argv=argv, cwd=home)
            check(evidence["returncode"] == 0 and (version_ok(client, raw.decode()) if key == "node_version" else re.fullmatch(r"\d+\.\d+\.\d+\n?", raw.decode())), "工具链版本不符合 CI")
        artifact(result["install"], "install", argv=install_argv(node, npm), cwd=cwd)
        check(result["install"]["returncode"] == 0, "lockfile 安装未成功")
        report = home / "vitest-report.json"
        check(result.get("report_path") == str(report), "结构化报告路径不符")
        raw = artifact(result["run"], "run", argv=run_argv(node, candidate, report), cwd=cwd)
        if client == "web":
            report_bytes = artifact(result["report"], "report")
            assessed = assess_web(report_bytes.decode(), result["run"]["returncode"], candidate, raw.decode(), str(Path(checkout) / candidate["file"]))
        else:
            assessed = assess_tap(raw.decode(), result["run"]["returncode"], candidate)
        check(assessed["state"] != "unknown" and assessed["state"] == result["state"] and assessed.get("signature") == result.get("signature"), "命名生命周期或签名不能从外部证据重建")
        return assessed
    except (ValueError, OSError, TypeError, KeyError, tarfile.TarError, RecursionError) as error:
        return unknown("客户端回放证明不足：" + str(error))
