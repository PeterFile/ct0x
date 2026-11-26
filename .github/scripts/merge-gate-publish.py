#!/usr/bin/env python3
"""Trusted base-side publisher; local consumption is explicit and stays under the shared mkdir lock."""
import argparse
import base64
from contextlib import contextmanager
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import secrets
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


class PolicyChange(ValueError):
    pass


class StaleBase(ValueError):
    pass


class UnknownMerge(ValueError):
    pass


class UnassignedRun(ValueError):
    pass


def integer(value):
    check(type(value) is int and value > 0, "API 数字身份非法")
    return value


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


def merge_state(pr, base, merge):
    valid = (merge is not None and merge.get("sha") == pr.get("merge_commit_sha") and
             [row.get("sha") for row in merge.get("parents", [])] == [base, pr["head"]["sha"]] and
             bool(re.fullmatch(gate.SHA, merge.get("tree", {}).get("sha", ""))))
    if pr.get("mergeable") is False and not valid and pr.get("mergeable_state") == "dirty":
        return "conflict"
    if pr.get("mergeable") is True and valid and pr["base"]["sha"] == base:
        return "ready"
    return "unknown"


def validate_proof(receipt, run, jobs, pr, master, merge, workflow_id):
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
    check(run["status"] == "completed" and run["conclusion"] == "success", "验证运行没有明确成功")
    check(merge_state(pr, master, merge) == "ready", "没有匹配的双亲测试合并")
    expected = {"repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"], "base": master,
                "sha": merge["sha"], "tree": merge["tree"]["sha"],
                "run_id": str(integer(run["id"])), "run_attempt": str(integer(run["run_attempt"]))}
    check(receipt.get("schema") == 1 and receipt.get("result") == "success" and
          all(receipt.get(key) == value for key, value in expected.items()), "回执不属于此 PR/精确合并/run/attempt")
    check(isinstance(receipt.get("tasks"), list) and 0 < len(receipt["tasks"]) <= 256, "回执任务清单非法")
    expected_jobs = ["Merge plan", "Merge evidence", *["Merge tests / " + task["unit"] for task in receipt["tasks"]]]
    check(len(jobs) == len(expected_jobs) and sorted(row["name"] for row in jobs) == sorted(expected_jobs), "显式工作任务缺失、重复或额外")
    check(all(row.get("status") == "completed" and row.get("conclusion") == "success" and
              row.get("run_id") == run["id"] and row.get("run_attempt") == run["run_attempt"] for row in jobs), "任务取消/跳过/失败或尝试身份不符")
    gate.finalize({**receipt, **expected}, {"plan": {"result": "success"}, "tests": {"result": "success"}}, receipt["evidence"])
    return expected


def decode_artifact(data, digest):
    check(len(data) <= MAX_ARTIFACT and isinstance(digest, str) and
          digest == "sha256:" + hashlib.sha256(data).hexdigest(), "远端 artifact 摘要缺失或不匹配")
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        entries = archive.infolist()
        check(len(entries) == 1 and entries[0].filename == "merge-gate.json" and entries[0].file_size <= MAX_ARTIFACT and not entries[0].is_dir(), "artifact 必须仅含有界 JSON 数据；禁止解压执行")
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
    raise ValueError("PR 持续变化，拒绝发布成功")


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
    if run["event"] != "pull_request" or run["status"] != "completed":
        return False
    jobs = api.pages(f"actions/runs/{integer(run['id'])}/attempts/{integer(run['run_attempt'])}/jobs", "jobs")
    # This excludes metadata-only runs, never treats skipped jobs as proof of success.
    return bool(jobs) and all(row.get("status") == "completed" and row.get("conclusion") == "skipped" for row in jobs)


def latest_run(api, pr, trusted_only=False):
    query = urlencode({"event": "pull_request", "head_sha": pr["head"]["sha"]})
    runs = api.pages("actions/workflows/merge-gate.yml/runs?" + query, "workflow_runs")
    native = [run for run in runs if run["event"] == "pull_request" and run["path"] == WORKFLOW and
              run["head_sha"] == pr["head"]["sha"] and run["head_branch"] == pr["head"]["ref"] and
              run["head_repository"]["id"] == pr["head"]["repo"]["id"] and run["head_repository"]["full_name"] == pr["head"]["repo"]["full_name"]]
    native = [] if trusted_only else [run for run in native if not ignored_run(api, run)]
    query = urlencode({"event": "workflow_dispatch"})
    dispatched = api.pages("actions/workflows/merge-gate.yml/runs?" + query, "workflow_runs")
    dispatched = [run for run in dispatched if run["event"] == "workflow_dispatch" and run["path"] == WORKFLOW and
                  run.get("display_title") == dispatch_title(pr) and run["head_repository"]["full_name"] == REPO and run["head_branch"] == "master"]
    candidates = native + dispatched
    return max(candidates, key=lambda run: integer(run["id"])) if candidates else None


def policy_digest(api, ref):
    digest = hashlib.sha256()
    blobs = {}
    for path in gate.POLICY_PATHS:
        remote = api.get("contents/" + path + "?" + urlencode({"ref": sha(ref)}))
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
            remote = api.get("contents/" + path + "?" + urlencode({"ref": sha(receipt["sha"])}))
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
    check(len(artifacts) == 1 and artifacts[0].get("expired") is False, "唯一回执 artifact 缺失/过期")
    artifact = artifacts[0]
    check(artifact.get("workflow_run", {}).get("id") == run_id and artifact.get("size_in_bytes", MAX_ARTIFACT + 1) <= MAX_ARTIFACT, "artifact 远端运行身份/大小非法")
    receipt = decode_artifact(api.archive(artifact["id"]), artifact.get("digest"))
    if receipt.get("pr") != pr["number"] or receipt.get("source") != source_identity(pr):
        raise UnassignedRun("此原生运行属于同 head 的其他 PR/目标；需要受信精确树验证")
    jobs = api.pages(f"actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
    original = api.get("git/commits/" + sha(receipt["sha"]))
    # Establish the original external proof before deciding that it is merely stale.
    original_pr = {**pr, "base": {**pr["base"], "sha": receipt["base"]}, "merge_commit_sha": receipt["sha"], "mergeable": True}
    identity = validate_proof(receipt, exact, jobs, original_pr, receipt["base"], original, workflow["id"])
    current_master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    trusted_policy(api, receipt, exact, current_master)
    if receipt["base"] != base:
        raise StaleBase("旧回执的真实 base 已过期，必须刷新原生合并检查")
    validate_proof(receipt, exact, jobs, pr, base, merge, workflow["id"])
    return {"identity": identity, "source": receipt["source"], "artifact_id": integer(artifact["id"]),
            "artifact_digest": artifact["digest"], "policy_digest": receipt["policy_digest"],
            "controller_sha": receipt.get("controller_sha", "")}


def external_id(pr):
    return f"merge-gate:v2:{REPO}:source:{pr['head']['sha']}"


def check_sha(pr):
    return sha(pr["head"]["sha"])


def owned(row, pr):
    return (row.get("app", {}).get("id") == APP_ID and row.get("app", {}).get("slug") == "github-actions" and
            row.get("external_id") == external_id(pr) and row.get("head_sha") == check_sha(pr))


def checks(api, pr):
    return [row for row in api.pages("commits/" + check_sha(pr) + "/check-runs?filter=all", "check_runs") if row["name"] == "Merge gate"]


def log_check(row, pr, metadata):
    identity = metadata.get("validation", {}).get("identity", {})
    publisher = metadata.get("publisher", {})
    record = {"event": "merge_check_published", "repo": REPO, "pr": pr["number"], "head": pr["head"]["sha"],
              "base": identity.get("base", pr["base"]["sha"]), "validated_sha": identity.get("sha"), "tree": identity.get("tree"),
              "check_id": row["id"], "target_sha": row.get("head_sha", check_sha(pr)), "name": row.get("name", "Merge gate"),
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
    for duplicate in existing[1:]:
        check(guard(), "发布前 PR/运行已变化")
        api.write("PATCH", "check-runs/" + str(duplicate["id"]), {"name": "Merge reporter superseded", "status": "completed", "conclusion": "neutral"})
    output = {"title": {"success": "合并验证通过", "failure": "合并已阻塞", "pending": "等待合并验证"}[conclusion],
              "summary": json.dumps(metadata, sort_keys=True, ensure_ascii=True)}
    data = {"name": "Merge gate", "external_id": external_id(pr), "status": "in_progress" if conclusion == "pending" else "completed", "output": output}
    if conclusion != "pending":
        data["conclusion"] = conclusion
    identity = metadata.get("validation", {}).get("identity", {})
    if identity:
        data["details_url"] = f"https://github.com/{REPO}/actions/runs/{identity['run_id']}/attempts/{identity['run_attempt']}"
    check(guard(), "发布前 PR/运行已变化")
    if existing:
        row = existing[0]
        if not all(row.get(key) == value for key, value in data.items()):
            row = api.write("PATCH", "check-runs/" + str(integer(row["id"])), data)
            log_check(row, pr, metadata)
    else:
        row = api.write("POST", "check-runs", {**data, "head_sha": check_sha(pr)})
        log_check(row, pr, metadata)
    if not guard():
        stale = {**metadata, "result": "pending", "reason": "发布期间身份变化，旧成功不可消费"}
        api.write("PATCH", "check-runs/" + str(integer(row["id"])), {"status": "in_progress", "output": {"title": "等待并发变化后的验证", "summary": json.dumps(stale, ensure_ascii=True)}})
        raise ValueError("发布期间身份变化；已撤回成功")
    return row


def dispatch_inputs(pr, base, merge, policy_sha):
    return {"pr": str(pr["number"]), "head": pr["head"]["sha"], "base": base, "sha": merge["sha"],
            "tree": merge["tree"]["sha"], "policy_sha": policy_sha, "source_repo_id": str(pr["head"]["repo"]["id"]),
            "source_repo": pr["head"]["repo"]["full_name"], "source_ref": pr["head"]["ref"], "base_ref": pr["base"]["ref"]}


def candidate_policy_changed(api, merge_sha, policy_sha):
    _, blobs = policy_digest(api, policy_sha)
    for path, blob in blobs.items():
        try:
            candidate = api.get("contents/" + path + "?" + urlencode({"ref": sha(merge_sha)}))
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


def reconcile(api, number, root, context):
    requested = api.get(f"pulls/{integer(number)}")
    targets = source_targets(api, requested["head"]["sha"])
    if len(targets) == 1 and targets[0]["head"].get("repo") is not None:
        return reconcile_single(api, targets[0]["number"], root, context)
    details = [target_details(pr) for pr in targets]
    metadata = {"schema": 1, "repo": REPO, "pr": number, "head": requested["head"]["sha"], "result": "pending",
                "reason_code": "shared_head_multiple_targets" if len(targets) > 1 else "source_identity_unknown" if targets else "source_head_no_open_targets", "targets": details,
                "publisher": {"workflow": PUBLISHER, "run_id": context["GITHUB_RUN_ID"], "run_attempt": context["GITHUB_RUN_ATTEMPT"],
                              "workflow_sha": context.get("MERGE_TRUSTED_WORKFLOW_SHA")}}
    if not targets and not checks(api, requested):
        return None
    return publish_check(api, requested, "pending", metadata,
                         guard=lambda: [target_details(pr) for pr in source_targets(api, requested["head"]["sha"])] == details)


def reconcile_single(api, number, root, context):
    state = stable_snapshot(api, number)
    pr, base, merge = state
    status = merge_state(pr, base, merge)
    # Confirm the trusted policy/tree before inspecting any candidate protocol or jobs.
    policy_sha = sha(api.get("git/ref/heads/master")["object"]["sha"]) if status == "ready" else None
    policy_changed = status == "ready" and candidate_policy_changed(api, merge["sha"], policy_sha)
    source = {"head_repository": pr["head"]["repo"], "head_branch": pr["head"]["ref"], "head_sha": pr["head"]["sha"]}
    multi_target = len(matching_prs(api, source)) > 1 if status == "ready" else False
    trusted_only = policy_changed or multi_target
    run = latest_run(api, pr, trusted_only)
    metadata = {"schema": 1, "repo": REPO, "pr": number, "head": pr["head"]["sha"], "targets": [target_details(pr)],
                "reason_code": "single_target_validation", "publisher": {"workflow": PUBLISHER, "run_id": context["GITHUB_RUN_ID"], "run_attempt": context["GITHUB_RUN_ATTEMPT"],
                              "workflow_sha": context.get("MERGE_TRUSTED_WORKFLOW_SHA")}}
    conclusion, dispatch = "pending", None
    metadata["reason"] = "等待原生运行或 GitHub 计算测试合并"
    previous = checks(api, pr)
    previous_metadata = [gate.verifier.decode(row["output"]["summary"]) for row in previous if owned(row, pr)]
    if trusted_only and (run is None or run["event"] != "workflow_dispatch"):
        dispatch = dispatch_inputs(pr, base, merge, policy_sha)
        metadata["dispatch_requested"] = dispatch
        metadata["dispatch_refresh_id"] = context.get("MERGE_REFRESH_ID")
        metadata["reason"] = "策略变更由默认分支只读控制器验证；不解析候选回执协议来决定派发"
    elif status == "conflict":
        conclusion, metadata["reason"] = "failure", "确认存在合并冲突；原生 pull_request 不会创建测试合并运行"
    elif status == "ready" and run is not None and run["status"] == "completed":
        try:
            metadata["validation"] = proof(api, state, run["id"], run["run_attempt"], root)
            conclusion = "success"
            metadata.pop("reason")
        except (PolicyChange, UnassignedRun):
            policy_sha = sha(api.get("git/ref/heads/master")["object"]["sha"])
            dispatch = dispatch_inputs(pr, base, merge, policy_sha)
            metadata["dispatch_requested"] = dispatch
            metadata["reason"] = "策略变更由默认分支只读控制器验证；候选不能自证策略执行"
        except StaleBase as error:
            metadata["reason"] = str(error)
        except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError, zipfile.BadZipFile) as error:
            conclusion, metadata["reason"] = "failure", "终态验证证据失败：" + str(error)
    metadata["result"] = conclusion

    def guard():
        try:
            targets = source_targets(api, pr["head"]["sha"])
            if len(targets) != 1 or targets[0]["number"] != number or fingerprint(snapshot(api, number)) != fingerprint(state):
                return False
            if policy_sha is not None and api.get("git/ref/heads/master")["object"]["sha"] != policy_sha:
                return False
            current = latest_run(api, pr, trusted_only)
            fields = ("id", "run_attempt", "status", "conclusion")
            return current is None and run is None or current is not None and run is not None and all(current[f] == run[f] for f in fields)
        except (ValueError, KeyError, TypeError, subprocess.CalledProcessError):
            return False

    requested = any(row.get("dispatch_requested") == dispatch for row in previous_metadata) if dispatch else False
    # Keep an unresolved dispatch intent until an accepted run is observed. An intent is not success.
    if not dispatch and conclusion == "pending":
        for row in previous_metadata:
            intent = row.get("dispatch_requested")
            if intent and intent.get("head") == pr["head"]["sha"] and intent.get("base") == base:
                metadata["dispatch_requested"] = intent
    if requested and dispatch:
        # An explicit fresh PR action permits retry only after the complete dispatch list was reconciled.
        refresh_id = context.get("MERGE_REFRESH_ID")
        previous_refreshes = [row.get("dispatch_refresh_id") for row in previous_metadata if row.get("dispatch_requested") == dispatch]
        requested = refresh_id is None or refresh_id in previous_refreshes
        if refresh_id is None and previous_refreshes:
            metadata["dispatch_refresh_id"] = previous_refreshes[0]
    result = publish_check(api, pr, conclusion, metadata, guard)
    if dispatch and not requested:
        check(guard(), "派发前 PR/master 已变化")
        # Unknown POST outcome is not retried; workflow_run events reconcile an accepted dispatch.
        api.write("POST", "actions/workflows/merge-gate.yml/dispatches", {"ref": "master", "inputs": dispatch})
    return result


def shared_lock_path(root):
    common = Path(gate.git(root, "rev-parse", "--path-format=absolute", "--git-common-dir"))
    check(common.name == ".git", "只支持共享 .git 的普通仓库/工作树锁")
    return common.parent / ".local/merge.lock"


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


def release_own_lock(lock, token, inode):
    owner = lock / "merge-gate-owner"
    check(lock.is_dir() and not lock.is_symlink() and lock.stat().st_ino == inode and owner.is_file() and
          not owner.is_symlink() and gate.read_json(owner)["token"] == token, "锁身份变化；绝不释放其他写入者的锁")
    check({p.name for p in lock.iterdir()} == {owner.name}, "锁中出现非本工具文件，保留锁并阻塞")
    owner.unlink()
    lock.rmdir()


@contextmanager
def merge_lock(root):
    lock = shared_lock_path(root)
    lock.parent.mkdir(exist_ok=True)
    check(not lock.parent.is_symlink(), "共享锁目录不能是符号链接")
    lock.mkdir()
    inode, token, keep = lock.stat().st_ino, secrets.token_hex(24), False
    durable_json(lock / "merge-gate-owner", {"schema": 1, "token": token, "inode": inode, "incident": None}, exclusive=True)
    try:
        yield lock
    except UnknownMerge:
        keep = True  # Persist across process exits; only external terminal proof can release it.
        raise
    finally:
        if not keep:
            release_own_lock(lock, token, inode)


def validate_command(command, number, reviewed_head):
    expected = ["gh", "pr", "merge", str(integer(number)), "--repo", REPO, "--merge", "--match-head-commit", sha(reviewed_head)]
    check(command == expected, "仅接受明确授权的指定 PR/head 普通 merge 命令，禁止 shell/admin/额外参数")


def verify_publisher(api, pr, validation):
    rows = checks(api, pr)
    check(len(rows) == 1 and owned(rows[0], pr) and rows[0].get("status") == "completed" and rows[0].get("conclusion") == "success", "唯一可信 Merge gate 未明确成功")
    metadata = gate.verifier.decode(rows[0]["output"]["summary"])
    check(metadata.get("repo") == REPO and metadata.get("pr") == pr["number"] and metadata.get("head") == pr["head"]["sha"] and
          metadata.get("result") == "success" and metadata.get("validation") == validation, "远端已发布结果不属于此精确回执")
    publisher = metadata["publisher"]
    check(publisher["workflow"] == PUBLISHER, "发布者路径不符")
    for field in ("run_id", "run_attempt"):
        check(re.fullmatch(r"[1-9][0-9]{0,63}", publisher[field]) is not None, "发布者运行身份非法")
    run = api.get(f"actions/runs/{publisher['run_id']}")
    exact = api.get(f"actions/runs/{publisher['run_id']}/attempts/{publisher['run_attempt']}")
    workflow = api.get("actions/workflows/merge-conflict.yml")
    check(run["run_attempt"] == int(publisher["run_attempt"]) and exact["id"] == int(publisher["run_id"]) and
          exact["run_attempt"] == int(publisher["run_attempt"]) and exact["path"] == PUBLISHER and exact["workflow_id"] == workflow["id"] and
          workflow["path"] == PUBLISHER and exact["repository"]["full_name"] == REPO and exact["event"] in ("pull_request_target", "workflow_run") and
          exact["status"] == "completed" and exact["conclusion"] == "success", "发布器远端 provenance 不完整或尚未完成")
    master = sha(api.get("git/ref/heads/master")["object"]["sha"])
    check(ancestor(api, sha(exact["head_sha"]), master) and
          (publisher.get("workflow_sha") is None or publisher["workflow_sha"] == exact["head_sha"]), "发布器不是已发布默认分支的工作流代码")


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
    lock = shared_lock_path(root)
    owner = gate.read_json(lock / "merge-gate-owner")
    record = {"schema": 1, "result": "unknown", "submitted": False, "identity": identity, "source": validation["source"],
              "reviewer": approval["reviewer"], "command_returncode": None,
              "lock": {"path": str(lock), "token": owner["token"], "inode": owner["inode"]}}
    durable_json(outcome, record, exclusive=True)
    owner["incident"] = str(Path(outcome).resolve())
    durable_json(lock / "merge-gate-owner", owner)
    try:
        record["submitted"] = True
        durable_json(outcome, record)
        try:
            record["command_returncode"] = subprocess.run(command, cwd=root, capture_output=True).returncode if execute is None else execute(command, root)
        except (FileNotFoundError, PermissionError):
            record.update(result="not_submitted", submitted=False)
            raise ValueError("合并进程未启动，未发送远端操作")
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
    if lock.exists():
        owner = gate.read_json(lock / "merge-gate-owner")
        check(owner["token"] == ownership["token"] and owner["inode"] == ownership["inode"] and
              owner["incident"] == str(Path(outcome).resolve()), "事故回执不能释放其他写入者的锁")
    record.update(remote_outcome(api, record))
    durable_json(outcome, record)
    check(record["result"] in ("merged", "not_merged"), "结果仍未知；保持锁，不重试写操作")
    if lock.exists():
        release_own_lock(lock, ownership["token"], ownership["inode"])
    print(json.dumps({"result": record["result"], "identity": record["identity"], "lock_released": True}, sort_keys=True, ensure_ascii=True))
    return record



def premerge(api, root, number, reviewed_head, run_id, attempt, consume, command, review=None, outcome=None, execute=None):
    if consume:
        validate_command(command, number, reviewed_head)
    else:
        check(not command, "只读检查不能接受合并命令")
    with merge_lock(root):
        state = snapshot(api, number)
        validate_live(state[0], reviewed_head, consuming=True)
        targets = source_targets(api, reviewed_head)
        check(len(targets) == 1 and targets[0]["number"] == number, "同 source head 存在其他开放 PR/目标，禁止歧义消费")
        try:
            validation = proof(api, state, run_id, attempt, root)
        except StaleBase:
            current = snapshot(api, number)
            validate_live(current[0], reviewed_head, consuming=True)
            if consume:
                refresh(api, current[0])
            raise ValueError("回执 base 已过期；消费模式请求 ci:refresh，未修改 head、未合并；等待新检查并核对已有审查范围")
        verify_publisher(api, state[0], validation)
        current = snapshot(api, number)
        validate_live(current[0], reviewed_head, consuming=True)
        if fingerprint(current) != fingerprint(state):
            if consume and current[1] != state[1]:
                refresh(api, current[0])
            raise ValueError("PR/base 已变化，旧回执不可消费，未合并")
        targets = source_targets(api, reviewed_head)
        check(len(targets) == 1 and targets[0]["number"] == number and merge_state(*current) == "ready", "目标歧义、合并状态未知或冲突")
        if consume:
            check(review is not None and outcome is not None, "消费必须提供独立审查和结果回执路径")
            consume_operation(api, root, number, reviewed_head, validation, command, review, outcome, execute)
        else:
            print(json.dumps({"ci_ready": True, **validation}, sort_keys=True))


def resolve_event(api, event, event_name):
    if event_name == "pull_request_target":
        if event.get("action") == "edited" and event.get("changes", {}).get("base", {}).get("ref", {}).get("from") is None:
            return []
        if event.get("action") == "labeled" and event.get("label", {}).get("name", "").casefold() != "ci:refresh":
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
