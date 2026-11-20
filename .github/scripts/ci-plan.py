#!/usr/bin/env python3
"""Route native CI events and run bounded, single-runner draft feedback."""
import argparse
import importlib.util
import json
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


def check(condition, message):
    if not condition:
        raise ValueError(message)


def mode(event_name, event):
    if event_name == "push":
        return "full" if event.get("ref") == "refs/heads/master" else "ignore"
    if event_name == "merge_group":
        return "full" if event.get("action") == "checks_requested" else "ignore"
    if event_name != "pull_request":
        return "ignore"
    pr = event["pull_request"]
    action = event.get("action")
    old_base = event.get("changes", {}).get("base", {}).get("ref", {}).get("from")
    label = event.get("label", {}).get("name", "").casefold()
    # Native edited payloads include base.ref.from only for an actual base-branch edit.
    relevant = (action in PR_ACTIONS or
                action == "edited" and old_base is not None or
                action == "labeled" and (label == "ci:refresh" or label == "ci:full" and pr["draft"]))
    if not relevant:
        return "ignore"
    override = label == "ci:full" or any(row.get("name", "").casefold() == "ci:full" for row in pr.get("labels", []))
    return "full" if not pr["draft"] or override else "feedback"


def bucket(event_name, event):
    if event_name == "pull_request":
        number = event["number"]
        check(type(number) is int and number > 0, "PR 编号非法")
        return number % 2
    return 1 if event_name == "merge_group" else 0


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True).stdout


def changed_paths(base, sha, root):
    # Disabling rename detection deliberately includes both old and new paths.
    data = git(root, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "--no-renames", "-z", base, sha, "--")
    check(not data or data.endswith(b"\0"), "变更路径列表截断")
    return [os.fsdecode(path) for path in data.split(b"\0")[:-1]]


def classify(paths):
    result = {"docs": True, "web": False, "bridge": False, "go": False, "conservative": False, "directories": []}
    directories = set()
    for path in paths:
        if path.startswith("docs/") or "/" not in path and path.endswith(".md"):
            continue
        result["docs"] = False
        if path.startswith("web/"):
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
        result.update(go=True, web=True, bridge=True)
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
    elif event_name == "merge_group":
        check(event["merge_group"]["head_sha"] == sha, "合并队列 SHA 不符")
    head = parents[1] if event_name == "pull_request" else sha
    return {"mode": selected_mode, "bucket": bucket(event_name, event), "sha": sha, "tree": tree, "base": base,
            "head": head, "matrix": shard_matrix(), **classify(paths)}


def shard_matrix():
    spec = importlib.util.spec_from_file_location("ci_shards", Path(__file__).with_name("ci-shards.py"))
    shards = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(shards)
    return {"include": [{"profile": profile, "worker": worker}
                        for profile, workers in shards.BATCHES.items() for worker in range(len(workers))]}


def feedback_packages(selected, available):
    directories = {"agentworkspace/" + directory for directory in selected["directories"]}
    return sorted(package for package in available
                  if not package.endswith(FAST_EXCLUDED) and
                  (selected["conservative"] or package in directories))


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
            if selected["conservative"] or "internal/" + profile in selected["directories"]:
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
        for key in ("mode", "bucket", "sha", "tree", "head", "base", "matrix", "go", "web", "bridge", "conservative"):
            value = selected[key]
            output.write(f"{key}={json.dumps(value) if isinstance(value, (bool, dict)) else value}\n")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print(f"CI 反馈失败：{error}", file=sys.stderr)
        sys.exit(1)
