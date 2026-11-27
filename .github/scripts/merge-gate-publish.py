#!/usr/bin/env python3
"""Trusted base-side publisher; local consumption is explicit and stays under the shared mkdir lock."""
import argparse
import base64
from contextlib import contextmanager, nullcontext
import fcntl
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import time
from urllib.parse import quote, urlencode
import zipfile

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("merge_gate", HERE / "merge-gate.py")
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)
check = gate.check
REPO = "PeterFile/ct0x"
WORKFLOW = ".github/workflows/merge-gate.yml"
PUBLISHER = ".github/workflows/merge-conflict.yml"
APP_ID = 15368
MAX_ARTIFACT = 16 * 1024 * 1024
MAX_COMMAND_OUTPUT = 65536
MAX_POLICY_CACHE_ENTRIES = 256
MAX_POLICY_CACHE_BYTES = 4 * 1024 * 1024


class PolicyChange(ValueError):
    pass


class StaleBase(ValueError):
    pass


class UnknownMerge(ValueError):
    pass


class UnassignedRun(ValueError):
    pass


class StateChanged(ValueError):
    pass


def state_check(condition, message):
    if not condition:
        raise StateChanged(message)


def integer(value):
    check(type(value) is int and value > 0, "API 数字身份非法")
    return value


def receipt_integer(value):
    check(isinstance(value, str) and re.fullmatch(r"[1-9][0-9]{0,63}", value) is not None, "回执数字身份非法")
    return int(value)


def sha(value):
    check(isinstance(value, str) and re.fullmatch(gate.SHA, value) is not None, "API SHA 非法")
    return value


class GitHub:
    def request(self, method, endpoint, data=None, binary=False):
        check(isinstance(endpoint, str) and not endpoint.startswith(("/", "http")) and ".." not in endpoint.split("?", 1)[0].split("/"), "API 路径非法")
        argv = ["gh", "api", "--hostname", "github.com", "--method", method,
                "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28",
                "repos/" + REPO + "/" + endpoint]
        if data is not None:
            argv += ["--input", "-"]
        result = subprocess.run(argv, input=json.dumps(data).encode() if data is not None else None,
                                capture_output=True, check=True)
        if method == "POST" and endpoint == "actions/workflows/merge-gate.yml/dispatches" and result.stdout == b"":
            return None  # This endpoint returns HTTP 204, unlike GET/Checks mutations.
        return result.stdout if binary else gate.verifier.decode(result.stdout.decode())

    def get(self, endpoint):
        return self.request("GET", endpoint)

    def write(self, method, endpoint, data=None):
        check(method in ("POST", "PATCH", "PUT", "DELETE"), "写入方法非法")
        return self.request(method, endpoint, data)

    def pages(self, endpoint, key):
        rows = []
        for page in range(1, 1001):
            query = ("&" if "?" in endpoint else "?") + urlencode({"per_page": 100, "page": page})
            value = self.get(endpoint + query)
            entries = value if key is None else value[key]
            check(isinstance(entries, list), "API 分页形状非法")
            rows.extend(entries)
            if len(entries) < 100:
                return rows
        raise ValueError("API 分页不完整；拒绝截断")

    def archive(self, artifact_id):
        return self.request("GET", f"actions/artifacts/{integer(artifact_id)}/zip", binary=True)


def source_identity(pr):
    return {"repo_id": pr["head"]["repo"]["id"], "repo": pr["head"]["repo"]["full_name"],
            "ref": pr["head"]["ref"], "base_ref": pr["base"]["ref"]}


def validate_live(pr, reviewed_head=None, consuming=False):
    integer(pr["number"])
    check(pr["state"] == "open" and not pr.get("merged", False), "PR 已关闭或已合并")
    check(pr["base"]["repo"]["full_name"] == REPO, "PR 不属于生产仓库")
    for side in ("head", "base"):
        sha(pr[side]["sha"])
        integer(pr[side]["repo"]["id"])
        check(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", pr[side]["repo"]["full_name"]) is not None, "PR 仓库身份非法")
        check(isinstance(pr[side]["ref"], str) and pr[side]["ref"], "PR 分支身份缺失")
    if consuming:
        check(pr.get("draft") is False and pr["base"]["ref"] == "master", "消费要求非草稿、目标为 master 的 PR")
    if reviewed_head is not None:
        check(pr["head"]["sha"] == sha(reviewed_head), "当前 head 不是已审查 head")


def validate_unblocked(pr):
    # Read/refresh operations keep validate_live's original readiness contract.
    check(pr.get("mergeable_state") != "blocked", "GitHub 合并状态为 blocked；禁止准备合并意图或执行合并")


def merge_state(pr, base, merge):
    valid = (merge is not None and merge.get("sha") == pr.get("merge_commit_sha") and
             [row.get("sha") for row in merge.get("parents", [])] == [base, pr["head"]["sha"]] and
             bool(re.fullmatch(gate.SHA, merge.get("tree", {}).get("sha", ""))))
    if pr.get("mergeable") is False and not valid and pr.get("mergeable_state") == "dirty":
        return "conflict"
    if pr.get("mergeable") is True and valid and pr["base"]["sha"] == base:
        return "ready"
    return "unknown"


def validate_run_identity(receipt, run, pr, workflow_id):
    validate_live(pr)
    check(run["path"] == WORKFLOW and run["workflow_id"] == workflow_id, "验证工作流路径/id 不符")
    check(run["repository"]["full_name"] == REPO and run["repository"]["id"] == pr["base"]["repo"]["id"], "验证运行不属于生产仓库")
    if run["event"] == "pull_request":
        check(run["head_repository"]["id"] == pr["head"]["repo"]["id"] and
              run["head_repository"]["full_name"] == pr["head"]["repo"]["full_name"] and
              run["head_branch"] == pr["head"]["ref"] and run["head_sha"] == pr["head"]["sha"], "原生验证的来源分支/仓库/head 不符")
        check(not receipt.get("controller_sha"), "原生验证不能伪装为可信控制器")
    else:
        check(run["event"] == "workflow_dispatch" and run["head_repository"]["id"] == pr["base"]["repo"]["id"] and
              run["head_repository"]["full_name"] == REPO and run["head_branch"] == "master" and
              run["head_sha"] == sha(receipt.get("controller_sha")) and run["display_title"] == dispatch_title(pr), "控制器未运行在生产默认分支的精确策略提交")
    check(receipt.get("source") == source_identity(pr), "回执来源 PR/fork/分支不匹配")


def validate_proof(receipt, run, jobs, pr, master, merge, workflow_id):
    validate_run_identity(receipt, run, pr, workflow_id)
    check(run["status"] == "completed" and run["conclusion"] == "success", "验证运行没有明确成功")
    check(merge_state(pr, master, merge) == "ready", "没有匹配的双亲测试合并")
    expected = {"repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"], "base": master,
                "sha": merge["sha"], "tree": merge["tree"]["sha"],
                "run_id": str(integer(run["id"])), "run_attempt": str(integer(run["run_attempt"]))}
    check(receipt.get("schema") == 1 and receipt.get("result") == "success" and
          all(receipt.get(key) == value for key, value in expected.items()), "回执不属于此 PR/精确合并/run/attempt")
    check(isinstance(receipt.get("tasks"), list) and 0 < len(receipt["tasks"]) <= 256, "回执任务清单非法")
    expected_jobs = ["Merge plan", "Merge evidence", *["Merge tests / " + task["unit"] for task in receipt["tasks"]]]
    check(len(jobs) == len(expected_jobs) and sorted(row["name"] for row in jobs) == sorted(expected_jobs), "显式工作任务缺失、重复或额外" + gate.RERUN_ALL)
    check(all(row.get("status") == "completed" and row.get("conclusion") == "success" and
              row.get("run_id") == run["id"] and row.get("run_attempt") == run["run_attempt"] for row in jobs), "任务取消/跳过/失败或尝试身份不符" + gate.RERUN_ALL)
    gate.finalize({**receipt, **expected}, {"plan": {"result": "success"}, "tests": {"result": "success"}}, receipt["evidence"])
    return expected


def decode_artifact(data, digest, member="merge-gate.json", limit=MAX_ARTIFACT):
    check(len(data) <= limit and isinstance(digest, str) and
          digest == "sha256:" + hashlib.sha256(data).hexdigest(), "远端 artifact 摘要缺失或不匹配")
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        entries = archive.infolist()
        check(len(entries) == 1 and entries[0].filename == member and entries[0].file_size <= limit and not entries[0].is_dir(), "artifact 必须仅含指定的有界 JSON 数据；禁止解压执行")
        return gate.verifier.decode(archive.read(entries[0]).decode("utf-8"))


def snapshot(api, number):
    pr = api.get(f"pulls/{integer(number)}")
    validate_live(pr)
    base = sha(api.get("git/ref/heads/" + quote(pr["base"]["ref"], safe=""))["object"]["sha"])
    merge_sha = pr.get("merge_commit_sha")
    merge = None
    if merge_sha is not None:
        try:
            merge = api.get("git/commits/" + sha(merge_sha))
        except subprocess.CalledProcessError:
            # GitHub may expose a stale/unavailable test merge while computing conflicts.
            pass
    return pr, base, merge


def fingerprint(state):
    pr, base, merge = state
    sides = {side: {"sha": pr[side]["sha"], "ref": pr[side]["ref"],
                    "repo_id": pr[side]["repo"]["id"], "repo": pr[side]["repo"]["full_name"]} for side in ("head", "base")}
    return {"number": pr["number"], "state": pr["state"], "draft": pr["draft"], "merged": pr.get("merged", False),
            **sides, "base_tip": base, "merge": None if merge is None else
            {"sha": merge.get("sha"), "tree": merge.get("tree"), "parents": [p.get("sha") for p in merge.get("parents", [])]},
            "mergeable": pr.get("mergeable"), "conflict": merge_state(pr, base, merge) == "conflict"}


def stable_snapshot(api, number):
    for attempt in range(3):
        first = snapshot(api, number)
        if first[0].get("mergeable") is None:
            time.sleep(1)
        second = snapshot(api, number)
        if fingerprint(first) == fingerprint(second) and (second[0].get("mergeable") is not None or attempt == 2):
            return second
    raise StateChanged("PR 持续变化，拒绝发布成功")


def matching_prs(api, run, base_ref=None):
    repository = run["head_repository"]
    check(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository["full_name"]) is not None, "运行来源仓库非法")
    query = {"state": "open", "head": repository["full_name"].split("/")[0] + ":" + run["head_branch"]}
    if base_ref is not None:
        query["base"] = base_ref
    return [pr for pr in api.pages("pulls?" + urlencode(query), None)
            if pr["head"]["repo"]["id"] == repository["id"] and pr["head"]["repo"]["full_name"] == repository["full_name"] and
            pr["head"]["ref"] == run["head_branch"] and pr["head"]["sha"] == run["head_sha"] and
            pr["base"]["repo"]["full_name"] == REPO and (base_ref is None or pr["base"]["ref"] == base_ref)]


def dispatch_title(pr):
    return f"Merge validation PR {pr['number']} head {pr['head']['sha']} base {pr['base']['sha']}"


def ignored_run(api, run):
    if run["event"] != "pull_request":
        return False
    # Only the protected workflow's positive event marker excludes work, including pending metadata.
    marker = re.fullmatch(r"Merge metadata ignored PR ([1-9][0-9]*) head ([0-9a-f]{40}) action (edited|labeled)", run.get("display_title", ""))
    return marker is not None and marker[2] == run["head_sha"]


def native_pr(api, run, multiple):
    title = re.fullmatch(r"Merge validation PR ([1-9][0-9]*)", run.get("display_title", ""))
    if title:
        return int(title[1])
    numbers = {pr["number"] for pr in run.get("pull_requests", [])}
    if len(numbers) == 1:
        return numbers.pop()
    if multiple and run["status"] == "completed" and run["conclusion"] == "success":
        artifacts = [row for row in api.pages(f"actions/runs/{integer(run['id'])}/artifacts", "artifacts")
                     if row["name"] == f"merge-gate-{run['id']}-{run['run_attempt']}" and row.get("expired") is False]
        if len(artifacts) == 1 and artifacts[0].get("workflow_run", {}).get("id") == run["id"]:
            receipt = decode_artifact(api.archive(artifacts[0]["id"]), artifacts[0].get("digest"))
            check(receipt.get("repo") == REPO and receipt.get("head") == run["head_sha"], "原生回执归属头不匹配")
            return integer(receipt["pr"])
    return None


def latest_run(api, pr, trusted_only=False):
    query = urlencode({"event": "pull_request", "head_sha": pr["head"]["sha"]})
    runs = api.pages("actions/workflows/merge-gate.yml/runs?" + query, "workflow_runs")
    native = [run for run in runs if run["event"] == "pull_request" and run["path"] == WORKFLOW and
              run["head_sha"] == pr["head"]["sha"] and run["head_branch"] == pr["head"]["ref"] and
              run["head_repository"]["id"] == pr["head"]["repo"]["id"] and run["head_repository"]["full_name"] == pr["head"]["repo"]["full_name"]]
    multiple = len(source_targets(api, pr["head"]["sha"])) > 1
    if trusted_only:
        native = []
    else:
        eligible = []
        for run in native:
            if ignored_run(api, run):
                continue
            owner = native_pr(api, run, multiple)
            if owner is None or owner == pr["number"]:
                eligible.append({**run, "_ambiguous": multiple and owner is None})
        native = eligible
    query = urlencode({"event": "workflow_dispatch"})
    dispatched = api.pages("actions/workflows/merge-gate.yml/runs?" + query, "workflow_runs")
    dispatched = [run for run in dispatched if run["event"] == "workflow_dispatch" and run["path"] == WORKFLOW and
                  run.get("display_title") == dispatch_title(pr) and run["head_repository"]["full_name"] == REPO and run["head_branch"] == "master"]
    candidates = native + dispatched
    return max(candidates, key=lambda run: integer(run["id"])) if candidates else None


def policy_contents(api, path, ref):
    check(path in gate.POLICY_PATHS, "仅允许缓存受保护策略路径")
    ref = sha(ref)
    key = (ref, path)
    cache = getattr(api, "_policy_contents_cache", {})
    if key in cache:
        data, blob = cache[key]
        return {"type": "file", "encoding": "base64", "content": base64.b64encode(data).decode("ascii"),
                "size": len(data), "sha": blob}
    remote = api.get("contents/" + path + "?" + urlencode({"ref": ref}))
    if not (isinstance(remote, dict) and remote.get("type") == "file" and remote.get("encoding") == "base64" and
            isinstance(remote.get("content"), str) and type(remote.get("size")) is int and
            0 <= remote["size"] <= MAX_POLICY_CACHE_BYTES and len(remote["content"]) <= 2 * MAX_POLICY_CACHE_BYTES):
        return remote
    try:
        data = base64.b64decode(remote["content"])
    except ValueError:
        return remote
    blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
    if (blob == remote.get("sha") and len(data) == remote["size"] and len(cache) < MAX_POLICY_CACHE_ENTRIES and
            sum(len(value[0]) for value in cache.values()) + len(data) <= MAX_POLICY_CACHE_BYTES):
        # Only verified immutable bytes are retained, scoped to this API client/process.
        cache[key] = (data, blob)
        api._policy_contents_cache = cache
    return remote


def policy_digest(api, ref):
    digest = hashlib.sha256()
    blobs = {}
    for path in gate.POLICY_PATHS:
        remote = policy_contents(api, path, ref)
        check(remote.get("type") == "file" and remote.get("encoding") == "base64", "可信策略文件缺失/不是普通文件")
        data = base64.b64decode(remote["content"])
        blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
        check(blob == remote.get("sha") and len(data) == remote.get("size"), "可信策略 Git blob 身份不匹配")
        blobs[path] = blob
        digest.update(path.encode() + b"\0" + data + b"\0")
    return digest.hexdigest(), blobs


def ancestor(api, old, new):
    if old == new:
        return True
    comparison = api.get("compare/" + sha(old) + "..." + sha(new))
    return comparison.get("status") in ("ahead", "identical") and comparison.get("merge_base_commit", {}).get("sha") == old


def trusted_policy(api, receipt, run, current_master):
    ref = receipt.get("controller_sha") or receipt["base"]
    if not ancestor(api, ref, current_master):
        raise PolicyChange("原生候选的策略不来自已发布主干；需要默认分支控制器")
    digest, blobs = policy_digest(api, ref)
    if run["event"] == "pull_request":
        for path, blob in blobs.items():
            remote = policy_contents(api, path, receipt["sha"])
            if remote.get("type") != "file" or remote.get("sha") != blob:
                raise PolicyChange("候选更改 CI 策略；切换到受信默认分支控制器")
    check(receipt["policy_digest"] == digest, "回执策略摘要不属于其受信工作流提交")


def proof(api, state, run_id, attempt, root):
    pr, base, merge = state
    run = api.get(f"actions/runs/{integer(run_id)}")
    check(run["run_attempt"] == integer(attempt), "运行已重试，旧 attempt 不可消费")
    exact = api.get(f"actions/runs/{run_id}/attempts/{attempt}")
    check(exact["id"] == run_id and exact["run_attempt"] == attempt, "远端尝试身份不符")
    if exact["event"] == "pull_request":
        candidates = matching_prs(api, exact, pr["base"]["ref"])
        check(len(candidates) == 1 and candidates[0]["number"] == pr["number"], "来源对应的开放 PR 不唯一/不匹配")
    workflow = api.get("actions/workflows/merge-gate.yml")
    check(workflow["path"] == WORKFLOW and workflow["state"] == "active", "验证工作流未启用")
    artifacts = [row for row in api.pages(f"actions/runs/{run_id}/artifacts", "artifacts") if row["name"] == f"merge-gate-{run_id}-{attempt}"]
    check(len(artifacts) == 1 and artifacts[0].get("expired") is False, "唯一回执 artifact 缺失/过期" + gate.RERUN_ALL)
    artifact = artifacts[0]
    check(artifact.get("workflow_run", {}).get("id") == run_id and artifact.get("size_in_bytes", MAX_ARTIFACT + 1) <= MAX_ARTIFACT, "artifact 远端运行身份/大小非法")
    receipt = decode_artifact(api.archive(artifact["id"]), artifact.get("digest"))
    if receipt.get("pr") != pr["number"] or receipt.get("source") != source_identity(pr):
        raise UnassignedRun("此原生运行属于同 head 的其他 PR/目标；需要受信精确树验证")
    jobs = api.pages(f"actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
    jobs = normalize_validation_jobs(api, jobs, pr, exact)
    original = api.get("git/commits/" + sha(receipt["sha"]))
    # Establish the original external proof before deciding that it is merely stale.
    original_pr = {**pr, "base": {**pr["base"], "sha": receipt["base"]}, "merge_commit_sha": receipt["sha"], "mergeable": True}
    identity = validate_proof(receipt, exact, jobs, original_pr, receipt["base"], original, workflow["id"])
    current_master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    trusted_policy(api, receipt, exact, current_master)
    if receipt["base"] != base or receipt["sha"] != merge["sha"] or receipt["tree"] != merge["tree"]["sha"]:
        raise StaleBase("旧回执的真实 base/合并/tree 已过期，必须刷新原生合并检查")
    validate_proof(receipt, exact, jobs, pr, base, merge, workflow["id"])
    return {"identity": identity, "source": receipt["source"], "artifact_id": integer(artifact["id"]),
            "artifact_digest": artifact["digest"], "policy_digest": receipt["policy_digest"],
            "controller_sha": receipt.get("controller_sha", "")}


def verify_failed_target(api, state, run):
    """A failed source-head run is not a failure of every later merge tree."""
    pr, base, merge = state
    run_id, attempt = integer(run["id"]), integer(run["run_attempt"])
    latest = api.get(f"actions/runs/{run_id}")
    check(latest["run_attempt"] == attempt, "失败运行已重试，旧 attempt 不能归因")
    exact = api.get(f"actions/runs/{run_id}/attempts/{attempt}")
    check(all(exact.get(key) == run.get(key) for key in ("id", "run_attempt", "status", "conclusion", "event")), "失败运行终态/尝试已变化")
    workflow = api.get("actions/workflows/merge-gate.yml")
    check(workflow["path"] == WORKFLOW and workflow["state"] == "active", "验证工作流未启用")
    artifacts = api.pages(f"actions/runs/{run_id}/artifacts", "artifacts")
    identity = [row for row in artifacts if row["name"] == f"merge-identity-{run_id}-{attempt}"]
    member, limit = ("merge-identity.json", 65536) if identity else ("merge-plan.json", MAX_ARTIFACT)
    artifacts = identity or [row for row in artifacts if row["name"] == f"merge-plan-{run_id}-{attempt}"]
    check(len(artifacts) == 1 and artifacts[0].get("expired") is False, "失败运行缺少唯一精确初始身份/计划" + gate.RERUN_ALL)
    artifact = artifacts[0]
    check(artifact.get("workflow_run", {}).get("id") == run_id and artifact.get("size_in_bytes", limit + 1) <= limit, "初始身份/计划远端运行身份/大小非法")
    plan = decode_artifact(api.archive(integer(artifact["id"])), artifact.get("digest"), member, limit)
    if identity:
        check(plan.get("schema") == 1 and plan.get("kind") == "merge-identity", "初始身份协议非法")
    validate_run_identity(plan, exact, pr, workflow["id"])
    expected = {"repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"], "base": base,
                "sha": merge["sha"], "tree": merge["tree"]["sha"], "run_id": str(run_id), "run_attempt": str(attempt)}
    check(all(plan.get(key) == value for key, value in expected.items()), "失败计划不属于当前精确合并树")
    original = api.get("git/commits/" + sha(plan["sha"]))
    check(merge_state(pr, base, original) == "ready" and original["tree"]["sha"] == plan["tree"], "失败计划的真实双亲/tree 不符")
    trusted_policy(api, plan, exact, sha(api.get("git/ref/heads/master")["object"]["sha"]))
    jobs = api.pages(f"actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
    planned = [job for job in jobs if job["name"] == "Merge plan"]
    check(len(planned) == 1 and all(planned[0].get(key) == value for key, value in
          {"run_id": run_id, "run_attempt": attempt, "status": "completed"}.items()) and
          (bool(identity) or planned[0].get("conclusion") == "success"), "失败运行未完成可核验的初始身份/计划步骤")


def external_id(pr):
    return f"merge-gate:v2:{REPO}:source:{pr['head']['sha']}"


def check_sha(pr):
    return sha(pr["head"]["sha"])


def owned(row, pr):
    return (row.get("app", {}).get("id") == APP_ID and row.get("app", {}).get("slug") == "github-actions" and
            row.get("external_id") == external_id(pr) and row.get("head_sha") == check_sha(pr))


def normalize_validation_jobs(api, jobs, pr, run):
    """GitHub can echo an app-created CheckRun as a runnerless row in its assigned suite's jobs."""
    native = []
    for job in jobs:
        # Expected native names are Merge plan, Merge evidence and Merge tests / <unit>, never Merge gate.
        if job.get("name") != "Merge gate":
            native.append(job)
            continue
        check(job.get("steps") == [] and job.get("labels") == [] and
              all(key in job and job[key] is None for key in ("runner_id", "runner_name", "runner_group_id", "runner_group_name")),
              "额外 Merge gate 任务有 runner/步骤或缺少空执行证明，不能过滤")
        job_id = integer(job["id"])
        api_root = f"https://api.github.com/repos/{REPO}"
        check_url, html_url = f"{api_root}/check-runs/{job_id}", f"https://github.com/{REPO}/runs/{job_id}"
        check(integer(job["run_id"]) == run["id"] and integer(job["run_attempt"]) == run["run_attempt"] and
              job.get("head_sha") == run["head_sha"] == check_sha(pr) and job.get("head_branch") == run["head_branch"] and
              job.get("run_url") == f"{api_root}/actions/runs/{run['id']}" and job.get("url") == f"{api_root}/actions/jobs/{job_id}" and
              job.get("check_run_url") == check_url and job.get("html_url") == html_url and
              sum(row.get("id") == job_id for row in jobs) == 1, "额外检查回显的运行、URL 或唯一身份不符")
        custom = api.get(f"check-runs/{job_id}")
        check(isinstance(custom, dict) and isinstance(custom.get("app"), dict) and owned(custom, pr) and
              integer(custom["id"]) == job_id and custom.get("name") == "Merge gate" and custom.get("url") == check_url and
              custom.get("html_url") == html_url and isinstance(job.get("node_id"), str) and job["node_id"] and
              custom.get("node_id") == job["node_id"] and isinstance(custom.get("check_suite"), dict) and
              custom["check_suite"].get("id") == integer(run.get("check_suite_id")), "无法认证自发布 CheckRun 回显的所有权/原生 suite 关联")
        metadata = gate.verifier.decode(custom["output"]["summary"])
        check(metadata.get("schema") == 1 and metadata.get("repo") == REPO and metadata.get("head") == check_sha(pr), "回显发布元数据身份不符")
        # This check is source-head scoped; its historical projection need not contain today's PR/targets.
        members, targets = metadata["membership"], metadata["targets"]
        check(isinstance(members, list) and isinstance(targets, list) and len(members) == len(targets) and
              all(isinstance(row, dict) for row in [*members, *targets]), "回显历史目标投影非法")
        check(len({integer(row["pr"]) for row in members}) == len(members), "回显历史目标重复")
        for member in members:
            source = {key: member[key] for key in ("pr", "head", "head_ref", "head_repo_id", "head_repo", "base_ref")}
            integer(source["head_repo_id"])
            check(source["head"] == check_sha(pr) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", source["head_repo"]) and
                  all(isinstance(source[key], str) and source[key] for key in ("head_ref", "base_ref")) and
                  sum(all(row.get(key) == value for key, value in source.items()) for row in targets) == 1, "回显历史 PR/来源投影不一致")
        sha(metadata["publisher"].get("workflow_sha"))
        publisher = verify_publisher_run(api, metadata["publisher"], terminal=False, source_head=check_sha(pr))
        check(publisher["status"] in ("in_progress", "completed"), "回显发布器没有实际执行身份")
        # Classification only. The unchanged native proof and terminal publisher checks still authorize consumption.
    return native


def checks(api, pr):
    return [row for row in api.pages("commits/" + check_sha(pr) + "/check-runs?filter=all", "check_runs") if row["name"] == "Merge gate"]


def log_check(row, pr, metadata):
    identity = metadata.get("validation", {}).get("identity", {})
    publisher = metadata.get("publisher", {})
    record = {"event": "merge_check_published", "repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"],
              "base": identity.get("base", pr["base"]["sha"]), "validated_sha": identity.get("sha"), "tree": identity.get("tree"),
              "check_id": row["id"], "check_suite_id": row.get("check_suite", {}).get("id"),
              "target_sha": row.get("head_sha", check_sha(pr)), "name": row.get("name", "Merge gate"),
              "status": row.get("status"), "conclusion": row.get("conclusion"),
              "publisher_run_id": publisher.get("run_id"), "publisher_run_attempt": publisher.get("run_attempt"),
              "validation_run_id": identity.get("run_id"), "validation_run_attempt": identity.get("run_attempt")}
    text = json.dumps(record, sort_keys=True, ensure_ascii=True, separators=(",", ":"))
    check(len(text) <= 2048, "结构化检查回执超过日志边界")
    print("MERGE_CHECK " + text, flush=True)


def publish_check(api, pr, conclusion, metadata, guard=lambda: True):
    if conclusion == "success":
        validate_live(pr)
    else:
        integer(pr["number"])
        sha(pr["head"]["sha"])
        check(pr["base"]["repo"]["full_name"] == REPO, "不能向其他仓库发布检查")
    check(conclusion in ("success", "failure", "pending"), "发布状态非法")
    existing = checks(api, pr)
    check(all(owned(row, pr) for row in existing), "同名检查存在其他 PR/写入者，拒绝碰撞授权")
    existing.sort(key=lambda row: integer(row["id"]))
    output = {"title": {"success": "合并验证通过", "failure": "合并已阻塞", "pending": "等待合并验证"}[conclusion],
              "summary": json.dumps(metadata, sort_keys=True, ensure_ascii=True)}
    data = {"name": "Merge gate", "external_id": external_id(pr), "status": "in_progress" if conclusion == "pending" else "completed", "output": output}
    if conclusion != "pending":
        data["conclusion"] = conclusion
    identity = metadata.get("validation", {}).get("identity", {})
    if identity:
        data["details_url"] = f"https://github.com/{REPO}/actions/runs/{identity['run_id']}/attempts/{identity['run_attempt']}"
    state_check(guard(), "发布前 PR/运行已变化")
    row = existing[-1] if existing else None
    unchanged = (row is not None and all(row.get(key) == value for key, value in data.items() if key != "output") and
                 all(row.get("output", {}).get(key) == value for key, value in output.items()))
    if not unchanged:
        # POST returns the actual suite assignment. The native publisher job's suite is not a selector.
        # Never mutate historical generations merely because their source head matches.
        row = api.write("POST", "check-runs", {**data, "head_sha": check_sha(pr)})
        log_check(row, pr, metadata)
    if not guard():
        stale = {key: value for key, value in metadata.items() if key != "validation"}
        stale.update(result="pending", reason_code="source_state_changed", reason="发布期间身份变化，旧成功不可消费")
        if "targets" in stale:
            stale["targets"] = [{**{key: value for key, value in target.items() if key != "validation"},
                                 "result": "pending", "verification": False} for target in stale["targets"]]
            stale.update(failed_prs=[], pending_prs=[target["pr"] for target in stale["targets"]])
        withdrawn = {**data, "status": "completed", "conclusion": "failure",
                     "output": {"title": "等待并发变化后的验证", "summary": json.dumps(stale, ensure_ascii=True)}}
        if unchanged:
            row = api.write("POST", "check-runs", {**withdrawn, "head_sha": check_sha(pr)})
        else:
            # Only this call's new row can be invalidated, and completed never becomes active again.
            row = api.write("PATCH", "check-runs/" + str(integer(row["id"])), withdrawn)
        log_check(row, pr, stale)
        raise StateChanged("发布期间身份变化；已撤回成功")
    return row


def dispatch_inputs(pr, base, merge, policy_sha):
    return {"pr": str(pr["number"]), "head": pr["head"]["sha"], "base": base, "sha": merge["sha"],
            "tree": merge["tree"]["sha"], "policy_sha": policy_sha, "source_repo_id": str(pr["head"]["repo"]["id"]),
            "source_repo": pr["head"]["repo"]["full_name"], "source_ref": pr["head"]["ref"], "base_ref": pr["base"]["ref"]}


def candidate_policy_changed(api, merge_sha, policy_sha):
    _, blobs = policy_digest(api, policy_sha)
    for path, blob in blobs.items():
        try:
            candidate = policy_contents(api, path, merge_sha)
        except subprocess.CalledProcessError:
            return True
        if candidate.get("type") != "file" or candidate.get("sha") != blob:
            return True
    return False


def source_targets(api, head):
    return sorted([pr for pr in api.pages("pulls?state=open", None)
                   if pr.get("state") == "open" and pr.get("head", {}).get("sha") == sha(head) and
                   pr.get("base", {}).get("repo", {}).get("full_name") == REPO], key=lambda pr: integer(pr["number"]))


def target_details(pr):
    head, base = pr["head"], pr["base"]
    repository = head.get("repo") or {}
    return {"pr": pr["number"], "head": head["sha"], "head_ref": head["ref"], "head_repo_id": repository.get("id"),
            "head_repo": repository.get("full_name"), "base_ref": base["ref"], "base": base["sha"], "draft": pr.get("draft")}


def aggregate(pr, targets, membership, context):
    result = "success" if targets and all(row["result"] == "success" for row in targets) else "failure" if any(row["result"] == "failure" for row in targets) else "pending"
    metadata = {"schema": 1, "repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"], "result": result,
                "reason_code": "source_head_intersection", "membership": membership, "targets": targets,
                "failed_prs": [row["pr"] for row in targets if row["result"] == "failure"],
                "pending_prs": [row["pr"] for row in targets if row["result"] == "pending"],
                "publisher": {"workflow": PUBLISHER, "run_id": context["GITHUB_RUN_ID"], "run_attempt": context["GITHUB_RUN_ATTEMPT"],
                              "workflow_sha": context.get("MERGE_TRUSTED_WORKFLOW_SHA")}}
    if len(targets) == 1:
        for key in ("validation", "reason", "dispatch_requested", "dispatch_refresh_id"):
            if key in targets[0]:
                metadata[key] = targets[0][key]
    return metadata


def project_target(api, number, root, context, previous):
    state = stable_snapshot(api, number)
    pr, base, merge = state
    state_check(context.get("MERGE_SOURCE_SHA", pr["head"]["sha"]) == pr["head"]["sha"], "PR 已离开此 source SHA 并发组")
    status = merge_state(pr, base, merge)
    policy_sha = sha(api.get("git/ref/heads/master")["object"]["sha"]) if status == "ready" else None
    # Policy detection precedes candidate artifact/job parsing.
    changed = status == "ready" and candidate_policy_changed(api, merge["sha"], policy_sha)
    current_pr = {**pr, "base": {**pr["base"], "sha": base}}
    run = latest_run(api, current_pr, trusted_only=changed) if status == "ready" else None
    target = {**target_details(current_pr), "merge_sha": merge["sha"] if status == "ready" else None,
              "tree": merge["tree"]["sha"] if status == "ready" else None, "verification": False, "result": "pending",
              "reason_code": "awaiting_validation", "reason": "等待精确目标的合并计算或验证"}
    dispatch = None
    if run is not None:
        target["authority"] = {field: run[field] for field in ("id", "run_attempt", "status", "conclusion", "event")}
    if status == "conflict":
        target.update(result="failure", reason_code="confirmed_conflict", reason="确认此目标存在合并冲突")
    elif status == "ready" and (changed and run is None or run is not None and run.get("_ambiguous")):
        dispatch = dispatch_inputs(pr, base, merge, policy_sha)
        target.update(reason_code="trusted_controller_required", reason="由默认分支控制器验证精确 PR/目标，不依赖候选协议")
    elif status == "ready" and run is not None and run["status"] == "completed":
        if run["conclusion"] != "success":
            try:
                verify_failed_target(api, state, run)
                target.update(result="failure", reason_code="terminal_validation_unsuccessful", reason="此精确目标最新验证明确未成功")
            except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError, zipfile.BadZipFile) as error:
                dispatch = dispatch_inputs(pr, base, merge, policy_sha)
                target.update(reason_code="trusted_controller_required", reason="失败归属尚未绑定当前合并树：" + str(error))
        else:
            try:
                target["validation"] = proof(api, state, run["id"], run["run_attempt"], root)
                target.update(result="success", verification=True, reason_code="exact_target_verified")
                target.pop("reason")
            except (PolicyChange, UnassignedRun, StaleBase) as error:
                dispatch = dispatch_inputs(pr, base, merge, policy_sha)
                target.update(reason_code="trusted_controller_required", reason=str(error))
            except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError, zipfile.BadZipFile) as error:
                target.update(result="failure", reason_code="invalid_terminal_evidence", reason="此目标终态证据无效：" + str(error))

    def guard():
        try:
            if fingerprint(snapshot(api, number)) != fingerprint(state):
                return False
            if policy_sha is not None and api.get("git/ref/heads/master")["object"]["sha"] != policy_sha:
                return False
            current = latest_run(api, current_pr, trusted_only=changed) if status == "ready" else None
            fields = ("id", "run_attempt", "status", "conclusion", "_ambiguous")
            return current is None and run is None or current is not None and run is not None and all(current.get(field) == run.get(field) for field in fields)
        except (ValueError, KeyError, TypeError, subprocess.CalledProcessError):
            return False

    old = [row for metadata in previous for row in metadata.get("targets", [metadata]) if row.get("pr") == number]
    requested = False
    for row in old:
        intent = row.get("dispatch_requested")
        if intent and intent.get("head") == pr["head"]["sha"] and intent.get("base") == base and target["result"] == "pending":
            target["dispatch_requested"] = intent
            target["dispatch_refresh_id"] = row.get("dispatch_refresh_id")
        if dispatch and intent == dispatch and (context.get("MERGE_REFRESH_ID") is None or context["MERGE_REFRESH_ID"] == row.get("dispatch_refresh_id")):
            requested = True
    return target, guard, None if requested else dispatch


def reconcile(api, number, root, context, retries=1):
    requested = api.get(f"pulls/{integer(number)}")
    head = sha(context.get("MERGE_SOURCE_SHA", requested["head"]["sha"]))
    requested = {**requested, "head": {**requested["head"], "sha": head}}
    targets = source_targets(api, head)
    membership = [target_details(pr) for pr in targets]
    existing = checks(api, requested)
    owned_rows = [row for row in existing if owned(row, requested)]
    previous = [gate.verifier.decode(max(owned_rows, key=lambda row: integer(row["id"]))["output"]["summary"])] if owned_rows else []
    if not targets and not existing:
        return None
    visible = targets[0] if targets else requested
    entries = [{**target_details(pr), "result": "pending", "verification": False, "reason_code": "source_identity_unknown"} for pr in targets]
    member_guard = lambda: [target_details(pr) for pr in source_targets(api, head)] == membership
    try:
        # Withdraw a previous green before evaluating a changed target set.
        if any(row.get("result") == "success" and row.get("membership") != membership for row in previous):
            pending = aggregate(visible, entries, membership, context)
            pending["reason_code"] = "source_membership_changed"
            publish_check(api, requested, "pending", pending, member_guard)
        evaluations = []
        for index, pr in enumerate(targets):
            if pr["head"].get("repo") is not None:
                entry, guard, dispatch = project_target(api, pr["number"], root, context, previous)
                entries[index] = entry
                evaluations.append((entry, guard, dispatch))
        guard = lambda: member_guard() and all(item[1]() for item in evaluations)
        metadata = aggregate(visible, entries, membership, context)
        result = publish_check(api, visible, metadata["result"], metadata, guard)
        for entry, target_guard, dispatch in evaluations:
            if dispatch is None:
                continue
            state_check(member_guard() and target_guard(), "派发前精确目标已变化")
            # Persist one intent at a time: later targets are not falsely marked as submitted.
            entry["dispatch_requested"] = dispatch
            entry["dispatch_refresh_id"] = context.get("MERGE_REFRESH_ID")
            metadata = aggregate(visible, entries, membership, context)
            result = publish_check(api, visible, metadata["result"], metadata, lambda: member_guard() and target_guard())
            if not member_guard() or not target_guard():
                entry.pop("dispatch_requested", None)
                publish_check(api, visible, "pending", {**aggregate(visible, entries, membership, context), "result": "pending"})
                raise StateChanged("派发前身份变化，未发送此写入")
            # Unknown POST outcomes retain this target's intent and are never automatically retried.
            api.write("POST", "actions/workflows/merge-gate.yml/dispatches", {"ref": "master", "inputs": dispatch})
        return result
    except StateChanged:
        if retries:
            return reconcile(api, number, root, context, retries - 1)
        metadata = aggregate(requested, entries, membership, context)
        metadata.update(result="pending", reason_code="source_state_changed")
        return publish_check(api, requested, "pending", metadata)


def shared_lock_path(root):
    common = Path(gate.git(root, "rev-parse", "--path-format=absolute", "--git-common-dir"))
    check(common.name == ".git", "只支持共享 .git 的普通仓库/工作树锁")
    return common.parent / ".local/merge.lock"


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def durable_json(path, value, exclusive=False):
    path = Path(path)
    temporary = path if exclusive else path.with_name(path.name + ".tmp-" + secrets.token_hex(8))
    with temporary.open("x", encoding="utf-8") as output:
        json.dump(value, output, sort_keys=True, ensure_ascii=True)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    if not exclusive:
        os.replace(temporary, path)
    sync_directory(path.parent)


def release_own_lock(lock, token, inode):
    owner = lock / "merge-gate-owner"
    check(lock.is_dir() and not lock.is_symlink() and lock.stat().st_ino == inode and owner.is_file() and
          not owner.is_symlink() and gate.read_json(owner)["token"] == token, "锁身份变化；绝不释放其他写入者的锁")
    metadata = gate.read_json(owner)
    lease = lock / "merge-gate-lease"
    expected = {owner.name}
    if metadata.get("schema") == 2:
        check(lease.is_file() and not lease.is_symlink() and lease.stat().st_ino == metadata["lease_inode"], "内核租约身份变化，保留锁")
        expected.add(lease.name)
    check({p.name for p in lock.iterdir()} == expected, "锁中出现非本工具文件，保留锁并阻塞")
    owner.unlink()
    if lease.name in expected:
        lease.unlink()
    lock.rmdir()
    sync_directory(lock.parent)


@contextmanager
def lock_lease(lock, owner=None, create=False):
    check(lock.is_dir() and not lock.is_symlink(), "共享锁目录身份非法")
    lease = lock / "merge-gate-lease"
    flags = os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC
    descriptor = os.open(lease, flags | (os.O_CREAT | os.O_EXCL if create else 0), 0o600)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise ValueError("共享锁拥有者仍活动；不能恢复或释放") from error
        info = os.fstat(descriptor)
        check(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and lease.stat().st_ino == info.st_ino, "内核租约文件身份非法")
        if owner is not None:
            check(owner.get("schema") == 2 and owner["inode"] == lock.stat().st_ino and owner["lease_inode"] == info.st_ino and
                  gate.read_json(lock / "merge-gate-owner") == owner, "恢复期间锁身份变化")
        yield info.st_ino
    finally:
        os.close(descriptor)


def recover_unsent_lock(lock):
    check(not lock.is_symlink(), "不能恢复符号链接锁")
    owner = gate.read_json(lock / "merge-gate-owner")
    check(owner.get("schema") == 2 and re.fullmatch(r"[0-9a-f]{48}", owner["token"]), "遗留/未知锁没有内核协议证明；保留锁")
    with lock_lease(lock, owner):
        if owner["incident"] is not None:
            record = gate.read_json(owner["incident"])
            check(record["lock"] == {"path": str(lock), **{key: owner[key] for key in ("token", "inode", "lease_inode")}} and
                  record.get("submitted") is False, "已经可能发送或事故身份不符；仅允许只读远端对账")
            record["result"] = "not_submitted"
            durable_json(owner["incident"], record)
        release_own_lock(lock, owner["token"], owner["inode"])


@contextmanager
def merge_lock(root):
    lock = shared_lock_path(root)
    lock.parent.mkdir(exist_ok=True)
    check(not lock.parent.is_symlink(), "共享锁目录不能是符号链接")
    try:
        lock.mkdir()
    except FileExistsError:
        try:
            recover_unsent_lock(lock)
        except (ValueError, OSError, KeyError, TypeError) as error:
            raise FileExistsError("共享锁仍被占用或缺少可恢复证明：" + str(error)) from error
        lock.mkdir()
    sync_directory(lock.parent)
    inode, token, keep = lock.stat().st_ino, secrets.token_hex(24), False
    with lock_lease(lock, create=True) as lease_inode:
        durable_json(lock / "merge-gate-owner", {"schema": 2, "token": token, "inode": inode,
                     "lease_inode": lease_inode, "incident": None}, exclusive=True)
        try:
            yield lock
        except UnknownMerge:
            keep = True  # The directory survives; kernel lease alone never proves a submitted outcome.
            raise
        finally:
            if not keep:
                release_own_lock(lock, token, inode)


def validate_command(command, number, reviewed_head):
    expected = ["gh", "pr", "merge", str(integer(number)), "--repo", REPO, "--merge", "--match-head-commit", sha(reviewed_head)]
    check(command == expected, "仅接受明确授权的指定 PR/head 普通 merge 命令，禁止 shell/admin/额外参数")


def verify_publisher(api, pr, validation):
    rows = checks(api, pr)
    check(rows and all(owned(row, pr) for row in rows), "可信 Merge gate 缺失或同名检查写入者不符")
    # Local generation selection does not prove GitHub's cross-suite ruleset rollup.
    current = max(rows, key=lambda row: integer(row["id"]))
    check(current.get("status") == "completed" and current.get("conclusion") == "success", "当前代次 Merge gate 未明确成功；历史成功不可消费")
    metadata = gate.verifier.decode(current["output"]["summary"])
    target = [row for row in metadata.get("targets", []) if row.get("pr") == pr["number"]]
    check(metadata.get("repo") == REPO and metadata.get("head") == pr["head"]["sha"] and metadata.get("result") == "success" and
          len(target) == 1 and target[0].get("result") == "success" and target[0].get("verification") is True and
          target[0].get("validation") == validation, "远端已发布交集不包含此目标的精确已验证回执")
    verify_publisher_run(api, metadata["publisher"])
    return metadata


def verify_publisher_run(api, publisher, terminal=True, source_head=None):
    check(publisher["workflow"] == PUBLISHER, "发布者路径不符")
    for field in ("run_id", "run_attempt"):
        check(re.fullmatch(r"[1-9][0-9]{0,63}", publisher[field]) is not None, "发布者运行身份非法")
    run = api.get(f"actions/runs/{publisher['run_id']}")
    exact = api.get(f"actions/runs/{publisher['run_id']}/attempts/{publisher['run_attempt']}")
    workflow = api.get("actions/workflows/merge-conflict.yml")
    # A rerun cannot revoke a historical row's ownership; only terminal authority must still be latest.
    attempt_valid = (run["run_attempt"] == int(publisher["run_attempt"]) if terminal else
                     run["id"] == int(publisher["run_id"]) and integer(run["run_attempt"]) >= int(publisher["run_attempt"]))
    check(attempt_valid and exact["id"] == int(publisher["run_id"]) and
          exact["run_attempt"] == int(publisher["run_attempt"]) and exact["path"] == PUBLISHER and exact["workflow_id"] == workflow["id"] and
          workflow["path"] == PUBLISHER and exact["repository"]["full_name"] == REPO and exact["event"] in ("pull_request_target", "workflow_run") and
          (not terminal or exact["status"] == "completed" and exact["conclusion"] == "success"), "发布器远端 provenance 不完整或尚未完成")
    master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    if not terminal and exact["event"] == "pull_request_target":
        # REST exposes the event's source head, not the base-owned workflow/checkout commit.
        policy = sha(publisher["workflow_sha"])
        associated = [row for row in exact.get("pull_requests", [])
                      if integer(row["number"]) and row["head"]["sha"] == exact["head_sha"] == sha(source_head) and
                      row["head"]["ref"] == exact["head_branch"] and
                      row["head"]["repo"]["id"] == integer(exact["head_repository"]["id"]) and
                      row["head"]["repo"]["url"] == "https://api.github.com/repos/" + exact["head_repository"]["full_name"] and
                      row["base"]["ref"] == "master" and row["base"]["sha"] == policy and
                      row["base"]["repo"]["id"] == integer(exact["repository"]["id"]) and
                      row["base"]["repo"]["url"] == "https://api.github.com/repos/" + REPO]
        check(len(associated) == 1 and ancestor(api, policy, master), "回显发布器的嵌入来源/目标或固定 master 工作流身份不符")
    else:
        check(ancestor(api, sha(exact["head_sha"]), master) and
              (publisher.get("workflow_sha") is None or publisher["workflow_sha"] == exact["head_sha"]), "发布器不是已发布默认分支的工作流代码")
    return exact


def refresh(api, pr):
    validate_live(pr, pr["head"]["sha"], consuming=True)
    check(pr.get("mergeable") is not False, "冲突需要人工解决；标签不能制造测试合并")
    labels = api.pages(f"issues/{pr['number']}/labels", None)
    for label in labels:
        if label["name"].casefold() == "ci:refresh":
            api.write("DELETE", f"issues/{pr['number']}/labels/" + quote(label["name"], safe=""))
    api.write("POST", f"issues/{pr['number']}/labels", {"labels": ["ci:refresh"]})


def consume_operation(api, root, number, reviewed_head, validation, command, review, outcome, execute=None):
    identity = validation["identity"]
    approval = gate.read_json(review)
    check(approval.get("result") == "approved" and approval.get("independent") is True and
          isinstance(approval.get("reviewer"), str) and approval["reviewer"].strip() and approval.get("head") == identity["head"] and
          all(k not in approval or approval[k] == identity[k] for k in ("base", "tree")), "独立审查必须批准此 head；审查明确限定的 base/tree 也必须匹配，CI 不是独立审查")
    evidence = Path(approval["evidence_path"])
    check(evidence.is_file() and hashlib.sha256(evidence.read_bytes()).hexdigest() == approval["evidence_sha256"], "独立审查原始证据缺失或摘要不匹配；禁止自建批准代替实际审查")
    current = snapshot(api, number)
    validate_live(current[0], reviewed_head, consuming=True)
    validate_unblocked(current[0])
    check(merge_state(*current) == "ready" and current[1] == identity["base"] and
          current[2]["sha"] == identity["sha"] and current[2]["tree"]["sha"] == identity["tree"] and
          source_identity(current[0]) == validation["source"], "发送前精确合并身份已变化，未准备合并意图")
    lock = shared_lock_path(root)
    owner = gate.read_json(lock / "merge-gate-owner")
    record = {"schema": 1, "result": "unknown", "submitted": False, "identity": identity, "source": validation["source"],
              "reviewer": approval["reviewer"], "command_returncode": None, "command_stdout": None, "command_stderr": None,
              "lock": {"path": str(lock), **{key: owner[key] for key in ("token", "inode", "lease_inode") if key in owner}}}
    durable_json(outcome, record, exclusive=True)
    owner["incident"] = str(Path(outcome).resolve())
    durable_json(lock / "merge-gate-owner", owner)
    try:
        record["submitted"] = True
        durable_json(outcome, record)
        try:
            if execute is None:
                child = subprocess.run(command, cwd=root, capture_output=True)
                record["command_returncode"] = child.returncode
                for stream in ("stdout", "stderr"):
                    data = getattr(child, stream)
                    record["command_" + stream] = data[-MAX_COMMAND_OUTPUT:].decode("utf-8", errors="replace")
                    record["command_" + stream + "_truncated"] = len(data) > MAX_COMMAND_OUTPUT
            else:
                record["command_returncode"] = execute(command, root)
        except (FileNotFoundError, PermissionError):
            record.update(result="not_submitted", submitted=False)
            raise ValueError("合并进程未启动，未发送远端操作")
        durable_json(outcome, record)
        record.update(remote_outcome(api, record))
        check(record["result"] != "unknown", "远端仍未返回终态")
    except BaseException as error:
        if record["submitted"] and record["result"] == "unknown":
            raise UnknownMerge("合并提交结果未知，保留共享锁；只读对账回执：" + str(outcome)) from error
        raise
    finally:
        try:
            durable_json(outcome, record)
        except BaseException as error:
            if record["submitted"]:
                raise UnknownMerge("发送后的回执未能持久化；保留共享锁和原始事故回执") from error
            raise
    check(record["result"] == "merged", "PR 已关闭但未合并；已记录终态，禁止自动重试")


def remote_outcome(api, record):
    identity = record["identity"]
    check(identity["repo"] == REPO, "事故回执仓库不匹配")
    after = api.get(f"pulls/{integer(identity['pr'])}")
    check(after["number"] == identity["pr"] and after["base"]["repo"]["full_name"] == REPO and
          after["base"]["ref"] == "master" and after["head"]["sha"] == sha(identity["head"]) and
          source_identity(after) == record["source"], "事故对账的 PR/head/来源身份不符")
    if after["state"] == "closed" and after.get("merged") is False:
        return {"result": "not_merged"}
    if after["state"] != "closed" or after.get("merged") is not True:
        return {"result": "unknown"}
    merged = api.get("git/commits/" + sha(after["merge_commit_sha"]))
    check([p["sha"] for p in merged["parents"]] == [sha(identity["base"]), identity["head"]] and merged["tree"]["sha"] == sha(identity["tree"]), "实际合并 parents/tree 不符；保留锁等待事故处理")
    master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    check(ancestor(api, merged["sha"], master), "无法确认合入后的主干关系")
    return {"result": "merged", "merged_sha": merged["sha"], "master_sha": master}


def reconcile_incident(api, root, outcome):
    record = gate.read_json(outcome)
    lock = shared_lock_path(root)
    ownership = record["lock"]
    check(str(lock) == ownership["path"], "事故回执不属于此共享工作树锁")
    owner = gate.read_json(lock / "merge-gate-owner") if lock.exists() else None
    lease = lock_lease(lock, owner) if owner is not None and owner.get("schema") == 2 else nullcontext(None)
    with lease as lease_inode:
        if owner is not None:
            check(owner["token"] == ownership["token"] and owner["inode"] == ownership["inode"] and
                  owner["incident"] == str(Path(outcome).resolve()) and owner.get("lease_inode") == ownership.get("lease_inode"),
                  "事故回执不能释放其他写入者的锁")
        if record.get("submitted") is False and ownership.get("lease_inode") is not None and (owner is None or lease_inode is not None):
            record["result"] = "not_submitted"
        else:
            record.update(remote_outcome(api, record))
        durable_json(outcome, record)
        check(record["result"] in ("merged", "not_merged", "not_submitted"), "结果仍未知；保持锁，不重试写操作")
        if owner is not None:
            release_own_lock(lock, ownership["token"], ownership["inode"])
    print(json.dumps({"result": record["result"], "identity": record["identity"], "lock_released": True}, sort_keys=True, ensure_ascii=True))
    return record



def verify_latest(api, state, run_id, attempt):
    pr, base, merge = state
    master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    changed = candidate_policy_changed(api, merge["sha"], master)
    run = latest_run(api, {**pr, "base": {**pr["base"], "sha": base}}, trusted_only=changed)
    check(run is not None and not run.get("_ambiguous") and run["id"] == integer(run_id) and
          run["run_attempt"] == integer(attempt) and run["status"] == "completed" and run["conclusion"] == "success",
          "最新相关权威运行不是请求的成功 run/attempt，旧绿灯不可消费")
    return run


def premerge(api, root, number, reviewed_head, run_id, attempt, consume, command, review=None, outcome=None, execute=None):
    if consume:
        validate_command(command, number, reviewed_head)
    else:
        check(not command, "只读检查不能接受合并命令")
    with merge_lock(root):
        state = snapshot(api, number)
        validate_live(state[0], reviewed_head, consuming=True)
        targets = source_targets(api, reviewed_head)
        check(any(pr["number"] == number for pr in targets), "当前开放目标集合不包含请求的 PR")
        try:
            validation = proof(api, state, run_id, attempt, root)
        except StaleBase:
            current = snapshot(api, number)
            validate_live(current[0], reviewed_head, consuming=True)
            if consume:
                refresh(api, current[0])
            raise ValueError("回执 base 已过期；消费模式请求 ci:refresh，未修改 head、未合并；等待新检查并核对已有审查范围")
        verify_latest(api, state, run_id, attempt)
        metadata = verify_publisher(api, state[0], validation)
        membership = [target_details(pr) for pr in targets]
        declared = metadata.get("targets", [])
        check(metadata.get("membership") == membership and len(declared) == len(targets) and
              {entry["pr"] for entry in declared} == {pr["number"] for pr in targets}, "共享 source 目标集合已变化，旧交集不可消费")
        verified = []
        for entry in declared:
            check(entry.get("result") == "success" and entry.get("verification") is True, "其他目标未明确验证成功")
            target_state = state if entry["pr"] == number else snapshot(api, entry["pr"])
            identity = entry["validation"]["identity"]
            fresh = validation if entry["pr"] == number else proof(api, target_state, receipt_integer(identity["run_id"]), receipt_integer(identity["run_attempt"]), root)
            check(fresh == entry["validation"] and target_state[0]["head"]["sha"] == reviewed_head and
                  entry["base"] == target_state[1] and entry["base_ref"] == target_state[0]["base"]["ref"] and
                  entry["tree"] == fresh["identity"]["tree"], "共享交集中的目标回执已过期或被错配")
            verify_latest(api, target_state, receipt_integer(identity["run_id"]), receipt_integer(identity["run_attempt"]))
            verified.append((target_state, identity))
        current = snapshot(api, number)
        validate_live(current[0], reviewed_head, consuming=True)
        validate_unblocked(current[0])
        if fingerprint(current) != fingerprint(state):
            if consume and current[1] != state[1]:
                refresh(api, current[0])
            raise ValueError("PR/base 已变化，旧回执不可消费，未合并")
        targets = source_targets(api, reviewed_head)
        check([target_details(pr) for pr in targets] == membership and merge_state(*current) == "ready", "目标集合、合并状态已变化或冲突")
        # Recompute each target's latest authority under the same held lock immediately before consumption.
        for target_state, identity in verified:
            final = snapshot(api, target_state[0]["number"])
            validate_live(final[0], reviewed_head, consuming=final[0]["number"] == number)
            if final[0]["number"] == number:
                validate_unblocked(final[0])
            check(fingerprint(final) == fingerprint(target_state), "共享目标在消费前发生变化")
            verify_latest(api, final, receipt_integer(identity["run_id"]), receipt_integer(identity["run_attempt"]))
        check([target_details(pr) for pr in source_targets(api, reviewed_head)] == membership, "消费前共享目标成员变化")
        verify_publisher(api, current[0], validation)
        if consume:
            check(review is not None and outcome is not None, "消费必须提供独立审查和结果回执路径")
            consume_operation(api, root, number, reviewed_head, validation, command, review, outcome, execute)
        else:
            print(json.dumps({"ci_ready": True, **validation}, sort_keys=True))


def resolve_event(api, event, event_name):
    if event_name == "pull_request_target":
        changes = event.get("changes", {})
        if event.get("action") == "edited" and "base" not in changes and ("title" in changes or "body" in changes):
            return []
        label = event.get("label", {}).get("name")
        if event.get("action") == "labeled" and isinstance(label, str) and label and label.casefold() != "ci:refresh":
            return []
        return [integer(event["number"])]
    source = api.get("actions/runs/" + str(integer(event["workflow_run"]["id"])))
    check(source["repository"]["full_name"] == REPO and source["path"] == WORKFLOW, "不是本仓库 Merge 验证运行")
    if source["event"] == "workflow_dispatch":
        match = re.fullmatch(r"Merge validation PR ([1-9][0-9]*) head ([0-9a-f]{40}) base ([0-9a-f]{40})", source["display_title"])
        check(match is not None, "控制器标题身份非法")
        live = api.get(f"pulls/{int(match[1])}")
        if live["state"] != "open" or live["head"]["sha"] != match[2] or live["base"]["sha"] != match[3]:
            return []
        return [int(match[1])]
    check(source["event"] == "pull_request", "不是原生 PR/受信控制器运行")
    candidates = matching_prs(api, source)
    check(len(candidates) <= 256, "同来源 PR 数超过有界矩阵")
    return sorted({integer(pr["number"]) for pr in candidates})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    report = commands.add_parser("report", help="仅可信基准 workflow 使用；写 Checks/派发只读控制器")
    report.add_argument("--pr", type=int, required=True)
    commands.add_parser("resolve", help="只读解析稳定 PR 并发键，不执行候选代码")
    incident = commands.add_parser("reconcile", help="只读远端事故对账；仅终态证明可释放自己的持久锁")
    incident.add_argument("--root", type=Path, default=Path.cwd())
    incident.add_argument("--outcome", type=Path, required=True)
    local = commands.add_parser("premerge", help="默认只读；--consume 执行锁内已授权操作")
    local.add_argument("--root", type=Path, default=Path.cwd())
    local.add_argument("--repo", required=True, choices=[REPO])
    local.add_argument("--pr", type=int, required=True)
    local.add_argument("--reviewed-head", required=True)
    local.add_argument("--run-id", type=int, required=True)
    local.add_argument("--run-attempt", type=int, required=True)
    local.add_argument("--consume", action="store_true")
    local.add_argument("--review", type=Path)
    local.add_argument("--outcome", type=Path)
    local.add_argument("operation", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    api = GitHub()
    if args.command == "reconcile":
        reconcile_incident(api, args.root, args.outcome)
        return
    if args.command == "premerge":
        operation = args.operation[1:] if args.operation[:1] == ["--"] else args.operation
        premerge(api, args.root, args.pr, args.reviewed_head, args.run_id, args.run_attempt, args.consume, operation, args.review, args.outcome)
        return
    check(os.environ.get("GITHUB_REPOSITORY") == REPO and os.environ.get("GITHUB_EVENT_NAME") in ("pull_request_target", "workflow_run"), "只允许生产仓库的可信基准事件发布")
    event = gate.read_json(os.environ["GITHUB_EVENT_PATH"])
    check(event["repository"]["full_name"] == REPO, "事件仓库不匹配")
    trusted_sha = sha(os.environ["MERGE_TRUSTED_WORKFLOW_SHA"])
    master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    check(ancestor(api, trusted_sha, master), "发布/解析代码不属于已发布默认分支；拒绝任意 PR target 代码")
    if args.command == "resolve":
        numbers = resolve_event(api, event, os.environ["GITHUB_EVENT_NAME"])
        by_head = {}
        for number in numbers:
            pr = api.get(f"pulls/{integer(number)}")
            by_head.setdefault(sha(pr["head"]["sha"]), number)
        matrix = {"include": [{"pr": integer(number), "source_sha": head} for head, number in sorted(by_head.items())]}
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write("matrix=" + json.dumps(matrix) + "\ncount=" + str(len(numbers)) + "\n")
        return
    context = dict(os.environ)
    if event.get("action") == "labeled" and event.get("label", {}).get("name", "").casefold() == "ci:refresh":
        context["MERGE_REFRESH_ID"] = os.environ["GITHUB_RUN_ID"]
    reconcile(api, integer(args.pr), Path.cwd(), context)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, UnicodeError, subprocess.CalledProcessError, zipfile.BadZipFile) as error:
        print(f"Merge 发布/消费失败：{error}", file=sys.stderr)
        sys.exit(1)
