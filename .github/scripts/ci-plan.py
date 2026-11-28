#!/usr/bin/env python3
"""Route native CI events: full on master pushes, path-scoped PR feedback unless ci:full."""
import argparse
import importlib.util
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys


PR_ACTIONS = {"opened", "reopened", "synchronize", "ready_for_review", "converted_to_draft"}
FAST_TESTS = {
    "store": ("TestOpenMigratesAndConfiguresConnections", "TestMissingAndCrossGroupReferences"),
    "platform": ("TestNativeSessionPreparePiIntakeBridgeSchemaCompatibility",),
    "httpapi": ("TestCoordinationReadinessHTTPExactlyOneOptInRoute",),
}
FAST_EXCLUDED = tuple("/internal/" + name for name in FAST_TESTS) + ("/cmd/workspace",)
SHA_PATTERN = r"[0-9a-f]{40}"
CI_PATHS = {
    ".github/workflows/ci.yml", ".github/workflows/ci-full.yml",
    ".github/scripts/ci-plan.py", ".github/scripts/ci-shards.py", ".github/scripts/ci-gate.py",
    ".github/scripts/test-ci-scheduling.py", ".github/scripts/test-store-shard.py", ".github/scripts/verify-store-shard.py",
    ".github/workflows/periodic-full.yml",
    *(".github/scripts/" + name + ".py" for name in (
        "periodic-full", "periodic-replay", "periodic-history", "periodic-clients", "periodic-diagnostics",
        "test-periodic-full", "test-periodic-history", "test-periodic-clients", "test-periodic-diagnostics")),
}


def check(condition, message):
    if not condition:
        raise ValueError(message)


def mode(event_name, event):
    if event_name == "push":
        return "full" if event.get("ref") == "refs/heads/master" else "ignore"
    if event_name != "pull_request":
        return "ignore"
    pr = event["pull_request"]
    action = event.get("action")
    old_base = event.get("changes", {}).get("base", {}).get("ref", {}).get("from")
    label = event.get("label", {}).get("name", "").casefold()
    # Native edited payloads include base.ref.from only for an actual base-branch edit.
    relevant = (action in PR_ACTIONS or
                action == "edited" and old_base is not None or
                action == "labeled" and label in ("ci:refresh", "ci:full"))
    if not relevant:
        return "ignore"
    # Master is validated after merge; PRs run full only when explicitly requested.
    override = label == "ci:full" or any(row.get("name", "").casefold() == "ci:full" for row in pr.get("labels", []))
    return "full" if override else "feedback"


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True).stdout


def changed_paths(base, sha, root):
    # Disabling rename detection deliberately includes both old and new paths.
    data = git(root, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "--no-renames", "-z", base, sha, "--")
    check(not data or data.endswith(b"\0"), "变更路径列表截断")
    return [os.fsdecode(path) for path in data.split(b"\0")[:-1]]


def classify(paths):
    result = {"docs": True, "web": False, "bridge": False, "go": False, "ci": False, "conservative": False, "directories": []}
    directories = set()
    for path in paths:
        if path.startswith("docs/") or "/" not in path and path.endswith(".md"):
            continue
        result["docs"] = False
        if path in CI_PATHS:
            result["ci"] = True
        elif path.startswith("web/"):
            result["web"] = True
        elif path.startswith("extensions/pi-bridge/"):
            result["bridge"] = result["go"] = True
            directories.update(("internal/provider", "internal/platform"))
        elif path.startswith(("internal/", "cmd/")):
            directory = path.rsplit("/", 1)[0]
            if re.fullmatch(r"(?:internal|cmd)(?:/[A-Za-z0-9_-]+)+", directory):
                result["go"] = True
                directories.add(directory)
            else:
                result["conservative"] = True
        else:
            result["conservative"] = True
    if result["conservative"]:
        result.update(go=True, web=True, bridge=True, ci=True)
    result["directories"] = sorted(directories)
    return result


def plan(event_name, event, sha, root):
    check(isinstance(sha, str) and re.fullmatch(SHA_PATTERN, sha), "触发 SHA 非法")
    actual = git(root, "rev-parse", "HEAD").decode().strip()
    check(actual == sha, "checkout 与触发 SHA 不符")
    tree = git(root, "rev-parse", "HEAD^{tree}").decode().strip()
    parents = git(root, "show", "-s", "--format=%P", "HEAD").decode().split()
    selected_mode = mode(event_name, event)
    check(selected_mode != "ignore", "不应为无关事件启动 CI")
    paths, base = [], parents[0] if parents else sha
    if event_name == "pull_request":
        check(len(parents) == 2 and parents[1] == event["pull_request"]["head"]["sha"],
              "checkout 不是此 PR 的双亲合并提交")
        # The payload's base.sha may be stale. HEAD's first parent is authoritative.
        paths = changed_paths(base, sha, root)
    head = parents[1] if event_name == "pull_request" else sha
    return {"mode": selected_mode, "sha": sha, "tree": tree, "base": base,
            "head": head, "matrix": shard_matrix(), **classify(paths)}


def shard_matrix():
    spec = importlib.util.spec_from_file_location("ci_shards", Path(__file__).with_name("ci-shards.py"))
    shards = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(shards)
    check(set(shards.BATCHES) == set(shards.SLOT_WEIGHTS) == set(shards.verifier.PROFILES),
          "worker、逻辑槽与历史权重 profile 不完整")
    weighted = []
    for profile, workers in shards.BATCHES.items():
        total = shards.verifier.PROFILES[profile][0]
        weights = shards.SLOT_WEIGHTS[profile]
        check(len(weights) == total and all(type(value) in (int, float) and math.isfinite(value) and value > 0 for value in weights),
              "历史槽权重缺失或非法")
        flat = [slot for slots in workers for slot in slots]
        check(workers and all(workers) and all(type(slot) is int for slot in flat) and sorted(flat) == list(range(total)),
              "worker 逻辑槽配置缺失、重复或非法")
        weighted.extend(({"profile": profile, "worker": worker}, sum(weights[slot] for slot in slots))
                        for worker, slots in enumerate(workers))
    # Original B uses canonical JSON for ties; its constant part=null prefix does not change ordering.
    include = [item for item, _ in sorted(weighted, key=lambda pair: (-pair[1], json.dumps(pair[0], sort_keys=True, separators=(",", ":"))))]
    return {"include": include}


def feedback_packages(selected, available):
    directories = {"agentworkspace/" + directory for directory in selected["directories"]}
    return sorted(package for package in available
                  if not package.endswith(FAST_EXCLUDED) and
                  (selected["conservative"] or any(directory == package or directory.startswith(package + "/") for directory in directories)))


def command(args, cwd=None, capture=False):
    print("CI:", json.dumps(args, ensure_ascii=True), flush=True)
    return subprocess.run(args, cwd=cwd, check=True, text=True, capture_output=capture)


def feedback(selected):
    if selected["mode"] != "feedback":
        return
    if selected["docs"]:
        print("仅文档变更：差异检查通过，不执行产品测试。")
        return
    if selected["go"]:
        # Compile every package, but never run an unsharded heavy suite in feedback.
        command(["go", "test", "-run", "^$", "./..."])
        available = command(["go", "list", "./..."], capture=True).stdout.splitlines()
        packages = feedback_packages(selected, available)
        if packages:
            command(["go", "test", "-count=1", *packages])
        for profile, names in FAST_TESTS.items():
            target = "internal/" + profile
            if selected["conservative"] or any(directory == target or directory.startswith(target + "/") for directory in selected["directories"]):
                pattern = "^(" + "|".join(names) + ")$"
                command(["go", "test", "-count=1", "-run", pattern, "./internal/" + profile])


def clients(selected, client):
    if selected["mode"] != "full" and not selected[client]:
        return
    directory = "web" if client == "web" else "extensions/pi-bridge"
    install = ["npm", "ci"] if client == "web" else ["npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"]
    command(install, cwd=directory)
    for script in (("test", "typecheck", "build") if client == "web" else ("test", "typecheck")):
        command(["npm", "run", script], cwd=directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "feedback", "web", "bridge"))
    parser.add_argument("file", type=Path)
    args = parser.parse_args()
    if args.command != "plan":
        selected = json.loads(args.file.read_text(encoding="utf-8"))
        if args.command == "feedback":
            feedback(selected)
        else:
            clients(selected, args.command)
        return
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
    selected = plan(os.environ["GITHUB_EVENT_NAME"], event, os.environ["GITHUB_SHA"], Path.cwd())
    if selected["mode"] == "feedback" and selected["docs"]:
        git(Path.cwd(), "diff", "--check", selected["base"], selected["sha"], "--")
    args.file.write_text(json.dumps(selected, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps(selected, sort_keys=True))
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        run_id = os.environ["GITHUB_RUN_ID"]
        check(re.fullmatch(r"[1-9][0-9]{0,63}", run_id), "run 身份非法")
        output.write(f"run_id={run_id}\n")
        for key in ("mode", "sha", "tree", "head", "base", "matrix", "go", "web", "bridge", "ci", "conservative"):
            value = selected[key]
            output.write(f"{key}={json.dumps(value) if isinstance(value, (bool, dict)) else value}\n")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print(f"CI 反馈失败：{error}", file=sys.stderr)
        sys.exit(1)
