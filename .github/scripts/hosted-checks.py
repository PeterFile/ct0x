#!/usr/bin/env python3
"""Task-only controller. Keep this file and all of its output outside the product."""
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

sys.dont_write_bytecode = True

CONTROL_PATHS = {".github/workflows/ci.yml", ".github/scripts/hosted-checks.py"}
GROUPS = {
    "baseline": ("test-ci-scheduling.py", "test-store-shard.py"),
    "full-trial": ("test-full-trial.py",),
    "merge-gate": ("test-merge-gate.py", "test-merge-gate-migration.py",
                   "test-merge-gate-publish.py", "test-merge-gate-workflow.py"),
    "periodic": tuple(f"test-periodic-{part}.py" for part in ("clients", "diagnostics", "full", "history")),
    "release-candidate": (
        "test-release-control.py", "test-release-duplicate.py", "test-release-finalize.py",
        "test-release-host-proof.py", "test-release-host.py", "test-release-policy.py",
        "test-release-reconcile.py", "test-release-tag-forward.py",
        "release-test-evidence.py", "release-test-machine.py", "release-test-workflow.py",
    ),
    "observer": ("test-ci-observer.py",),
}
EXPECTED = {name for names in GROUPS.values() for name in names}
OBSERVER_FILES = {
    "observer/test_codex_receiver.py", "observer/test_merge_intersection.py",
    "observer/test_merge_reports.py", "observer/test_observer.py", "observer/test_rc_refs.py",
    "observer/test-pi-receiver.mjs",
}
MAX_RECEIPT = 65536
SUMMARY = re.compile(r"^(?:Ran [0-9]+ tests? in |OK(?:$| \()|FAILED(?:$| \()|NO TESTS RAN$|"
                     r"TAP version |1\.\.|# (?:tests|suites|pass|fail|cancelled|skipped|todo) [0-9]+$)")
CASE = re.compile(r"^\S+ \([^)]+\.[^)]+\)(?: \.\.\.|$)")


def check(condition, message):
    if not condition:
        raise ValueError(message)


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True,
                          timeout=30).stdout.decode("utf-8").strip()


def save(path, value):
    data = json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(",", ":")) + "\n"
    check(len(data.encode()) <= MAX_RECEIPT, "回执超过 64 KiB")
    path.write_text(data, encoding="utf-8")


def clean_product(root, product, untracked):
    check(git(root, "rev-parse", "HEAD") == product["sha"], "产品 HEAD 改变")
    check(git(root, "rev-parse", "HEAD^{tree}") == product["tree"], "产品 Git 树改变")
    check(not git(root, "status", "--porcelain", "--untracked-files=" + untracked), "产品工作目录不干净")
    check(not (root / ".github/scripts/hosted-checks.py").exists(), "控制驱动残留在产品目录")


def discover(root, receipt):
    directory = root / ".github/scripts"
    tracked = set(git(root, "ls-files", "-z", "--", ".github/scripts").split("\0")) - {""}
    top = {Path(p).name for p in tracked if Path(p).parent.as_posix() == ".github/scripts"
           and Path(p).name.startswith(("test-", "release-test-")) and p.endswith(".py")}
    names = sorted(EXPECTED | top)
    receipt["suites"] = [
        {"name": name, "group": next((group for group, entries in GROUPS.items() if name in entries), "additional"),
         "result": "not_run" if name in top else "missing", "unittest": None}
        for name in names
    ]
    missing = EXPECTED - top
    check(not missing, "缺少预期 CI 自测入口：" + ", ".join(sorted(missing)))
    for name in top:
        path = directory / name
        check(path.is_file() and not path.is_symlink(), "CI 自测不是普通文件：" + name)
    for name in OBSERVER_FILES:
        path = directory / name
        check(".github/scripts/" + name in tracked and path.is_file() and not path.is_symlink(),
              "观察器组合入口缺少嵌套测试：" + name)
    receipt["observer_included"] = sorted(OBSERVER_FILES)
    spec = importlib.util.spec_from_file_location("hosted_product_merge_gate", directory / "merge-gate.py")
    gate = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(gate)
    manifest = json.loads((directory / "merge-gate-manifest.json").read_text(encoding="utf-8"))
    # Call the product selector, never replace it with a controller glob.
    selected = list(gate.ci_tests(root, manifest))
    relative = []
    for path in selected:
        path = Path(path)
        check(path.is_file() and not path.is_symlink() and path.parent.resolve() == directory.resolve(),
              "真实选择器返回非顶层普通文件")
        relative.append(path.name)
    actual = set(relative)
    receipt["discovery"] = {"result": "failure", "selected": sorted(actual), "tracked": sorted(top)}
    check(len(relative) == len(actual), "真实选择器重复登记了入口")
    check(actual == top, "真实选择器与已跟踪入口不符，漏选：" + ", ".join(sorted(top - actual))
          + "；多选：" + ", ".join(sorted(actual - top)))
    receipt["discovery"]["result"] = "success"


def prepare(root, temporary, receipt):
    sha, tree, control = (os.environ[key] for key in ("TARGET_SHA", "TARGET_TREE", "CONTROL_SHA"))
    for name, value in (("sha", sha), ("tree", tree), ("control", control)):
        check(re.fullmatch(r"[0-9a-f]{40}", value) is not None, name + " 必须是完整小写 SHA")
    check(os.environ["GITHUB_EVENT_NAME"] == "workflow_dispatch", "只接受手动控制运行")
    check(control == os.environ["GITHUB_SHA"] == git(root, "rev-parse", "HEAD"), "checkout 不是运行创建时的控制 SHA")
    for key in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        check(re.fullmatch(r"[1-9][0-9]{0,63}", os.environ[key]) is not None, "运行身份非法")
    parents = git(root, "show", "-s", "--format=%P", control).split()
    check(parents == [sha], "控制提交必须是以指定产品 SHA 为唯一父提交的普通提交")
    check(git(root, "cat-file", "-t", sha) == "commit", "产品父对象不是提交")
    check(git(root, "rev-parse", sha + "^{tree}") == tree, "产品父提交的 Git 树与输入不符")
    changed = set(filter(None, git(root, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", sha, control, "--").split("\0")))
    check(changed == CONTROL_PATHS, "控制提交只能修改工作流和新增任务驱动")
    check(not git(root, "ls-tree", sha, "--", ".github/scripts/hosted-checks.py"), "产品树已包含控制驱动")
    for path in CONTROL_PATHS:
        check(git(root, "ls-tree", control, "--", path).startswith("100644 blob "), "控制路径必须是普通文件")
    check(not git(root, "status", "--porcelain", "--untracked-files=all"), "控制 checkout 不干净")
    driver = subprocess.run(["git", "-C", str(root), "show", control + ":.github/scripts/hosted-checks.py"],
                            check=True, capture_output=True, timeout=30).stdout
    check(Path(__file__).read_bytes() == driver, "临时驱动字节与控制提交不符")
    workflow = subprocess.run(["git", "-C", str(root), "show", control + ":.github/workflows/ci.yml"],
                              check=True, capture_output=True, timeout=30).stdout
    (temporary / "hosted-control.yml").write_bytes(workflow)
    subprocess.run(["git", "-C", str(root), "-c", "core.hooksPath=/dev/null", "checkout", "--detach", sha],
                   check=True, timeout=30)
    product = {"sha": sha, "tree": tree}
    clean_product(root, product, "all")
    receipt.update(product=product, control_sha=control, identity="success")
    try:
        discover(root, receipt)
    except Exception as error:
        receipt["discovery"].update(result="failure", error=str(error)[:256])
        raise


def execute(argv, root, env):
    """Stream native output; retain only summary lines and a verbose case count."""
    summaries, cases = [], 0
    with subprocess.Popen(argv, cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as process:
        for raw in process.stdout:
            sys.stdout.buffer.write(raw)
            sys.stdout.buffer.flush()
            line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
            cases += bool(CASE.match(line))
            if SUMMARY.match(line):
                summaries.append(line)
        code = process.wait()
    return code, "\n".join(summaries), cases


def unittest_counts(output, cases):
    totals = re.findall(r"(?m)^Ran ([0-9]+) tests? in [^\n]+$", output)
    statuses = re.findall(r"(?m)^(OK|FAILED|NO TESTS RAN)(?: \(([^\n]*)\))?$", output)
    check(len(totals) == len(statuses) == 1, "缺少唯一原生 unittest 摘要")
    status, details = statuses[0]
    counts = {"tests": int(totals[0]), "verbose_cases": cases, "successful": status == "OK"}
    for label in ("skipped", "failures", "errors", "expected failures", "unexpected successes"):
        value = re.search(r"(?:^|, )" + label + r"=([0-9]+)(?:,|$)", details)
        counts[label.replace(" ", "_")] = int(value[1]) if value else 0
    return counts


def tap_counts(output):
    check(len(re.findall(r"(?m)^TAP version 13$", output)) == 1, "观察器缺少唯一 Node TAP 输出")
    check(len(re.findall(r"(?m)^1\.\.[0-9]+(?: #.*)?$", output)) == 1, "观察器缺少唯一 TAP 结束计划")
    counts = {}
    for key in ("tests", "suites", "pass", "fail", "cancelled", "skipped", "todo"):
        values = re.findall(r"(?m)^# " + key + r" ([0-9]+)$", output)
        check(len(values) == 1, "观察器缺少唯一 TAP 计数：" + key)
        counts[key] = int(values[0])
    check(counts["tests"] == sum(counts[k] for k in ("pass", "fail", "cancelled", "skipped", "todo")), "TAP 计数不一致")
    return counts


def run(root, temporary, receipt, receipt_path):
    check(receipt["identity"] == "success" and receipt["discovery"]["result"] == "success", "产品身份或发现未通过")
    workflows = sorted(p for p in git(root, "ls-files", "-z", "--", ".github/workflows").split("\0")
                       if p.endswith((".yml", ".yaml")))
    check(workflows and all((root / p).is_file() and not (root / p).is_symlink() for p in workflows), "产品工作流缺失或非法")
    print("::group::actionlint 1.7.12", flush=True)
    lint = {"result": "failure", "version": "1.7.12", "workflows": workflows + ["control:.github/workflows/ci.yml"]}
    receipt["actionlint"] = lint
    try:
        code, _, _ = execute([str(temporary / "hosted-actionlint/actionlint"), "-color", *workflows,
                              str(temporary / "hosted-control.yml")], root, dict(os.environ))
        lint.update(returncode=code, result="success" if code == 0 else "failure")
    except OSError as error:
        lint["error"] = str(error)[:128]
    print("::endgroup::", flush=True)
    save(receipt_path, receipt)
    selected = {row["name"] for row in receipt["suites"]}
    periodic_selected = set(GROUPS["periodic"]) <= selected
    for row in receipt["suites"]:
        name = row["name"]
        print("::group::" + name, flush=True)
        row["result"] = "failure"
        try:
            env = dict(os.environ)
            env.pop("CI_TESTS_DISCOVERED", None)
            if name == "test-ci-scheduling.py" and periodic_selected:
                env["CI_TESTS_DISCOVERED"] = "1"
            if name == "test-ci-observer.py":
                env["NODE_OPTIONS"] = "--test-reporter=tap"
            path = root / ".github/scripts" / name
            code, output, cases = execute([sys.executable, "-B", str(path), "-v"], root, env)
            row["returncode"] = code
            counts = row["unittest"] = unittest_counts(output, cases)
            if name == "test-ci-observer.py":
                row["tap"] = tap_counts(output)
                check(row["tap"]["pass"] > 0 and row["tap"]["fail"] == row["tap"]["cancelled"] == 0,
                      "观察器 Node 测试为空、全跳过或失败")
            check(code == 0, "测试入口非零退出")
            check(cases > 0 and counts["tests"] > counts["skipped"] and counts["tests"] > 0, "unittest 未执行、为空或全跳过")
            check(counts["successful"] and not counts["failures"] and not counts["errors"]
                  and not counts["unexpected_successes"], "原生 unittest 结果失败")
            row["result"] = "success"
        except Exception as error:
            row["error"] = str(error)[:64]
            print("契约失败：" + str(error), file=sys.stderr, flush=True)
        print("::endgroup::", flush=True)
        save(receipt_path, receipt)
    clean_product(root, receipt["product"], "no")
    receipt["result"] = "success" if lint["result"] == "success" and receipt["suites"] and all(row["result"] == "success" for row in receipt["suites"]) else "failure"
    save(receipt_path, receipt)
    return 0 if receipt["result"] == "success" else 1


def main():
    check(len(sys.argv) == 2 and sys.argv[1] in ("prepare", "run"), "用法：hosted-checks.py prepare|run")
    command = sys.argv[1]
    root, temporary = (Path(os.environ[key]).resolve() for key in ("GITHUB_WORKSPACE", "RUNNER_TEMP"))
    check(Path.cwd().resolve() == root, "必须从产品 checkout 根目录运行")
    check(temporary != root and root not in temporary.parents, "RUNNER_TEMP 不能位于产品目录内")
    check(root not in Path(__file__).resolve().parents, "必须先将控制驱动复制到 RUNNER_TEMP")
    receipt_path = temporary / "hosted-receipt.json"
    receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
    try:
        if command == "prepare":
            prepare(root, temporary, receipt)
            save(receipt_path, receipt)
            return 0
        return run(root, temporary, receipt, receipt_path)
    except Exception as error:
        receipt.update(result="failure", error=str(error)[:256])
        save(receipt_path, receipt)
        print("控制检查失败：" + str(error), file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
