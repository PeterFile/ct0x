#!/usr/bin/env python3
"""Experimental read-only historical evidence; never a product/required-check controller."""
import argparse
import ast
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request


REPOSITORY = "PeterFile/ct0x"
REF = "refs/heads/codex/ci-historical-audit-20261126"
POLICY_REF = "refs/heads/codex/ci-merge-gate-20261126"
PUBLIC_BASE = "18d11a3c0176fe98856aeeef6396e9ddfde0cca0"
REMOTE = "https://github.com/PeterFile/ct0x.git"
SHA = r"[0-9a-f]{40}"
HERE = Path(__file__).resolve().parent


def check(condition, message):
    if not condition:
        raise ValueError(message)


def decode(text):
    def fields(pairs):
        result = dict(pairs)
        check(len(result) == len(pairs), "JSON 字段重复")
        return result
    return json.loads(text, object_pairs_hook=fields,
                      parse_constant=lambda _: check(False, "JSON 常量非法"))


def valid_sha(value):
    return isinstance(value, str) and re.fullmatch(SHA, value) is not None


def request(inputs, size):
    check(isinstance(inputs, dict) and not inputs.keys() - {"policy_sha", "phase", "offset", "count", "focus_sha"}, "输入字段非法")
    check(valid_sha(inputs.get("policy_sha")), "policy_sha 必须为小写四十位 SHA")
    check(inputs.get("phase") in ("selector", "diagnostics", "both", "misses"), "phase 非法")
    result = {"policy_sha": inputs["policy_sha"], "phase": inputs["phase"]}
    focus = inputs.get("focus_sha", "")
    check(isinstance(focus, str), "focus_sha 必须为空或冻结清单内的小写四十位 SHA")
    if focus:
        check(valid_sha(focus), "focus_sha 必须为空或小写四十位 SHA")
        cases = manifest((HERE / "ci-historical-cases.json").read_text(encoding="utf-8"))
        check(focus in {row["sha"] for row in cases}, "focus_sha 不在冻结历史清单中")
        if inputs["phase"] == "misses":
            check(focus in MISSES, "misses focus_sha 必须是三例固定历史漏选原件之一")
        else:
            check(inputs["phase"] == "selector" or focus in DIAGNOSTICS, "诊断 focus_sha 必须是已有固定诊断原件")
        result["focus_sha"] = focus
    for key, default, low, high in (("offset", "0", 0, size - 1), ("count", "150", 1, 150)):
        value = inputs.get(key, default)
        if value == "":
            value = default
        check(isinstance(value, str) and re.fullmatch(r"0|[1-9][0-9]{0,2}", value) is not None,
              f"{key} 必须为有界十进制整数")
        number = int(value)
        check(low <= number <= high, f"{key} 越界")
        result[key] = number
    return result


def manifest(text):
    rows = decode(text)
    check(isinstance(rows, list) and rows, "历史清单为空或非法")
    seen, runs = set(), set()
    for row in rows:
        check(isinstance(row, dict) and set(row) == {"sha", "tree", "base", "parents", "runs"}, "清单包含非公开身份字段")
        check(all(valid_sha(row[key]) for key in ("sha", "tree", "base")), "清单 SHA 非法")
        check(isinstance(row["parents"], list) and row["parents"] and all(map(valid_sha, row["parents"]))
              and row["base"] == row["parents"][0], "第一亲或完整亲列表非法")
        check(row["sha"] not in seen and isinstance(row["runs"], list) and row["runs"], "checkout 重复或缺少原运行")
        seen.add(row["sha"])
        for pair in row["runs"]:
            check(isinstance(pair, list) and len(pair) == 2 and all(type(n) is int and n > 0 for n in pair), "原运行身份非法")
            check(tuple(pair) not in runs, "原运行/尝试重复")
            runs.add(tuple(pair))
    return rows


def select_sources(cases, options):
    focus = options.get("focus_sha")
    if options["phase"] == "misses":
        by_sha = {row["sha"]: row for row in cases}
        if focus:
            check(focus in MISSES, "misses focus_sha 必须是三例固定历史漏选原件之一")
        shas = [focus] if focus else list(MISSES)
        check(shas and all(sha in by_sha for sha in shas), "misses 原件不在冻结清单")
        return [by_sha[sha] for sha in shas], set()
    if focus:
        focused = [row for row in cases if row["sha"] == focus]
        check(len(focused) == 1, "focus_sha 未对应唯一冻结原件；不能忽略或回退到全清单")
        check(options["phase"] == "selector" or focus in DIAGNOSTICS, "诊断 focus_sha 不是固定诊断原件")
        return ([] if options["phase"] == "diagnostics" else focused,
                set() if options["phase"] == "selector" else {focus})
    selected = cases[options["offset"]:options["offset"] + options["count"]] if options["phase"] != "diagnostics" else []
    return selected, set(DIAGNOSTICS) if options["phase"] != "selector" else set()


def commit(raw, sha):
    check(valid_sha(sha) and hashlib.sha1(b"commit " + str(len(raw)).encode() + b"\0" + raw).hexdigest() == sha,
          "原生 commit 对象哈希不符")
    check(b"\n\n" in raw, "commit 头不完整")
    headers = raw.split(b"\n\n", 1)[0].splitlines()
    trees = [line[5:].decode("ascii") for line in headers if line.startswith(b"tree ")]
    parents = [line[7:].decode("ascii") for line in headers if line.startswith(b"parent ")]
    check(len(trees) == 1 and valid_sha(trees[0]) and all(map(valid_sha, parents)), "commit tree/parents 非法")
    return {"sha": sha, "tree": trees[0], "parents": parents}


def same_source(actual, frozen, head):
    check(actual == {key: frozen[key] for key in ("sha", "tree", "parents")} and head == frozen["sha"],
          "实际 SHA/tree/有序 parents/HEAD 与冻结原提交不符")
    check(actual["parents"] and actual["parents"][0] == frozen["base"], "第一亲不符")


def selector(text, rc):
    selected = decode(text)
    check(isinstance(selected, dict) and isinstance(selected.get("unresolved"), list)
          and all(isinstance(name, str) for name in selected["unresolved"])
          and isinstance(selected.get("tasks"), list) and all(isinstance(task, dict) for task in selected["tasks"]),
          "inspect JSON 缺少实际 unresolved/tasks")
    refused = bool(selected["unresolved"]) or not selected["tasks"]
    check(not selected["unresolved"] or not selected["tasks"], "拒绝结果仍含可执行任务")
    check(rc == 0 or rc == 1 and refused, "inspect 非正常退出，不能作为有效选择")
    return {"status": "policy-refusal" if refused else "selected", "selection": selected}


def go_events(text, package, names):
    tests = {name: dict.fromkeys(("run", "pass", "fail", "skip"), 0) for name in names}
    started, terminal = False, None
    for line in text.splitlines(keepends=True):
        check(line.endswith("\n") and terminal is None, "Go JSON 截断或包完成后仍有事件")
        row = decode(line)
        check(isinstance(row, dict) and row.get("Package") == package, "Go 包身份不符")
        action, name = row.get("Action"), row.get("Test", "")
        check(isinstance(name, str) and action in ("start", "run", "pause", "cont", "pass", "fail", "skip", "output", "attr", "artifacts"),
              "Go 事件非法")
        if not started:
            check(action == "start" and not name, "缺少包 start")
            started = True
        elif not name:
            check(action in ("output", "pass", "fail"), "包终态非法")
            if action != "output":
                terminal = action
        else:
            root = name.split("/", 1)[0]
            check(root in tests and action != "start", "出现非选中顶层测试")
            counts = tests[root]
            if name == root and action in counts:
                check(not sum(counts[key] for key in ("pass", "fail", "skip")), "顶层重复终态")
                counts[action] += 1
            check(counts["run"] == 1, "顶层 run 缺失或重复")
    check(started and terminal is not None and all(counts["run"] == 1 and
          sum(counts[key] for key in ("pass", "fail", "skip")) == 1 for counts in tests.values()), "顶层实际 run/终态覆盖不完整")
    return {"package_result": terminal, "tests": tests}


def race_outcome(summary, rc):
    counts = summary["tests"].values()
    if rc == 0 and summary["package_result"] == "pass" and all(row["pass"] == 1 for row in counts):
        return "complete-no-red"
    if rc == 1 and summary["package_result"] == "fail" and any(row["fail"] for row in counts) and not any(row["skip"] for row in counts):
        return "red-reproduced"
    return "unknown"


def bind_run(api, expected):
    check(isinstance(api, dict) and type(api.get("id")) is int and api["id"] == int(expected["run_id"])
          and type(api.get("run_attempt")) is int and api["run_attempt"] == int(expected["run_attempt"])
          and api.get("event") == "workflow_dispatch" and api.get("head_sha") == expected["sha"]
          and api.get("head_branch") == REF.removeprefix("refs/heads/")
          and api.get("path") == ".github/workflows/ci.yml"
          and api.get("repository", {}).get("full_name") == REPOSITORY, "API 运行与 controller SHA/仓库/分支/尝试不符")


def finish(index):
    rows = index["rows"]
    for row in rows:
        if row["source_state"] == "pending":
            row["source_state"] = "unknown"
        for phase in ("selector", "diagnostic"):
            if row[phase]["status"] == "pending":
                row[phase] = {"status": "unknown", "error": "未完成；不能推定成功"}
    exact = sum(row["source_state"] == "exact" for row in rows)
    index["coverage"] = {"requested": len(rows), "exact": exact, "missing": len(rows) - exact}
    index["coverage_note"] = "missing 指缺少完整 checkout 身份证据；未执行/unknown 不代表 GitHub 对象不存在"
    known = {"not-requested", "selected", "policy-refusal", "red-reproduced", "complete-no-red"}
    if index.get("request", {}).get("phase") == "misses":
        known |= {"not-selected", "selected-but-unreached", "original-assertion-unconfirmed", "structural-refusal"}
    complete = bool(rows) and not index["errors"] and exact == len(rows) and all(
        row[phase]["status"] in known for row in rows for phase in ("selector", "diagnostic"))
    index["status"] = "complete" if complete else "infrastructure-error"
    index["finished_at"] = datetime.now(timezone.utc).isoformat()


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=True, sort_keys=True, allow_nan=False) + "\n", encoding="utf-8")
    temporary.replace(path)


# This adapter calls the pinned verifier itself, not a reimplementation of its gate.
VERIFY = '''import importlib.util, json, sys
from collections import Counter
spec = importlib.util.spec_from_file_location("pinned_verifier", sys.argv[1])
v = importlib.util.module_from_spec(spec)
spec.loader.exec_module(v)
try:
    runs, passes = v.verify(sys.argv[2], "agentworkspace/internal/store", Counter({"TestNativeThreadHandoffChild": 1}))
except ValueError as error:
    print(json.dumps({"accepted": False, "exception": "ValueError", "error": str(error)}))
else:
    print(json.dumps({"accepted": True, "runs": runs, "passes": passes}))
'''


# Project only a target unit from the unchanged inspect result. This never approves its refused full plan.
PROJECT = '''import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("pinned_policy", sys.argv[1])
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)
selected = json.load(open(sys.argv[2], encoding="utf-8"))
print(json.dumps(p.tasks({**selected, "unresolved": []})))
'''

# Select the exact emitted unit containing one original test through the pinned planner helpers.
SELECT = '''import importlib.util, json, sys
from collections import Counter
policy, task_file, representative, package = sys.argv[1:5]
spec = importlib.util.spec_from_file_location("pinned_policy", policy)
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)
tasks = json.load(open(task_file, encoding="utf-8"))
matches, reasons = [], []
try:
    for task in tasks:
        if task.get("kind") != "go" or task.get("package") != package:
            continue
        if task.get("race") is not True:
            reasons.append({"unit": task.get("unit"), "reason": "wrong-mode"})
            continue
        names, bucket, worker = task.get("names"), task.get("bucket"), task.get("worker")
        if names is not None:
            if bucket is not None or worker is not None:
                raise ValueError("unsupported named shape")
            if representative in names:
                matches.append(task)
            else:
                reasons.append({"unit": task.get("unit"), "reason": "named-list-excludes-original"})
        elif bucket is not None:
            if worker is not None or task.get("buckets") != 8 or not hasattr(p, "bucket_selection"):
                raise ValueError("unsupported bucket shape")
            selected = p.bucket_selection(Counter({representative: 1}), bucket, task["buckets"])
            if selected.get(representative) == 1:
                matches.append(task)
            else:
                reasons.append({"unit": task.get("unit"), "reason": "bucket-excludes-original"})
        elif worker is not None:
            profile = task.get("profile")
            if not isinstance(profile, str) or not hasattr(p, "shards") or not hasattr(p, "verifier"):
                raise ValueError("unsupported partition shape")
            slots = p.shards.BATCHES[profile][worker]
            selected = sum((p.verifier.selection(Counter({representative: 1}), slot, profile)[0] for slot in slots), Counter())
            if selected.get(representative) == 1:
                matches.append(task)
            else:
                reasons.append({"unit": task.get("unit"), "reason": "partition-excludes-original"})
        else:
            matches.append(task)
    print(json.dumps({"matches": matches, "reasons": reasons}))
except Exception as error:
    print(json.dumps({"exception": type(error).__name__, "error": str(error)}))
    sys.exit(1)
'''

# Observe the real policy command boundary without replacing its commands, verifier, or return codes.
WORKER = '''import hashlib, importlib.util, json, shutil, subprocess, sys, tempfile, time
from collections import Counter
from pathlib import Path
policy, root, task_file, receipt_file = map(Path, sys.argv[1:])
request = task = json.loads(task_file.read_text())
receipt = {"task": task, "commands": [], "returned": False, "api_invoked": False}
def save():
    temporary = receipt_file.with_suffix(".tmp")
    temporary.write_text(json.dumps(receipt))
    temporary.replace(receipt_file)
save()
try:
    spec = importlib.util.spec_from_file_location("pinned_policy", policy)
    p = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(p)
    def partition(compiled):
        matches = []
        for emitted in request["tasks"]:
            slots = p.shards.BATCHES["store"][emitted["worker"]]
            names = sum((p.verifier.selection(compiled, slot, "store")[0] for slot in slots), Counter())
            if names.get(request["representative"]) == 1:
                matches.append((emitted, names, slots))
        if len(matches) != 1:
            raise ValueError("原历史名称不在唯一实际编译 Store 分区中")
        return matches[0]
    original = p.command
    def observed(argv, root, **kwargs):
        row = {"argv": list(map(str, argv)), "rc": None}
        receipt["commands"].append(row)
        save()
        start = time.monotonic()
        completed = None
        try:
            completed = original(argv, root, **kwargs)
            row["rc"] = completed.returncode
            return completed
        except subprocess.CalledProcessError as error:
            row["rc"] = error.returncode
            completed = error
            raise
        finally:
            row["elapsed_seconds"] = time.monotonic() - start
            try:
                output = kwargs.get("stdout")
                raw = receipt_file.parent / ("command-" + str(len(receipt["commands"])) + ".stdout.json")
                if hasattr(output, "name"):
                    output.flush()
                    shutil.copyfile(output.name, raw)
                    row["stdout"] = raw.name
                elif completed is not None and completed.stdout is not None:
                    data = completed.stdout
                    raw.write_bytes(data.encode() if isinstance(data, str) else data)
                    row["stdout"] = raw.name
                if completed is not None and completed.stderr is not None:
                    error_output = receipt_file.parent / ("command-" + str(len(receipt["commands"])) + ".stderr.txt")
                    data = completed.stderr
                    error_output.write_bytes(data.encode() if isinstance(data, str) else data)
                    row["stderr"] = error_output.name
            except Exception as error:
                row["evidence_error"] = str(error)
            save()
    p.command = observed
    if hasattr(p, "inventory"):
        original_inventory = p.inventory
        receipt["inventories"] = []
        def inventory(root, package, race, directory, build_flags):
            item = {"package": package, "race": race, "build_flags": build_flags, "overlays": []}
            receipt["inventories"].append(item)
            for flag in build_flags:
                if flag.startswith("-overlay="):
                    overlay = json.loads(Path(flag.split("=", 1)[1]).read_text())
                    item["overlays"].append(overlay)
            generated = Path(root) / "internal/store/merge_gate_authority_generated_test.go"
            if generated.is_file():
                raw = receipt_file.parent / "policy-generated_test.go"
                shutil.copyfile(generated, raw)
                item["generated_source"] = raw.name
                item["generated_sha256"] = hashlib.sha256(raw.read_bytes()).hexdigest()
            save()
            compiled = original_inventory(root, package, race, directory, build_flags)
            item["compiled"] = dict(compiled)
            if request["kind"] == "store-partition" and task["kind"] == "go":
                slots = p.shards.BATCHES["store"][task["worker"]]
                selected = sum((p.verifier.selection(compiled, slot, "store")[0] for slot in slots), Counter())
                receipt["partition"].update(compiled=dict(compiled), selected=dict(selected), slots=slots,
                                            confirmed=selected.get(request["representative"]) == 1)
            save()
            return compiled
        p.inventory = inventory
    if request["kind"] == "store-partition":
        receipt["partition"] = {"confirmed": False, "selected": {}}
        try:
            with p.module("merge-gate-migration").generated(root, p.HERE / "merge-gate-migrations.json") as flags:
                with tempfile.TemporaryDirectory(prefix="audit-inventory-") as directory:
                    compiled = p.inventory(root, "agentworkspace/internal/store", request["tasks"][0]["race"], directory, flags)
            task, selected, slots = partition(compiled)
            receipt["partition"].update(compiled=dict(compiled), selected=dict(selected), slots=slots, confirmed=True)
        except Exception as error:
            receipt["partition"]["inventory_refusal"] = {"exception": type(error).__name__, "error": str(error)}
            # Fixed logical slots are name-hashed by the pinned verifier. Still try its exact emitted worker;
            # the actual run_go inventory, not this provisional slot, remains authoritative.
            task, _, slots = partition(Counter({request["representative"]: 1}))
            receipt["partition"]["provisional_slots"] = slots
        receipt["task"] = task
        save()
    api_name = "run_vet" if task["kind"] == "vet" else "ci_tests" if task["kind"] == "ci" else "run_go"
    api = getattr(p, api_name)
    if not callable(api):
        raise TypeError("Pinned worker API is not callable")
    receipt.update(api_invoked=True, api="ci_tests+native-script-loop" if task["kind"] == "ci" else api_name)
    save()
    if task["kind"] == "ci":
        scripts = api(root, p.read_json(p.HERE / "merge-gate-manifest.json"))
        receipt["ci_scripts"] = list(map(str, scripts))
        save()
        for script in scripts:
            p.command([sys.executable, "-B", str(script), "-v"], root)
        detail = None
    elif task["kind"] == "vet":
        api(root, task["packages"])
        detail = None
    else:
        detail = api(root, task)
    receipt.update(returned=True, detail=detail)
except Exception as error:
    receipt.update(exception=type(error).__name__, error=str(error))
finally:
    save()
sys.exit(0 if receipt["returned"] else 1)
'''


class Runner:
    def __init__(self, root):
        self.root = root.resolve()
        self.output = self.root / "evidence"
        self.objects = self.root / "objects.git"
        self.history = self.root / "historical"
        self.policy = self.root / "policy"
        sandbox = self.root / "sandbox"
        for directory in (self.output, sandbox / "home", sandbox / "tmp"):
            directory.mkdir(parents=True, exist_ok=True)
        # No Actions artifact token, provider/auth configuration, Git credentials, or fixture override reaches code.
        self.env = {"PATH": os.environ.get("PATH", os.defpath), "HOME": str(sandbox / "home"),
                    "TMPDIR": str(sandbox / "tmp"), "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TZ": "UTC",
                    "PYTHONDONTWRITEBYTECODE": "1", "GOTOOLCHAIN": "local", "GOENV": "off", "GOFLAGS": "-mod=readonly",
                    "GOCACHE": str(sandbox / "go-build"), "GOMODCACHE": str(sandbox / "go-mod"), "GOPATH": str(sandbox / "go"),
                    "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
                    "GIT_TERMINAL_PROMPT": "0", "GIT_ASKPASS": "/bin/false", "GIT_NO_REPLACE_OBJECTS": "1",
                    "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_COUNT": "3",
                    "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": "/dev/null",
                    "GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "",
                    "GIT_CONFIG_KEY_2": "core.fsmonitor", "GIT_CONFIG_VALUE_2": "false"}
        self.deadline = time.monotonic() + 190 * 60

    def record(self, argv, cwd, prefix, timeout):
        prefix = self.output / prefix
        prefix.parent.mkdir(parents=True, exist_ok=True)
        json_output = prefix.name in ("selector", "go-test", "verifier", "project", "worker")
        stdout = prefix.with_name(prefix.name + (".stdout.json" if json_output else ".stdout.txt"))
        stderr = prefix.with_name(prefix.name + ".stderr.txt")
        result = {"argv": list(map(str, argv)), "rc": None, "timeout": False,
                  "stdout": stdout.relative_to(self.output).as_posix(), "stderr": stderr.relative_to(self.output).as_posix()}
        start = time.monotonic()
        process = None
        with stdout.open("wb") as out, stderr.open("wb") as err:
            try:
                remaining = min(timeout, self.deadline - start)
                check(remaining > 0, "审计总预算已耗尽")
                process = subprocess.Popen(result["argv"], cwd=cwd, env=self.env, stdin=subprocess.DEVNULL,
                                           stdout=out, stderr=err, start_new_session=True)
                try:
                    process.wait(timeout=remaining)
                except subprocess.TimeoutExpired:
                    result["timeout"] = True
                    for signum in (signal.SIGTERM, signal.SIGKILL):
                        try:
                            os.killpg(process.pid, signum)
                        except ProcessLookupError:
                            pass
                        try:
                            process.wait(timeout=3)
                            break
                        except subprocess.TimeoutExpired:
                            continue
                result["rc"] = process.returncode
            except (OSError, ValueError) as error:
                result["error"] = str(error)
                err.write(str(error).encode("utf-8", errors="replace"))
        result.update(elapsed_seconds=round(time.monotonic() - start, 3),
                      stdout_bytes=stdout.stat().st_size, stderr_bytes=stderr.stat().st_size)
        write_json(prefix.with_name(prefix.name + ".command.json"), result)
        return result

    def git(self, root, *args):
        remaining = self.deadline - time.monotonic()
        check(remaining > 0, "审计总预算已耗尽")
        return subprocess.run(["git", "-C", str(root), *map(str, args)], env=self.env, stdin=subprocess.DEVNULL,
                              capture_output=True, check=True, timeout=min(120, remaining)).stdout

    def identity(self, root, sha):
        check(self.git(root, "cat-file", "-t", sha).strip() == b"commit", "SHA 不是 commit")
        actual = commit(self.git(root, "cat-file", "-p", sha), sha)
        check(self.git(root, "show", "-s", "--format=%T", sha).decode().strip() == actual["tree"], "git show tree 不符")
        check(self.git(root, "cat-file", "-t", actual["tree"]).strip() == b"tree", "原树缺失")
        for parent in actual["parents"]:
            check(self.git(root, "cat-file", "-t", parent).strip() == b"commit", "真实 parent 缺失")
        return actual

    def exists_commit(self, sha):
        try:
            return self.git(self.objects, "cat-file", "-t", sha).strip() == b"commit"
        except (subprocess.SubprocessError, ValueError):
            return False

    def prefetch(self, shas):
        missing = [sha for sha in shas if not self.exists_commit(sha)]
        receipts = []
        for offset in range(0, len(missing), 20):
            group = missing[offset:offset + 20]
            argv = ["git", "-C", str(self.objects), "fetch", "--no-tags", "--depth=2", "origin"]
            receipts.append(self.record([*argv, *group], self.root, f"fetch/group-{offset}", 180))
            # A failed batch may still have supplied independent objects. Never discard them.
            for sha in group:
                if not self.exists_commit(sha) and time.monotonic() < self.deadline:
                    receipts.append(self.record([*argv, sha], self.root, f"fetch/{sha}", 90))
        return receipts

    def checkout_source(self, frozen):
        sha = frozen["sha"]
        actual = self.identity(self.objects, sha)
        same_source(actual, frozen, sha)
        if not self.history.exists():
            self.git(self.objects, "worktree", "add", "--detach", self.history, sha)
        else:
            self.git(self.history, "clean", "-ffdqx")
            self.git(self.history, "checkout", "--detach", "--force", sha)
        self.git(self.history, "clean", "-ffdqx")
        head = self.git(self.history, "rev-parse", "HEAD").decode().strip()
        same_source(actual, frozen, head)
        check(not self.git(self.history, "status", "--porcelain=v1", "--untracked-files=all"), "历史工作树不干净")
        return {**actual, "observed_head": head}

    def prepare_policy(self, sha):
        for name, refs in (("policy-branch", [POLICY_REF + ":refs/audit/policy"]), ("policy-sha", [sha])):
            receipt = self.record(["git", "-C", str(self.objects), "fetch", "--no-tags", "origin", *refs],
                                  self.root, "fetch/" + name, 300)
            check(receipt["rc"] == 0 and not receipt["timeout"], f"无法获取固定策略：{name}")
        self.git(self.objects, "merge-base", "--is-ancestor", sha, "refs/audit/policy")
        self.git(self.objects, "merge-base", "--is-ancestor", PUBLIC_BASE, sha)
        actual = self.identity(self.objects, sha)
        tip = self.git(self.objects, "rev-parse", "refs/audit/policy").decode().strip()
        self.git(self.objects, "worktree", "add", "--detach", self.policy, sha)
        check(self.git(self.policy, "rev-parse", "HEAD").decode().strip() == sha and
              not self.git(self.policy, "status", "--porcelain=v1", "--untracked-files=all"), "固定策略工作树身份不符")
        scripts = self.policy / ".github/scripts"
        self.policy_identity = {**actual, "branch": POLICY_REF, "observed_branch_tip": tip,
                                "script_sha256": hashlib.sha256((scripts / "merge-gate.py").read_bytes()).hexdigest(),
                                "verifier_sha256": hashlib.sha256((scripts / "verify-store-shard.py").read_bytes()).hexdigest()}
        return self.policy_identity

    def trusted_policy(self):
        identity = getattr(self, "policy_identity", None)
        check(identity and valid_sha(identity["sha"]) and
              self.git(self.policy, "rev-parse", "HEAD").decode().strip() == identity["sha"] and
              not self.git(self.policy, "status", "--porcelain=v1", "--untracked-files=all"), "固定策略未知或工作树已改变")

    def inspect(self, frozen):
        result = {"status": "unknown", "observed_source": frozen["observed_source"]}
        try:
            self.trusted_policy()
            command = self.record([sys.executable, "-B", str(self.policy / ".github/scripts/merge-gate.py"), "inspect",
                                   "--root", str(self.history), "--base", frozen["base"], "--sha", frozen["sha"]],
                                  self.policy, Path("sources") / frozen["sha"] / "selector", 180)
            result["command"] = command
            check(not command["timeout"], "inspect 超时，基础设施/覆盖失败")
            result.update(selector((self.output / command["stdout"]).read_text(encoding="utf-8"), command["rc"]))
        except Exception as error:
            result["error"] = f"{type(error).__name__}: {error}"
        return result

    def project(self, inspection):
        self.trusted_policy()
        selected = inspection["selection"]
        if selected["tasks"]:
            return selected["tasks"]
        check(selected["unresolved"], "没有真实任务或可投影的拒绝结果")
        raw = self.output / inspection["command"]["stdout"]
        command = self.record([sys.executable, "-B", "-c", PROJECT, str(self.policy / ".github/scripts/merge-gate.py"), str(raw)],
                              self.policy, raw.parent.relative_to(self.output) / "project", 30)
        check(command["rc"] == 0 and not command["timeout"], "固定策略无法投影目标任务；完整计划仍被拒绝")
        return decode((self.output / command["stdout"]).read_text(encoding="utf-8"))

    def select_unit(self, tasks, package, representative, prefix):
        self.trusted_policy()
        directory = self.output / prefix
        request = directory / "tasks.json"
        write_json(request, tasks)
        command = self.record([sys.executable, "-B", "-c", SELECT, str(self.policy / ".github/scripts/merge-gate.py"),
                               str(request), representative, package], self.policy,
                              directory.relative_to(self.output) / "select", 30)
        result = {"command": command, "status": "unknown", "matches": [], "reasons": []}
        if command["rc"] != 0 or command["timeout"]:
            result.update(status="unsupported-worker-shape",
                          error=(self.output / command["stderr"]).read_text(encoding="utf-8", errors="replace").strip()
                          or "无法使用固定策略选择实际执行单元")
            return result
        data = decode((self.output / command["stdout"]).read_text(encoding="utf-8"))
        check(isinstance(data, dict) and isinstance(data.get("matches"), list)
              and all(isinstance(row, dict) for row in data["matches"])
              and isinstance(data.get("reasons"), list)
              and all(isinstance(row, dict) for row in data["reasons"]), "单元选择结果非法")
        result.update(matches=data["matches"], reasons=data["reasons"])
        if len(data["matches"]) == 1:
            result["status"] = "selected"
        elif len(data["matches"]) > 1:
            result.update(status="unsupported-worker-shape", error="多个实际执行单元同时包含原测试")
        elif any(row.get("reason") in ("bucket-excludes-original", "partition-excludes-original")
                 for row in data["reasons"]):
            result.update(status="unsupported-worker-shape", error="实际分区未覆盖原测试")
        elif any(row.get("reason") == "wrong-mode" for row in data["reasons"]) and not any(
                row.get("reason") == "named-list-excludes-original" for row in data["reasons"]):
            result["status"] = "wrong-mode"
        else:
            result["status"] = "not-selected"
        return result

    def worker(self, task, prefix):
        self.trusted_policy()
        directory = self.output / prefix / "worker"
        request = directory / "task.json"
        receipt = directory / "receipt.json"
        write_json(request, task)
        before = self.source_status()
        command = self.record([sys.executable, "-B", "-c", WORKER, str(self.policy / ".github/scripts/merge-gate.py"),
                               str(self.history), str(request), str(receipt)], self.policy, directory.relative_to(self.output) / "worker", 900)
        result = {"command": command, "directory": directory.relative_to(self.output).as_posix(),
                  "receipt": decode(receipt.read_text(encoding="utf-8")) if receipt.exists() else {"commands": [], "returned": False}}
        result["source_before"] = before
        try:
            after = self.source_status()
            result.update(source_after=after, tracked_unchanged=before == after and not after["tracked_status"])
        except Exception as error:
            result.update(tracked_unchanged=False, cleanup_error=f"{type(error).__name__}: {error}")
        result["harness_note"] = "仅由固定 policy 生成临时测试及 Go overlay；原样调用并等待其清理，审计不替换 overlay"
        actual = result["receipt"].get("task", task)
        check(actual in task["tasks"] if task["kind"] == "store-partition" else actual == task,
              "worker 回执与实际目标任务不符")
        return result

    def source_status(self):
        return {"sha": self.git(self.history, "rev-parse", "HEAD").decode().strip(),
                "tree": self.git(self.history, "rev-parse", "HEAD^{tree}").decode().strip(),
                "tracked_status": self.git(self.history, "status", "--porcelain=v1", "--untracked-files=no").decode()}

    def verify(self, verifier, original):
        prefix = original.parent.relative_to(self.output) / "verifier"
        command = self.record([sys.executable, "-B", "-c", VERIFY, str(verifier), str(original)], self.root, prefix, 30)
        check(command["rc"] == 0 and not command["timeout"], "固定 verifier 无有效结果")
        result = decode((self.output / command["stdout"]).read_text(encoding="utf-8"))
        check(isinstance(result, dict) and type(result.get("accepted")) is bool, "verifier 回执非法")
        return {"command": command, "result": result}


RACE_TESTS = (
    "TestIntentPassBindingBudgetAndRotation", "TestIntentPassHeldAndGenerationConflict",
    "TestIntentPassCommitUnknownAndOriginalGenerationReplay", "TestIntentPassPreservesUnknownAndCancelResources",
    "TestIntentPumpSchedulerPersistentFailurePrefixReaches51",
)
RACE_SIGNATURES = [
    {"test": RACE_TESTS[0], "file": "intent_pump_test.go", "line": 145, "literal": "物化工作分配"},
    {"test": RACE_TESTS[1] + "/held-both-kinds", "file": "intent_pump_test.go", "line": 283,
     "literal": "held is a persisted disposition, not a pass error"},
    {"test": RACE_TESTS[2], "file": "intent_pump_test.go", "line": 429, "literal": "unknown commit stopped cursor progress"},
    {"test": RACE_TESTS[3] + "/(?:unknown|cancel_requested)", "file": "intent_pump_test.go", "line": 808, "literal": "物化工作分配"},
    {"test": RACE_TESTS[4], "file": "intent_pump_scheduler_test.go", "line": 483,
     "literal": "controlled prefix setup failed: segment diagnostic bound exceeded:"},
]
CHILD = "TestNativeThreadHandoffChild"
DIAGNOSTICS = {
    "66e71aa66a1a58f52c8fa0b545d0aa135eab381a": {"kind": "vet", "pr": 196, "run_id": 44390331394, "job_id": 135115665712,
        "file": "internal/platform/prepare_task_work.go", "literal": "literal copies lock value from base"},
    "4656668d795b576fc91acd0d4ec626c2c63e809f": {"kind": "compile", "pr": 196, "run_id": 44348629019, "job_id": 134957803477,
        "file": "internal/store/runtime_task_work_resource.go", "literal": "undefined: taskWorkFollowupProvenance"},
    "eb02c8c2ad80571d0d46336b85a8eb851ca8ad4b": {"kind": "skip", "pr": 238, "run_id": 44719422158, "job_id": 136232495248},
    "1dc588d7986397f773c6ddfd493105243490e7a5": {"kind": "race", "pr": 146, "run_id": 43807288938, "job_id": 133104018265,
        "same_sha_replays": [{"job_id": 140041178065}]},
    "7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d": {"kind": "effect", "pr": 264, "run_id": 45829773899, "job_id": 139945232158,
        "file": "internal/store/task_work_input_effect_test.go", "line": 75, "literal": "expected conflict without a receipt",
        "same_sha_replays": [{"run_id": 45829773899, "run_attempt": 2, "job_id": 140042364582}]},
    "3e56ab2fa42856c05bc31e8364144ffa43f0399d": {"kind": "migration", "pr": 153, "run_id": 44037303018,
        "job_id": 133883154642, "representative": "TestQueueV3UpgradePreservesMessagesAndReceipts", "originals": [
            {"test": "TestQueueV3UpgradePreservesMessagesAndReceipts", "job_id": 133883154642,
             "file": "internal/store/queue_schema_test.go", "line": 126, "literal": "upgrade changed message receipt",
             "assertion": 't.Fatalf("upgrade changed message receipt:'},
            {"test": "TestArtifactV5UpgradePreservesExistingReads", "job_id": 133883154723,
             "file": "internal/store/artifact_migration_test.go", "line": 92, "literal": "v6 changed message",
             "assertion": 't.Fatalf("v6 changed %s:'},
            {"test": "TestTaskV4UpgradePreservesOriginalReads", "job_id": 133883154456,
             "file": "internal/store/task_migration_test.go", "line": 71, "literal": "v5 changed original message read",
             "assertion": 't.Fatalf("v5 changed original message read:'},
            {"test": "TestTaskArtifactV6UpgradePreservesTextHistoryAndClaims", "job_id": 133883154595,
             "file": "internal/store/task_artifact_migration_test.go", "line": 136, "literal": "v7 changed legacy value",
             "assertion": 't.Fatalf("v7 changed legacy value:'}]},
    "41e24737b4b1cfe545219378694dcb1608d8a629": {"kind": "migration", "pr": 194, "run_id": 44282887126,
        "job_id": 134727936958, "representative": "TestCoordinationAssignmentV13MigrationFrozenHistory", "originals": [
            {"test": "TestCoordinationAssignmentV13MigrationFrozenHistory", "job_id": 134727936958,
             "file": "internal/store/coordination_assignment_migration_test.go", "line": 70,
             "literal": "unexpected source after the appended schema identifiers", "assertion": "t.Fatal(err)",
             "source_call": "assignmentCheckMigrationSource(source)", "helper_file": "internal/store/coordination_assignment_migration_test.go",
             "helper_literal": 'return fmt.Errorf("unexpected source after the appended schema identifiers")'},
            {"test": "TestCoordinationAssignmentResolutionV14FrozenHistoryAndFinalFK", "job_id": 134727937004,
             "file": "internal/store/coordination_assignment_resolution_migration_test.go", "line": 324,
             "literal": "unexpected source after the appended schema identifiers", "assertion": "t.Fatal(err)",
             "source_call": "assignmentCheckMigrationSource(source)", "helper_file": "internal/store/coordination_assignment_migration_test.go",
             "helper_literal": 'return fmt.Errorf("unexpected source after the appended schema identifiers")'},
            {"test": "TestCoordinationAssignmentV13MigrationSourceHistoryBoundaries", "job_id": 134727936818,
             "file": "internal/store/coordination_assignment_migration_test.go", "line": 161,
             "literal": "source valid=false, want true: unexpected source after the appended schema identifiers",
             "assertion": 't.Fatalf("source valid=%t, want %t: %v",', "source_call": "assignmentCheckMigrationSource(",
             "helper_file": "internal/store/coordination_assignment_migration_test.go",
             "helper_literal": 'return fmt.Errorf("unexpected source after the appended schema identifiers")'}]},
    "85b488fa772dc1197241e730131aee5f2c8d365a": {"kind": "ci", "pr": 146, "run_id": 43816639142, "job_id": 133133787471,
        "file": ".github/scripts/test-store-shard.py", "line": 706,
        "test": "WorkflowTests.test_remaining_package_exclusion_executes_actual_shell_without_go",
        "native_step": "Test Store shard gates", "literal": "AssertionError: False != True",
        "assertion": "self.assertEqual(result.returncode == 0, accepted, result.stderr)"},
}
MISSES = {
    "5aa8203bda038d3ce08ef51bfef1e20ba8c5dca1": {
        "pr": 162, "run_id": 44088192148, "run_attempt": 1, "job_id": 134063669160,
        "package": "agentworkspace/cmd/workspace",
        "test": "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims",
        "case": "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims/shutdown",
        "file": "cmd/workspace/coordination_composition_test.go",
        "original_tests": ["TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims",
                           "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims/shutdown"],
        "signatures": [
            {"test": "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims/shutdown",
             "file": "coordination_composition_test.go", "line": 528,
             "literal": "race safety prerequisite did not reconcile the same original assignment generation/receipt: materialize=工作分配事务失败"},
            {"test": "TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims/shutdown",
             "file": "coordination_composition_test.go", "line": 628,
             "literal": "complete composition output:"},
        ],
        "evidence": "coordination_composition_test.go:528: race safety prerequisite did not reconcile the same original assignment generation/receipt: materialize=工作分配事务失败 | coordination_composition_test.go:628: complete composition output:",
    },
    "56d236dda96e1a5140031b6d7069917394a23d2f": {
        "pr": 176, "run_id": 44235577412, "run_attempt": 1, "job_id": 134570782516,
        "package": "agentworkspace/internal/platform",
        "test": "TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues",
        "case": "TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues",
        "file": "internal/platform/prepare_repair_wiring_test.go",
        "original_tests": ["TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues"],
        "signatures": [
            {"test": "TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues",
             "file": "prepare_repair_wiring_test.go", "line": 254,
             "literal": "database is locked (5) (SQLITE_BUSY)"},
        ],
        "evidence": "prepare_repair_wiring_test.go:254: database is locked (5) (SQLITE_BUSY)",
    },
    "4b87e75cfcaeaac19d7e1e8e1db076bf673651ba": {
        "pr": 239, "run_id": 45512885246, "run_attempt": 1, "job_id": 138866224228,
        "package": "agentworkspace/internal/coordination",
        "test": "TestIntentPassCancellationAndQueryFailure",
        "case": "TestIntentPassCancellationAndQueryFailure/cancel-between-items-and-resume",
        "file": "internal/coordination/intent_pump_test.go",
        "original_tests": ["TestIntentPassCancellationAndQueryFailure",
                           "TestIntentPassCancellationAndQueryFailure/cancel-between-items-and-resume"],
        "signatures": [
            {"test": "TestIntentPassCancellationAndQueryFailure/cancel-between-items-and-resume",
             "file": "intent_pump_test.go", "line": 789, "literal": "context deadline exceeded"},
        ],
        "evidence": "intent_pump_test.go:789: context deadline exceeded",
    },
}
EFFECT = "TestTaskWorkInputEffectKeys"


def candidate_task(inspection, kind, projected):
    check(inspection.get("status") in ("selected", "policy-refusal"), "无有效 selector 证据，不能推断候选执行点")
    selected = inspection["selection"]
    tasks = selected["tasks"] or projected
    check(isinstance(tasks, list) and all(isinstance(task, dict) for task in tasks), "实际策略任务非法")
    package = "agentworkspace/internal/" + ("coordination" if kind == "race" else "platform" if kind == "vet" else "store")
    matches = [task for task in tasks if (task.get("kind") == kind if kind in ("vet", "ci") else
               task.get("kind") == "go" and task.get("package") == package and task.get("race") is (kind not in ("compile", "migration")))]
    check(matches, "实际策略没有选中目标执行单元")
    task = matches[0]
    full_effect = kind == "effect" and (selected.get("all_race") is True or package in selected.get("race_full", []))
    if full_effect:
        required = selected.get("race", {}).get(package, [])
        check(package in selected.get("packages", []) and all(
            row.get("profile") == "store" and type(row.get("worker")) is int and row["worker"] >= 0
            and row.get("names") is None and row.get("required") == required
            and row.get("bucket") is None and row.get("buckets") is None for row in matches)
            and len({row["worker"] for row in matches}) == len(matches), "必须保留实际完整 Store race 分区及 required；不能改名或改槽")
    elif kind == "ci":
        check(selected.get("ci") is True and len(matches) == 1, "CI 未被实际 selector 选中")
    elif kind == "vet":
        check(task.get("packages") == selected.get("packages") and package in task["packages"], "候选 vet 不含原故障包的完整实际范围")
    elif kind in ("compile", "migration"):
        check(package in selected.get("packages", []) and task.get("required") == selected.get("ordinary_required", {}).get(package, [])
              and task.get("names") is None, "候选普通包任务与实际选择不符")
        if kind == "migration":
            check(all(row.get("profile") == "store" and type(row.get("worker")) is int and row.get("names") is None
                      and row.get("required") == task["required"] for row in matches), "必须使用完整实际 Store 普通分区")
    else:
        names = selected.get("race", {}).get(package)
        expected = set(RACE_TESTS) if kind == "race" else {EFFECT}
        check(isinstance(names, list) and len(names) == len(set(names)) and expected <= set(names), "原历史名称未被实际 RACE 选择完整选中")
        check(len(matches) == 1 and not selected.get("all_race") and package not in selected.get("race_full", [])
              and task.get("names") == names and task.get("required") == names and task.get("worker") is None,
              "必须保留实际 RACE 名称及 required 全集；不缩减或执行未知全套范围")
    return {"task": task, "target_tasks": matches, "partitioned": kind == "migration" or full_effect,
            "scope_selected": True, "unit_exercised": False,
            "task_origin": "emitted-task" if selected["tasks"] else "target-projection-from-refused-inspect",
            "selector_status": inspection["status"], "full_plan_executable": inspection["status"] == "selected",
            "global_unresolved": selected["unresolved"], "caught_original_defect": False, "status": "unknown"}


def named_outcome(summary, go_rc, worker_rc, originals):
    counts = summary["tests"]
    if type(go_rc) is not int or type(worker_rc) is not int or not set(originals) <= counts.keys():
        return "unknown"
    if go_rc == 0 and worker_rc == 0 and race_outcome(summary, go_rc) == "complete-no-red":
        return "complete-no-red"
    if go_rc == 1 and worker_rc == 1 and race_outcome(summary, go_rc) == "red-reproduced":
        return "red-reproduced" if any(counts[name]["fail"] for name in originals) else "original-assertion-unconfirmed"
    return "unknown"


def observations(text, package, names):
    result = {"complete": False, "tests": {name: dict.fromkeys(("run", "pass", "fail", "skip"), 0) for name in names},
              "test_events": {}, "outputs": []}
    for line in text.splitlines():
        try:
            row = decode(line)
            if not isinstance(row, dict) or row.get("Package") != package:
                continue
            name, action = row.get("Test", ""), row.get("Action")
            root = name.split("/", 1)[0]
            if root in result["tests"]:
                if action in ("run", "pass", "fail", "skip"):
                    counts = result["test_events"].setdefault(name, dict.fromkeys(("run", "pass", "fail", "skip"), 0))
                    counts[action] += 1
                if name == root and action in result["tests"][root]:
                    result["tests"][root][action] += 1
                if action == "output" and isinstance(row.get("Output"), str):
                    result["outputs"].append({"test": name, "output": row["Output"]})
        except (ValueError, TypeError, AttributeError):
            continue
    try:
        result.update(go_events(text, package, names), complete=True)
    except ValueError as error:
        result["validation_error"] = str(error)
    return result


def original_signatures(summary, matchers):
    result = []
    for matcher in matchers:
        pattern = rf"(?m)^\s*{re.escape(Path(matcher['file']).name)}:{matcher['line']}:\s+{re.escape(matcher['literal'])}"
        outputs = [row for row in summary["outputs"] if re.fullmatch(matcher["test"], row["test"])
                   and summary.get("test_events", {}).get(row["test"], {}).get("fail") == 1
                   and summary.get("test_events", {}).get(row["test"], {}).get("run") == 1
                   and summary["tests"].get(row["test"].split("/", 1)[0], {}).get("fail") == 1
                   and summary["tests"].get(row["test"].split("/", 1)[0], {}).get("run") == 1
                   and re.search(pattern, row["output"])]
        result.append({**matcher, "matches": bool(outputs), "outputs": outputs})
    return result


def test_sources(root, directory, names, strict=True):
    found = []
    for path in sorted((root / directory).glob("*_test.go")):
        text = path.read_text(encoding="utf-8")
        functions = list(re.finditer(r"(?m)^func[ \t]+([A-Za-z0-9_]+)\(", text))
        for index, match in enumerate(functions):
            name = match[1]
            if name in names:
                end = functions[index + 1].start() if index + 1 < len(functions) else len(text)
                found.append({"test": name, "file": path.relative_to(root).as_posix(),
                              "line": text[:match.start()].count("\n") + 1, "text": text[match.start():end]})
    if strict:
        check(Counter(row["test"] for row in found) == Counter(names), "原源码测试名称缺失或重复")
    return found


def required_sources(runner, task):
    names = list(dict.fromkeys((task["names"] or []) + task["required"]))
    sources = test_sources(runner.history, task["package"].removeprefix("agentworkspace/"), names, strict=False)
    counts = Counter(row["test"] for row in sources)
    return sources, [name for name in names if counts[name] != 1]


def effect_signature(runner, summary, spec):
    path = runner.history / spec["file"]
    lines = path.read_text(encoding="utf-8").splitlines() if path.is_file() else []
    source_line = lines[spec["line"] - 1] if len(lines) >= spec["line"] else ""
    originals = test_sources(runner.history, "internal/store", [EFFECT], strict=False)
    source_matches = len(originals) == 1 and originals[0]["file"] == spec["file"] and spec["literal"] in originals[0]["text"] and "conflict(f.s.HandBackWorkInputWithKey(" in source_line
    pattern = rf"(?m)^\s*(?:internal/store/)?{re.escape(Path(spec['file']).name)}:{spec['line']}:\s+{re.escape(spec['literal'])}"
    outputs = [row for row in summary["outputs"] if row["test"].split("/", 1)[0] == EFFECT
               and summary["tests"].get(EFFECT, {}).get("run") == 1 and summary["tests"].get(EFFECT, {}).get("fail") == 1
               and summary.get("test_events", {}).get(row["test"], {}).get("fail") == 1 and re.search(pattern, row["output"])]
    return {"file": spec["file"], "line": spec["line"], "literal": spec["literal"], "source_line": source_line,
            "source_matches": source_matches, "matches": bool(outputs) and source_matches, "outputs": outputs}


def isolated_effect(runner, spec, prefix):
    # No candidate unit is claimed here. Run exactly one unchanged original after a structural refusal.
    command = runner.record(["go", "test", "-count=1", "-race", "-json", "-run", "^" + EFFECT + "$", "./internal/store"],
                            runner.history, prefix / "isolated-original-probe" / "go-test", 900)
    text = (runner.output / command["stdout"]).read_text(encoding="utf-8")
    summary = observations(text, "agentworkspace/internal/store", [EFFECT])
    signature = effect_signature(runner, summary, spec)
    outcome = race_outcome(summary, command["rc"]) if summary["complete"] else "unknown"
    if outcome == "red-reproduced" and not signature["matches"]:
        outcome = "unknown"
    timeout = command["timeout"] or "panic: test timed out after " in text
    return {"origin": "isolated-original-probe", "status": "unknown" if timeout else outcome,
            "command": command, "go_events": summary, "source_signature": signature,
            "failure_kind": "timeout-infrastructure" if timeout else None, "candidate_unit_caught": False}


def worker_observations(runner, worker, task, names=None):
    if names is None:
        names = task.get("names")
        if names is None:
            names = list(worker["receipt"].get("partition", {}).get("selected", {}))
    result = {"complete": False, "tests": {name: dict.fromkeys(("run", "pass", "fail", "skip"), 0) for name in names},
              "outputs": [], "test_events": {}, "commands": []}
    for command in worker["receipt"]["commands"]:
        argv = command["argv"]
        if not (argv[:2] == ["go", "test"] and "-json" in argv and "-run" in argv
                and ("-race" in argv) == task["race"] and argv[-1] == task["package"]):
            continue
        pattern = argv[argv.index("-run") + 1]
        selected = [name for name in names if re.fullmatch(pattern, name)]
        path = runner.output / worker["directory"] / command.get("stdout", "missing.stdout")
        text = path.read_text(encoding="utf-8") if "stdout" in command and not command.get("evidence_error") and path.is_file() else ""
        summary = observations(text, task["package"], selected)
        if command.get("evidence_error") or "stdout" not in command:
            summary["complete"] = False
        result["commands"].append({"command": command, "go_events": summary})
        result["outputs"].extend(summary["outputs"])
        for key in ("tests", "test_events"):
            for name, counts in summary[key].items():
                total = result[key].setdefault(name, dict.fromkeys(("run", "pass", "fail", "skip"), 0))
                for action, count in counts.items():
                    total[action] += count
    result["complete"] = bool(result["commands"]) and all(row["go_events"]["complete"] for row in result["commands"]) and all(
        counts["run"] == 1 and sum(counts[key] for key in ("pass", "fail", "skip")) == 1 for counts in result["tests"].values())
    if result["complete"]:
        result["package_result"] = "fail" if any(row["go_events"]["package_result"] == "fail" for row in result["commands"]) else "pass"
    return result


def target_red(summary, worker_rc, task, target, signature, timed_out=False):
    """Return the exact command with the target's terminal red; whole-group coverage may be partial."""
    if timed_out or type(worker_rc) is not int or worker_rc != 1:
        return None
    for row in summary.get("commands", []):
        command = row.get("command", {})
        argv = command.get("argv", [])
        events = row.get("go_events")
        if not (argv[:2] == ["go", "test"] and "-race" in argv and "-json" in argv and "-run" in argv
                and argv[-1] == task["package"] and type(command.get("rc")) is int and command["rc"] == 1
                and not command.get("evidence_error") and isinstance(events, dict)):
            continue
        pattern = argv[argv.index("-run") + 1]
        if re.fullmatch(pattern, target) is None:
            continue
        evidence = signature(events)
        if evidence:
            return {"command": command, "go_events": events, "evidence": evidence}
    return None


def migration_sources(runner, spec):
    result = []
    for original in spec["originals"]:
        path = runner.history / original["file"]
        lines = path.read_text(encoding="utf-8").splitlines() if path.is_file() else []
        line = lines[original["line"] - 1] if len(lines) >= original["line"] else ""
        found = test_sources(runner.history, "internal/store", [original["test"]], strict=False)
        matches = len(found) == 1 and found[0]["file"] == original["file"] and original["assertion"] in line
        if found:
            matches = matches and found[0]["line"] <= original["line"] < found[0]["line"] + len(found[0]["text"].splitlines())
            matches = matches and original.get("source_call", "") in found[0]["text"]
        if original.get("helper_file"):
            helper = runner.history / original["helper_file"]
            matches = matches and helper.is_file() and original["helper_literal"] in helper.read_text(encoding="utf-8")
        result.append({**original, "source_line": line, "source_matches": bool(matches), "source": found})
    return result


def migration_signatures(summary, sources):
    signatures = original_signatures(summary, [{**row, "test": re.escape(row["test"]) + r"(?:/.*)?"} for row in sources])
    for signature in signatures:
        signature["matches"] = signature["matches"] and signature["source_matches"]
    return signatures


def python_signature(runner, spec, text):
    path = runner.history / spec["file"]
    source = path.read_text(encoding="utf-8") if path.is_file() and not path.is_symlink() else ""
    lines = source.splitlines()
    source_line = lines[spec["line"] - 1].strip() if len(lines) >= spec["line"] else ""
    cls, method = spec["test"].split(".")
    found = []
    try:
        for node in ast.parse(source).body:
            if isinstance(node, ast.ClassDef) and node.name == cls:
                found.extend(item for item in node.body if isinstance(item, ast.FunctionDef) and item.name == method)
    except SyntaxError:
        pass
    workflow = runner.history / ".github/workflows/ci.yml"
    workflow_text = workflow.read_text(encoding="utf-8") if workflow.is_file() else ""
    native_step = f'- name: {spec["native_step"]}\n        run: python3 {spec["file"]} -v' in workflow_text
    valid_case = len(found) == 1 and any(isinstance(node, ast.Tuple) and node.elts and
        isinstance(node.elts[0], ast.Constant) and node.elts[0].value == "valid" and
        isinstance(node.elts[-1], ast.Constant) and node.elts[-1].value is True for node in ast.walk(found[0]))
    source_matches = bool(valid_case and found[0].lineno <= spec["line"] <= found[0].end_lineno and
                          source_line == spec["assertion"] and native_step)
    header = re.escape(method + " (__main__." + spec["test"] + ")") + r" \(case='valid'\)"
    trace = rf'File "{re.escape(str(path))}", line {spec["line"]}, in {re.escape(method)}\b'
    blocks = []
    for block in re.split(r"(?m)^FAIL: ", text)[1:]:
        block = re.split(r"(?m)^(?:={5,}|FAIL: |ERROR: |Ran [0-9]+ tests?)", block)[0]
        if block and re.fullmatch(header, block.splitlines()[0]) and re.search(trace, block) and re.search(
                r"(?m)^" + re.escape(spec["literal"]) + r"(?: :[^\n]*)?$", block):
            blocks.append(block)
    return {"test": spec["test"], "file": spec["file"], "line": spec["line"], "literal": spec["literal"],
            "source_line": source_line, "source_matches": source_matches, "native_step": native_step,
            "matches": source_matches and bool(blocks), "outputs": blocks,
            "passed": source_matches and bool(re.search(r"(?m)^" + re.escape(method + " (__main__." + spec["test"] + ")") + r" \.\.\. ok\s*$", text))}


def isolated_class(runner, spec, sources, prefix):
    if spec["kind"] == "ci":
        if not python_signature(runner, spec, "")["source_matches"]:
            return {"origin": "isolated-original-probe", "status": "structural-refusal", "candidate_unit_caught": False,
                    "error": "原 Python 测试/断言坐标或原生步骤 guard 缺失；不执行替代源码"}
        argv = [sys.executable, "-B", str(runner.history / spec["file"]), "-v", spec["test"]]
    else:
        names = [row["test"] for row in sources]
        argv = ["go", "test", "-count=1", "-race", "-json", "-run", "^(" + "|".join(map(re.escape, names)) + ")$", "./internal/store"]
    command = runner.record(argv, runner.history, prefix / "isolated-original-probe" / "go-test", 900)
    text = "\n".join((runner.output / command[key]).read_text(encoding="utf-8", errors="replace") for key in ("stdout", "stderr"))
    result = {"origin": "isolated-original-probe", "command": command, "status": "unknown", "candidate_unit_caught": False}
    if spec["kind"] == "ci":
        signature = python_signature(runner, spec, text)
        result["source_signature"] = signature
        if command["rc"] == 1 and signature["matches"]:
            result["status"] = "red-reproduced"
        elif command["rc"] == 0 and signature["passed"]:
            result["status"] = "complete-no-red"
    else:
        summary = observations((runner.output / command["stdout"]).read_text(encoding="utf-8"), "agentworkspace/internal/store", names)
        result.update(go_events=summary, source_signatures=migration_signatures(summary, sources))
        if command["rc"] == 1 and any(row["matches"] for row in result["source_signatures"]):
            result["status"] = "red-reproduced"
        elif command["rc"] == 0 and summary["complete"] and race_outcome(summary, 0) == "complete-no-red":
            result["status"] = "complete-no-red"
    if command["timeout"] or "panic: test timed out after " in text:
        result["failure_kind"] = "timeout-infrastructure"
        if result["status"] != "red-reproduced":
            result["status"] = "unknown"
    return result


def diagnose_class(runner, spec, result, inspection, prefix):
    sources = migration_sources(runner, spec) if spec["kind"] == "migration" else []
    result["source"] = sources if sources else python_signature(runner, spec, "")
    projected = runner.project(inspection)
    candidate = candidate_task(inspection, spec["kind"], projected)
    result["candidate"] = candidate
    request = {"kind": "store-partition", "tasks": candidate["target_tasks"], "representative": spec["representative"]} if sources else candidate["task"]
    worker = runner.worker(request, prefix)
    candidate.update(worker=worker, api_invoked=worker["receipt"].get("api_invoked", False))
    result["command"] = worker["command"]
    check(worker.get("tracked_unchanged"), "worker 清理后的 SHA/tree/跟踪文件状态改变或证据缺失")
    task = candidate["task"] = worker["receipt"]["task"]
    text = "\n".join((runner.output / worker["command"][key]).read_text(encoding="utf-8", errors="replace") for key in ("stdout", "stderr"))
    receipt = worker["receipt"]
    if worker["command"]["timeout"] or "panic: test timed out after " in text:
        result["failure_kind"] = "timeout-infrastructure"
    if sources:
        candidate["required_source"], candidate["missing_source_names"] = required_sources(runner, task)
        summary = worker_observations(runner, worker, task)
        result["go_events"] = summary
        candidate["unit_exercised"] = bool(summary["commands"])
        candidate["actual_go_commands"] = [row for row in receipt["commands"] if row["argv"][:1] == ["go"]]
        # Each recorded command has its own package lifecycle. Do not concatenate Go JSON streams.
        signatures = [signature for row in summary["commands"] if row["command"]["rc"] == 1
                      for signature in migration_signatures(row["go_events"], sources)]
        result["source_signatures"] = signatures
        matched = receipt.get("partition", {}).get("confirmed") and any(row["matches"] for row in signatures)
        original = summary["tests"].get(spec["representative"], {})
        if matched:
            result["status"] = "red-reproduced"
        elif summary["complete"] and worker["command"]["rc"] == 0 and original.get("pass") == 1:
            result["status"] = "complete-no-red"
        elif any(counts["fail"] for counts in summary["tests"].values()):
            result["status"] = "original-assertion-unconfirmed"
        if not summary["commands"]:
            candidate["status"] = "structural-refusal"
            candidate["reason"] = "actual-compiled-inventory-or-worker-setup-refusal"
        needs_probe = not matched and original.get("pass") != 1
    else:
        signature = python_signature(runner, spec, text)
        result["source_signature"] = signature
        actual = [row for row in receipt["commands"] if row["argv"] == [sys.executable, "-B", str(runner.history / spec["file"]), "-v"]]
        candidate["unit_exercised"] = bool(receipt["commands"])
        candidate["target_script_exercised"] = bool(actual)
        if actual and any(row["rc"] == 1 for row in actual) and signature["matches"]:
            result["status"] = "red-reproduced"
        elif actual and worker["command"]["rc"] == 0 and signature["passed"]:
            result["status"] = "complete-no-red"
        elif receipt.get("exception") == "ValueError" and receipt.get("error", "").startswith("必需 CI 自测缺失："):
            candidate.update(status="structural-refusal", reason="pinned-ci-registration-refusal")
        needs_probe = result["status"] not in ("red-reproduced", "complete-no-red")
    candidate.update(caught_original_defect=result["status"] == "red-reproduced")
    if candidate["status"] != "structural-refusal":
        candidate["status"] = result["status"]
    if needs_probe:
        result["isolated_original"] = isolated_class(runner, spec, sources, prefix)
    return result


def diagnose(runner, frozen, policy, inspection=None):
    spec = DIAGNOSTICS[frozen["sha"]]
    result = {"status": "unknown", "origin": "pinned-candidate-unit", "original_job": {"run_id": spec["run_id"], "run_attempt": 1,
              "job_id": spec["job_id"], "pr": spec["pr"]}, "same_sha_replays": spec.get("same_sha_replays", [])}
    prefix = Path("sources") / frozen["sha"]
    try:
        check(policy is not None, "固定策略未知；拒绝执行诊断代码")
        kind = spec["kind"]
        selected = inspection.get("selection", {}) if inspection else {}
        result["candidate"] = {"status": "unknown", "caught_original_defect": False, "unit_exercised": False,
                               "selector_status": inspection.get("status", "unknown") if inspection else "unknown",
                               "full_plan_executable": bool(inspection and inspection.get("status") == "selected")}
        if kind == "skip":
            result["origin"] = "isolated-original-probe-with-pinned-verifier"
            required = selected.get("race", {}).get("agentworkspace/internal/store", []) + selected.get("ordinary_required", {}).get("agentworkspace/internal/store", [])
            known_selection = inspection is not None and inspection.get("status") in ("selected", "policy-refusal")
            enforcement = CHILD in required if known_selection else None
            result["candidate"].update(enforcement_point=enforcement, required_by_policy=enforcement,
                                       status="selection-only" if known_selection else "unknown", unit_exercised=False)
            source = test_sources(runner.history, "internal/store", [CHILD])
            result["source"] = source
            command = runner.record(["go", "test", "-count=1", "-race", "-json", "-run", "^" + CHILD + "$", "./internal/store"],
                                    runner.history, prefix / "go-test", 900)
            result["command"] = command
            text = (runner.output / command["stdout"]).read_text(encoding="utf-8")
            summary = observations(text, "agentworkspace/internal/store", [CHILD])
            result["go_events"] = summary
            if command["timeout"] or "panic: test timed out after " in text:
                result["failure_kind"] = "timeout-infrastructure"
                return result
            check(summary["complete"], "原始 Go run/终态覆盖不完整")
            guards = {"child_env_unset": "AW_F_HANDOFF_CHILD" not in runner.env,
                      "original_skip_guard": 'os.Getenv("AW_F_HANDOFF_CHILD") != "1"' in source[0]["text"] and 't.Skip("subprocess fixture")' in source[0]["text"],
                      "selected": {CHILD: 1}, "top_level_run": summary["tests"][CHILD]["run"] == 1,
                      "go_exit_zero": command["rc"] == 0, "package_pass": summary["package_result"] == "pass"}
            result["semantic_guards"] = guards
            verification = runner.verify(runner.policy / ".github/scripts/verify-store-shard.py", runner.output / command["stdout"])
            result["verifier"] = verification
            verdict, counts = verification["result"], summary["tests"][CHILD]
            if all(guards[key] for key in ("child_env_unset", "original_skip_guard", "top_level_run", "go_exit_zero", "package_pass")):
                if counts["skip"] == 1 and not verdict["accepted"] and verdict.get("exception") == "ValueError" and verdict.get("error") == "事件非法或包含失败/跳过":
                    result.update(status="red-reproduced", expected_failure="selected-but-skipped rejected by pinned verifier.verify")
                elif counts["pass"] == 1 and verdict["accepted"]:
                    result["status"] = "complete-no-red"
            return result
        check(inspection is not None, "缺少实际 inspect 选择证据")
        if kind in ("migration", "ci"):
            return diagnose_class(runner, spec, result, inspection, prefix)
        projected = runner.project(inspection)
        candidate = candidate_task(inspection, kind, projected)
        result["candidate"] = candidate
        task = candidate["task"]
        if kind in ("race", "effect"):
            originals = list(RACE_TESTS) if kind == "race" else [EFFECT]
            result["source"] = test_sources(runner.history, task["package"].removeprefix("agentworkspace/"), originals, strict=False)
        else:
            lines = (runner.history / spec["file"]).read_text(encoding="utf-8").splitlines()
            result["source"] = {"file": spec["file"], "line": 95, "text": "\n".join(lines[94:106])}
            result["original_log"] = {"file": spec["file"], "line": 100, "literal": spec["literal"]}
        if task["kind"] == "go":
            sources, missing = required_sources(runner, task)
            candidate.update(required_source=sources, missing_source_names=missing)
            # Static absence is metadata: the pinned worker may supply trusted generated tests.
        worker_request = {"kind": "store-partition", "tasks": candidate["target_tasks"], "representative": EFFECT} if candidate["partitioned"] else task
        worker = runner.worker(worker_request, prefix)
        candidate.update(worker=worker, api_invoked=worker["receipt"].get("api_invoked", False), unit_exercised=False)
        if candidate["partitioned"]:
            task = worker["receipt"]["task"]
            check(task in candidate["target_tasks"], "worker 未执行实际发出的完整 Store race 分区")
            candidate["task"] = task
        command = worker["command"]
        result["command"] = command
        check(worker.get("tracked_unchanged"), "worker 清理后的 SHA/tree/跟踪文件状态改变或证据缺失")
        directory = runner.output / worker["directory"]
        commands = worker["receipt"]["commands"]
        raw = [(directory / row["stdout"]).read_text(encoding="utf-8") for row in commands if "stdout" in row]
        text = (runner.output / command["stdout"]).read_text(encoding="utf-8")
        stderr = (runner.output / command["stderr"]).read_text(encoding="utf-8", errors="replace")
        combined = text + stderr + "\n".join(raw)
        timed_out = command["timeout"] or "panic: test timed out after " in combined
        if timed_out:
            result["failure_kind"] = "timeout-infrastructure"
        go = [row for row in commands if row["argv"][:1] == ["go"]]
        candidate["actual_go_commands"] = go
        if kind in ("vet", "compile"):
            outputs = []
            for stream in raw:
                for line in stream.splitlines():
                    try:
                        row = decode(line)
                        if isinstance(row, dict) and isinstance(row.get("Output"), str):
                            outputs.append(row["Output"])
                    except ValueError:
                        continue
            locations = [{"file": file, "line": int(line), "column": int(column), "message": message}
                         for file, line, column, message in re.findall(r"(?m)^(.+?\.go):(\d+):(\d+): (.*)$", combined + "\n".join(outputs))]
            result["locations"] = locations
            actual = [row for row in go if row["argv"][1:2] == ["vet"] and row["argv"][2:] == task["packages"]] if kind == "vet" else [row for row in go if row["argv"][1:2] == ["test"] and row["argv"][-1] == task["package"]]
            candidate["unit_exercised"] = bool(actual)
            reproduced = any(row["file"] == spec["file"] and row["line"] == 100 and spec["literal"] in row["message"] for row in locations)
            if actual and command["rc"] == 0 and all(row["rc"] == 0 for row in actual):
                result["status"] = "complete-no-red"
            elif actual and command["rc"] == 1 and any(row["rc"] == 1 for row in actual) and reproduced:
                result["status"] = "red-reproduced"
            else:
                result["failure_kind"] = "unrelated-setup-or-candidate-refusal"
        else:
            summary = worker_observations(runner, worker, task)
            result["go_events"] = summary
            runs = [row["command"] for row in summary["commands"]]
            candidate["unit_exercised"] = bool(runs)
            if not runs and kind == "effect" and candidate["api_invoked"]:
                missing = worker["receipt"].get("error") == "必需名称不在实际编译测试清单中"
                candidate.update(status="structural-refusal", reason="compiled-required-name-absent" if missing else "actual-worker-setup-refusal")
                result["isolated_original"] = isolated_effect(runner, spec, prefix)
                return result
            check(runs, "真实 worker 未产生目标 Go JSON；不伪造覆盖")
            codes = [row["rc"] for row in runs]
            go_rc = 1 if 1 in codes else 0 if all(code == 0 for code in codes) else None
            candidate["actual_go_rc"] = codes
            candidate["tests_exercised"] = [name for name, counts in summary["tests"].items() if counts["run"]]
            candidate["original_failed_tests"] = [name for name in originals if summary["tests"].get(name, {}).get("fail")]
            if kind == "effect":
                result["source_signature"] = effect_signature(runner, summary, spec)
                result["partial_coverage"] = not summary["complete"]
                candidate["partial_coverage"] = not summary["complete"]
                def effect_red(events):
                    signature = effect_signature(runner, events, spec)
                    return signature if signature["matches"] else None
                target = target_red(summary, command["rc"], task, EFFECT, effect_red, timed_out)
                if target:
                    result.update(status="red-reproduced", target_evidence=target)
                    candidate["target_command"] = target["command"]
                elif summary["complete"]:
                    result["status"] = named_outcome(summary, go_rc, command["rc"], originals)
                    if result["status"] == "red-reproduced":
                        # Complete aggregate coverage still needs per-command target evidence.
                        result["status"] = "original-assertion-unconfirmed"
                        candidate["reason"] = "target-command-evidence-unavailable"
            else:
                result["source_signatures"] = original_signatures(summary, RACE_SIGNATURES)
                if summary["complete"]:
                    result["status"] = named_outcome(summary, go_rc, command["rc"], originals)
                    matched = any(row["matches"] for row in result["source_signatures"])
                    if result["status"] == "red-reproduced" and not matched:
                        result["status"] = "original-assertion-unconfirmed"
            if kind == "effect" and not summary["tests"].get(EFFECT, {}).get("run"):
                candidate["reason"] = "original-not-reached-by-actual-worker"
                result["isolated_original"] = isolated_effect(runner, spec, prefix)
        if timed_out:
            result.update(status="unknown", failure_kind="timeout-infrastructure")
        candidate.update(status=result["status"], caught_original_defect=result["status"] == "red-reproduced")
    except Exception as error:
        result["error"] = f"{type(error).__name__}: {error}"
    return result


def miss_sources(runner, spec):
    found = test_sources(runner.history, spec["package"].removeprefix("agentworkspace/"), [spec["test"]], strict=False)
    matches = len(found) == 1 and Path(found[0]["file"]).name == Path(spec["file"]).name
    return found, bool(matches)


def miss_matchers(spec):
    result = []
    for signature in spec["signatures"]:
        result.append({**signature, "test": re.escape(signature["test"])})
        if "/" in signature["test"]:
            result.append({**signature, "test": re.escape(signature["test"].split("/", 1)[0])})
    return result


def case_failed(summary, case):
    if "/" in case:
        event = summary.get("test_events", {}).get(case, {})
        root = summary["tests"].get(case.split("/", 1)[0], {})
        return event.get("run") == 1 and event.get("fail") == 1 and root.get("run") == 1 and root.get("fail") == 1
    counts = summary["tests"].get(case, {})
    return counts.get("run") == 1 and counts.get("fail") == 1


def diagnose_miss(runner, frozen, policy, inspection=None):
    spec = MISSES[frozen["sha"]]
    result = {"status": "unknown", "origin": "pinned-emitted-unit",
              "original_job": {"run_id": spec["run_id"], "run_attempt": spec["run_attempt"],
                               "job_id": spec["job_id"], "pr": spec["pr"]},
              "original_tests": spec["original_tests"], "evidence": spec["evidence"]}
    prefix = Path("sources") / frozen["sha"] / "misses"
    try:
        check(policy is not None, "固定策略未知；拒绝执行漏选回放")
        check(inspection is not None and inspection.get("status") in ("selected", "policy-refusal"),
              "缺少实际 inspect 选择证据")
        selected = inspection.get("selection", {})
        origin = "emitted-task" if inspection["status"] == "selected" else "target-projection-from-refused-inspect"
        tasks = selected.get("tasks", []) if inspection["status"] == "selected" else runner.project(inspection)
        check(isinstance(tasks, list) and all(isinstance(task, dict) for task in tasks), "实际策略任务非法")
        result.update(origin="pinned-emitted-unit" if origin == "emitted-task" else "pinned-projected-unit",
                      task_origin=origin, selector_status=inspection["status"],
                      full_plan_executable=inspection["status"] == "selected",
                      global_unresolved=selected.get("unresolved", []))
        sources, source_matches = miss_sources(runner, spec)
        result.update(source=sources, source_matches=source_matches)
        check(source_matches, "历史检出中缺少原测试源码或文件映射不符")
        choice = runner.select_unit(tasks, spec["package"], spec["test"], prefix)
        result["unit_selection"] = choice
        result["candidate"] = {"status": "unknown", "caught_original_defect": False, "unit_exercised": False,
                               "selected_but_unreached": False, "projected_unit": origin != "emitted-task",
                               "task_origin": origin, "selector_status": inspection["status"],
                               "full_plan_executable": inspection["status"] == "selected"}
        if choice["status"] != "selected":
            result["status"] = choice["status"]
            result["candidate"].update(status=choice["status"], reason=choice.get("error") or choice["status"])
            return result
        task = choice["matches"][0]
        result["candidate"]["task"] = task
        worker = runner.worker(task, prefix)
        result["candidate"]["worker"] = worker
        result["command"] = worker["command"]
        receipt = worker["receipt"]
        result["candidate"]["api_invoked"] = receipt.get("api_invoked", False)
        check(worker.get("tracked_unchanged"), "worker 清理后的 SHA/tree/跟踪文件状态改变或证据缺失")
        check(receipt.get("task") == task, "worker 回执与实际执行单元不符")
        returned = bool(receipt.get("returned"))
        exception = receipt.get("exception", "")
        if not returned and exception != "CalledProcessError":
            status = ("unsupported-worker-shape" if exception in ("AttributeError", "TypeError")
                      else "structural-refusal" if exception == "ValueError" else "worker-error")
            result["candidate"].update(status=status, reason=receipt.get("error") or exception or "worker 未返回")
            result["status"] = status
            return result
        go_commands = [row for row in receipt.get("commands", [])
                       if row.get("argv", [])[:2] == ["go", "test"] and "-run" in row["argv"]]
        result["candidate"]["actual_go_commands"] = go_commands
        if not go_commands:
            if returned:
                result["candidate"].update(status="selected-but-unreached", selected_but_unreached=True,
                                           reason="实际 worker 未产生 Go 测试命令")
                result["status"] = "selected-but-unreached"
            else:
                status = ("unsupported-worker-shape" if exception in ("AttributeError", "TypeError")
                          else "structural-refusal" if exception == "ValueError" else "worker-error")
                result["candidate"].update(status=status, reason=receipt.get("error") or exception or "worker 未返回")
                result["status"] = status
            return result
        native = all("-race" in row["argv"] and "-json" in row["argv"] and "-run" in row["argv"]
                     and row["argv"][-1] == task["package"] for row in go_commands)
        result["candidate"]["native_race"] = native
        if not native:
            result["candidate"].update(status="unsupported-worker-shape", reason="实际 worker 未使用原生 -race JSON 单元")
            result["status"] = "unsupported-worker-shape"
            return result
        detail = receipt.get("detail") if isinstance(receipt.get("detail"), dict) else {}
        compiled = detail.get("compiled")
        selected_names = detail.get("selected")
        patterns = [row["argv"][row["argv"].index("-run") + 1] for row in go_commands if "-run" in row["argv"]]
        pattern_selected = any(re.fullmatch(pattern, spec["test"]) for pattern in patterns)
        observed = set()
        for row in go_commands:
            path = runner.output / worker["directory"] / row.get("stdout", "missing.stdout")
            text = path.read_text(encoding="utf-8") if "stdout" in row and not row.get("evidence_error") and path.is_file() else ""
            for line in text.splitlines():
                try:
                    event = decode(line)
                except (ValueError, TypeError):
                    continue
                if isinstance(event, dict) and event.get("Package") == task["package"] and event.get("Action") == "run":
                    name = event.get("Test", "")
                    if name and "/" not in name:
                        observed.add(name)
        result["candidate"]["compiled_contains_original"] = (isinstance(compiled, dict) and spec["test"] in compiled) or spec["test"] in observed
        result["candidate"]["selected_contains_original"] = (spec["test"] in selected_names if isinstance(selected_names, dict)
                                                             else pattern_selected)
        if not result["candidate"]["selected_contains_original"]:
            result["candidate"].update(status="selected-but-unreached", selected_but_unreached=True,
                                       reason="实际 worker 选择未包含原测试")
            result["status"] = "selected-but-unreached"
            return result
        partition_names = worker["receipt"].get("partition", {}).get("selected")
        names = (list(task["names"]) if task.get("names") is not None else
                 list(selected_names) if isinstance(selected_names, dict) else
                 list(partition_names) if isinstance(partition_names, dict) else sorted(observed))
        if not names:
            result["candidate"].update(status="selected-but-unreached", selected_but_unreached=True,
                                       reason="实际 worker 未产生原测试事件")
            result["status"] = "selected-but-unreached"
            return result
        summary = worker_observations(runner, worker, task, names)
        result["go_events"] = summary
        result["candidate"]["unit_exercised"] = bool(summary["commands"])
        result["candidate"]["tests_exercised"] = [name for name, counts in summary["tests"].items() if counts["run"]]
        result["candidate"]["original_failed_tests"] = [name for name in spec["original_tests"]
                                                        if summary.get("test_events", {}).get(name, {}).get("fail") == 1
                                                        or summary["tests"].get(name, {}).get("fail") == 1]
        signatures = original_signatures(summary, miss_matchers(spec))
        result["source_signatures"] = signatures
        matched = any(row["matches"] for row in signatures)
        failed = case_failed(summary, spec["case"])
        go_rc = 1 if any(row["rc"] == 1 for row in go_commands) else 0 if all(row["rc"] == 0 for row in go_commands) else None
        result["candidate"].update(original_case_failed=failed, source_signature_matches=matched,
                                   actual_go_rc=[row["rc"] for row in go_commands])
        text = "\n".join((runner.output / worker["command"][key]).read_text(encoding="utf-8", errors="replace")
                         for key in ("stdout", "stderr"))
        timed_out = worker["command"]["timeout"] or "panic: test timed out after " in text
        result["partial_coverage"] = not summary["complete"]
        result["candidate"]["partial_coverage"] = not summary["complete"]
        def miss_red(events):
            here = original_signatures(events, miss_matchers(spec))
            failed_here = case_failed(events, spec["case"])
            return ({"source_signatures": here, "original_case_failed": failed_here}
                    if failed_here and any(row["matches"] for row in here) else None)
        target = target_red(summary, worker["command"]["rc"], task, spec["test"], miss_red, timed_out)
        if target:
            result.update(status="red-reproduced", target_evidence=target)
            result["candidate"].update(status="red-reproduced", caught_original_defect=True,
                                       target_command=target["command"])
        elif timed_out:
            result.update(status="unknown", failure_kind="timeout-infrastructure")
            result["candidate"].update(status="unknown", reason="timeout-infrastructure")
        elif summary["complete"] and summary["tests"].get(spec["test"], {}).get("skip") == 1:
            result.update(status="selected-but-unreached")
            result["candidate"].update(status="selected-but-unreached", selected_but_unreached=True,
                                       reason="原测试被跳过")
        elif summary["complete"] and go_rc == 0 and not any(
                counts["fail"] for counts in summary["tests"].values()):
            result.update(status="complete-no-red")
            result["candidate"]["status"] = "complete-no-red"
        elif summary["complete"] and go_rc == 1:
            result.update(status="original-assertion-unconfirmed")
            result["candidate"].update(status="original-assertion-unconfirmed",
                                       reason="原测试未失败或诊断行/文件不匹配")
        else:
            result["candidate"].update(status="selected-but-unreached",
                                       selected_but_unreached=not any(
                                           counts["run"] for counts in summary["tests"].values()),
                                       reason="Go JSON 覆盖不完整")
            result["status"] = "unknown"
        return result
    except Exception as error:
        result["error"] = f"{type(error).__name__}: {error}"
        return result


def context():
    check(os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch" and os.environ.get("GITHUB_REPOSITORY") == REPOSITORY
          and os.environ.get("GITHUB_REF") == REF and os.environ.get("GITHUB_SERVER_URL") == "https://github.com", "仅允许固定仓库/实验分支的手动调度")
    sha = os.environ.get("GITHUB_SHA")
    check(valid_sha(sha) and os.environ.get("GITHUB_WORKFLOW_SHA") == sha, "controller/workflow SHA 非法或不一致")
    expected_ref = REPOSITORY + "/.github/workflows/ci.yml@" + REF
    check(os.environ.get("GITHUB_WORKFLOW_REF") == expected_ref, "controller workflow 身份不符")
    run_id, attempt = os.environ.get("GITHUB_RUN_ID", ""), os.environ.get("GITHUB_RUN_ATTEMPT", "")
    check(all(re.fullmatch(r"[1-9][0-9]{0,19}", n) is not None for n in (run_id, attempt)), "run/attempt 非法")
    return {"repository": REPOSITORY, "ref": REF, "sha": sha, "workflow_sha": sha,
            "workflow_ref": expected_ref, "run_id": run_id, "run_attempt": attempt}


def public_run(expected):
    url = f'https://api.github.com/repos/{REPOSITORY}/actions/runs/{expected["run_id"]}/attempts/{expected["run_attempt"]}'
    request = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28",
                                                   "User-Agent": "ci-historical-audit"})
    token = os.environ.pop("AUDIT_READ_TOKEN", "")
    if token:
        # Initial fixed GitHub API read only; never forward authorization through redirects or child environments.
        request.add_unredirected_header("Authorization", "Bearer " + token)
    for attempt in range(2):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                result = decode(response.read().decode("utf-8"))
            bind_run(result, expected)
            return {key: result[key] for key in ("id", "run_attempt", "head_sha", "head_branch", "path", "event", "repository")}
        except urllib.error.URLError as error:
            transient = not isinstance(error, urllib.error.HTTPError) or error.code in (429, 500, 502, 503, 504)
            if attempt or not transient:
                raise
            time.sleep(1)


def run_audit(root):
    runner = Runner(root)
    index = {"schema": 1, "experimental": True, "public_base": PUBLIC_BASE, "status": "in-progress",
             "scope": "仅采集历史证据，不代表策略接受、Full 或产品就绪",
             "started_at": datetime.now(timezone.utc).isoformat(), "rows": [], "errors": [],
             "graph_cache": "none; actual pinned inspect runs independently for every source SHA"}
    start = time.monotonic()
    path = runner.output / "index.json"
    try:
        expected = context()
        check(HERE.parents[1] == root / "controller", "controller 目录不符")
        controller = runner.identity(root / "controller", expected["sha"])
        check(runner.git(root / "controller", "rev-parse", "HEAD").decode().strip() == expected["sha"] and
              not runner.git(root / "controller", "status", "--porcelain=v1", "--untracked-files=all"), "controller 检出不符或不干净")
        cases_path = HERE / "ci-historical-cases.json"
        cases = manifest(cases_path.read_text(encoding="utf-8"))
        check(len(cases) == 113, "冻结清单必须为 113 个公开 checkout")
        event = decode(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
        options = request(event.get("inputs"), len(cases))
        index.update(context=expected, controller=controller, request=options, manifest_count=len(cases),
                     manifest_sha256=hashlib.sha256(cases_path.read_bytes()).hexdigest())
        if options["phase"] == "misses":
            index["scope"] = "仅采集三例历史 Merge gate 漏选的实际/投影执行证据，不代表策略接受、Full 或产品就绪"
        selected, diagnostic_shas = select_sources(cases, options)
        selector_shas = {row["sha"] for row in selected}
        exercise_shas = diagnostic_shas | (selector_shas if options["phase"] == "misses" else set())
        by_sha = {row["sha"]: row for row in cases}
        for sha in diagnostic_shas:
            check(sha in by_sha and [DIAGNOSTICS[sha]["run_id"], 1] in by_sha[sha]["runs"], "诊断原始 run/SHA 不在冻结清单")
        if options["phase"] == "misses":
            for sha in selector_shas:
                check(sha in MISSES and [MISSES[sha]["run_id"], MISSES[sha]["run_attempt"]] in by_sha[sha]["runs"],
                      "漏选原始 run/SHA 不在冻结清单")
        # Diagnostics remain independent of planner names and selector pagination.
        order = list(dict.fromkeys([sha for sha in DIAGNOSTICS if sha in diagnostic_shas] + [row["sha"] for row in selected]))
        index["rows"] = [{**by_sha[sha], "source_state": "pending",
                          "selector": {"status": "pending" if sha in selector_shas | diagnostic_shas else "not-requested"},
                          "diagnostic": {"status": "pending" if sha in exercise_shas else "not-requested"}} for sha in order]
        write_json(path, index)
        index["api_run"] = public_run(expected)
        version = subprocess.run(["go", "version"], env=runner.env, capture_output=True, check=True, timeout=30).stdout.decode().strip()
        index["runtime"] = {"go": version, "python": sys.version, "host": platform.node(), "platform": platform.platform(),
                            "git": runner.git(root, "--version").decode().strip()}
        check(version == "go version go1.26.0 linux/amd64", "必须使用 Linux/amd64 Go 1.26.0")
        runner.git(root, "init", "--bare", runner.objects)
        runner.git(runner.objects, "remote", "add", "origin", REMOTE)
        index["public_remote"] = REMOTE
        policy = None
        try:
            policy = runner.prepare_policy(options["policy_sha"])
            index["policy"] = policy
        except Exception as error:
            index["errors"].append(f"策略固定失败：{type(error).__name__}: {error}")
        write_json(path, index)
        index["fetches"] = runner.prefetch(order)
        write_json(path, index)
        for row in index["rows"]:
            source_dir = runner.output / "sources" / row["sha"]
            try:
                row["observed_source"] = runner.checkout_source(row)
                row["source_state"] = "exact"
            except Exception as error:
                row.update(source_state="unknown", source_error=f"{type(error).__name__}: {error}")
                if time.monotonic() < runner.deadline and not runner.exists_commit(row["sha"]):
                    row["source_state"] = "missing"
            write_json(path, index)
            if row["source_state"] == "exact":
                # Inspect once, before any diagnostic, including diagnostics-only dispatches.
                if row["selector"]["status"] == "pending":
                    row["selector"] = runner.inspect(row) if policy is not None else {"status": "unknown", "error": "固定策略不可用"}
                    write_json(path, index)
                if row["diagnostic"]["status"] == "pending":
                    row["diagnostic"] = (diagnose_miss(runner, row, policy, row["selector"])
                                         if options["phase"] == "misses" else diagnose(runner, row, policy, row["selector"]))
                    write_json(path, index)
            write_json(source_dir / "row.json", row)
            write_json(path, index)
    except Exception as error:
        index["errors"].append(f"{type(error).__name__}: {error}")
    finally:
        finish(index)
        index["elapsed_seconds"] = round(time.monotonic() - start, 3)
        for row in index["rows"]:
            write_json(runner.output / "sources" / row["sha"] / "row.json", row)
        write_json(path, index)
    return index


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("run", "finalize"))
    args = parser.parse_args()
    check(os.environ.get("GITHUB_WORKSPACE"), "只能在隔离的 Actions 工作区执行")
    root = Path(os.environ["GITHUB_WORKSPACE"]).resolve()
    path = root / "evidence/index.json"
    if args.command == "run":
        # Refuse before creating any local checkout when called outside the fixed controller.
        context()
        index = run_audit(root)
    else:
        try:
            index = decode(path.read_text(encoding="utf-8"))
        except (ValueError, OSError) as error:
            if path.exists():
                path.replace(path.with_name("index.invalid.json"))
            index = {"schema": 1, "rows": [], "errors": [f"终态索引不可用：{error}"]}
        finish(index)
        for row in index["rows"]:
            write_json(root / "evidence/sources" / row["sha"] / "row.json", row)
        write_json(path, index)
    print(json.dumps({key: index[key] for key in ("status", "coverage")}, ensure_ascii=False))
    return int(args.command == "run" and index["status"] != "complete")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        print(f"历史审计失败：{type(error).__name__}: {error}", file=sys.stderr)
        sys.exit(1)
