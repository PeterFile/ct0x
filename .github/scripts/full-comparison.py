#!/usr/bin/env python3
"""Bind a branch-only production Full rehearsal to fixed product and policy identities."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

REPO = "PeterFile/ct0x"
PRODUCT = "18d11a3c0176fe98856aeeef6396e9ddfde0cca0"
TREE = "6df8bf3ccb986f8f4088dc310e0257b3f6285d9a"
PARENTS = ["7d70dc49ff1f3c3ae3876b2a519059c60d88ec3d", "7d72e560945bcb7d4aa055691ee6a15167008504"]
OLD_POLICY = "01883613c5bf8e80a5856ca07e1ad455cb2d6bac"
PREFIX = "codex/ci-full-production-proof-"
WORKFLOW_PATH = ".github/workflows/release-receipt.yml"
WORKFLOW_ID = 432540775
OVERLAY = {WORKFLOW_PATH, ".github/scripts/full-comparison.py", ".github/full-comparison-pin.json"}
ROOT = Path(__file__).resolve().parents[2]


def check(condition, message):
    if not condition:
        raise ValueError(message)


def unique(pairs):
    result = dict(pairs)
    check(len(result) == len(pairs), "JSON 字段重复")
    return result


def load(path):
    return json.loads(Path(path).read_text(encoding="utf-8"), object_pairs_hook=unique)


def save(path, value):
    with Path(path).open("x", encoding="utf-8") as stream:
        stream.write(json.dumps(value, ensure_ascii=True, sort_keys=True) + "\n")


def script(root, name):
    path = Path(root) / ".github/scripts" / (name + ".py")
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], env={**os.environ, "GIT_OPTIONAL_LOCKS": "0"}).decode().strip()


def pin(root):
    value = load(Path(root) / ".github/full-comparison-pin.json")
    check(set(value) == {"schema", "role", "policy_sha", "workflow_path", "workflow_id"}
          and type(value["schema"]) is int and value["schema"] == 1, "策略 pin 字段非法")
    check(value["workflow_path"] == WORKFLOW_PATH and type(value["workflow_id"]) is int
          and value["workflow_id"] == WORKFLOW_ID, "只允许隔离 proof ref 的注册 receipt 路径；禁止 RC 或生产 CI 挂载")
    check(value["role"] in ("old", "future") and re.fullmatch(r"[0-9a-f]{40}", value["policy_sha"]), "策略 pin 身份非法")
    check(value["role"] != "old" or value["policy_sha"] == OLD_POLICY, "旧策略 pin 不符")
    return value


def context(value, env):
    ref = "refs/heads/" + PREFIX + value["role"] + "-" + value["policy_sha"]
    workflow = env["GITHUB_WORKFLOW_SHA"]
    check(env["GITHUB_REPOSITORY"] == REPO and env["GITHUB_EVENT_NAME"] == "workflow_dispatch"
          and env["GITHUB_RUN_ATTEMPT"] == "1" and env["GITHUB_REF_TYPE"] == "branch"
          and env["GITHUB_REF"] == ref, "仅接受固定 proof 分支的首次手动运行")
    check(env["GITHUB_WORKFLOW_REF"] == REPO + "/" + WORKFLOW_PATH + "@" + ref
          and re.fullmatch(r"[0-9a-f]{40}", workflow) and env["GITHUB_SHA"] == workflow, "工作流路径或调用者 SHA 不符")
    check(re.fullmatch(r"[1-9][0-9]{0,63}", env["GITHUB_RUN_ID"]), "run 身份非法")
    return {"repo": REPO, "run_id": int(env["GITHUB_RUN_ID"]), "run_attempt": 1,
            "workflow_sha": workflow, "workflow_ref": env["GITHUB_WORKFLOW_REF"], "ref": ref,
            "workflow_path": WORKFLOW_PATH, "workflow_id": WORKFLOW_ID,
            "role": value["role"], "policy_source_sha": value["policy_sha"],
            "sha": PRODUCT, "tree": TREE, "head": PRODUCT, "base": PARENTS[0], "product_parents": PARENTS}


def product_identity(root):
    check(git(root, "rev-parse", "HEAD") == PRODUCT and git(root, "rev-parse", "HEAD^{tree}") == TREE
          and git(root, "show", "-s", "--format=%P", "HEAD").split() == PARENTS, "实际产品 SHA/tree/原始父提交不符")
    check(not git(root, "status", "--porcelain=v1", "--untracked-files=all"), "产品工作树不干净")


def policy_identity(root, identity):
    check(git(root, "rev-parse", "HEAD") == identity["workflow_sha"]
          and git(root, "show", "-s", "--format=%P", "HEAD").split() == [identity["policy_source_sha"]],
          "包装提交必须仅有精确策略提交这一位父提交")
    changed = git(root, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "--no-renames",
                  identity["policy_source_sha"], "HEAD", "--").splitlines()
    check(set(changed) == OVERLAY, "包装提交只能改动三个启用文件；不得替换真实 Full 或产品")
    check(not git(root, "status", "--porcelain=v1", "--untracked-files=all"), "暂存策略工作树不干净")
    identity["workflow_tree"] = git(root, "rev-parse", "HEAD^{tree}")


def full_plan(root, identity):
    # Keep the policy's complete matrix object and include order, without projecting its shape.
    return {"mode": "full", "sha": PRODUCT, "tree": TREE, "head": PRODUCT, "base": PARENTS[0],
            "matrix": script(root, "ci-plan").shard_matrix(), "web": True, "bridge": True}


def outputs(identity, plan):
    return {"mode": "full", "run_id": str(identity["run_id"]),
            **{key: identity[key] for key in ("sha", "tree", "head", "base")},
            "matrix": json.dumps(plan["matrix"], separators=(",", ":"))}


def verify_outputs(root, identity, plan, needs):
    check(set(needs) == {"preflight", "full"}, "调用者结果缺失或多余")
    expected = outputs(identity, plan)
    preflight = needs["preflight"].get("outputs", {})
    check(all(preflight.get(key) == value for key, value in expected.items()), "预检输出与固定身份或原矩阵不符")
    # Reuse the existing output gate; its CLI event router intentionally does not admit dispatch.
    script(root, "ci-gate").check_outer({"feedback": needs["preflight"], "full": needs["full"]},
                                        str(identity["run_id"]), PRODUCT)


def snapshot(root, product, env):
    identity = context(pin(root), env)
    policy_identity(root, identity)
    product_identity(product)
    return identity, full_plan(root, identity)


def plan_command(root, product, output):
    identity, selected = snapshot(root, product, os.environ)
    output.mkdir(parents=True, exist_ok=False)
    save(output / "identity.json", identity)
    save(output / "full-plan.json", selected)
    # Git blob identities bind all staged policy files, not only the scheduling scripts.
    sources = git(root, "ls-tree", "-r", "--full-tree", "HEAD", "--", ".github")
    (output / "policy-tree.txt").write_text(sources + "\n", encoding="utf-8")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as stream:
        for key, value in outputs(identity, selected).items():
            stream.write(f"{key}={value}\n")


def record_command(root, product, plan_dir, output):
    identity, selected = snapshot(root, product, os.environ)
    check(load(plan_dir / "identity.json") == identity and load(plan_dir / "full-plan.json") == selected,
          "本次预检身份或策略计划已变化")
    needs = json.loads(os.environ["NEEDS_JSON"], object_pairs_hook=unique)
    verify_outputs(root, identity, selected, needs)
    output.mkdir(parents=True, exist_ok=False)
    receipt = {"schema": 1, "kind": "production-full-comparison", "status": "success", "series6_sample": False,
               "release": False, **identity, "full_outputs": needs["full"]["outputs"]}
    save(output / "receipt.json", receipt)
    save(output / "needs.json", needs)
    save(output / "full-plan.json", selected)
    (output / "policy-tree.txt").write_bytes((plan_dir / "policy-tree.txt").read_bytes())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "record"))
    parser.add_argument("--product", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--plan", type=Path)
    args = parser.parse_args()
    if args.command == "plan":
        plan_command(ROOT, args.product, args.output)
    else:
        check(args.plan is not None, "record 缺少本次预检目录")
        record_command(ROOT, args.product, args.plan, args.output)


if __name__ == "__main__":
    sys.dont_write_bytecode = True
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError) as error:
        print(f"生产 Full 对照拒绝：{error}", file=sys.stderr)
        sys.exit(1)
