#!/usr/bin/env python3
"""Bounded exact-test replay. This process never collects authenticated API evidence."""
import argparse
from collections import Counter
from decimal import Decimal
import hashlib
import importlib.util
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
REPEATS, MAX_LOG = 3, 16 * 1024 * 1024


def check(condition, message):
    if not condition:
        raise ValueError(message)


def duration(value, limit=21600):
    check(isinstance(value, str) and 0 < len(value) <= 64 and
          re.fullmatch(r"(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|μs|ms|s|m|h))+", value), "Go timeout 必须为可信有限 duration literal")
    units = {"h": 3600, "m": 60, "s": 1, "ms": Decimal("0.001"), "us": Decimal("0.000001"),
             "µs": Decimal("0.000001"), "μs": Decimal("0.000001"), "ns": Decimal("0.000000001")}
    seconds = sum(Decimal(number) * units[unit] for number, unit in re.findall(r"([0-9]+(?:\.[0-9]+)?)(ns|us|µs|μs|ms|s|m|h)", value))
    check(0 < seconds <= limit, "Go timeout 超过原始 hosted 作业预算或禁用了超时")
    return int(seconds) if seconds == int(seconds) else float(seconds)


def clean_line(line):
    return re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", line).rstrip()


def message(line):
    match = re.match(r"\s*(?:[^\s:]+/)*[^\s:]+\.go:\d+(?::\d+)?:\s*(.+)", line)
    return match[1].strip() if match else None


def unique_pairs(items):
    result = {}
    for key, value in items:
        check(key not in result, "Go JSON 重复字段")
        result[key] = value
    return result


def failure_records(text):
    """Keep completed, scoped failures alongside separate package interruptions."""
    problem = re.compile(r"panic:|timed out|deadline exceeded|i/o timeout|fatal error:|signal: killed|\[build failed\]", re.I)
    groups, json_rows, pending, unbound, tainted, active = {}, [], {}, [], False, None
    failed_packages, unknown_packages, reports = set(), set(), {}
    verbose = False

    def update(tests, name, action, implicit=False):
        prior, root = tests.get(name), name.split("/", 1)[0]
        parent_active = name == root or tests.get(root, {}).get("state") == "run"
        if action == "run":
            tests[name] = {"state": "run" if prior is None and parent_active else "invalid", "messages": set()}
        elif prior and prior["state"] == "run":
            prior["state"] = action if parent_active else "invalid"
        else:
            tests[name] = {"state": action if prior is None and implicit else "invalid", "messages": set()}

    def incomplete(tests):
        return any(value["state"] in ("run", "invalid", "fail") for value in tests.values())

    def retain(package, tests, interrupted):
        accounted = set()
        if re.fullmatch(PACKAGE_RE, package):
            roots = {}
            for name, value in tests.items():
                roots.setdefault(name.split("/", 1)[0], {})[name] = value
            for root, scoped in sorted(roots.items()):
                if not re.fullmatch(TEST_RE, root) or scoped.get(root, {}).get("state") != "fail" or any(
                        value["state"] in ("run", "invalid") for value in scoped.values()):
                    continue
                names = sorted(name for name, value in scoped.items() if value["state"] == "fail")
                diagnostics = sorted({value for name in names for value in scoped[name]["messages"]})
                # A timeout does not prove an assertion; only its own completed scope can.
                if interrupted and not diagnostics:
                    continue
                signature = {"tests": names, "messages": diagnostics}
                groups[(package, root, tuple(names), tuple(diagnostics))] = {"package": package, "test": root, "signature": signature}
                accounted.update(names)
        if interrupted or any(value["state"] in ("run", "invalid", "fail") and name not in accounted
                              for name, value in tests.items()):
            unknown_packages.add(package)

    for raw in text.splitlines():
        line = clean_line(raw)
        if line.startswith("{"):
            try:
                row = json.loads(line, object_pairs_hook=unique_pairs)
            except ValueError:
                if '"Action"' in line:
                    unbound.append({"package": None, "reason": "Go JSON 截断或字段重复"})
                continue
            if isinstance(row, dict) and row.get("Action") in ("build-output", "build-fail"):
                if isinstance(row.get("ImportPath"), str) and re.fullmatch(PACKAGE_RE, row["ImportPath"]):
                    if row["Action"] == "build-fail":
                        unknown_packages.add(row["ImportPath"])
                else:
                    unbound.append({"package": None, "reason": "Go build 归属字段非法"})
                continue
            if isinstance(row, dict) and "Action" in row:
                if not isinstance(row.get("Package"), str) or "Test" in row and (not isinstance(row["Test"], str) or not row["Test"]):
                    unbound.append({"package": None, "reason": "Go JSON 身份字段非法"})
                else:
                    if pending or tainted:
                        if tainted or incomplete(pending):
                            unbound.append({"package": None, "reason": "纯文本 Go 失败缺少包终止，不能借用 JSON 归属"})
                        pending, tainted, active, reports, verbose = {}, False, None, {}, False
                    json_rows.append(row)
            continue
        terminal = re.fullmatch(r"( *)--- (FAIL|PASS|SKIP): (Test[^\s]+) \([0-9.]+s\)", line)
        marker = re.fullmatch(r"=== (RUN|PAUSE|CONT|NAME)\s+(Test[^\s]+)", line)
        if not tainted:
            if marker:
                action, name = marker[1].lower(), marker[2]
                reports = {}
                if action == "run":
                    verbose = True
                    update(pending, name, action)
                active = name if action != "pause" and pending.get(name, {}).get("state") == "run" else None
            elif line == "=== NAME":
                active, reports = None, {}
            elif terminal:
                indent, action, active = len(terminal[1]), terminal[2].lower(), terminal[3]
                update(pending, active, action, implicit=not verbose)
                reports = {depth: name for depth, name in reports.items() if depth < indent}
                reports[indent] = active
            diagnostic = message(line)
            if diagnostic:
                # Non-verbose Go buffers nested reports; indentation can return to the parent.
                indent = len(line) - len(line.lstrip(" "))
                depth = max((depth for depth in reports if depth < indent), default=None)
                scope = reports[depth] if depth is not None else active
                if scope in pending and pending[scope]["state"] != "invalid":
                    pending[scope]["messages"].add(diagnostic)
                elif problem.search(line):
                    tainted = True
            elif problem.search(line):
                tainted = True
        package = re.fullmatch(r"FAIL\s+([A-Za-z0-9_./-]+)\s+(?:[0-9.]+s|\[build failed\])", line)
        if package:
            value = package[1]
            failed_packages.add(value)
            retain(value, pending, tainted)
            pending, tainted, active, reports, verbose = {}, False, None, {}, False
        elif re.match(r"(?:ok|\?|FAIL)\s", line):
            pending, tainted, active, reports, verbose = {}, False, None, {}, False
        if terminal or marker or re.match(r"(?:ok|\?|FAIL)\s", line):
            if json_rows and json_rows[-1] is not None:
                json_rows.append(None)

    scopes = {}
    for row in json_rows:
        if row is None:
            # Plain Go output starts a separate execution, never a JSON continuation.
            unknown_packages.update(package for package, scope in scopes.items() if scope["tainted"] or incomplete(scope["tests"]))
            scopes = {}
            continue
        package, name, action = row["Package"], row.get("Test"), row["Action"]
        if not re.fullmatch(PACKAGE_RE, package):
            if action == "fail":
                unknown_packages.add(package)
            continue
        if action == "start":
            if package in scopes and (scopes[package]["tainted"] or incomplete(scopes[package]["tests"])):
                unknown_packages.add(package)
            scopes[package] = {"tests": {}, "tainted": False}
        scope = scopes.setdefault(package, {"tests": {}, "tainted": False})
        tests = scope["tests"]
        if isinstance(row.get("Output"), str):
            for line in row["Output"].splitlines():
                diagnostic = message(line)
                if not scope["tainted"] and diagnostic and action == "output" and tests.get(name, {}).get("state") == "run":
                    tests[name]["messages"].add(diagnostic)
                elif problem.search(line):
                    scope["tainted"] = True
        if not name and action in ("pass", "fail", "skip"):
            if action == "fail":
                failed_packages.add(package)
                retain(package, tests, scope["tainted"])
            scopes.pop(package)
        elif name and not scope["tainted"] and action in ("run", "pass", "fail", "skip"):
            update(tests, name, action)
    for package, scope in scopes.items():
        if scope["tainted"] or incomplete(scope["tests"]):
            unknown_packages.add(package)
    known = [value for _, value in sorted(groups.items())]
    covered = {row["package"] for row in known}
    unresolved = [{"package": value, "reason": "包超时、基础设施失败或缺少完整命名失败"} for value in sorted(failed_packages - covered | unknown_packages)]
    if tainted or incomplete(pending):
        unresolved.append({"package": None, "reason": "未绑定包的超时、基础设施诊断或缺少包终止的命名失败"})
    return known, unresolved + unbound


def failures(text):
    return failure_records(text)[0]


def rows(text, package):
    values = [json.loads(line, object_pairs_hook=unique_pairs) for line in text.splitlines() if line]
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
        found, unresolved = failure_records(text)
        check(not unresolved and len(found) == 1 and found[0]["test"] == test and events[-1]["Action"] == "fail", "命名失败签名缺失、超时或有歧义")
        return {"state": "fail", "signature": found[0]["signature"]}
    except (ValueError, TypeError, KeyError) as error:
        return {"state": "unknown", "reason": str(error)}


def unknown(reason, **evidence):
    return {"status": "unknown", "reason": reason, **evidence}


def localize(chain, candidate, probe, *, remaining=None, prior=None):
    return worker("periodic-history").locate(chain, candidate, probe, remaining=remaining, prior=prior)


def first_parent_chain(root, initial):
    baseline = initial.get("ancestor")
    check(isinstance(baseline, dict), "无可信首亲绿点")
    for endpoint in (initial, baseline):
        check(isinstance(endpoint.get("sha"), str) and re.fullmatch(SHA_RE, endpoint["sha"]) and
              git(root, "rev-parse", endpoint["sha"] + "^{tree}") == endpoint.get("tree"), "范围端点 SHA/tree 不符")
    history = git(root, "rev-list", "--first-parent", initial["sha"]).splitlines()
    check(baseline["sha"] in history, "可信绿点不在实际首亲链中或历史对象缺失")
    return list(reversed(history[:history.index(baseline["sha"]) + 1]))


def child_env(home):
    # Allowlist, rather than a denylist: no Actions/API tokens, CLI auth, git config or secrets.
    return {"PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"), "HOME": str(home),
            "TMPDIR": str(home), "LANG": "C.UTF-8", "GOENV": "off", "GOWORK": "off",
            "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly", "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0", "GOCACHE": str(home / "go-cache"),
            "GOMODCACHE": str(home / "go-mod")}


def git(root, *args, strip=True):
    with tempfile.TemporaryDirectory(prefix="periodic-git-") as home:
        text = subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
                                        "-C", str(root), *args], env=child_env(Path(home)), stderr=subprocess.PIPE,
                                       timeout=30).decode()
        return text.strip() if strip else text


def definition(root, sha, candidate):
    directory = candidate["package"].removeprefix("agentworkspace/")
    paths = git(root, "ls-tree", "-r", "--name-only", "-z", sha, "--", directory).split("\0")
    files, mentions, declarations = {}, [], []
    for path in paths:
        if not path.endswith("_test.go") or str(Path(path).parent) != directory:
            continue
        source = git(root, "show", sha + ":" + path, strip=False)
        check(len(source.encode()) <= MAX_LOG, "测试源码超过上限")
        files[path] = hashlib.sha256(source.encode()).hexdigest()
        if re.search(r"\b" + re.escape(candidate["test"]) + r"\b", source):
            mentions.append(path)
        if re.search(r"(?m)^func\s+" + re.escape(candidate["test"]) + r"\s*\(\s*\w+\s+\*testing\.T\s*\)", source):
            declarations.append(path)
    state = "present" if len(declarations) == 1 else "unknown" if mentions else "absent"
    return {"state": state, "files": files, "declarations": declarations}


class Probe:
    def __init__(self, root, output, timeout=None, budget=12600):
        self.root, self.output, self.timeout = Path(root), Path(output), timeout
        self.output.mkdir(parents=True, exist_ok=True)
        self.started, self.budget, self.index = time.monotonic(), budget, 0
        self.deadline = self.started + budget
        self.definitions = {}
        self.home = tempfile.TemporaryDirectory(prefix="periodic-replay-home-")
        self.env = child_env(Path(self.home.name))

    def close(self):
        self.home.cleanup()

    def execute(self, argv, cwd, label, timeout=None):
        path = self.output / f"{self.index:04d}-{label}.jsonl"
        result = {"argv": argv, "log": path.name}
        limit = timeout if self.timeout is None else min(timeout, self.timeout)
        remaining = min(limit, self.deadline - time.monotonic())
        started = time.monotonic()
        result.update(limit_seconds=max(0, remaining))
        if remaining <= 0:
            return {**result, "state": "unknown", "reason": "回放总时间达到上限", "elapsed_seconds": time.monotonic() - self.started}, ""
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
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        with path.open("rb") as stream:
            result.update(returncode=process.returncode, sha256=hashlib.file_digest(stream, "sha256").hexdigest(),
                          elapsed_seconds=time.monotonic() - started)
        if path.stat().st_size > MAX_LOG or result.get("timed_out"):
            return {**result, "state": "unknown", "reason": "回放超时或日志超过上限"}, ""
        return result, path.read_text(encoding="utf-8", errors="replace")

    def __call__(self, sha, candidate):
        check(isinstance(sha, str) and re.fullmatch(SHA_RE, sha), "回放 SHA 非法")
        package, test = candidate.get("package"), candidate.get("test")
        check(isinstance(package, str) and re.fullmatch(PACKAGE_RE, package), "回放包非法")
        check(isinstance(test, str) and re.fullmatch(TEST_RE, test), "回放测试名非法")
        check(type(candidate.get("race")) is bool, "回放 race 类型非法")
        mode = candidate.get("execution", {})
        check(mode.get("mode") == ("race" if candidate["race"] else "ordinary") and
              type(mode.get("timeout_seconds")) in (int, float) and
              mode["timeout_seconds"] == duration(mode.get("timeout_arg"), mode.get("job_timeout_seconds", 21600)) and
              type(mode.get("setup_seconds")) is int and mode["setup_seconds"] == 300, "原始测试模式或超时预算未证明")
        self.index += 1
        if time.monotonic() >= self.deadline:
            return {"state": "unknown", "reason": "回放总时间达到上限", "sha": sha,
                    "elapsed_seconds": time.monotonic() - self.started, "budget_seconds": self.budget}
        details = {"sha": sha, "tree": git(self.root, "rev-parse", sha + "^{tree}"), "execution": mode}
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
                                                    "./" + package.removeprefix("agentworkspace/")], checkout, "list", timeout=mode["setup_seconds"])
                    details["list"] = listed
                    check(not listed.get("state"), "列举测试失败")
                    if not inventory(listing, listed.get("returncode"), candidate):
                        return {**details, "state": "absent"}
                    execution, text = self.execute(["go", "test", "-count=1", *flags, "-json", "-timeout=" + mode["timeout_arg"], "-run", pattern,
                                                   "./" + package.removeprefix("agentworkspace/")], checkout, "run",
                                                   timeout=mode["timeout_seconds"] + mode["setup_seconds"])
                    details["run"] = execution
                    check(not execution.get("state"), "测试执行异常")
                    return {**details, **assess(listing, listed.get("returncode"), text, execution.get("returncode"), candidate)}
                finally:
                    git(self.root, "worktree", "remove", "--force", str(checkout))
        except (ValueError, OSError, subprocess.SubprocessError) as error:
            return {**details, "state": "unknown", "reason": str(error)}


def worker(name):
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def candidate_id(candidate):
    return hashlib.sha256(json.dumps(candidate, sort_keys=True).encode()).hexdigest()[:16]


def replay(bundle, root, output):
    initial = json.loads((bundle / "receipt.json").read_text())
    candidates, baseline = initial.get("failure_candidates", []), initial.get("ancestor")
    started = time.monotonic()
    deadline, chain, reason = started + 12600, [], "无可信首亲绿点"
    if baseline:
        try:
            chain = first_parent_chain(root, initial)
        except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
            reason = str(error)
    items, seen = [], set()
    for candidate in candidates:
        item_id = candidate_id(candidate)
        check(item_id not in seen, "失败候选执行身份重复")
        seen.add(item_id)
        result = unknown(reason, candidate=candidate, probes=[], chain=chain)
        remaining = deadline - time.monotonic()
        if chain and remaining > 0:
            directory = output / item_id
            runner = None
            try:
                if candidate.get('kind') == 'client':
                    runner = worker('periodic-clients').ClientProbe(root, directory, timeout=300, budget=remaining)
                elif candidate.get('kind') == 'diagnostic':
                    runner = worker('periodic-diagnostics').DiagnosticProbe(root, directory, budget=remaining)
                else:
                    runner = Probe(root, directory, budget=remaining)
                result = localize(chain, candidate, runner, remaining=lambda: deadline - time.monotonic())
            except (ValueError, KeyError, OSError, TypeError, subprocess.SubprocessError) as error:
                result = unknown("此测试回放无法完成：" + str(error), candidate=candidate, probes=[], chain=chain)
            finally:
                if runner:
                    runner.close()
        elif chain:
            result = localize(chain, candidate, lambda *_: None, remaining=lambda: 0)
        result.update(id=item_id, directory=item_id, elapsed_seconds=max(0, remaining - (deadline - time.monotonic())))
        result.setdefault("good_sha", baseline["sha"] if baseline else None)
        result.setdefault("bad_sha", initial["sha"])
        result.setdefault("commit", None)
        items.append(result)
    result = {"schema": 1, "items": items, "unresolved": initial.get("unresolved_failures", []),
              "elapsed_seconds": time.monotonic() - started, "budget_seconds": 12600,
              "initial_sha256": hashlib.sha256((bundle / "receipt.json").read_bytes()).hexdigest()}
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
