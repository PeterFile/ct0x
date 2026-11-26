#!/usr/bin/env python3
"""Bounded exact-test replay. This process never collects authenticated API evidence."""
import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time


SHA_RE = r"[0-9a-f]{40}"
PACKAGE_RE = r"agentworkspace/(?:cmd/workspace|internal/[A-Za-z][A-Za-z0-9_]*)"
TEST_RE = r"Test[A-Z_][A-Za-z0-9_]*"
MAX_COMMITS, REPEATS, MAX_LOG = 32, 3, 16 * 1024 * 1024


def check(condition, message):
    if not condition:
        raise ValueError(message)


def clean_line(line):
    return re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", line).rstrip()


def message(line):
    match = re.match(r"\s*(?:[^\s:]+/)*[^\s:]+\.go:\d+(?::\d+)?:\s*(.+)", line)
    return match[1].strip() if match else None


def failures(text):
    """Only Go failure lifecycle/package records establish a named failure, never mentions."""
    if re.search(r"panic:|timed out|deadline exceeded|i/o timeout|fatal error:|signal: killed|\[build failed\]", text, re.I):
        return []
    groups, json_rows, pending, messages = {}, [], [], []
    for raw in text.splitlines():
        line = clean_line(raw)
        if line.startswith("{"):
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if isinstance(row, dict) and "Action" in row:
                json_rows.append(row)
            continue
        failed = re.match(r"\s*--- FAIL: (Test[^\s]+) \([0-9.]+s\)$", line)
        if failed:
            pending.append(failed[1])
        diagnostic = message(line)
        if diagnostic:
            messages.append(diagnostic)
        package = re.fullmatch(r"FAIL\s+(" + PACKAGE_RE + r")\s+[0-9.]+s", line)
        if package:
            for name in pending:
                root = name.split("/", 1)[0]
                if re.fullmatch(TEST_RE, root):
                    group = groups.setdefault((package[1], root), {"tests": set(), "messages": set()})
                    group["tests"].add(name)
                    group["messages"].update(messages)
            pending, messages = [], []
        elif line.startswith(("ok \t", "FAIL\t", "FAIL ")):
            pending, messages = [], []
    failed_packages = {row.get("Package") for row in json_rows if row.get("Action") == "fail" and not row.get("Test")}
    for row in json_rows:
        package, name = row.get("Package"), row.get("Test")
        if not isinstance(package, str) or not re.fullmatch(PACKAGE_RE, package) or package not in failed_packages:
            continue
        if not isinstance(name, str) or not re.fullmatch(TEST_RE, name.split("/", 1)[0]):
            continue
        root = name.split("/", 1)[0]
        if row.get("Action") == "fail":
            group = groups.setdefault((package, root), {"tests": set(), "messages": set()})
            group["tests"].add(name)
    for row in json_rows:
        package, name = row.get("Package"), row.get("Test")
        if row.get("Action") != "output" or not isinstance(name, str) or not isinstance(row.get("Output"), str):
            continue
        group = groups.get((package, name.split("/", 1)[0]))
        if group:
            group["messages"].update(value for line in row["Output"].splitlines() if (value := message(line)))
    return [{"package": package, "test": root,
             "signature": {key: sorted(values) for key, values in signature.items()}}
            for (package, root), signature in sorted(groups.items())]


def rows(text, package):
    def pairs(items):
        result = {}
        for key, value in items:
            check(key not in result, "Go JSON 重复字段")
            result[key] = value
        return result
    values = [json.loads(line, object_pairs_hook=pairs) for line in text.splitlines() if line]
    actions = {"start", "run", "pause", "cont", "pass", "fail", "skip", "output", "bench", "attr", "artifacts"}
    check(bool(values) and all(isinstance(row, dict) and row.get("Package") == package and row.get("Action") in actions
                              for row in values), "Go JSON 包身份或动作不符")
    check(sum(row.get("Action") == "start" for row in values) == 1 and values[0]["Action"] == "start", "包 start 缺失或重复")
    terminal = [index for index, row in enumerate(values) if not row.get("Test") and row["Action"] in ("pass", "fail", "skip")]
    check(terminal == [len(values) - 1], "包终止事件缺失、重复或不在末尾")
    for row in values:
        check("Test" not in row or isinstance(row["Test"], str) and bool(row["Test"]), "Go JSON Test 类型非法")
        check("Output" not in row or isinstance(row["Output"], str), "Go JSON Output 类型非法")
    return values


def inventory(listing, list_code, candidate):
    listed = rows(listing, candidate["package"])
    check(type(list_code) is int and list_code == 0 and listed[-1]["Action"] == "pass" and
          not any(row.get("Test") for row in listed), "编译清单不完整")
    names = [line for row in listed if row["Action"] == "output" for line in row.get("Output", "").splitlines()
             if re.fullmatch(TEST_RE, line)]
    check(names in ([], [candidate["test"]]), "编译清单未精确匹配同名测试")
    return bool(names)


def assess(listing, list_code, text, code, candidate):
    """Pure proof checker, shared by credential-free replay and authenticated finalization."""
    try:
        package, test = candidate["package"], candidate["test"]
        if not inventory(listing, list_code, candidate):
            return {"state": "absent"}
        events = rows(text, package)
        check(type(code) is int and code in (0, 1), "测试执行异常")
        check(all(not row.get("Test") or row["Test"] == test or row["Test"].startswith(test + "/") for row in events), "执行了其他测试")
        check(not any(row["Action"] == "skip" for row in events), "测试或子测试跳过")
        runs = Counter(row.get("Test") for row in events if row["Action"] == "run")
        terminal = Counter(row.get("Test") for row in events if row["Action"] in ("pass", "fail") and row.get("Test"))
        check(runs.get(test) == 1 and runs == terminal and all(count == 1 for count in runs.values()), "精确测试生命周期不完整")
        active = set()
        for row in events:
            name, action = row.get("Test"), row["Action"]
            if not name:
                continue
            if action == "run":
                check(name not in active and (name == test or test in active), "子测试缺少活动根")
                active.add(name)
            elif action in ("pass", "fail"):
                check(name in active, "测试终止早于执行")
                active.remove(name)
        check(not active, "测试未终止")
        if code == 0:
            check(events[-1]["Action"] == "pass" and not any(row["Action"] == "fail" for row in events), "通过证据不完整")
            return {"state": "pass"}
        found = failures(text)
        check(len(found) == 1 and found[0]["test"] == test and events[-1]["Action"] == "fail", "命名失败签名缺失、超时或有歧义")
        return {"state": "fail", "signature": found[0]["signature"]}
    except (ValueError, TypeError, KeyError) as error:
        return {"state": "unknown", "reason": str(error)}


def unknown(reason, **evidence):
    return {"status": "unknown", "reason": reason, **evidence}


def localize(chain, candidate, probe):
    evidence = {"chain": chain, "candidate": candidate, "probes": []}
    if not 2 <= len(chain) <= MAX_COMMITS or len(set(chain)) != len(chain):
        return unknown("首亲范围缺失或超过回放上限", **evidence)
    states = []
    for sha in chain:
        results = [probe(sha, candidate) for _ in range(REPEATS)]
        evidence["probes"].append({"sha": sha, "results": results})
        state = results[0].get("state")
        if state not in ("pass", "fail", "absent") or any(row.get("state") != state for row in results):
            return unknown("测试不稳定、缺少执行证明或工具链/基础设施失败", **evidence)
        if state == "fail" and any(row.get("signature") != candidate["signature"] for row in results):
            return unknown("失败签名不一致", **evidence)
        states.append(state)
    if states[-1] != "fail" or states[0] == "fail":
        return unknown("未在可信绿点与失败点之间复现转换", **evidence)
    index = states.index("fail")
    if any(state != "fail" for state in states[index:]):
        return unknown("范围内存在多个转换", **evidence)
    if states[index - 1] == "absent":
        if any(state != "absent" for state in states[:index]):
            return unknown("测试消失后重现，不能证明首次引入", **evidence)
        absent = evidence["probes"][index - 1]["results"]
        present = evidence["probes"][index]["results"]
        if any(row.get("definition", {}).get("state") != "absent" for row in absent) or any(
                row.get("definition", {}).get("state") != "present" for row in present):
            return unknown("仅编译清单变化，未证明测试源码首次引入", **evidence)
        transition = "new-test"
    else:
        if "absent" in states[:index]:
            return unknown("绿点缺少同名测试，不能证明同一回归", **evidence)
        transition = "regression"
    return unknown("已重复复现，等待精确合并 PR 核验", localization="reproduced", transition=transition,
                   good_sha=chain[index - 1], bad_sha=chain[-1], commit=chain[index], **evidence)


def child_env(home):
    # Allowlist, rather than a denylist: no Actions/API tokens, CLI auth, git config or secrets.
    return {"PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"), "HOME": str(home),
            "TMPDIR": str(home), "LANG": "C.UTF-8", "GOENV": "off", "GOWORK": "off",
            "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly", "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0", "GOCACHE": str(home / "go-cache"),
            "GOMODCACHE": str(home / "go-mod")}


def git(root, *args):
    with tempfile.TemporaryDirectory(prefix="periodic-git-") as home:
        return subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
                                        "-C", str(root), *args], env=child_env(Path(home)), stderr=subprocess.PIPE,
                                       timeout=30).decode().strip()


def definition(root, sha, candidate):
    directory = candidate["package"].removeprefix("agentworkspace/")
    paths = git(root, "ls-tree", "-r", "--name-only", "-z", sha, "--", directory).split("\0")
    files, mentions, declarations = {}, [], []
    for path in paths:
        if not path.endswith("_test.go") or str(Path(path).parent) != directory:
            continue
        source = git(root, "show", sha + ":" + path)
        check(len(source.encode()) <= MAX_LOG, "测试源码超过上限")
        files[path] = hashlib.sha256(source.encode()).hexdigest()
        if re.search(r"\b" + re.escape(candidate["test"]) + r"\b", source):
            mentions.append(path)
        if re.search(r"(?m)^func\s+" + re.escape(candidate["test"]) + r"\s*\(\s*\w+\s+\*testing\.T\s*\)", source):
            declarations.append(path)
    state = "present" if len(declarations) == 1 else "unknown" if mentions else "absent"
    return {"state": state, "files": files, "declarations": declarations}


class Probe:
    def __init__(self, root, output, timeout=90, budget=1200):
        self.root, self.output, self.timeout = Path(root), Path(output), timeout
        self.output.mkdir(parents=True, exist_ok=True)
        self.deadline, self.index = time.monotonic() + budget, 0
        self.definitions = {}
        self.home = tempfile.TemporaryDirectory(prefix="periodic-replay-home-")
        self.env = child_env(Path(self.home.name))

    def close(self):
        self.home.cleanup()

    def execute(self, argv, cwd, label):
        path = self.output / f"{self.index:04d}-{label}.jsonl"
        result = {"argv": argv, "log": path.name}
        remaining = min(self.timeout, self.deadline - time.monotonic())
        if remaining <= 0:
            return {**result, "state": "unknown", "reason": "回放总时间达到上限"}, ""
        with path.open("wb") as output:
            process = subprocess.Popen(argv, cwd=cwd, env=self.env, stdout=output, stderr=subprocess.STDOUT,
                                       start_new_session=True)
            deadline = time.monotonic() + remaining
            while process.poll() is None:
                if time.monotonic() >= deadline or path.stat().st_size > MAX_LOG:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.wait()
                    result["timed_out"] = True
                    break
                time.sleep(0.1)
        with path.open("rb") as stream:
            result.update(returncode=process.returncode, sha256=hashlib.file_digest(stream, "sha256").hexdigest())
        if path.stat().st_size > MAX_LOG or result.get("timed_out"):
            return {**result, "state": "unknown", "reason": "回放超时或日志超过上限"}, ""
        return result, path.read_text(encoding="utf-8", errors="replace")

    def __call__(self, sha, candidate):
        check(isinstance(sha, str) and re.fullmatch(SHA_RE, sha), "回放 SHA 非法")
        package, test = candidate.get("package"), candidate.get("test")
        check(isinstance(package, str) and re.fullmatch(PACKAGE_RE, package), "回放包非法")
        check(isinstance(test, str) and re.fullmatch(TEST_RE, test), "回放测试名非法")
        check(type(candidate.get("race")) is bool, "回放 race 类型非法")
        self.index += 1
        if time.monotonic() >= self.deadline:
            return {"state": "unknown", "reason": "回放总时间达到上限"}
        details = {"sha": sha, "tree": git(self.root, "rev-parse", sha + "^{tree}")}
        try:
            if sha not in self.definitions:
                self.definitions[sha] = definition(self.root, sha, candidate)
            details["definition"] = self.definitions[sha]
            with tempfile.TemporaryDirectory(prefix="periodic-probe-") as temporary:
                checkout = Path(temporary) / "source"
                git(self.root, "worktree", "add", "--quiet", "--detach", str(checkout), sha)
                try:
                    check(re.search(r"(?m)^module agentworkspace\s*$", (checkout / "go.mod").read_text()), "模块身份不符")
                    check(git(checkout, "rev-parse", "HEAD") == sha, "回放 checkout 不符")
                    pattern = "^" + test + "$"
                    flags = ["-race"] if candidate["race"] else []
                    listed, listing = self.execute(["go", "test", "-json", *flags, "-list", pattern,
                                                    "./" + package.removeprefix("agentworkspace/")], checkout, "list")
                    details["list"] = listed
                    check(not listed.get("state"), "列举测试失败")
                    if not inventory(listing, listed.get("returncode"), candidate):
                        return {**details, "state": "absent"}
                    execution, text = self.execute(["go", "test", "-count=1", *flags, "-json", "-timeout=60s", "-run", pattern,
                                                   "./" + package.removeprefix("agentworkspace/")], checkout, "run")
                    details["run"] = execution
                    check(not execution.get("state"), "测试执行异常")
                    return {**details, **assess(listing, listed.get("returncode"), text, execution.get("returncode"), candidate)}
                finally:
                    git(self.root, "worktree", "remove", "--force", str(checkout))
        except (ValueError, OSError, subprocess.SubprocessError) as error:
            return {**details, "state": "unknown", "reason": str(error)}

def replay(bundle, root, output):
    initial = json.loads((bundle / "receipt.json").read_text())
    candidates = initial.get("failure_candidates", [])
    baseline = initial.get("ancestor")
    result = unknown("无可信绿点、失败日志缺失、多个失败签名或非 Go 作业")
    if len(candidates) == 1 and baseline:
        good, bad = baseline["sha"], initial["sha"]
        chain = git(root, "rev-list", "--first-parent", "--max-count=33", bad).splitlines()
        if good in chain:
            chain = list(reversed(chain[:chain.index(good) + 1]))
            check(git(root, "rev-parse", bad + "^{tree}") == initial["tree"], "失败点 tree 不符")
            check(git(root, "rev-parse", good + "^{tree}") == baseline["tree"], "绿点 tree 不符")
            runner = Probe(root, output)
            try:
                result = localize(chain, candidates[0], runner)
            finally:
                runner.close()
        else:
            result = unknown("可信绿点不在有界首亲范围内")
    result["initial_sha256"] = hashlib.sha256((bundle / "receipt.json").read_bytes()).hexdigest()
    (output / "localization.json").write_text(json.dumps(result, sort_keys=True) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "root", "output"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    try:
        replay(args.bundle, args.root, args.output)
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        result = unknown("回放无法完成：" + str(error))
        result["initial_sha256"] = hashlib.sha256((args.bundle / "receipt.json").read_bytes()).hexdigest()
        (args.output / "localization.json").write_text(json.dumps(result, sort_keys=True) + "\n")
        print(result["reason"], file=sys.stderr)
