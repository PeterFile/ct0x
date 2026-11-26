#!/usr/bin/env python3
"""Read-only periodic Full admission, attempt-bound evidence and receipt publication."""
import argparse
from datetime import datetime
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import zipfile


SHA_RE = r"[0-9a-f]{40}"
WORKFLOWS = {".github/workflows/ci.yml": ("legacy", "Full CI"),
             ".github/workflows/periodic-full.yml": ("periodic", "Periodic Full evidence"),
             ".github/workflows/release-candidate.yml": ("release", "Release Full evidence")}
MAX_API, MAX_BYTES = 240, 32 * 1024 * 1024


def check(condition, message):
    if not condition:
        raise ValueError(message)


def positive(value):
    return type(value) is int and value > 0


def sha(value):
    return isinstance(value, str) and re.fullmatch(SHA_RE, value) is not None


def strict_json(text):
    def pairs(rows):
        result = {}
        for key, value in rows:
            check(key not in result, "JSON 包含重复字段")
            result[key] = value
        return result
    return json.loads(text, object_pairs_hook=pairs,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError("JSON 包含非有限值")))


def load(name, root=None):
    path = (Path(root) / ".github/scripts" if root else Path(__file__).parent) / (name + ".py")
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def digest(data):
    return hashlib.sha256(data).hexdigest()


class API:
    """Only GET; retain response bytes and endpoint/digest, never headers or credentials."""
    def __init__(self, output):
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=True)
        self.count = 0
        self.cache = {}

    def get(self, endpoint, raw=False):
        check(isinstance(endpoint, str) and endpoint.startswith("repos/") and ".." not in endpoint and
              not re.search(r"[\s#]", endpoint), "API 路径非法")
        if endpoint not in self.cache:
            check(self.count < MAX_API, "只读取证达到 API 上限")
            self.count += 1
            env = {key: os.environ[key] for key in ("PATH", "HOME", "GH_TOKEN") if key in os.environ}
            env.update(GH_PROMPT_DISABLED="1", GH_NO_UPDATE_NOTIFIER="1", GH_TELEMETRY="0")
            check(bool(env.get("GH_TOKEN")), "缺少只读 API 凭据")
            for attempt in range(2):
                with tempfile_file() as output:
                    process = subprocess.run(["gh", "api", "--method", "GET", endpoint], env=env,
                                             stdout=output, stderr=subprocess.PIPE, timeout=70)
                    output.seek(0)
                    data = output.read(MAX_BYTES + 1)
                if process.returncode == 0:
                    break
                if attempt == 0 and re.search(rb"HTTP (?:429|5\d\d)", process.stderr):
                    time.sleep(1)
                    continue
                raise ValueError("只读 API 证据不可用：" + endpoint)
            check(len(data) <= MAX_BYTES, "API 证据超过大小上限")
            filename = f"{self.count:03d}-{digest(endpoint.encode())[:16]}.bin"
            (self.output / filename).write_bytes(data)
            with (self.output / "index.jsonl").open("a") as index:
                index.write(json.dumps({"endpoint": endpoint, "file": filename, "sha256": digest(data)}) + "\n")
            self.cache[endpoint] = data
        data = self.cache[endpoint]
        return data if raw else strict_json(data)


def tempfile_file():
    import tempfile
    return tempfile.TemporaryFile()


def pages(api, path, key, complete=True):
    result = []
    for page in range(1, 3):
        data = api.get(path + f"?per_page=100&page={page}")
        check(isinstance(data, dict) and type(data.get("total_count")) is int and data["total_count"] >= 0 and
              isinstance(data.get(key), list), "分页证据结构不完整")
        result.extend(data[key])
        if len(result) >= data["total_count"]:
            check(len(result) == data["total_count"], "分页总数不符")
            return result
    check(not complete, "分页超过取证上限")
    return result


class Evidence:
    def __init__(self, api, repo, ancestor):
        check(isinstance(repo, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo), "仓库身份非法")
        self.api, self.repo, self.ancestor = api, repo, ancestor
        self.prefix = "repos/" + repo

    def commit(self, value):
        check(sha(value), "提交 SHA 非法")
        commit = self.api.get(self.prefix + "/commits/" + value)
        check(commit.get("sha") == value and sha(commit.get("commit", {}).get("tree", {}).get("sha")), "API 提交/tree 身份不符")
        parents = commit.get("parents")
        check(isinstance(parents, list) and all(sha(row.get("sha")) for row in parents), "API 父提交缺失")
        return {"sha": value, "tree": commit["commit"]["tree"]["sha"], "head": value,
                "base": parents[0]["sha"] if parents else value}

    def run(self, run_id, attempt, trusted_head=True):
        check(positive(run_id) and positive(attempt), "run/attempt 类型非法")
        repo = self.api.get(self.prefix)
        check(repo.get("full_name") == self.repo and repo.get("fork") is False and positive(repo.get("id")) and
              repo.get("default_branch") == "master", "不是可信的非 fork master 仓库")
        run = self.api.get(f"{self.prefix}/actions/runs/{run_id}/attempts/{attempt}")
        check(type(run.get("id")) is int and run["id"] == run_id and type(run.get("run_attempt")) is int and
              run["run_attempt"] == attempt and sha(run.get("head_sha")), "API run/attempt 身份不符")
        for key in ("repository", "head_repository"):
            source = run.get(key, {})
            check(source.get("full_name") == self.repo and type(source.get("id")) is int and source["id"] == repo["id"] and
                  source.get("fork") is False, "运行来自其他仓库或 fork")
        check(run.get("path") in WORKFLOWS and positive(run.get("workflow_id")), "未知工作流路径")
        workflow = self.api.get(f"{self.prefix}/actions/workflows/{run['workflow_id']}")
        check(workflow.get("path") == run["path"] and workflow.get("id") == run["workflow_id"], "工作流 API 身份不符")
        if trusted_head:
            check(self.ancestor(run["head_sha"]), "触发源不在可信 master 历史中")
        return run

    def source(self, run, receipt):
        kind = WORKFLOWS[run["path"]][0]
        event, branch = run.get("event"), run.get("head_branch")
        if kind == "legacy":
            check(event == "push" and branch == "master" and receipt["sha"] == run["head_sha"], "不是可信 master push 证明")
        elif kind == "periodic":
            check(event in ("schedule", "workflow_dispatch", "push") and branch == "master", "不是 master 控制的周期证明")
        elif event == "workflow_dispatch":
            check(branch == "master", "候选 Full dispatch 不来自 master")
        else:
            check(event == "push" and isinstance(branch, str) and re.fullmatch(r"rc/[A-Za-z0-9_.+-]+", branch), "候选 Full 来源未知")
            # Tag executions require additional API-bound master workflow provenance; otherwise rerun.
            ref = self.api.get(f"{self.prefix}/git/ref/tags/{branch}")
            obj = ref.get("object", {})
            for _ in range(3):
                if obj.get("type") != "tag":
                    break
                check(sha(obj.get("sha")), "候选 tag 对象非法")
                obj = self.api.get(f"{self.prefix}/git/tags/{obj['sha']}").get("object", {})
            check(ref.get("ref") == "refs/tags/" + branch and obj.get("type") == "commit" and
                  obj.get("sha") == run["head_sha"], "候选 tag 未精确固定 master 祖先")
            expected = self.repo + "/.github/workflows/release-candidate.yml@refs/heads/master"
            source_sha = receipt.get("workflow_sha")
            check(receipt.get("workflow_ref") == expected and sha(source_sha) and self.ancestor(source_sha) and
                  any(row.get("path") == expected and row.get("sha") == source_sha
                      for row in run.get("referenced_workflows", [])), "缺少 API 绑定的 master 工作流源")
        check(self.ancestor(receipt["sha"]), "被测提交不是可信 master 祖先")

    def jobs(self, run):
        rows = pages(self.api, f"{self.prefix}/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs", "jobs")
        ids = set()
        for job in rows:
            check(positive(job.get("id")) and job["id"] not in ids and type(job.get("run_id")) is int and
                  job["run_id"] == run["id"] and type(job.get("run_attempt")) is int and
                  job["run_attempt"] == run["run_attempt"] and job.get("head_sha") == run["head_sha"], "作业不是同一次 run/attempt")
            check(job.get("html_url") == f"https://github.com/{self.repo}/actions/runs/{run['id']}/job/{job['id']}", "作业链接身份不符")
            ids.add(job["id"])
        return rows

    @staticmethod
    def successful_job(jobs, name, suffix=False):
        rows = [job for job in jobs if (job.get("name", "").rsplit(" / ", 1)[-1] if suffix else job.get("name")) == name]
        check(len(rows) == 1 and rows[0].get("status") == "completed" and rows[0].get("conclusion") == "success",
              "缺少同 attempt 的成功作业：" + name)
        return rows[0]

    def canonical(self, job):
        text = self.api.get(f"{self.prefix}/actions/jobs/{job['id']}/logs", raw=True).decode("utf-8")
        found = []
        for raw in text.splitlines():
            line = re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", raw)
            if line.startswith("{"):
                value = strict_json(line)
                if isinstance(value, dict) and ("verified" in value or "kind" in value):
                    found.append(value)
        check(len(found) == 1, "成功 producer 日志缺少唯一规范 JSON")
        return found[0]

    def artifact(self, run, kind):
        artifacts = pages(self.api, f"{self.prefix}/actions/runs/{run['id']}/artifacts", "artifacts")
        name = f"{kind}-{run['id']}-{run['run_attempt']}"
        rows = [row for row in artifacts if row.get("name") == name]
        check(len(rows) == 1, "attempt 限定的 receipt artifact 缺失或重复")
        item = rows[0]
        check(item.get("expired") is False and positive(item.get("id")), "receipt artifact 已过期")
        source = item.get("workflow_run", {})
        repo_id = run["repository"]["id"]
        for key, value in {"id": run["id"], "repository_id": repo_id, "head_repository_id": repo_id,
                           "head_sha": run["head_sha"], "head_branch": run["head_branch"]}.items():
            check(type(source.get(key)) is type(value) and source[key] == value, "artifact 与 API run 身份不符")
        created, start, end = (datetime.fromisoformat(value.replace("Z", "+00:00")) for value in
                               (item["created_at"], run["run_started_at"], run["updated_at"]))
        check(start <= created <= end, "artifact 不在此 attempt 时间内")
        data = self.api.get(f"{self.prefix}/actions/artifacts/{item['id']}/zip", raw=True)
        check(item.get("digest") == "sha256:" + digest(data), "artifact API 摘要不符")
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            names = archive.namelist()
            check(len(names) == len(set(names)) and names.count("receipt.json") == 1, "ZIP 根 receipt.json 缺失或重复")
            info = archive.getinfo("receipt.json")
            check(info.file_size <= 2 * 1024 * 1024 and info.external_attr >> 16 & 0o170000 != 0o120000, "receipt 过大或为符号链接")
            return strict_json(archive.read(info))

    def verify(self, run_id, attempt):
        run = self.run(run_id, attempt)
        check(run.get("status") == "completed", "运行尚未完成")
        jobs = self.jobs(run)
        kind, producer = WORKFLOWS[run["path"]]
        job = self.successful_job(jobs, producer)
        self.successful_job(jobs, "Full suite evidence", suffix=True)
        receipt = self.artifact(run, "release-full") if kind == "release" else self.canonical(job)
        check(isinstance(receipt, dict), "receipt 不是对象")
        check(receipt.get("verified") is True and sha(receipt.get("sha")) and sha(receipt.get("tree")), "receipt 未证明成功身份")
        target = self.commit(receipt["sha"])
        check(receipt["tree"] == target["tree"], "receipt tree 与 API commit 不符")
        if kind == "legacy":
            self.successful_job(jobs, "CI preflight and feedback")
            check(set(receipt) == {"verified", "run_id", "sha", "tree", "head", "base"} and
                  type(receipt.get("run_id")) is str and receipt["run_id"] == str(run_id), "legacy 外层证明形状不符")
        else:
            expected_kind = "periodic-full" if kind == "periodic" else "release-full"
            check(type(receipt.get("schema")) is int and receipt["schema"] == 1 and receipt.get("kind") == expected_kind and
                  receipt.get("repo") == self.repo and type(receipt.get("run_id")) is int and receipt["run_id"] == run_id and
                  type(receipt.get("run_attempt")) is int and receipt["run_attempt"] == attempt and receipt.get("status") == "success",
                  "receipt schema/repo/run/attempt/status 不符")
            if kind == "periodic":
                check(self.artifact(run, expected_kind) == receipt, "artifact 与成功 producer 规范日志不符")
            if kind == "periodic":
                self.successful_job(jobs, "Periodic Full preflight")
                check(receipt.get("components") == {key: "success" for key in ("resolve", "preflight", "full")} and
                      receipt.get("checkout") == {key: target[key] for key in ("sha", "tree")}, "缺少完整预检/checkout 证明")
                expected = {**target, "run_id": run_id, "run_attempt": attempt, "repo": self.repo, "verified": True}
                check(receipt.get("full") == expected and type(receipt["full"].get("run_attempt")) is int and
                      type(receipt["full"].get("run_id")) is int and receipt["full"].get("verified") is True, "Full 聚合身份不符")
            else:
                check(isinstance(receipt.get("artifact_sha256"), str) and re.fullmatch(r"[0-9a-f]{64}", receipt["artifact_sha256"]),
                      "候选产物摘要缺失")
        for key in ("head", "base"):
            check((key not in receipt and kind == "release") or receipt.get(key) == target[key], "receipt head/base 与 API commit 不符")
        self.source(run, receipt)
        return {**target, "repo": self.repo, "run_id": run_id, "run_attempt": attempt, "verified": True,
                "workflow": run["path"], "event": run["event"], "producer_job_id": job["id"], "url": job["html_url"]}

    def search(self, chain):
        found, reasons, examined = {}, [], 0
        for path in WORKFLOWS:
            try:
                runs = pages(self.api, f"{self.prefix}/actions/workflows/{path.rsplit('/', 1)[-1]}/runs", "workflow_runs", complete=False)
                for run in runs:
                    if not positive(run.get("id")) or not positive(run.get("run_attempt")) or run.get("head_branch") != "master" and path != ".github/workflows/release-candidate.yml":
                        continue
                    if path == ".github/workflows/ci.yml" and run.get("head_sha") not in chain:
                        continue
                    if run.get("id") == int(os.environ.get("GITHUB_RUN_ID", "0")):
                        continue
                    for attempt in range(run["run_attempt"], max(0, run["run_attempt"] - 3), -1):
                        examined += 1
                        check(examined <= 40, "历史证明候选达到上限")
                        try:
                            proof = self.verify(run["id"], attempt)
                            if proof["sha"] in chain:
                                found.setdefault(proof["sha"], proof)
                        except (ValueError, KeyError, OSError, zipfile.BadZipFile) as error:
                            reasons.append(str(error))
            except (ValueError, KeyError, OSError) as error:
                reasons.append(str(error))
        return found, reasons


def receipt(identity, needs, decision, prior):
    check(isinstance(needs, dict) and set(needs) == {"resolve", "preflight", "full"} and
          all(isinstance(value, dict) for value in needs.values()), "periodic needs 缺失或多余")
    check(decision in ("run", "skip"), "periodic admission 决策非法")
    check(positive(identity.get("run_id")) and positive(identity.get("run_attempt")) and
          all(sha(identity.get(key)) for key in ("sha", "tree", "head", "base")), "periodic 身份类型非法")
    components = {key: value.get("result") for key, value in needs.items()}
    check(all(value in ("success", "failure", "cancelled", "skipped") for value in components.values()), "组件状态非法")
    result = {"schema": 1, "kind": "periodic-full", **identity, "components": components, "verified": False,
              "status": "failure", "checkout": {}, "full": {}, "ancestor": prior,
              "attribution": {"status": "unknown", "good_sha": prior["sha"] if prior else None,
                              "bad_sha": identity["sha"], "commit": None, "reason": "完整套件未成功，等待诊断"}}
    if decision == "skip":
        check(prior and prior.get("verified") is True and prior["sha"] == identity["sha"] and prior["tree"] == identity["tree"] and
              components == {"resolve": "success", "preflight": "skipped", "full": "skipped"}, "没有同 SHA 的可信成功证明，不能跳过")
        result.update(status="skipped", skip_proof=prior)
        result["attribution"]["reason"] = "同 SHA 已有可信完整成功证明"
    elif components == {key: "success" for key in needs}:
        preflight = needs["preflight"].get("outputs", {})
        check(all(preflight.get(key) == identity[key] for key in ("sha", "tree")), "预检 checkout 身份不符")
        full = needs["full"].get("outputs", {})
        check(full.get("verified") == "true" and all(full.get(key) == str(identity[key]) for key in
                                                     ("run_id", "sha", "tree", "head", "base")), "Full 聚合与目标身份不符")
        result.update(verified=True, status="success", checkout={key: identity[key] for key in ("sha", "tree")},
                      full={**identity, "verified": True})
        result["attribution"]["reason"] = "完整套件成功"
    return result


def attribute(localization, pulls, repo):
    result = {key: value for key, value in localization.items() if key != "pr"}
    result["status"] = "unknown"
    if localization.get("localization") != "reproduced" or not sha(localization.get("commit")):
        return result
    rows = [row for row in pulls if positive(row.get("number")) and isinstance(row.get("merged_at"), str) and
            row.get("merge_commit_sha") == localization["commit"] and row.get("base", {}).get("ref") == "master" and
            row.get("base", {}).get("repo", {}).get("full_name") == repo]
    if len(rows) == 1:
        result.update(status="confirmed", pr=rows[0]["number"], reason="同名测试和签名已重复复现，并核验精确 master 合并 PR")
    else:
        result["reason"] = "精确合并 PR 缺失或不唯一"
    return result


def output(values):
    with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
        for key, value in values.items():
            stream.write(f"{key}={json.dumps(value, separators=(',', ':')) if isinstance(value, (dict, bool)) else value}\n")


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=True, sort_keys=True) + "\n")


def admit(args, api, evidence):
    replay = load("periodic-replay")
    ref = strict_json(args.ref.read_bytes())
    target_sha = ref.get("object", {}).get("sha")
    check(ref.get("ref") == "refs/heads/master" and ref.get("object", {}).get("type") == "commit" and sha(target_sha), "master API ref 非法")
    target = evidence.commit(target_sha)
    check(replay.git(args.root, "rev-parse", "HEAD") == target_sha and
          replay.git(args.root, "rev-parse", "HEAD^{tree}") == target["tree"], "控制代码 checkout 未固定 API SHA/tree")
    # The static local reusable-workflow reference follows the trigger. Verify its bytes before admitting it.
    event_sha = os.environ["GITHUB_SHA"]
    check(sha(event_sha), "事件 SHA 非法")
    for path in (".github/workflows/ci-full.yml",):
        left = api.get(f"{evidence.prefix}/contents/{path}?ref={event_sha}")
        right = api.get(f"{evidence.prefix}/contents/{path}?ref={target_sha}")
        check(sha(left.get("sha")) and left.get("sha") == right.get("sha"), "静态 Full 工作流与可信 master 源不同")
    chain = replay.git(args.root, "rev-list", "--first-parent", "--max-count=33", target_sha).splitlines()
    proofs, reasons = evidence.search(chain)
    prior = next((proofs[value] for value in chain[1:] if value in proofs), None)
    skip = proofs.get(target_sha)
    decision = "skip" if skip and os.environ.get("PERIODIC_FORCE", "false") != "true" else "run"
    identity = {**target, "repo": evidence.repo, "run_id": int(os.environ["GITHUB_RUN_ID"]),
                "run_attempt": int(os.environ["GITHUB_RUN_ATTEMPT"])}
    check(positive(identity["run_id"]) and positive(identity["run_attempt"]), "当前 run/attempt 非法")
    current = evidence.run(identity["run_id"], identity["run_attempt"], trusted_head=False)
    check(current["path"] == ".github/workflows/periodic-full.yml" and current["event"] in ("schedule", "workflow_dispatch", "push"),
          "当前运行不是周期工作流")
    plan = {"identity": identity, "decision": decision, "ancestor": prior, "skip_proof": skip,
            "lookup_status": "proven" if skip else "unknown", "lookup_reasons": reasons}
    write_json(args.output / "plan.json", plan)
    selected = {"mode": "full", **target, "matrix": load("ci-plan", args.root).shard_matrix()}
    write_json(args.output / "full-plan.json", selected)
    output({**identity, "decision": decision, "matrix": selected["matrix"]})


def collect_failures(evidence, run, result):
    replay = load("periodic-replay")
    candidates, failed, blocking = {}, [], []
    unsupported = False
    for job in evidence.jobs(run):
        if job.get("conclusion") != "failure":
            continue
        info = {key: job[key] for key in ("id", "name", "html_url")}
        steps = [step for step in job.get("steps", []) if step.get("conclusion") == "failure"]
        first = min(steps, key=lambda step: step.get("number", 999), default={})
        info["first_failed_step"] = first
        blocking.append({**info, "started_at": first.get("started_at", job.get("started_at", ""))})
        name = job["name"].rsplit(" / ", 1)[-1]
        failed.append(info)
        if name in ("Full suite evidence", "Periodic Full evidence"):
            continue
        try:
            data = evidence.api.get(f"{evidence.prefix}/actions/jobs/{job['id']}/logs", raw=True)
            info.update(log_status="available", log_sha256=digest(data))
            parsed = replay.failures(data.decode("utf-8"))
            if name == "Periodic Full preflight":
                check(first.get("name") == "Ordinary test cmd package", "失败不在普通 cmd 测试")
                expected_package, race = "agentworkspace/cmd/workspace", False
            elif re.fullmatch(r"Go (store|platform|httpapi) worker [0-9]+", name):
                expected_package, race = "agentworkspace/internal/" + name.split()[1], True
            elif name == "Go workspace race":
                expected_package, race = "agentworkspace/cmd/workspace", True
            elif name == "Go ordinary and remaining race":
                check(first.get("name") in ("Ordinary test coordination package", "Race test coordination package", "Race test all other packages"),
                      "失败不在已知 Go 测试步骤")
                expected_package, race = None, first["name"].startswith("Race")
            else:
                raise ValueError("失败不是可回放的已知 Go 作业")
            check(bool(parsed) and all(expected_package is None or row["package"] == expected_package for row in parsed), "缺少已知包的命名失败")
            for candidate in parsed:
                candidate["race"] = race
                key = json.dumps(candidate, sort_keys=True)
                candidates.setdefault(key, {**candidate, "sources": []})["sources"].append(info)
        except (ValueError, KeyError, OSError, UnicodeError) as error:
            unsupported = True
            info.update(log_status="unknown", reason=str(error))
    result.update(failure_jobs=failed, first_blocking_event=min(blocking, key=lambda row: row["started_at"], default=None),
                  failure_candidates=[] if unsupported else list(candidates.values()),
                  diagnostic_status="unknown" if unsupported or not candidates else "available")


def record(args, api, evidence):
    plan = strict_json((args.bundle / "plan.json").read_bytes())
    identity = plan["identity"]
    check(identity["run_id"] == int(os.environ["GITHUB_RUN_ID"]) and identity["run_attempt"] == int(os.environ["GITHUB_RUN_ATTEMPT"]) and
          identity["repo"] == evidence.repo and evidence.commit(identity["sha"]) == {key: identity[key] for key in ("sha", "tree", "head", "base")},
          "plan 与 API/当前 attempt 不符")
    prior = plan["skip_proof"] if plan["decision"] == "skip" else plan["ancestor"]
    try:
        result = receipt(identity, strict_json(os.environ["NEEDS_JSON"]), plan["decision"], prior)
    except ValueError as error:
        # Invalid success claims must fail the evidence job, not be downgraded into a green workflow.
        result = {"schema": 1, "kind": "periodic-full", **identity, "status": "failure", "verified": False,
                  "attribution": {"status": "unknown", "good_sha": None, "bad_sha": identity["sha"],
                                  "commit": None, "reason": str(error)}}
        write_json(args.output / "receipt.json", result)
        output({"status": "failure"})
        raise
    if result["status"] == "failure":
        try:
            run = evidence.run(identity["run_id"], identity["run_attempt"])
            collect_failures(evidence, run, result)
        except (ValueError, KeyError, OSError) as error:
            result.update(diagnostic_status="unknown", diagnostic_reason=str(error))
    write_json(args.output / "receipt.json", result)
    output({"status": result["status"]})
    print(json.dumps(result, sort_keys=True))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as stream:
            stream.write(f"## 周期 Full：{result['status']}\n\n提交 `{identity['sha']}`，attempt `{identity['run_attempt']}`。\n\n")
            for job in result.get("failure_jobs", []):
                stream.write(f"- [失败作业 {job['id']}]({job['html_url']})；首个失败步骤：`{job['first_failed_step'].get('name', '未知')}`\n")
            stream.write("\n归因暂为 unknown；后续回放不会改变原始失败状态。\n")


def verify_localization(initial, local, directory, root):
    replay = load("periodic-replay")
    check(local.get("initial_sha256") == digest((directory.parent / "initial" / "receipt.json").read_bytes()), "回放未绑定初始 receipt")
    if local.get("localization") != "reproduced":
        return local
    baseline = initial.get("ancestor")
    check(baseline and len(initial.get("failure_candidates", [])) == 1 and local.get("candidate") == initial["failure_candidates"][0], "回放失败源不符")
    chain = replay.git(root, "rev-list", "--first-parent", "--max-count=33", initial["sha"]).splitlines()
    check(baseline["sha"] in chain, "绿点不在首亲范围")
    chain = list(reversed(chain[:chain.index(baseline["sha"]) + 1]))
    check(local.get("chain") == chain and len(local.get("probes", [])) == len(chain), "回放范围不完整")
    by_sha = {}
    candidate = initial["failure_candidates"][0]
    for sha_value, group in zip(chain, local["probes"]):
        check(group.get("sha") == sha_value and len(group.get("results", [])) == replay.REPEATS, "缺少重复回放")
        for result in group["results"]:
            check(result.get("sha") == sha_value and result.get("tree") == replay.git(root, "rev-parse", sha_value + "^{tree}"), "probe checkout 身份不符")
            for key in ("list", "run"):
                if key not in result:
                    check(key == "run" and result.get("state") == "absent", "probe 日志缺失")
                    continue
                evidence = result[key]
                name = evidence.get("log", "")
                check(isinstance(name, str) and re.fullmatch(r"[0-9]{4}-(list|run)\.jsonl", name), "probe 日志路径非法")
                data = (directory / name).read_bytes()
                check(evidence.get("sha256") == digest(data) and not evidence.get("timed_out"), "probe 日志摘要不符")
                rows = replay.Probe.rows(data.decode(), candidate["package"])
                if key == "list":
                    check(evidence.get("returncode") == 0 and rows[-1].get("Action") == "pass", "编译清单失败")
                    listed = [line for row in rows if row.get("Action") == "output" for line in row.get("Output", "").splitlines()
                              if re.fullmatch(replay.TEST_RE, line)]
                    check(listed == ([] if result.get("state") == "absent" else [candidate["test"]]), "精确测试清单不符")
                else:
                    found = replay.failures(data.decode())
                    check(sum(row.get("Action") == "run" and row.get("Test") == candidate["test"] for row in rows) == 1, "缺少精确测试执行")
                    if result.get("state") == "pass":
                        check(evidence.get("returncode") == 0 and rows[-1].get("Action") == "pass" and not found, "通过日志不符")
                    else:
                        check(evidence.get("returncode") == 1 and len(found) == 1 and found[0]["signature"] == candidate["signature"] and
                              result.get("signature") == candidate["signature"], "失败日志签名不符")
        by_sha[sha_value] = iter(group["results"])
    recomputed = replay.localize(chain, candidate, lambda value, _: next(by_sha[value]))
    check(recomputed.get("localization") == "reproduced" and all(recomputed.get(key) == local.get(key) for key in
                                                               ("commit", "good_sha", "bad_sha", "transition")), "回放分类不能重建")
    return local


def finish(args, api, evidence):
    initial_path = args.bundle / "receipt.json"
    result = strict_json(initial_path.read_bytes())
    check(result.get("repo") == evidence.repo and type(result.get("run_id")) is int and result["run_id"] == int(os.environ["GITHUB_RUN_ID"]) and
          type(result.get("run_attempt")) is int and result["run_attempt"] == int(os.environ["GITHUB_RUN_ATTEMPT"]), "最终 receipt attempt 身份不符")
    if result["status"] == "failure" and args.localization and (args.localization / "localization.json").exists():
        try:
            local = strict_json((args.localization / "localization.json").read_bytes())
            check(local.get("initial_sha256") == digest(initial_path.read_bytes()), "回放与原始 receipt 摘要不符")
            # Validate source jobs again through authenticated attempt metadata, separately from replay.
            run = evidence.run(result["run_id"], result["run_attempt"])
            expected = dict(result)
            collect_failures(evidence, run, expected)
            check(expected.get("failure_candidates") == result.get("failure_candidates"), "原始失败源与 API 不符")
            local = verify_localization(result, local, args.localization, args.root)
            if local.get("localization") == "reproduced":
                pulls = []
                for page in range(1, 3):
                    rows = api.get(f"{evidence.prefix}/commits/{local['commit']}/pulls?per_page=100&page={page}")
                    check(isinstance(rows, list), "commit/pulls 结构非法")
                    pulls.extend(rows)
                    if len(rows) < 100:
                        break
                else:
                    raise ValueError("合并 PR 结果超过上限")
                result["attribution"] = attribute(local, pulls, evidence.repo)
            else:
                result["attribution"].update(local)
        except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
            result["attribution"].update(status="unknown", reason="归因证据不足：" + str(error))
            result["attribution"].pop("pr", None)
    write_json(args.output / "receipt.json", result)
    # The successful producer log remains byte-compatible with the immutable final success receipt.
    if result["status"] != "success":
        print(json.dumps(result, sort_keys=True))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as stream:
            attribution = result["attribution"]
            stream.write(f"## 最终归因：{attribution['status']}\n\n{attribution.get('reason', '')}\n")
            if attribution.get("status") == "confirmed":
                stream.write(f"\n[PR #{attribution['pr']}](https://github.com/{evidence.repo}/pull/{attribution['pr']})\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("admit", "record", "finish"))
    parser.add_argument("--repo", default=os.environ.get("GITHUB_REPOSITORY"), required=False)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--ref", type=Path)
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--localization", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    api = API(args.output / ("api-" + args.command))
    replay = load("periodic-replay")
    evidence = Evidence(api, args.repo, lambda value: subprocess.run(
        ["git", "-C", str(args.root), "merge-base", "--is-ancestor", value, "HEAD"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=replay.child_env(args.output)).returncode == 0)
    {"admit": admit, "record": record, "finish": finish}[args.command](args, api, evidence)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, UnicodeError, subprocess.SubprocessError, zipfile.BadZipFile) as error:
        print(f"周期 Full 证据失败：{error}", file=sys.stderr)
        sys.exit(1)
