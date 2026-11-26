#!/usr/bin/env python3
"""Resolve immutable master candidates and reserve one durable native run owner."""
import argparse
import os
from pathlib import Path
import re
import sys
from urllib.parse import quote

# Hyphenated standalone scripts deliberately need no package installation.
import importlib.util
_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
check = core.check

# Controller policy only; product CI files may differ or be absent.
FULL_PATHS = (".github/workflows/ci-full.yml",) + tuple(".github/scripts/" + name for name in (
    "ci-plan.py", "ci-shards.py", "ci-gate.py", "verify-store-shard.py", "test-ci-scheduling.py", "test-store-shard.py"))


def object_get(api, path):
    return getattr(api, "immutable", api.get)(path)


def peel_tag(api, prefix, obj):
    raw, seen = core.sha(obj["sha"]), set()
    while obj["type"] == "tag":
        identity = core.sha(obj["sha"])
        check(identity not in seen and len(seen) < 8, "标签剥离循环或深度超限")
        seen.add(identity)
        tag = object_get(api, prefix + "/git/tags/" + identity)
        check(tag.get("sha") == identity, "原生 tag 对象身份不符")
        obj = tag["object"]
    check(obj["type"] == "commit", "标签没有指向提交")
    return core.sha(obj["sha"]), raw


def resolve_target(api, repo, event_name, event, ref, native_sha=None):
    prefix = "/repos/" + core.repository(repo)
    check(event.get("repository", {}).get("full_name") == repo, "触发仓库不符")
    if event_name == "workflow_dispatch":
        check(ref == "refs/heads/master", "仅允许从 master 手动触发")
        value = event.get("inputs", {}).get("sha", "")
        core.exact(value, r"[0-9a-fA-F]{40}", "必须指定完整提交 SHA，不能指定分支或标签")
        target = value.lower()
    else:
        check(event_name == "push" and ref == event.get("ref"), "只接受 rc/* 标签创建事件")
        core.rc_ref(ref)
        check(event.get("created") is True and event.get("deleted") is False and event.get("forced") is False
              and event.get("before") == "0" * 40, "拒绝标签删除、更新或强制推送")
        row = api.get(prefix + "/git/ref/tags/" + quote(ref[len("refs/tags/"):], safe=""))
        check(row.get("ref") == ref, "原生标签 ref 不符")
        target, raw = peel_tag(api, prefix, row["object"])
        check(core.sha(event["after"]) in (raw, target) and core.sha(native_sha) in (raw, target),
              "标签已改变，或事件/native SHA 不是该标签的确切目标")
    commit = api.get(prefix + "/git/commits/" + target)
    check(commit["sha"] == target, "目标不是指定的确切提交")
    master = core.sha(api.get(prefix + "/commits/master")["sha"])
    comparison = api.get(prefix + f"/compare/{target}...{master}")
    check(comparison.get("status") in ("ahead", "identical") and comparison.get("merge_base_commit", {}).get("sha") == target,
          "目标提交不可从当前远端 master 到达")
    parents = commit.get("parents", [])
    return {"sha": target, "tree": core.sha(commit["tree"]["sha"]), "master_sha": master,
            "head": target, "base": core.sha(parents[0]["sha"]) if parents else target}


def source_identity(root, target, workflow_sha, master, api=None):
    core.sha(workflow_sha)
    core.sha(master)
    core.sha(target)
    check(core.git(root, "rev-parse", "HEAD") == workflow_sha, "控制检出不是原生不可变 workflow SHA")
    for identity in (workflow_sha, target):
        if api is not None:
            reachable(api, identity, master)
        else:
            check(core.git(root, "merge-base", identity, master) == identity, "控制或目标提交不可从已核实的 master 到达")
    control_paths = [core.WORKFLOW, core.INTAKE] + core.git(root, "ls-tree", "-r", "--name-only", workflow_sha, ".github/scripts").splitlines()
    control_paths = [path for path in control_paths if path in (core.WORKFLOW, core.INTAKE) or Path(path).name.startswith("release-")]
    policy = core.git(root, "show", workflow_sha + ":.github/workflows/ci-full.yml")
    check(re.search(r"(?m)^ +policy_sha:\s*$", policy) is not None, "实际 reusable Full 不支持不可变调用者策略")
    return {path: core.sha(core.git(root, "rev-parse", workflow_sha + ":" + path))
            for path in sorted(set(control_paths) | set(FULL_PATHS))}


def admission_name(sha):
    return "Release admission " + core.sha(sha)


def commit_identity(api, identity):
    identity = core.sha(identity)
    commit = object_get(api, f"{api.prefix}/git/commits/{identity}")
    check(commit.get("sha") == identity, "原生提交对象不符")
    core.sha(commit["tree"]["sha"])
    return commit


def reachable(api, ancestor, head):
    comparison = object_get(api, f"{api.prefix}/compare/{core.sha(ancestor)}...{core.sha(head)}")
    check(comparison.get("status") in ("ahead", "identical")
          and comparison.get("merge_base_commit", {}).get("sha") == ancestor, "提交不可从受信任的 master 到达")


def tree_entries(api, identity, recursive=False):
    path = f"{api.prefix}/git/trees/{core.sha(identity)}" + ("?recursive=1" if recursive else "")
    response = object_get(api, path)
    check(response.get("sha") == identity and response.get("truncated") is False, "原生源码树不完整或不符")
    result = {}
    for row in response["tree"]:
        check(row["path"] not in result, "原生树包含重复路径")
        result[row["path"]] = row
    return result


def subtree(api, rows, name, recursive=False):
    row = rows.get(name, {})
    check(row.get("type") == "tree" and row.get("mode") == "040000", "控制源码目录缺失或不是原生树：" + name)
    return tree_entries(api, row["sha"], recursive)


def source_blobs(api, commit):
    # Walk only policy directories; cache by immutable Git identity, never by ref.
    root = tree_entries(api, commit["tree"]["sha"])
    github = subtree(api, root, ".github")
    workflows = subtree(api, github, "workflows")
    scripts = subtree(api, github, "scripts", recursive=True)
    result = {}
    for directory, rows in ((".github/workflows/", workflows), (".github/scripts/", scripts)):
        for name, row in rows.items():
            path = directory + name
            if row.get("type") == "tree":
                continue
            if path not in (core.WORKFLOW, core.INTAKE) + FULL_PATHS and not (
                    directory == ".github/scripts/" and Path(name).name.startswith("release-")):
                continue
            check(row.get("type") == "blob" and row.get("mode") in ("100644", "100755"), "控制源码不是普通文件")
            result[path] = core.sha(row["sha"])
    return result


def native_head(api, run):
    core.native_candidate(run, api.repo)
    head = run.get("head_commit") or {}
    identity = core.sha(head.get("id"))
    raw = core.sha(run["head_sha"])
    if raw != identity:
        check(run["event"] == "push", "dispatch head SHA 不是确切提交")
        target, _ = peel_tag(api, api.prefix, {"type": "tag", "sha": raw})
        check(target == identity, "原生 tag SHA 没有剥离到 native head commit")
    commit = commit_identity(api, identity)
    check(head.get("tree_id") == commit["tree"]["sha"], "原生 run 的 head 树与提交对象不符")
    return commit


def native_sources(api, run, target, historical=False):
    core.native_candidate(run, api.repo)
    master = core.sha(api.get(api.prefix + "/commits/master")["sha"])
    control_commit = native_head(api, run)
    control = control_commit["sha"]
    reachable(api, control, master)
    reachable(api, target, master)
    candidate = commit_identity(api, target)
    sources = source_blobs(api, control_commit)
    required = {core.WORKFLOW, core.INTAKE, ".github/scripts/release-core.py", ".github/scripts/release-control.py"} | set(FULL_PATHS)
    check(required <= sources.keys(), "已发布的原生控制源码不支持本回执契约")
    if run["event"] == "push":
        check(control == target, "原生标签 head SHA 与候选目标不符")
        if not historical and canonical_status(api, api.repo, target) != run["id"]:
            ref = "refs/tags/" + run["head_branch"]
            row = api.get(api.prefix + "/git/ref/tags/" + quote(run["head_branch"], safe=""))
            check(row.get("ref") == ref, "原生标签 ref 不符")
            current, _ = peel_tag(api, api.prefix, row["object"])
            check(current == target, "标签当前目标不符，且没有独立的历史原生 owner 记录")
    return candidate, sources, master


def canonical_status(api, repo, sha):
    rows = sorted((row for row in api.statuses_for(sha) if row.get("context") == core.CONTEXT), key=lambda row: row["id"])
    if not rows:
        return None
    owner = None
    for row in rows:
        check(all(row.get("creator", {}).get(key) == value for key, value in core.BOT.items())
              and (row.get("state"), row.get("description")) in (
                  ("success", core.DESCRIPTION), ("pending", "release-owner-v1")),
              "首认领状态身份或语义不符")
        match = re.fullmatch(re.escape(f"https://github.com/{repo}/actions/runs/") + r"([1-9][0-9]*)", row.get("target_url", ""))
        check(match is not None, "首认领 URL 非法")
        candidate = core.number(int(match[1]))
        check(api.check_owner(sha, row) == candidate, "认领未绑定受信任的原生 admission 作业")
        check(owner is None or owner == candidate, "检测到冲突认领；拒绝产生新证据")
        owner = candidate
    return owner


def admit(api, repo, sha, run_id, attempt, allow_retry=False):
    """Reserve the native owner; the legacy allow_retry argument grants nothing."""
    core.repository(repo)
    core.sha(sha)
    core.number(run_id)
    core.number(attempt)
    owner = canonical_status(api, repo, sha)
    if owner is None:
        # The caller holds the SHA lock. No downstream may start without a
        # verified durable owner; an absent owner therefore authorizes no work.
        # Never reinterpret unrelated expired artifacts as target tombstones.
        check(attempt == 1, "没有首 attempt 认领，禁止重试建立新 owner")
        body = {"context": core.CONTEXT, "state": "success", "description": core.DESCRIPTION,
                "target_url": f"https://github.com/{repo}/actions/runs/{run_id}"}
        try:
            api.post_claim(sha, body)
        except OSError:
            pass
        # Never trust a successful response alone; reconcile durable canonical state.
        owner = canonical_status(api, repo, sha)
        check(owner is not None, "认领 POST 结果不确定；本次停止，不允许任何下游工作")
    admitted = owner == run_id
    if admitted and attempt > 1:
        api.authorize_retry(run_id, attempt)
        jobs = api.jobs(run_id, attempt)
        core.require_jobs(jobs, run_id, attempt, ["Release normalize"])
        rows = [job for job in jobs if job["name"] in (admission_name(sha), "Release admission")]
        check(len(rows) == 1 and (rows[0].get("status") == "in_progress" or
              (rows[0].get("status") == "completed" and rows[0].get("conclusion") == "success")),
              "重跑缺少本次原生 admission；不能继承先前结果")
    return {"admitted": admitted, "owner_run_id": owner, "attempt": attempt,
            "owner_url": f"https://github.com/{repo}/actions/runs/{owner}"}


def actor_identity(value):
    check(isinstance(value, dict) and value.get("type") in ("User", "Bot"), "缺少原生 actor 身份")
    identity = core.number(value.get("id"))
    login = core.exact(value.get("login"), r"[A-Za-z0-9][A-Za-z0-9-]*(?:\[bot\])?", "原生 actor login 非法")
    return identity, login, value["type"]


class AdmissionAPI(core.GitHub):
    def authorize_retry(self, run_id, attempt):
        check(attempt > 1, "不是原生重跑 attempt")
        latest, current, original = self.run(run_id), self.run(run_id, attempt), self.run(run_id, 1)
        check(latest.get("run_attempt") == attempt and latest.get("status") == "in_progress"
              and current.get("status") == "in_progress", "原生重跑已停止或 attempt 过期")
        core.native_candidate(current, self.repo)
        core.native_candidate(original, self.repo)
        fields = ("path", "event", "head_branch", "head_sha", "head_commit")
        check(all(current.get(key) == original.get(key) == latest.get(key) for key in fields), "重跑不是原始不可变 native run")
        check(actor_identity(original.get("actor")) == actor_identity(current.get("actor")) == actor_identity(latest.get("actor")),
              "重跑改变了原始 actor 身份")
        actor = actor_identity(current.get("triggering_actor"))
        check(actor == actor_identity(latest.get("triggering_actor")), "重跑 actor 与原生最新 attempt 不符")
        repo = self.get(self.prefix)
        check(repo.get("full_name") == self.repo, "原生仓库根身份不符")
        core.number(repo.get("id"))
        owner = core.number(repo.get("owner", {}).get("id"))
        if actor[0] != owner:
            permission = self.get(self.prefix + "/collaborators/" + quote(actor[1], safe="") + "/permission")
            check(actor_identity(permission.get("user")) == actor and permission.get("permission") in ("admin", "write"),
                  "原生重跑 actor 没有当前仓库写权限")
        return current

    def statuses_for(self, sha):
        return self.pages(f"{self.prefix}/commits/{core.sha(sha)}/statuses")

    def check_owner(self, sha, status):
        owner = core.number(int(status["target_url"].rsplit("/", 1)[1]))
        run = self.run(owner, 1)
        jobs = self.jobs(owner, 1)
        core.require_jobs(jobs, owner, 1, ["Release normalize"])
        rows = [job for job in jobs if job["name"] in (admission_name(sha), "Release admission")]
        check(len(rows) == 1, "缺少绑定目标 SHA 的原生首 admission")
        job = rows[0]
        started, created = core.timestamp(job.get("started_at")), core.timestamp(status.get("created_at"))
        check(started <= created and (created <= core.timestamp(job["completed_at"]) if job.get("completed_at")
              else job.get("status") == "in_progress"), "status 不在首 admission 的原生执行窗口内")
        # The native job name is a durable register independent of artifact TTL.
        # Legacy unnamed jobs remain fail-closed if their binding has expired.
        if job["name"] == "Release admission":
            intent = read_intent(self, owner, 1, historical=True)
            check(intent["sha"] == sha, "旧版首认领与持久 intent 不符")
        else:
            native_sources(self, run, sha, historical=True)
        return owner

    def post_claim(self, sha, body):
        return self.request("POST", f"{self.prefix}/statuses/{core.sha(sha)}", body)


def read_intent(api, run_id, attempt, historical=False):
    run = api.run(run_id, attempt)
    core.native_candidate(run, api.repo)
    jobs = api.jobs(run_id, attempt)
    normalize = core.require_jobs(jobs, run_id, attempt, ["Release normalize"])[0]
    intent, _, _ = api.receipt(run_id, f"release-intent-{run_id}-{attempt}", normalize)
    fields = {"schema", "kind", "repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha",
              "master_sha", "head", "base", "policy_sha", "policy", "sources"}
    check(set(intent) in (fields, fields | {"allow_retry"}) and type(intent.get("schema")) is int
          and intent.get("kind") == "release-intent" and intent.get("policy") == "caller-policy-v1"
          and ("allow_retry" not in intent or type(intent["allow_retry"]) is bool), "候选 intent 形状或策略非法")
    core.scope(intent, api.repo, intent["sha"], intent["tree"], run_id, attempt)
    candidate, sources, master = native_sources(api, run, intent["sha"], historical)
    parents = candidate.get("parents", [])
    check(intent["tree"] == candidate["tree"]["sha"] and intent["head"] == candidate["sha"]
          and intent["base"] == (parents[0]["sha"] if parents else candidate["sha"]), "目标 SHA、树或父提交与原生对象不符")
    control = run["head_commit"]["id"]
    check(intent["workflow_sha"] == control and intent["policy_sha"] == control
          and intent["sources"] == sources, "回执策略源码不是已发布的原生控制源码")
    reachable(api, core.sha(intent["master_sha"]), master)
    reachable(api, intent["sha"], intent["master_sha"])
    reachable(api, control, intent["master_sha"])
    return intent


def guard(api, run_id, attempt):
    latest = api.run(run_id)
    check(latest.get("run_attempt") == attempt and latest.get("status") == "in_progress", "owner 已停止、取消或 attempt 过期")
    intent = read_intent(api, run_id, attempt)
    check(canonical_status(api, api.repo, intent["sha"]) == run_id, "当前 run 不是 canonical owner")
    jobs = api.jobs(run_id, attempt)
    name = admission_name(intent["sha"])
    if not any(job["name"] == name for job in jobs):
        name = "Release admission"
    core.require_jobs(jobs, run_id, attempt, ["Release normalize", name])
    if attempt > 1:
        api.authorize_retry(run_id, attempt)
    return intent


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("normalize", "claim", "guard"))
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    api = AdmissionAPI(os.environ["GITHUB_REPOSITORY"])
    run_id, attempt = int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"])
    if args.command == "normalize":
        event = core.decode(Path(os.environ["GITHUB_EVENT_PATH"]).read_bytes(), 8 << 20)
        target = resolve_target(api, api.repo, os.environ["GITHUB_EVENT_NAME"], event, os.environ["GITHUB_REF"], os.environ["GITHUB_SHA"])
        workflow_sha = core.sha(os.environ["GITHUB_WORKFLOW_SHA"])
        run = api.run(run_id, attempt)
        check(native_head(api, run)["sha"] == workflow_sha and run["event"] == os.environ["GITHUB_EVENT_NAME"], "normalize 不是原生控制提交或事件")
        if run["event"] == "push":
            check("refs/tags/" + run["head_branch"] == os.environ["GITHUB_REF"] and workflow_sha == target["sha"],
                  "normalize 标签 ref 或目标不属于原生 run")
        if attempt > 1:
            api.authorize_retry(run_id, attempt)
        sources = source_identity(Path.cwd(), target["sha"], workflow_sha, target["master_sha"], api)
        matrix = core.load_script("ci-plan").shard_matrix()
        intent = dict(schema=1, kind="release-intent", repo=api.repo, run_id=run_id, run_attempt=attempt,
                      workflow_sha=workflow_sha, policy_sha=workflow_sha, policy="caller-policy-v1", sources=sources, **target)
        core.save(args.output / "receipt.json", intent)
        core.outputs(target | {"workflow_sha": workflow_sha, "policy_sha": workflow_sha, "matrix": matrix, "attempt": attempt})
    elif args.command == "claim":
        intent = read_intent(api, run_id, attempt)
        # Resolve again while holding the SHA concurrency lock (tags may have moved).
        event = core.decode(Path(os.environ["GITHUB_EVENT_PATH"]).read_bytes(), 8 << 20)
        target = resolve_target(api, api.repo, os.environ["GITHUB_EVENT_NAME"], event, os.environ["GITHUB_REF"], os.environ["GITHUB_SHA"])
        check(target["sha"] == intent["sha"] and target["tree"] == intent["tree"], "admission 目标已改变")
        result = admit(api, api.repo, intent["sha"], run_id, attempt)
        core.outputs(result)
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as stream:
            stream.write(f"Canonical owner: {result['owner_url']}\n\n")
            stream.write("当前 run 已认领；尚未验证。\n" if result["admitted"] else "重复触发：仅引用原 owner，不产生新验证证据。\n")
    else:
        intent = guard(api, run_id, attempt)
        if args.output:
            core.save(args.output, intent)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError) as error:
        print("候选准入失败：" + str(error), file=sys.stderr)
        sys.exit(1)
