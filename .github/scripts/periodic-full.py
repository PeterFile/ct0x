#!/usr/bin/env python3
"""Read-only periodic Full admission, attempt-bound evidence and receipt publication."""
import argparse
import ast
import base64
from datetime import datetime, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import resource
import shlex
import subprocess
import sys
import time
import zipfile
from urllib.parse import quote


SHA_RE = r"[0-9a-f]{40}"
WORKFLOWS = {".github/workflows/ci.yml": ("legacy", "Full CI"),
             ".github/workflows/periodic-full.yml": ("periodic", "Periodic Full evidence"),
             ".github/workflows/release-candidate.yml": ("release", "Release Full evidence")}
MAX_API, MAX_BYTES, MAX_CACHE = 240, 32 * 1024 * 1024, 512 * 1024 * 1024


class BudgetExceeded(ValueError):
    pass


class SkippedFull(ValueError):
    def __init__(self, receipt):
        super().__init__("周期 skip 不是 Full 证明，仅可作为一次定位提示")
        self.receipt = receipt


class HintAPI:
    """A hint can locate one source, never consume the whole discovery budget."""
    def __init__(self, api):
        self.api, self.endpoints = api, set()

    def get(self, endpoint, raw=False):
        self.endpoints.add(endpoint)
        check(len(self.endpoints) <= 24, "skip 定位提示达到独立的 24 个 GET 上限")
        return self.api.get(endpoint, raw=raw)


def timestamp(value):
    check(isinstance(value, str) and value.endswith("Z"), "缺少 UTC 时间证据")
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def check(condition, message):
    if not condition:
        raise ValueError(message)


def positive(value):
    return type(value) is int and value > 0


def sha(value):
    return isinstance(value, str) and re.fullmatch(SHA_RE, value) is not None


def same(left, right):
    if type(left) is not type(right):
        return False
    if isinstance(left, dict):
        return left.keys() == right.keys() and all(same(left[key], right[key]) for key in left)
    if isinstance(left, list):
        return len(left) == len(right) and all(same(a, b) for a, b in zip(left, right))
    return left == right


def obj(value):
    check(isinstance(value, dict), "API 证据不是对象")
    return value


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
    def __init__(self, output, budget=600):
        self.deadline = time.monotonic() + budget
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=True)
        self.count, self.bytes = 0, 0
        self.cache = {}

    def get(self, endpoint, raw=False):
        check(isinstance(endpoint, str) and endpoint.startswith("repos/") and ".." not in endpoint and
              not re.search(r"[\s#]", endpoint), "API 路径非法")
        if endpoint not in self.cache:
            if self.count >= MAX_API or time.monotonic() >= self.deadline:
                raise BudgetExceeded("只读取证的真实时间或 API 预算耗尽；未核验历史仍为 unknown")
            self.count += 1
            env = {key: os.environ[key] for key in ("PATH", "HOME", "GH_TOKEN") if key in os.environ}
            env.update(GH_PROMPT_DISABLED="1", GH_NO_UPDATE_NOTIFIER="1", GH_TELEMETRY="0")
            check(bool(env.get("GH_TOKEN")), "缺少只读 API 凭据")
            escaped, transient = False, False
            while True:
                flags = ["--allow-escape-sequences"] if escaped else []
                remaining = self.deadline - time.monotonic()
                if remaining <= 0:
                    raise BudgetExceeded("只读取证的真实时间预算耗尽；未核验历史仍为 unknown")
                with tempfile_file() as output, tempfile_file() as error:
                    process = subprocess.run(["gh", "api", *flags, "--method", "GET", endpoint], env=env,
                                             stdout=output, stderr=error, timeout=min(70, remaining),
                                             preexec_fn=lambda: resource.setrlimit(resource.RLIMIT_FSIZE, (MAX_BYTES + 1, MAX_BYTES + 1)))
                    output.seek(0)
                    data = output.read(MAX_BYTES + 1)
                    error.seek(0)
                    diagnostic = error.read(64 * 1024)
                if process.returncode == 0:
                    break
                guard = b"the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway"
                if (not escaped and raw and re.fullmatch(r"repos/[^/]+/[^/]+/actions/jobs/[1-9][0-9]*/logs", endpoint) and
                        guard in diagnostic and not re.search(rb"HTTP [45]\d\d", diagnostic)):
                    escaped = True
                    continue
                if not transient and re.search(rb"HTTP (?:429|5\d\d)", diagnostic):
                    transient = True
                    time.sleep(1)
                    continue
                raise ValueError("只读 API 证据不可用：" + endpoint)
            check(len(data) <= MAX_BYTES and self.bytes + len(data) <= MAX_CACHE, "API 证据超过单项/总缓存大小上限")
            filename = f"{self.count:03d}-{digest(endpoint.encode())[:16]}.bin"
            (self.output / filename).write_bytes(data)
            with (self.output / "index.jsonl").open("a") as index:
                index.write(json.dumps({"endpoint": endpoint, "file": filename, "sha256": digest(data)}) + "\n")
            self.cache[endpoint] = self.output / filename
            self.bytes += len(data)
        with self.cache[endpoint].open('rb') as stream:
            data = stream.read(MAX_BYTES + 1)
        check(len(data) <= MAX_BYTES, '缓存 API 证据超过大小上限')
        return data if raw else strict_json(data)


def tempfile_file():
    import tempfile
    return tempfile.TemporaryFile()


def page_rows(api, path, key):
    page, count, total = 1, 0, None
    while True:
        data = api.get(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
        check(isinstance(data, dict) and type(data.get("total_count")) is int and data["total_count"] >= 0 and
              isinstance(data.get(key), list) and all(isinstance(row, dict) for row in data[key]), "分页证据结构不完整")
        if total is None:
            total = data["total_count"]
        count += len(data[key])
        check(total == data["total_count"] and count <= total and (data[key] or count == total), "分页变化、缺页或总数不符；历史仍为 unknown")
        yield data[key]
        if count == total:
            return
        page += 1


def pages(api, path, key):
    return [row for page in page_rows(api, path, key) for row in page]


class Evidence:
    def __init__(self, api, repo, ancestor):
        check(isinstance(repo, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo), "仓库身份非法")
        self.api, self.repo, self.ancestor = api, repo, ancestor
        self.prefix = "repos/" + repo

    def commit(self, value):
        check(sha(value), "提交 SHA 非法")
        commit = obj(self.api.get(self.prefix + "/commits/" + value))
        check(commit.get("sha") == value and sha(obj(obj(commit.get("commit")).get("tree")).get("sha")), "API 提交/tree 身份不符")
        parents = commit.get("parents")
        check(isinstance(parents, list) and all(isinstance(row, dict) and sha(row.get("sha")) for row in parents), "API 父提交缺失")
        return {"sha": value, "tree": commit["commit"]["tree"]["sha"], "head": value,
                "base": parents[0]["sha"] if parents else value}

    def peel_tag(self, target):
        raw, seen = obj(target).get("sha"), set()
        check(sha(raw), "候选 tag 对象非法")
        while target.get("type") == "tag":
            value = target.get("sha")
            check(sha(value), "候选 tag 对象非法")
            check(value not in seen and len(seen) < 8, "候选 tag 剥离循环或深度超限")
            seen.add(value)
            tag = obj(self.api.get(f"{self.prefix}/git/tags/{value}"))
            check(tag.get("sha") == value, "候选 tag 对象身份不符")
            target = obj(tag.get("object"))
        check(target.get("type") == "commit" and sha(target.get("sha")), "候选 tag 没有指向合法提交")
        return target["sha"], raw

    def control_sha(self, run):
        value = run["head_sha"]
        if WORKFLOWS[run["path"]][0] == "release" and run.get("event") == "push" and "head_commit" in run:
            head = obj(run["head_commit"])
            identity = head.get("id")
            check(sha(identity), "候选原生 head 提交身份非法")
            if value != identity:
                target, _ = self.peel_tag({"type": "tag", "sha": value})
                check(target == identity, "候选原生 tag head 与提交不符")
            commit = self.commit(identity)
            check(head.get("tree_id") == commit["tree"], "候选原生 head tree 与 API 提交不符")
            value = identity
        return value

    def run(self, run_id, attempt, trusted_head=True):
        check(positive(run_id) and positive(attempt), "run/attempt 类型非法")
        repo = obj(self.api.get(self.prefix))
        check(repo.get("full_name") == self.repo and repo.get("fork") is False and positive(repo.get("id")) and
              repo.get("default_branch") == "master", "不是可信的非 fork master 仓库")
        run = obj(self.api.get(f"{self.prefix}/actions/runs/{run_id}/attempts/{attempt}"))
        check(type(run.get("id")) is int and run["id"] == run_id and type(run.get("run_attempt")) is int and
              run["run_attempt"] == attempt and sha(run.get("head_sha")), "API run/attempt 身份不符")
        for key in ("repository", "head_repository"):
            source = obj(run.get(key))
            check(source.get("full_name") == self.repo and type(source.get("id")) is int and source["id"] == repo["id"] and
                  source.get("fork") is False, "运行来自其他仓库或 fork")
        check(isinstance(run.get("path"), str) and positive(run.get("workflow_id")), "未知工作流路径")
        path, _, ref = run["path"].partition("@")
        branch = run.get("head_branch")
        check(path in WORKFLOWS and isinstance(branch, str) and (not ref or ref in
              (branch, "refs/heads/" + branch, "refs/tags/" + branch, "refs/heads/master")), "工作流路径/ref 与 API 来源不符")
        run = {**run, "path": path, "path_ref": ref}
        workflow = obj(self.api.get(f"{self.prefix}/actions/workflows/{run['workflow_id']}"))
        check(workflow.get("path") == run["path"] and type(workflow.get("id")) is int and workflow["id"] == run["workflow_id"], "工作流 API 身份不符")
        # Preserve the native head for job/artifact identity; source and ancestry use its commit.
        run["control_sha"] = self.control_sha(run)
        if trusted_head:
            check(self.ancestor(run["control_sha"]), "触发源不在可信 master 历史中")
        return run

    def periodic_policy(self, control, target):
        check(sha(control) and sha(target), "周期静态策略提交 SHA 非法")
        if control == target:
            return
        for path in (".github/workflows/periodic-full.yml", ".github/workflows/ci-full.yml"):
            _, actual = source_text(self, control, path)
            _, tested = source_text(self, target, path)
            check(actual["blob"] == tested["blob"], "静态周期策略与被测提交不符：" + path +
                  "；本次不能证明目标提交，请等待下一次使用新策略的周期运行")

    def source(self, run, receipt):
        kind = WORKFLOWS[run["path"]][0]
        event, branch = run.get("event"), run.get("head_branch")
        if kind == "legacy":
            check(event == "push" and branch == "master" and receipt["sha"] == run["head_sha"], "不是可信 master push 证明")
        elif kind == "periodic":
            check(event in ("schedule", "workflow_dispatch", "push") and branch == "master", "不是 master 控制的周期证明")
            self.periodic_policy(run.get("control_sha", run["head_sha"]), receipt["sha"])
        elif event == "workflow_dispatch":
            check(branch == "master", "候选 Full dispatch 不来自 master")
        else:
            check(event == "push" and isinstance(branch, str) and branch.startswith("rc/") and len(branch) > 3 and
                  not re.search(r"[\x00-\x20\x7f~^:?*\[\\]", branch) and ".." not in branch and "@{" not in branch and
                  not branch.endswith(".") and all(part and not part.startswith(".") and not part.endswith(".lock") for part in branch.split("/")),
                  "候选 Full tag 来源未知")
            ref = obj(self.api.get(f"{self.prefix}/git/ref/tags/{quote(branch, safe='')}"))
            check(ref.get("ref") == "refs/tags/" + branch, "候选 tag ref 身份不符")
            target, raw = self.peel_tag(obj(ref.get("object")))
            control = run.get("control_sha", run["head_sha"])
            check(run["head_sha"] in (raw, target) and target == control == receipt["sha"],
                  "候选 tag 已移动或与实际被测 SHA 不符")
            workflow, _ = source_text(self, control, run["path"])
            full, producer = block(workflow, "full"), block(workflow, "full-evidence")
            display = re.search(r'^    name: ([A-Za-z][A-Za-z0-9 _-]*)$', full, re.M)
            check(display is not None, '候选 Full 调用名称不是原生 literal')
            self.successful_job(self.jobs(run), display[1] + ' / Full suite evidence')
            check("uses: ./.github/workflows/ci-full.yml" in full and "name: Release Full evidence" in producer and
                  re.search(r"needs: \[[^\n]*\bfull\b[^\n]*\]", producer) and
                  'release-evidence.py full --output "$RUNNER_TEMP/release-full"' in producer and
                  "name: release-full-${{ github.run_id }}-${{ github.run_attempt }}" in producer and
                  "path: ${{ runner.temp }}/release-full/receipt.json" in producer, "候选顶层工作流 blob 不符合原生 producer 协议")
            references = run.get("referenced_workflows", [])
            expected = self.repo + "/.github/workflows/ci-full.yml"
            check(isinstance(references, list) and all(isinstance(row, dict) for row in references) and any(
                  row.get("sha") == control and isinstance(row.get("path"), str) and row["path"].partition("@")[0] == expected and
                  row["path"].partition("@")[2] in ("", "refs/tags/" + branch, branch, "refs/heads/master", control) for row in references), "缺少实际调用的 Full 原生源")
            source_text(self, control, ".github/workflows/ci-full.yml")
        check(self.ancestor(receipt["sha"]), "被测提交不是可信 master 祖先")

    def jobs(self, run):
        rows = pages(self.api, f"{self.prefix}/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs", "jobs")
        ids = set()
        for job in rows:
            check(isinstance(job.get("name"), str) and positive(job.get("id")) and job["id"] not in ids and type(job.get("run_id")) is int and
                  job["run_id"] == run["id"] and type(job.get("run_attempt")) is int and
                  job["run_attempt"] == run["run_attempt"] and job.get("head_sha") == run["head_sha"], "作业不是同一次 run/attempt")
            check(job.get("html_url") == f"https://github.com/{self.repo}/actions/runs/{run['id']}/job/{job['id']}", "作业链接身份不符")
            ids.add(job["id"])
        return sorted(rows, key=lambda row: row["id"])

    @staticmethod
    def successful_job(jobs, name, suffix=False):
        rows = [job for job in jobs if (job.get("name", "").rsplit(" / ", 1)[-1] if suffix else job.get("name")) == name]
        check(len(rows) == 1 and rows[0].get("status") == "completed" and rows[0].get("conclusion") == "success",
              "缺少同 attempt 的成功作业：" + name)
        return rows[0]

    @staticmethod
    def fresh_job(run, job, *, observed_at=None):
        start = timestamp(run.get("run_started_at"))
        end = timestamp(run.get("updated_at")) if observed_at is None else observed_at
        check(start <= timestamp(job.get("started_at")) <= timestamp(job.get("completed_at")) <= end,
              "成功证明作业没有在此 attempt 内新鲜执行")

    def legacy_preflight(self, run, job):
        current_start = timestamp(run.get("run_started_at"))
        started, completed = timestamp(job.get("started_at")), timestamp(job.get("completed_at"))
        if started >= current_start:
            self.fresh_job(run, job)
            return None
        check(started <= completed <= current_start, "继承预检的原生时间不完整或跨越当前 attempt")
        fields = ("name", "status", "conclusion", "started_at", "completed_at", "run_id", "head_sha", "runner_id", "steps")
        check(all(key in job for key in fields) and positive(job.get("runner_id")) and
              isinstance(job.get("steps"), list) and bool(job["steps"]), "继承预检缺少 runner 或完整步骤指纹")
        numbers = set()
        for step in job["steps"]:
            check(isinstance(step, dict) and positive(step.get("number")) and step["number"] not in numbers and
                  isinstance(step.get("name"), str) and bool(step["name"]) and step.get("status") == "completed" and
                  step.get("conclusion") in ("success", "skipped") and
                  started <= timestamp(step.get("started_at")) <= timestamp(step.get("completed_at")) <= completed,
                  "继承预检的完整原生步骤证据缺失或不成功")
            numbers.add(step["number"])
        fingerprint = {key: job[key] for key in fields}
        raw = self.api.get(f"{self.prefix}/actions/jobs/{job['id']}/logs", raw=True)
        check(isinstance(raw, bytes) and bool(raw), "继承预检缺少完整原始日志")
        matches = []
        for attempt in range(run["run_attempt"] - 1, 0, -1):
            previous = self.run(run["id"], attempt)
            check(previous.get("status") == "completed" and all(same(previous.get(key), run.get(key)) for key in
                  ("head_sha", "path", "event", "head_branch", "workflow_id")), "继承预检的实际来源控制不符")
            start, end = timestamp(previous.get("run_started_at")), timestamp(previous.get("updated_at"))
            check(start <= end <= current_start, "前次 attempt 的原生时间窗不可信")
            if not start <= started <= completed <= end:
                continue
            original = self.successful_job(self.jobs(previous), "CI preflight and feedback")
            self.fresh_job(previous, original)
            check(all(key in original for key in fields) and same({key: original[key] for key in fields}, fingerprint),
                  "继承预检与实际源作业的状态、时间、runner 或完整步骤不符")
            original_raw = self.api.get(f"{self.prefix}/actions/jobs/{original['id']}/logs", raw=True)
            check(raw == original_raw, "继承预检与实际源作业的完整日志不符")
            self.source(previous, {"sha": run["head_sha"]})
            _, control = source_text(self, previous["head_sha"], previous["path"])
            matches.append({"inherited": True, "reported": {"run_id": run["id"], "run_attempt": run["run_attempt"], "job_id": job["id"]},
                            "actual": {"run_id": previous["id"], "run_attempt": attempt, "job_id": original["id"]},
                            "source": control, "started_at": job["started_at"], "completed_at": job["completed_at"],
                            "log_sha256": digest(raw), "fingerprint_sha256": digest(json.dumps(fingerprint, sort_keys=True).encode())})
        check(len(matches) == 1, "继承预检的实际执行 attempt 缺失或不唯一")
        return matches[0]

    def full_job(self, run, jobs):
        control = run.get("control_sha", run["head_sha"])
        workflow, _ = source_text(self, control, run["path"])
        caller = block(workflow, "full")
        display = re.search(r"^    name: ([A-Za-z][A-Za-z0-9 _-]*)$", caller, re.M)
        check(display and "uses: ./.github/workflows/ci-full.yml" in caller, "原生 Full 调用源缺失或有歧义")
        expected = self.repo + "/.github/workflows/ci-full.yml"
        references = run.get("referenced_workflows")
        check(isinstance(references, list) and any(isinstance(row, dict) and row.get("sha") == control and
              isinstance(row.get("path"), str) and row["path"].partition("@")[0] == expected for row in references),
              "缺少实际调用的 Full 原生源")
        full, _ = source_text(self, control, ".github/workflows/ci-full.yml")
        check("name: Full suite evidence" in block(full, "gate"), "原生 Full 聚合 producer 不符")
        return self.successful_job(jobs, display[1] + " / Full suite evidence")

    def canonical(self, job, run):
        text = self.api.get(f"{self.prefix}/actions/jobs/{job['id']}/logs", raw=True).decode("utf-8")
        found = []
        for raw in text.splitlines():
            line = re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", raw)
            if line.startswith("{"):
                value = strict_json(line)
                if isinstance(value, dict) and ("verified" in value or "kind" in value):
                    stamp = re.match(r"^(\d{4}-\d\d-\d\dT[0-9:.]+Z) ", raw)
                    if stamp:
                        check(timestamp(job.get("started_at")) <= timestamp(stamp[1]) <= timestamp(job.get("completed_at")) and
                              timestamp(run.get("run_started_at")) <= timestamp(stamp[1]), "规范成功日志来自旧 attempt")
                    found.append(value)
        check(len(found) == 1, "成功 producer 日志缺少唯一规范 JSON")
        return found[0]

    def artifact(self, run, kind, *, producer=None, observed_at=None):
        artifacts = pages(self.api, f"{self.prefix}/actions/runs/{run['id']}/artifacts", "artifacts")
        name = f"{kind}-{run['id']}-{run['run_attempt']}"
        rows = [row for row in artifacts if row.get("name") == name]
        check(len(rows) == 1, "attempt 限定的 receipt artifact 缺失或重复")
        item = rows[0]
        check(item.get("expired") is False and positive(item.get("id")), "receipt artifact 已过期")
        source = obj(item.get("workflow_run"))
        repo_id = run["repository"]["id"]
        for key, value in {"id": run["id"], "repository_id": repo_id, "head_repository_id": repo_id,
                           "head_sha": run["head_sha"], "head_branch": run["head_branch"]}.items():
            check(type(source.get(key)) is type(value) and source[key] == value, "artifact 与 API run 身份不符")
        created = timestamp(item.get("created_at"))
        if producer is not None:
            self.fresh_job(run, producer, observed_at=observed_at)
            check(timestamp(producer.get("started_at")) <= created <= timestamp(producer.get("completed_at")),
                  "artifact 不在实际成功 producer 的原生执行时间内")
        else:
            check(timestamp(run.get("run_started_at")) <= created <= timestamp(run.get("updated_at")), "artifact 不在此 attempt 时间内")
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
        kind, producer = WORKFLOWS[run["path"]]
        check(run.get("status") == "completed" or kind == "release" and run.get("status") == "in_progress", "运行尚未完成")
        jobs = self.jobs(run)
        # An open release may be waiting for machines long after its Full producers.
        # Its updated_at is not a terminal bound. Use the authenticated observation.
        observed_at = datetime.now(timezone.utc) if kind == "release" and run["status"] == "in_progress" else None
        job = self.successful_job(jobs, producer)
        self.fresh_job(run, job, observed_at=observed_at)
        receipt = (self.artifact(run, "release-full", producer=job, observed_at=observed_at) if kind == "release" else
                   self.canonical(job, run))
        check(isinstance(receipt, dict), "receipt 不是对象")
        if kind == "periodic" and receipt.get("status") == "skipped":
            self.verify_skip(run, receipt)
            raise SkippedFull(receipt)
        aggregate = self.full_job(run, jobs)
        self.fresh_job(run, aggregate, observed_at=observed_at)
        preflight = None
        check(receipt.get("verified") is True and sha(receipt.get("sha")) and sha(receipt.get("tree")), "receipt 未证明成功身份")
        target = self.commit(receipt["sha"])
        check(receipt["tree"] == target["tree"], "receipt tree 与 API commit 不符")
        control = run.get("control_sha", run["head_sha"])
        if target["sha"] != control:
            _, actual_full = source_text(self, control, ".github/workflows/ci-full.yml")
            _, target_full = source_text(self, target["sha"], ".github/workflows/ci-full.yml")
            check(actual_full["blob"] == target_full["blob"], "实际调用的 Full 与被测提交的 Full 源不符：.github/workflows/ci-full.yml")
        if kind == "legacy":
            preflight = self.legacy_preflight(run, self.successful_job(jobs, "CI preflight and feedback"))
            check(set(receipt) == {"verified", "run_id", "sha", "tree", "head", "base"} and
                  type(receipt.get("run_id")) is str and receipt["run_id"] == str(run_id), "legacy 外层证明形状不符")
        else:
            expected_kind = "periodic-full" if kind == "periodic" else "release-full"
            check(type(receipt.get("schema")) is int and receipt["schema"] == 1 and receipt.get("kind") == expected_kind and
                  receipt.get("repo") == self.repo and type(receipt.get("run_id")) is int and receipt["run_id"] == run_id and
                  type(receipt.get("run_attempt")) is int and receipt["run_attempt"] == attempt and receipt.get("status") == "success",
                  "receipt schema/repo/run/attempt/status 不符")
            if kind == "periodic":
                check(same(self.artifact(run, expected_kind), receipt), "artifact 与成功 producer 规范日志不符")
            if kind == "periodic":
                self.fresh_job(run, self.successful_job(jobs, "Periodic Full preflight"))
                check(receipt.get("components") == {key: "success" for key in ("resolve", "preflight", "full")} and
                      receipt.get("checkout") == {key: target[key] for key in ("sha", "tree")}, "缺少完整预检/checkout 证明")
                expected = {**target, "run_id": run_id, "run_attempt": attempt, "repo": self.repo, "verified": True}
                check(same(receipt.get("full"), expected), "Full 聚合身份不符")
            else:
                check(isinstance(receipt.get("artifact_sha256"), str) and re.fullmatch(r"[0-9a-f]{64}", receipt["artifact_sha256"]),
                      "候选产物摘要缺失")
        for key in ("head", "base"):
            check((key not in receipt and kind == "release") or receipt.get(key) == target[key], "receipt head/base 与 API commit 不符")
        self.source(run, receipt)
        proof = {**target, "repo": self.repo, "run_id": run_id, "run_attempt": attempt, "verified": True,
                 "workflow": run["path"], "event": run["event"], "producer_job_id": job["id"], "url": job["html_url"]}
        if preflight is not None:
            proof["preflight_provenance"] = preflight
        return proof

    def verify_skip(self, run, receipt):
        check(type(receipt.get("schema")) is int and receipt["schema"] == 1 and receipt.get("kind") == "periodic-full" and
              receipt.get("repo") == self.repo and type(receipt.get("run_id")) is int and receipt["run_id"] == run["id"] and
              type(receipt.get("run_attempt")) is int and receipt["run_attempt"] == run["run_attempt"] and receipt.get("verified") is False and
              receipt.get("components") == {"resolve": "success", "preflight": "skipped", "full": "skipped"} and
              receipt.get("checkout") == {} and receipt.get("full") == {}, "skip receipt 身份或跳过组件不符")
        target = self.commit(receipt.get("sha"))
        check(all(same(receipt.get(key), target[key]) for key in ("sha", "tree", "head", "base")), "skip receipt 的实际目标 SHA/tree 不符")
        check(same(self.artifact(run, "periodic-full"), receipt), "skip artifact 与成功 producer 规范日志不符")
        self.source(run, receipt)

    def follow_skip(self, receipt):
        pointer = receipt.get("skip_proof")
        check(isinstance(pointer, dict) and pointer.get("repo") == self.repo and pointer.get("verified") is True and
              positive(pointer.get("run_id")) and positive(pointer.get("run_attempt")) and
              all(same(pointer.get(key), receipt.get(key)) for key in ("sha", "tree")) and
              (pointer["run_id"], pointer["run_attempt"]) != (receipt["run_id"], receipt["run_attempt"]),
              "skip 定位提示跨仓库、目标不符、循环或身份非法")
        # Direct verification only: a pointed-to skip cannot follow another hint.
        proof = Evidence(HintAPI(self.api), self.repo, self.ancestor).verify(pointer["run_id"], pointer["run_attempt"])
        check(all(same(proof.get(key), receipt.get(key)) for key in ("repo", "sha", "tree")), "定位到的真实 Full 与 skip 目标不符")
        return proof

    def search(self, chain):
        check(bool(chain) and len(set(chain)) == len(chain) and all(sha(value) for value in chain), "缺少可信首亲链")
        found, reasons, seen, idle = {}, [], set(), object()
        positions = {value: index for index, value in enumerate(chain)}
        current = int(os.environ.get("GITHUB_RUN_ID", "0"))
        current_attempt = int(os.environ.get("GITHUB_RUN_ATTEMPT", "1"))
        hint_used = False
        legacy_path = f"{self.prefix}/actions/workflows/ci.yml/runs"

        def safe_pages(path):
            try:
                yield from page_rows(self.api, path, "workflow_runs")
            except BudgetExceeded:
                raise
            except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
                reasons.append(str(error))

        def attempts(runs):
            for run in runs:
                if not positive(run.get("id")) or not positive(run.get("run_attempt")):
                    continue
                for attempt in range(run["run_attempt"], 0, -1):
                    key = run["id"], attempt
                    if key in seen or run["id"] == current and attempt >= current_attempt:
                        continue
                    seen.add(key)
                    yield key

        def exact(value, recent):
            # A noisy recent page must not hide a legacy master push at this SHA.
            rows = [row for row in recent if row.get("head_sha") == value and row.get("head_branch") == "master"]
            for key in attempts(rows):
                yield key
                if value in found:
                    return
            for page in safe_pages(legacy_path + f"?head_sha={value}&event=push&branch=master"):
                for key in attempts(row for row in page if row.get("head_sha") == value and row.get("head_branch") == "master"):
                    yield key
                    if value in found:
                        return
                yield idle
            # Even a missing exact lookup yields to the other source classes.
            yield idle

        def ancestors(recent):
            for value in chain[1:]:
                best = min((positions[key] for key in found), default=len(chain))
                if positions[value] > best:
                    return
                yield from exact(value, recent)

        def other_source(iterator):
            for page in iterator:
                yield from attempts(page)
                yield idle

        def verify_one(key):
            nonlocal hint_used
            try:
                proof = self.verify(*key)
                if proof["sha"] in positions:
                    found.setdefault(proof["sha"], proof)
            except SkippedFull as skip:
                reasons.append(str(skip))
                if not hint_used and skip.receipt["sha"] == chain[0]:
                    hint_used = True
                    try:
                        proof = self.follow_skip(skip.receipt)
                        found.setdefault(proof["sha"], proof)
                    except BudgetExceeded:
                        raise
                    except (ValueError, KeyError, OSError, UnicodeError, subprocess.SubprocessError, zipfile.BadZipFile) as error:
                        reasons.append("skip 定位提示未知：" + str(error))
            except BudgetExceeded:
                raise
            except (ValueError, KeyError, OSError, UnicodeError, subprocess.SubprocessError, zipfile.BadZipFile) as error:
                reasons.append(str(error))

        try:
            recent = next(safe_pages(legacy_path), [])
            sources = [other_source(safe_pages(f"{self.prefix}/actions/workflows/{name}/runs"))
                       for name in ("periodic-full.yml", "release-candidate.yml")]
            direct = exact(chain[0], recent)
            # All source classes get a target opportunity before ancestor fanout.
            queues = [direct, *sources]
            active = list(queues)
            first = True
            while active:
                for queue in list(active):
                    key = next(queue, None)
                    if key is None:
                        active.remove(queue)
                        continue
                    if key is idle:
                        continue
                    verify_one(key)
                    if chain[0] in found:
                        return found, reasons
                if first:
                    active.append(ancestors(recent))
                    first = False
        except BudgetExceeded as error:
            reasons.append(str(error))
        if chain[0] not in found:
            reasons.append("未找到可核验的同 SHA Full；已验证祖先仅作为回放绿点，不代表遗漏历史已执行")
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
    rows = [row for row in pulls if isinstance(row, dict) and positive(row.get("number")) and
            isinstance(row.get("merged_at"), str) and re.fullmatch(r"\d{4}-\d\d-\d\dT[0-9:]+Z", row["merged_at"]) and
            row.get("merge_commit_sha") == localization["commit"] and row.get("base", {}).get("ref") == "master" and
            row.get("base", {}).get("repo", {}).get("full_name") == repo]
    if len(rows) == 1:
        result.update(status="confirmed", pr=rows[0]["number"], reason="同名测试和签名已重复复现，并核验精确 master 合并 PR")
    else:
        result["reason"] = "精确合并 PR 缺失或不唯一"
    return result


def combine_attribution(identity, items):
    result, groups = [], {}
    for raw in items:
        item = dict(raw)
        candidate = item.get("candidate", {})
        sources = item.get("sources", candidate.get("sources", []))
        proof = item.get("evidence", {})
        bound = (all(sha(item.get(key)) for key in ("good_sha", "bad_sha", "commit")) and item["bad_sha"] == identity["sha"] and
                 (isinstance(candidate.get("test"), str) and bool(candidate["test"]) or
                  candidate.get("kind") == "diagnostic" and candidate.get("replay_kind") in ("go-compile", "go-vet") and
                  isinstance(candidate.get("file"), str) and re.fullmatch(r'(?:cmd/workspace|internal/[A-Za-z][A-Za-z0-9_]*)/[A-Za-z][A-Za-z0-9_]*\.go', candidate['file']) and
                  candidate.get('package') == 'agentworkspace/' + candidate['file'].rsplit('/', 1)[0]) and bool(sources) and
                 all(positive(source.get("id")) and type(source.get("run_id")) is int and source["run_id"] == identity["run_id"] and
                     type(source.get("run_attempt")) is int and source["run_attempt"] == identity["run_attempt"] and
                     isinstance(source.get("log_sha256"), str) and re.fullmatch(r"[0-9a-f]{64}", source["log_sha256"]) for source in sources) and
                 proof.get("artifact") == f"periodic-full-localization-{identity['run_id']}-{identity['run_attempt']}" and
                 proof.get("path") == "localization.json" and isinstance(proof.get("sha256"), str) and re.fullmatch(r"[0-9a-f]{64}", proof["sha256"]))
        if item.get("status") != "confirmed" or not positive(item.get("pr")) or not bound:
            item.update(status="unknown")
            item.pop("pr", None)
        else:
            groups.setdefault((item["pr"], item["commit"]), []).append(item.get("id"))
        item["sources"] = sources
        result.append(item)
    complete = bool(result) and all(item["status"] == "confirmed" for item in result)
    combined = {"status": "unknown", "coverage": "complete" if complete else "partial" if groups else "unknown",
                "good_sha": None, "bad_sha": identity["sha"], "commit": None, "items": result,
                "groups": [{"pr": pr, "commit": commit, "items": ids} for (pr, commit), ids in groups.items()],
                "reason": "逐项证据已保留；仍有未知阻塞或多个独立转换，不能指定单一 PR"}
    if complete and len(groups) == 1:
        first = result[0]
        combined.update(status="confirmed", pr=first["pr"], commit=first["commit"], good_sha=first["good_sha"],
                        reason="所有阻塞签名均独立复现到同一精确合并 PR")
    return combined


def artifact_ref(identity, name, path, data):
    return {"artifact": f"periodic-full-{name}-{identity['run_id']}-{identity['run_attempt']}", "path": path, "sha256": digest(data),
            "run_id": identity["run_id"], "run_attempt": identity["run_attempt"],
            "url": f"https://github.com/{identity['repo']}/actions/runs/{identity['run_id']}/attempts/{identity['run_attempt']}"}


def compact_receipt(result):
    if result["status"] != "failure":
        return result
    result.pop("failure_candidates", None)
    result.pop("unresolved_failures", None)
    for item in result["attribution"].get("items", []):
        item.pop("probes", None)
        item.pop("chain", None)
        item.pop("directory", None)
        candidate = item.get("candidate", {})
        if candidate:
            raw = json.dumps(candidate, sort_keys=True).encode()
            signature = candidate.get("signature", {})
            item["candidate_sha256"] = digest(raw)
            item["candidate"] = {key: value for key, value in candidate.items() if key not in ("signature", "sources")}
            item["candidate"]["signature_sha256"] = digest(json.dumps(signature, sort_keys=True).encode())
        item["reason"] = str(item.get("reason", ""))[:2048]
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
    event_sha = os.environ["GITHUB_SHA"]
    check(sha(event_sha), "事件 SHA 非法")
    run_id, attempt = int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"])
    current = evidence.run(run_id, attempt, trusted_head=False)
    check(current["path"] == ".github/workflows/periodic-full.yml" and current["head_sha"] == event_sha and
          current["event"] in ("workflow_dispatch", "schedule", "push") and current["head_branch"] == "master",
          "当前运行不是可信 master 周期/手动工作流")
    # Both static workflows follow the authenticated trigger, not the later product checkout.
    evidence.periodic_policy(current.get("control_sha", current["head_sha"]), target_sha)
    chain = replay.git(args.root, "rev-list", "--first-parent", target_sha).splitlines()
    proofs, reasons = evidence.search(chain)
    prior = next((proofs[value] for value in chain[1:] if value in proofs), None)
    skip = proofs.get(target_sha)
    decision = "skip" if skip else "run"
    identity = {**target, "repo": evidence.repo, "run_id": int(os.environ["GITHUB_RUN_ID"]),
                "run_attempt": int(os.environ["GITHUB_RUN_ATTEMPT"])}
    check(positive(identity["run_id"]) and positive(identity["run_attempt"]), "当前 run/attempt 非法")
    plan = {"identity": identity, "decision": decision, "ancestor": prior, "skip_proof": skip,
            "lookup_status": "proven" if skip else "unknown", "lookup_reasons": reasons}
    write_json(args.output / "plan.json", plan)
    selected = {"mode": "full", **target, "matrix": load("ci-plan", args.root).shard_matrix()}
    write_json(args.output / "full-plan.json", selected)
    output({**identity, "decision": decision, "matrix": selected["matrix"]})


def source_text(evidence, value, path):
    check(sha(value), "原始控制源 SHA 缺失")
    record = obj(evidence.api.get(f"{evidence.prefix}/contents/{path}?ref={value}"))
    check(record.get("encoding") == "base64" and sha(record.get("sha")), "原始控制源内容缺失")
    data = base64.b64decode(record["content"], validate=False)
    blob = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
    check(blob == record["sha"], "原始控制源 blob 摘要不符")
    return data.decode(), {"sha": value, "path": path, "blob": blob}


def block(text, name, indent=2):
    match = re.search(r"^" + " " * indent + re.escape(name) + r":\n(.*?)(?=^" + " " * indent + r"[\w-]+:|\Z)", text, re.M | re.S)
    check(match is not None, "已知原始控制块缺失：" + name)
    return match[1]


def default_policy_workspace(evidence, run, job, workflow, body, chunk):
    """Prove only the reviewed empty-policy path; other policy sources stay unknown."""
    def clean(value):
        return '\n'.join(line for line in value.splitlines() if line.strip() and not line.lstrip().startswith('#'))

    path = run['path'].partition('@')[0]
    callers = {'.github/workflows/periodic-full.yml': ('resolve', '[resolve, preflight]',
                "${{ needs.resolve.outputs.decision == 'run' }}", ('schedule', 'workflow_dispatch', 'push')),
               '.github/workflows/ci.yml': ('feedback', 'feedback',
                "${{ needs.feedback.outputs.mode == 'full' }}", ('push',))}
    check(path in callers and run.get('head_branch') == 'master' and run.get('event') in callers[path][3],
          'CI 策略默认目录缺少已知 master 调用源；其他策略源仍为 unknown')
    source, _ = source_text(evidence, run['head_sha'], path)
    owner, needs, condition, _ = callers[path]
    expected = ('    name: Full suite\n    needs: ' + needs + '\n    if: ' + condition +
                '\n    uses: ./.github/workflows/ci-full.yml\n    with:\n' +
                ''.join('      ' + key + ': ${{ needs.' + owner + '.outputs.' + key + ' }}\n'
                        for key in ('sha', 'tree', 'head', 'base', 'matrix')))
    caller = clean(block(source, 'full'))
    check(job['name'].split(' / ', 1)[0] == 'Full suite' and caller in
          {clean(expected), clean(expected + "      policy_sha: ''\n"), clean(expected + '      policy_sha: ""\n')},
          'CI 策略调用名称、产品输入或空 policy_sha 绑定未证明；策略源仍为 unknown')
    header, job_header = workflow.split('\njobs:', 1)[0], body.split('\n    steps:', 1)[0]
    declared = re.findall(r'^      policy_sha:\n((?:^        .*\n)+)', header, re.M)
    check(len(declared) == 1 and clean(declared[0]) == "        type: string\n        required: false\n        default: ''",
          'CI 策略 policy_sha 不是已知可选空默认值；策略源仍为 unknown')
    check(not re.search(r'^\s*env:', header, re.M) and 'working-directory:' not in header and
          re.findall(r'^\s*shell: (.+)$', header, re.M) == ['bash'] and
          not re.search(r'^    (?:env|defaults):', job_header, re.M),
          'CI 策略默认 workspace 存在未证明的环境或目录覆盖')
    steps = re.findall(r'^      - (.*?)(?=^      - |\Z)', body, re.M | re.S)
    check(steps.count(chunk) == 1, 'CI 策略原生步骤不唯一')
    before = steps[:steps.index(chunk)]
    is_shard = job['name'].rsplit(' / ', 1)[-1].startswith('Go ') and chunk.startswith('name: Run and verify logical slots\n')
    check(len(before) == (5 if is_shard else 6), 'CI 策略默认目录前存在未证明的步骤')
    for item, anchor, keys in zip(before, ('verify-policy-input', 'checkout-policy', 'stage-policy'),
                                  (['name', 'if', 'env', 'run'], ['name', 'if', 'uses', 'with'], ['name', 'if', 'env', 'run'])):
        definitions = re.findall(r'^      - (&' + anchor + r'\n.*?)(?=^      - |\Z)', workflow, re.M | re.S)
        check(len(definitions) == 1 and workflow.count('&' + anchor) == 1, 'CI 策略条件步骤 anchor 未证明')
        guarded = clean(definitions[0])
        check(clean(item) in (guarded, '*' + anchor), 'CI 策略默认目录前不是已知条件步骤')
        lines = guarded.splitlines()[1:]
        check(all(line.startswith('        ') for line in lines) and
              [line.strip().split(':', 1)[0] for line in lines if not line.startswith('         ')] == keys and
              re.findall(r'^        if: (.+)$', guarded, re.M) == ["${{ inputs.policy_sha != '' }}"],
              'CI 策略设置步骤没有被空 policy_sha 明确跳过')
    setup = '''uses: actions/checkout@v7
        with:
          ref: ${{ inputs.sha }}
          persist-credentials: false
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
          cache-dependency-path: go.sum'''
    actual = clean('      - '.join(before[3:]))
    check(actual == setup + ('' if is_shard else '\n      - run: go vet ./...'),
          'CI 策略产品 checkout 或 workspace 前置步骤未证明')
    if is_shard:
        metadata = '''name: Run and verify logical slots
        env:
          SHARD_PROFILE: ${{ matrix.profile }}
          SHARD_WORKER: ${{ matrix.worker }}
          CHECKOUT_SHA: ${{ inputs.sha }}
          CHECKOUT_TREE: ${{ inputs.tree }}'''
        check(clean(chunk.split('        run:', 1)[0]) == metadata, 'CI 策略分片步骤存在未证明的环境或目录覆盖')
    else:
        expected = '''name: Test CI scheduling and shard gates
        working-directory: ${{ env.CI_POLICY_ROOT || github.workspace }}
        run: |
          set -euo pipefail
          python3 .github/scripts/test-ci-scheduling.py -v
          python3 .github/scripts/test-store-shard.py -v'''
        check(clean(chunk) == expected, 'CI 策略 Python 步骤不是已知 workspace 默认 recipe')


def native_step(evidence, run, job, step):
    def named(body, key, actual):
        match = re.search(r"^    name: (.+)$", body, re.M)
        if not match:
            return key == actual
        display = match[1].strip("'\"")
        if display == ">-":
            folded = re.search(r"^    name: >-\n((?:^      .*\n)+)", body, re.M)
            return bool(folded and ("'" + actual + "'" in folded[1] or '"' + actual + '"' in folded[1]))
        parts = re.split(r"(\$\{\{\s*matrix\.[A-Za-z_]+\s*\}\})", display)
        return re.fullmatch("".join(r"[^\r\n]+" if part.startswith("${{") else re.escape(part) for part in parts), actual) is not None
    path = run["path"].partition("@")[0]
    text, proof = source_text(evidence, run["head_sha"], path)
    proofs, name = [proof], job["name"]
    if " / " in name:
        caller, name = name.split(" / ", 1)
        callers = [body for key, body in re.findall(r"^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)", text, re.M | re.S) if named(body, key, caller)]
        check(len(callers) == 1 and "uses: ./.github/workflows/ci-full.yml" in callers[0], "Full 原生调用图不唯一或不是本地 reusable")
        path = ".github/workflows/ci-full.yml"
        text, proof = source_text(evidence, run["head_sha"], path)
        proofs.append(proof)
    jobs = [(key, body) for key, body in re.findall(r"^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)", text, re.M | re.S) if named(body, key, name)]
    check(len(jobs) == 1, "原生作业在可信源码中缺失或有歧义")
    key, body = jobs[0]
    selected = []
    for chunk in re.findall(r"^      - (.*?)(?=^      - |\Z)", body, re.M | re.S):
        declared = re.match(r"name: (.+)", chunk)
        run_value = re.search(r"^(?:run:|        run:) (.+)$", chunk, re.M)
        if not run_value:
            continue
        value = run_value[1]
        if value in ("|", "|-"):
            lines = []
            for line in chunk[run_value.end():].splitlines():
                if line and not line.startswith("          "):
                    break
                lines.append(line[10:] if line else "")
            value = "\n".join(lines).strip()
        display = declared[1].strip("'\"") if declared else "Run " + value.splitlines()[0]
        if display == step.get("name"):
            selected.append((chunk, value))
    check(len(selected) == 1, "原生失败步骤无法唯一绑定源码 run recipe")
    chunk, recipe = selected[0]
    ceilings = []
    for scope, indent in ((body.split("\n    steps:", 1)[0], 4), (chunk, 8)):
        limit = re.search(r"^" + " " * indent + r"timeout-minutes: ([1-9][0-9]*)$", scope, re.M)
        check(limit or "timeout-minutes:" not in scope, "原生诊断作业/步骤预算是动态值")
        if limit:
            ceilings.append(int(limit[1]) * 60)
    seconds = min(ceilings, default=21600)
    check(seconds <= 21600 and "runs-on: ubuntu-" in body, "原生诊断 runner/预算不受支持")
    directory = '.'
    for scope in (text.split('\njobs:', 1)[0], body.split('\n    steps:', 1)[0], chunk):
        workdir = re.search(r'^\s*working-directory: (.+)$', scope, re.M)
        check(workdir or 'working-directory:' not in scope, '原生工作目录不是简单 literal')
        if workdir:
            if workdir[1] == '${{ env.CI_POLICY_ROOT || github.workspace }}':
                check(path == '.github/workflows/ci-full.yml' and key == 'go-vet', 'CI 策略目录 fallback 不属于已知 Full 步骤')
                default_policy_workspace(evidence, run, job, text, body, chunk)
                directory = '.'
            else:
                check(re.fullmatch(r'[A-Za-z0-9_./-]+', workdir[1]), '原生工作目录不是简单 literal')
                directory = workdir[1]
    check(not directory.startswith("/") and ".." not in Path(directory).parts, "原生工作目录越界")
    slug = evidence.repo.split("/")[1]
    root = str(Path("/home/runner/work") / slug / slug / directory)
    return {"run": recipe, "step_text": chunk, "job_text": body, "workflow": text, "job_key": key,
            "proof": proofs, "source_root": root, "timeout_seconds": seconds,
            "recipe": {"sha256": digest(chunk.encode()), "job": key, "step": step["name"], "path": path}}


def shard_source(evidence, run, target, job, native):
    """Bind both Go phases to the same immutable, actually selected sharder."""
    legacy = '.github/scripts/ci-shards.py'
    fallback = '"${CI_POLICY_ROOT:-.}"/.github/scripts/ci-shards.py'
    suffix = (' run "$SHARD_PROFILE" "$SHARD_WORKER" "$RUNNER_TEMP/shard-evidence" \\\n'
              '  --run-id "$GITHUB_RUN_ID" --sha "$CHECKOUT_SHA" --tree "$CHECKOUT_TREE"')
    recipes = ['set -euo pipefail\npython3 ' + path + suffix for path in (legacy, fallback)]
    check(native['recipe']['path'] == '.github/workflows/ci-full.yml' and native['job_key'] == 'go-shards' and
          native['run'] in recipes, '分片原始 shell recipe 不受支持；执行源仍为 unknown')
    slug = evidence.repo.split('/')[1]
    check(native['source_root'] == str(Path('/home/runner/work') / slug / slug), '分片原始目录不是产品 workspace')
    if native['run'] == recipes[1]:
        default_policy_workspace(evidence, run, job, native['workflow'], native['job_text'], native['step_text'])
    profile = re.fullmatch(r'Go (store|platform|httpapi) worker [0-9]+', job['name'].rsplit(' / ', 1)[-1])
    check(profile, '分片原生 profile 不受支持')
    script, proof = source_text(evidence, target, '.github/scripts/ci-shards.py')
    try:
        parsed = ast.parse(script)
    except SyntaxError as error:
        raise ValueError('分片原始源码无法解析；执行模式仍为 unknown') from error
    commands = []
    for name, selector in (('run_worker', ['-list', '^(Test|Example|Fuzz)']), ('run_slot', ['-run', '<original-slot-pattern>'])):
        functions = [node for node in parsed.body if isinstance(node, ast.FunctionDef) and node.name == name]
        check(len(functions) == 1, '分片原生 Go 函数不唯一')
        function = functions[0]
        original = ast.get_source_segment(script, function)
        targets = [node for node in ast.walk(function) if isinstance(node, ast.Name) and node.id == 'target' and isinstance(node.ctx, ast.Store)]
        assignment = ast.parse('target = "./internal/" + profile').body[0]
        check(len(targets) == 1 and any(ast.dump(node) == ast.dump(assignment) for node in function.body) and
              'GOFLAGS' not in original and '-timeout' not in original, '分片原始目标、模式或预算不受支持')
        calls = [node for node in ast.walk(function) if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and
                 node.func.id == 'command' and node.args and isinstance(node.args[0], ast.List) and
                 len(node.args[0].elts) >= 2 and all(isinstance(node.args[0].elts[index], ast.Constant) and
                 node.args[0].elts[index].value == value for index, value in enumerate(('go', 'test')))]
        check(len(calls) == 1, '分片 Go argv 不唯一')
        argv, values = [], {'pattern': '<original-slot-pattern>', 'target': './internal/' + profile[1]}
        for node in calls[0].args[0].elts:
            check(isinstance(node, ast.Constant) and isinstance(node.value, str) or isinstance(node, ast.Name) and node.id in values,
                  '分片 Go argv 包含未证明的动态值')
            argv.append(node.value if isinstance(node, ast.Constant) else values[node.id])
        check(argv == ['go', 'test', '-count=1', '-race', '-json', *selector, values['target']], '分片原始 Go 模式不受支持')
        commands.append(argv)
    return commands, proof


def diagnostic_records(evidence, run, target, job, step, log, native):
    diagnostics, replay = load('periodic-diagnostics'), load('periodic-replay')
    commands, proofs = [], list(native["proof"])
    for line in native["run"].splitlines():
        if re.match(r"\s*(?:go (?:test|build|vet) |python3 (?:-B )?\.github/scripts/test-(?:ci-scheduling|store-shard)\.py )", line):
            argv = shlex.split(line)
            if "|" in argv:
                index = argv.index("|")
                check(argv[index + 1:index + 2] == ["tee"], "原生诊断 Go pipeline 不是已知 tee")
                argv = argv[:index]
            commands.append(argv)
    if native['job_key'] == 'go-shards' and step.get('name') == 'Run and verify logical slots':
        original, proof = shard_source(evidence, run, target, job, native)
        commands.extend(original)
        proofs.append(proof)
    rest = re.search(r'^\s*GOFLAGS="\$\{GOFLAGS:\+\$GOFLAGS \}-timeout=([^"\s]+)" go test -race "\$\{rest\[@\]\}"$', native['run'], re.M)
    if rest:
        mode = go_execution(evidence, run, target, job, step)
        roots = set()
        for line in log.splitlines():
            plain = replay.clean_line(line)
            try:
                row = strict_json(plain)
                if isinstance(row, dict) and row.get('Action') == 'fail' and not row.get('Test'):
                    value = row.get('Package')
                    if isinstance(value, str) and re.fullmatch(replay.PACKAGE_RE, value): roots.add(value)
            except ValueError:
                match = re.fullmatch(r'FAIL\s+(' + replay.PACKAGE_RE + r')\s+\[build failed\]', plain)
                if match: roots.add(match[1])
        roots.difference_update(mode['excluded_packages'])
        if roots:
            commands.append(['go', 'test', '-race', '-timeout=' + mode['timeout_arg'], *sorted(roots)])
    known, issues = [], []
    for original in commands:
        argv = list(original)
        phase = "unittest" if argv[0] == "python3" else "vet" if argv[1] == "vet" else "compile"
        recipe = {**native['recipe'], 'original_argv': original}
        if phase == 'compile':
            # Test-binary timeouts do not govern test compilation; retain the literal proof.
            timeouts = [value.removeprefix('-timeout=') for value in argv if value.startswith('-timeout=')]
            check(len(timeouts) <= 1, '原始 Go timeout 不唯一')
            if timeouts:
                seconds = replay.duration(timeouts[0], native['timeout_seconds'])
                recipe['test_timeout'] = {'literal': timeouts[0], 'seconds': seconds}
                argv = [value for value in argv if not value.startswith('-timeout=')]
            # Listing and running the same test selection compile identical inputs.
            # Do not pretend the log proves which sequential source command was reached.
            _, build_flags, targets = diagnostics.go_args(argv)
            argv = [argv[0], argv[1], *build_flags, *targets]
        recipe['command_status'] = 'source-alternatives' if len(commands) > 1 else 'source-command'
        if len(commands) > 1:
            recipe['alternatives'] = commands
        if phase == "unittest":
            script = next((arg for arg in argv if arg in diagnostics.SCRIPTS), None)
            if not script or not re.search(r'File "(?:' + re.escape(native["source_root"].rstrip("/") + "/") + r')?' + re.escape(script) + r'", line [0-9]+, in test_', log):
                continue
        else:
            check(not re.search(r"^\s+(?:GOFLAGS|GOOS|GOARCH|CGO_ENABLED):", native["step_text"] + native["job_text"].split("\n    steps:", 1)[0], re.M), "原生诊断 Go 环境模式尚未解析")
        context = {"argv": argv, "job": job["name"], "step": step["name"], "source_root": native["source_root"],
                   "timeout_seconds": native["timeout_seconds"], "job_timeout_seconds": native["timeout_seconds"], "proof": proofs}
        rows, residual = diagnostics.failure_records(phase, log, context)
        for candidate in rows:
            candidate["recipe"] = recipe
        known.extend(rows)
        if rows:
            issues.extend(residual)
    return known, issues


def go_execution(evidence, run, target, job, step):
    """Derive only reviewed original modes; never invent a shorter replay package timeout."""
    name, step_name = job["name"].rsplit(" / ", 1)[-1], step.get("name")
    path, job_key, race, duration = ".github/workflows/ci-full.yml", None, True, "10m"
    packages, excluded, proofs, native = [], [], [], None
    if name in ("Periodic Full preflight", "CI preflight and feedback"):
        check(step_name == "Ordinary test cmd package", "失败不在普通 cmd 步骤")
        path = run["path"].partition("@")[0]
        job_key, race, command = ("preflight" if name == "Periodic Full preflight" else "feedback"), False, "go test -count=1 ./cmd/workspace"
        packages = ["agentworkspace/cmd/workspace"]
    elif name == "Go workspace race":
        check(step_name == "Race test workspace package", "失败不在 workspace 测试步骤")
        job_key, command = "go-workspace", "go test -count=1 -race ./cmd/workspace"
        packages = ["agentworkspace/cmd/workspace"]
    elif name == "Go ordinary and remaining race":
        job_key = "go-other"
        if step_name in ("Ordinary test coordination package", "Race test coordination package"):
            race = step_name.startswith("Race")
            command = "go test -count=1 " + ("-race " if race else "") + "./internal/coordination"
            packages = ["agentworkspace/internal/coordination"]
        else:
            check(step_name == "Race test all other packages", "失败不在已知 Go 测试步骤")
            command = ""
            excluded = ["agentworkspace/internal/" + name for name in ("store", "platform", "httpapi", "coordination")] + ["agentworkspace/cmd/workspace"]
    else:
        match = re.fullmatch(r"Go (store|platform|httpapi) worker [0-9]+", name)
        check(match and step_name == "Run and verify logical slots", "不是已知分片测试步骤")
        job_key = 'go-shards'
        packages = ["agentworkspace/internal/" + match[1]]
        native = native_step(evidence, run, job, step)
        _, proof = shard_source(evidence, run, target, job, native)
        proofs = [*native['proof'], proof]
        command = native['run']
    workflow, proof = source_text(evidence, run["head_sha"], path)
    job_text = block(workflow, job_key)
    selected = re.search(r"^      - name: " + re.escape(step_name) + r"\n(.*?)(?=^      - |\Z)", job_text, re.M | re.S)
    check(selected is not None, "原始测试步骤缺失")
    check("GOFLAGS" not in workflow.split("\njobs:", 1)[0] and "GOFLAGS" not in job_text.split("\n    steps:", 1)[0], "全局/作业自定义 Go 模式不受支持")
    if job_key != "go-shards":
        lines = [line.strip().removeprefix("run: ") for line in selected[1].splitlines() if "go test" in line]
        check(len(lines) == 1, "原始测试命令不唯一")
        if excluded:
            match = re.fullmatch(r'GOFLAGS="\$\{GOFLAGS:\+\$GOFLAGS \}-timeout=([^"\s]+)" go test -race "\$\{rest\[@\]\}"', lines[0])
            check(match is not None, "其余包原始模式不是已知的 literal timeout")
            duration = match[1]
        else:
            check("GOFLAGS" not in selected[1], "步骤 Go 模式包含未解析自定义 flags")
            explicit = re.findall(r"\s+-timeout(?:=|\s+)([^\s]+)", lines[0])
            stripped = re.sub(r"\s+-timeout(?:=|\s+)[^\s]+", "", lines[0])
            check(len(explicit) <= 1 and stripped == command, "原始测试命令包含额外参数或包装")
            if explicit:
                duration = explicit[0]
        command = lines[0]
    else:
        check('GOFLAGS' not in selected[1], '分片原始模式或预算未知')
    ceiling = re.search(r"^    timeout-minutes: ([1-9][0-9]*)$", job_text, re.M)
    check(ceiling or "timeout-minutes:" not in job_text, "原始作业预算是动态值")
    job_seconds = int(ceiling[1]) * 60 if ceiling else 21600
    check(0 < job_seconds <= 21600, "原始 hosted 作业预算超限")
    seconds = load("periodic-replay").duration(duration, job_seconds)
    result = {"mode": "race" if race else "ordinary", "timeout_seconds": seconds, "job_timeout_seconds": job_seconds,
              "timeout_arg": duration, "setup_seconds": 300, "packages": packages, "excluded_packages": excluded,
              "proof": proofs or [proof], "step": step_name, "command": command}
    if native is not None:
        result['recipe'] = native['recipe']
    return result


def client_records(evidence, run, job, step, log, target=None):
    clients = load("periodic-clients")
    client = {"Web test, typecheck and build": "web", "Pi bridge test and typecheck": "bridge"}.get(step.get("name"))
    check(client and job["name"] in ("Periodic Full preflight", "CI preflight and feedback"), "不是已知客户端预检步骤")
    native = native_step(evidence, run, job, step)
    job_text = native['job_text']
    version = "22.x" if client == "web" else "24.13.0"
    selected = re.search(r"^      - name: " + re.escape(step["name"]) + r"\n(.*?)(?=^      - |\Z)", job_text, re.M | re.S)
    check(selected and re.search(r"ci-plan\.py[\"']?\s+" + client + r"\s", selected[1]) and
          "node-version: '" + version + "'" in job_text and not re.search('npm_config_ignore_scripts', job_text, re.I), "客户端原始工具链/命令未证明")
    helper, proof = source_text(evidence, target or run['head_sha'], '.github/scripts/ci-plan.py')
    reviewed = '''def clients(selected, client):
    if selected["mode"] != "full" and not selected[client]:
        return
    directory = "web" if client == "web" else "extensions/pi-bridge"
    install = ["npm", "ci"] if client == "web" else ["npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"]
    command(install, cwd=directory)
    for script in (("test", "typecheck", "build") if client == "web" else ("test", "typecheck")):
        command(["npm", "run", script], cwd=directory)
def command(args, cwd=None, capture=False):
    print("CI:", json.dumps(args, ensure_ascii=True), flush=True)
    return subprocess.run(args, cwd=cwd, check=True, text=True, capture_output=capture)
'''
    actual = {node.name: ast.dump(node) for node in ast.parse(helper).body if isinstance(node, ast.FunctionDef)}
    check(all(actual.get(node.name) == ast.dump(node) for node in ast.parse(reviewed).body), '客户端完整安装/执行 helper recipe 已变化；不能使用不同生命周期语义归因')
    known, issues = clients.failures(client, log), []
    clean = clients.cleaned(log)
    if client == "web":
        headers = list(re.finditer(r"(?m)^\s*FAIL\s+(\S+) > (.+)$", clean))
        for index, header in enumerate(headers):
            section = clean[header.start():headers[index + 1].start() if index + 1 < len(headers) else len(clean)]
            if not clients.failures(client, section):
                issues.append({"file": header[1], "test": header[2], "reason": "客户端命名失败不是可比较断言"})
        total = re.search(r"(?m)^\s*Tests\s+(\d+) failed", clean)
        if total and int(total[1]) > len(known) + len(issues) or re.search(r"Unhandled (?:Error|Rejection)|unhandled errors|^\s*FAIL\s+\S+\s*$", clean, re.M | re.I):
            issues.append({"reason": "客户端还存在未分类或未关联的阻塞失败"})
    elif clean.startswith("TAP version 13"):
        records, _, complete, error = clients.tap_records(clean)
        for record in records:
            if record["status"] == "not ok" and not record["children"] and not any(row["test"] == record["test"] for row in known):
                issues.append({"test": record["test"], "reason": "Node 叶失败缺少可比较断言"})
        if not complete:
            issues.append({"reason": "Node 报告不完整：" + error})
    else:
        total = re.search(r"(?m)^ℹ fail\s+(\d+)", clean)
        if total and int(total[1]) > len(known):
            issues.append({"reason": "Node 还存在未分类的叶失败"})
    check(known or issues, "没有可用的客户端失败证据")
    for candidate in known:
        candidate['execution'] = {'client': client, 'node': version, 'proof': [*native['proof'], proof], 'step': step['name']}
        candidate['recipe'] = native['recipe']
    return known, issues


def failure_key(candidate):
    if candidate.get("replay_kind") == "go-compile":
        mode = candidate["execution"]
        value = {key: candidate[key] for key in ("kind", "replay_kind", "package", "file", "signature")}
        value["inputs"] = {key: mode[key] for key in ("argv", "proof", "source_root")}
    else:
        value = candidate
    return json.dumps(value, sort_keys=True)


def collect_failures(evidence, run, result):
    replay = load("periodic-replay")
    candidates, failed, unresolved = {}, [], []
    jobs = evidence.jobs(run)
    synthetic = {"Full suite evidence", "Periodic Full evidence", "Full CI", "Periodic attribution evidence"}
    blocking = {"failure", "timed_out", "cancelled", "action_required", "stale", "startup_failure"}
    for job in jobs:
        if job.get("conclusion") not in blocking:
            continue
        info = {key: job[key] for key in ("id", "name", "html_url", "run_id", "run_attempt")}
        info.update(completed_at=job.get("completed_at"), conclusion=job["conclusion"], status=job.get("status"))
        steps = job.get("steps", [])
        steps = [step for step in steps if isinstance(step, dict) and step.get("conclusion") in blocking and positive(step.get("number"))] if isinstance(steps, list) else []
        first = min(steps, key=lambda step: step["number"], default={})
        info["first_failed_step"] = first
        name = job["name"].rsplit(" / ", 1)[-1]
        failed.append(info)
        if name in synthetic:
            continue
        try:
            check(job.get("status") == "completed", "失败作业尚未完成")
            data = evidence.api.get(f"{evidence.prefix}/actions/jobs/{job['id']}/logs", raw=True)
            info.update(log_status="available", log_sha256=digest(data), log_endpoint=f"{evidence.prefix}/actions/jobs/{job['id']}/logs")
            check(job["conclusion"] == "failure", "阻塞作业终态为 " + job["conclusion"] + "，不能证明完整测试失败")
            log, target = data.decode("utf-8"), result.get("sha", run["head_sha"])
            if first.get("name") in ("Web test, typecheck and build", "Pi bridge test and typecheck"):
                parsed, issues = client_records(evidence, run, job, first, log, target)
            else:
                native = native_step(evidence, run, job, first)
                diagnosed, residual = diagnostic_records(evidence, run, target, job, first, log, native)
                parsed, issues, mode_error = [], [], None
                try:
                    mode = go_execution(evidence, run, target, job, first)
                    named, issues = replay.failure_records(log)
                    for candidate in named:
                        if (mode["packages"] and candidate["package"] not in mode["packages"]) or candidate["package"] in mode["excluded_packages"]:
                            issues.append({"package": candidate["package"], "reason": "失败包不属于原始步骤"})
                            continue
                        parsed.append({**candidate, "race": mode["mode"] == "race", "execution": mode})
                except ValueError as error:
                    mode_error = str(error)
                covered = {candidate["package"] for candidate in diagnosed if candidate.get("package")}
                named_failures = set()
                for line in log.splitlines():
                    try:
                        row = strict_json(replay.clean_line(line))
                        if isinstance(row, dict) and row.get("Action") == "fail":
                            if isinstance(row.get("FailedBuild"), str) and row["FailedBuild"] in covered and isinstance(row.get("Package"), str):
                                covered.add(row["Package"])
                            if isinstance(row.get("Test"), str):
                                named_failures.add((row.get("Package"), row["Test"]))
                    except ValueError:
                        pass
                issues = [issue for issue in issues if issue.get("package") not in covered]
                accounted = {(candidate["package"], name) for candidate in parsed for name in candidate["signature"]["tests"]}
                for issue in residual:
                    if issue.get("package") in {candidate.get("package") for candidate in parsed} and issue["reason"] == "另有未绑定已知诊断的 Go 包失败":
                        continue
                    if issue["reason"] == "命名测试失败不是编译诊断" and named_failures and named_failures <= accounted:
                        continue
                    issues.append(issue)
                parsed.extend(diagnosed)
                check(parsed or issues, mode_error or "缺少已知原生诊断")
            check(bool(parsed) or bool(issues), "缺少已知包的命名失败或可识别诊断")
            for candidate in parsed:
                key = failure_key(candidate)
                combined = candidates.setdefault(key, {**candidate, "sources": []})
                if candidate.get("replay_kind") == "go-compile":
                    for field in ("timeout_seconds", "job_timeout_seconds"):
                        combined["execution"][field] = min(combined["execution"][field], candidate["execution"][field])
                    source = next((row for row in combined["sources"] if row["id"] == info["id"]), None)
                    if source is None:
                        source = {**info, "diagnostic": {"execution": dict(candidate["execution"]), "recipes": []}}
                        combined["sources"].append(source)
                    if candidate["recipe"] not in source["diagnostic"]["recipes"]:
                        source["diagnostic"]["recipes"].append(candidate["recipe"])
                else:
                    combined["sources"].append(info)
            unique = {json.dumps(issue, sort_keys=True): issue for issue in issues}
            unresolved.extend({**issue, "sources": [info]} for issue in unique.values())
        except (ValueError, KeyError, OSError, TypeError, UnicodeError) as error:
            info.setdefault("log_status", "unknown")
            info["replay_status"] = "unknown"
            unresolved.append({"reason": str(error), "sources": [info]})
    leaves = [job for job in failed if job["name"].rsplit(" / ", 1)[-1] not in synthetic]
    if not leaves and failed:
        unresolved.append({"reason": "只有聚合/证据失败，没有已完成的测试叶子", "sources": failed})
    times = []
    for job in leaves or failed:
        try:
            stamp = datetime.fromisoformat(job["completed_at"].replace("Z", "+00:00"))
            check(stamp.tzinfo is not None and job["status"] == "completed", "完成时间/状态不完整")
            times.append((stamp.timestamp(), job))
        except (ValueError, TypeError, AttributeError):
            pass
    first_blocking = min(times, key=lambda value: value[0])[1] if times else None
    result.update(failure_jobs=failed, first_blocking_event=first_blocking,
                  blocking_order_status="complete" if len(times) == len(leaves or failed) else "unknown", unresolved_failures=unresolved,
                  job_timeline=[{key: job.get(key) for key in ("id", "name", "conclusion", "completed_at")} for job in jobs],
                  failure_candidates=list(candidates.values()),
                  diagnostic_status="partial" if unresolved and candidates else "unknown" if unresolved or not candidates else "available")


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
            run = evidence.run(identity["run_id"], identity["run_attempt"], trusted_head=False)
            collect_failures(evidence, run, result)
        except (ValueError, KeyError, OSError) as error:
            result.update(diagnostic_status="unknown", diagnostic_reason=str(error))
    result["evidence_artifacts"] = []
    for prefix, name, path in (("plan", "plan.json", args.bundle / "plan.json"),
                               ("initial", "api-record/index.jsonl", args.output / "api-record/index.jsonl")):
        if path.exists():
            result["evidence_artifacts"].append(artifact_ref(result, prefix, name, path.read_bytes()))
    write_json(args.output / "receipt.json", result)
    output({"status": result["status"]})
    print(json.dumps(result, sort_keys=True))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as stream:
            stream.write(f"## 周期 Full：{result['status']}\n\n提交 `{identity['sha']}`，attempt `{identity['run_attempt']}`。\n\n")
            first = result.get("first_blocking_event")
            if first:
                stream.write(f"首阻塞：[作业 {first['id']}]({first['html_url']})，完成于 `{first['completed_at']}`。\n\n")
            for job in result.get("failure_jobs", []):
                stream.write(f"- [失败作业 {job['id']}]({job['html_url']})；首个失败步骤：`{job['first_failed_step'].get('name', '未知')}`\n")
            stream.write("\n归因暂为 unknown；后续回放不会改变原始失败状态。\n")


def verify_go_probe(result, candidate, directory, replay, seen):
    mode = candidate["execution"]
    check(same(result.get("execution"), mode), "probe 原始模式/预算不符")
    logs, codes = {"run": ""}, {"run": None}
    for key in ("list", "run"):
        if key not in result:
            check(key == "run" and result.get("state") == "absent", "probe 日志缺失")
            continue
        evidence = result[key]
        name = evidence.get("log", "")
        check(isinstance(name, str) and re.fullmatch(r"[0-9]{4,}-" + key + r"\.jsonl", name) and name not in seen, "probe 日志路径非法或重复")
        seen.add(name)
        path = directory / name
        check(not path.is_symlink() and path.stat().st_size <= replay.MAX_LOG, "probe 日志超过上限或为链接")
        data = path.read_bytes()
        check(evidence.get("sha256") == digest(data) and not evidence.get("timed_out") and not evidence.get("state"), "probe 日志摘要/状态不符")
        pattern, flags = "^" + candidate["test"] + "$", ["-race"] if candidate["race"] else []
        expected = (["go", "test", "-json", *flags, "-list", pattern] if key == "list" else
                    ["go", "test", "-count=1", *flags, "-json", "-timeout=" + mode["timeout_arg"], "-run", pattern])
        expected.append("./" + candidate["package"].removeprefix("agentworkspace/"))
        check(evidence.get("argv") == expected, "probe argv 未保持原始包预算和精确测试")
        logs[key], codes[key] = data.decode(), evidence.get("returncode")
    return replay.assess(logs["list"], codes["list"], logs["run"], codes["run"], candidate)


def verify_localization(initial, local, directory, root, *, remaining=None):
    replay = load("periodic-replay")
    candidate = local.get("candidate")
    check(isinstance(candidate, dict) and sum(same(candidate, row) for row in initial.get("failure_candidates", [])) == 1, "回放失败源不符")
    if local.get("localization") != "reproduced":
        return local
    baseline = initial.get("ancestor")
    check(baseline is not None, "缺少可信绿点")
    check(remaining is None or remaining() > 0, "最终原始证据复核真实时间预算已耗尽")
    chain = replay.first_parent_chain(root, initial)
    check(local.get("chain") == chain and isinstance(local.get("probes"), list), "回放首亲范围不符")
    check(local.get("original_good_sha") == chain[0] and local.get("original_bad_sha") == chain[-1], "原始范围端点不符")
    by_sha, seen, counts = {}, set(), {}
    clients = load("periodic-clients") if candidate.get("kind") == "client" else None
    diagnostics = load('periodic-diagnostics') if candidate.get('kind') == 'diagnostic' else None
    check(not directory.is_symlink(), "回放证据目录为链接")
    for interruption in local.get("interruptions", []):
        check(isinstance(interruption, dict) and interruption.get("sha") in chain and
              isinstance(interruption.get("result"), dict) and interruption["result"].get("state") not in ("pass", "fail", "absent"),
              "中断记录隐藏了已完成观察")
        # Interrupted observations are never proof; their identities cannot be reused.
        seen.update(value for _, value in load("periodic-history")._identities(interruption))
    for group in local["probes"]:
        check(remaining is None or remaining() > 0, "最终原始证据复核真实时间预算已耗尽")
        sha_value = group.get("sha")
        check(sha_value in chain and sha_value not in by_sha and 1 <= len(group.get("results", [])) <= replay.REPEATS,
              "probe 越界、重复或缺少重复确认")
        source = None if clients or diagnostics else replay.definition(root, sha_value, candidate)
        tree = replay.git(root, "rev-parse", sha_value + "^{tree}")
        for result in group["results"]:
            check(remaining is None or remaining() > 0, "最终原始证据复核真实时间预算已耗尽")
            check(result.get("sha") == sha_value and result.get("tree") == tree, "probe checkout 身份不符")
            if clients:
                probe_id = result.get("probe_id")
                check(isinstance(probe_id, str) and probe_id not in seen, "客户端重复使用执行证据")
                seen.add(probe_id)
                assessed = clients.verify_probe(result, candidate, directory, seen=seen)
                inventory = clients.read_bounded(directory / result["source"]["inventory"]["log"], clients.MAX_LOG)
                actual = subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", str(root),
                                                  "ls-tree", "-rz", "--full-tree", sha_value, "--", clients.PROJECTS[candidate["client"]]],
                                                 env=replay.child_env(directory), timeout=30)
                check(inventory == actual, "客户端原始源码清单不属于可信首亲提交")
            elif diagnostics:
                assessed = diagnostics.verify_probe(result, candidate, directory, replay=replay, root=root, seen=seen)
            else:
                check(same(result.get("definition"), source), "probe 源码定义不符")
                assessed = verify_go_probe(result, candidate, directory, replay, seen)
            check(assessed.get("state") == result.get("state") and assessed.get("state") != "unknown" and
                  same(assessed.get("signature"), result.get("signature")), "probe 生命周期或签名不能重建")
        by_sha[sha_value], counts[sha_value] = iter(group["results"]), 0
    def recorded(value, _):
        counts[value] += 1
        result = next(by_sha[value], None)
        check(result is not None, "相邻转换缺少重复执行证明")
        return result
    recomputed = replay.localize(chain, candidate, recorded, remaining=remaining)
    check(recomputed.get("localization") == "reproduced" and all(same(recomputed.get(key), local.get(key)) for key in
          ("chain", "candidate", "original_good_sha", "original_bad_sha", "commit", "good_sha", "bad_sha", "transition", "scope", "continuation")) and
          all(counts[group["sha"]] == len(group["results"]) for group in local["probes"]), "相邻边界、连续失败后缀或稀疏覆盖不能重建")
    return local


def merged_pulls(api, evidence, commit):
    pulls = []
    page = 1
    while True:
        rows = api.get(f"{evidence.prefix}/commits/{commit}/pulls?per_page=100&page={page}")
        check(isinstance(rows, list) and all(isinstance(row, dict) for row in rows), "commit/pulls 结构非法")
        pulls.extend(rows)
        if len(rows) < 100:
            return pulls
        page += 1


def finish(args, api, evidence):
    replay = load("periodic-replay")
    deadline = min(time.monotonic() + 600, getattr(api, "deadline", float("inf")))
    initial_path = args.bundle / "receipt.json"
    initial_bytes = initial_path.read_bytes()
    result = strict_json(initial_bytes)
    check(result.get("repo") == evidence.repo and type(result.get("run_id")) is int and result["run_id"] == int(os.environ["GITHUB_RUN_ID"]) and
          type(result.get("run_attempt")) is int and result["run_attempt"] == int(os.environ["GITHUB_RUN_ATTEMPT"]), "最终 receipt attempt 身份不符")
    if result["status"] == "failure":
        envelope, raw, source_error = {}, None, None
        try:
            check(args.localization and (args.localization / "localization.json").is_file(), "没有完成的回放证据")
            raw = (args.localization / "localization.json").read_bytes()
            envelope = strict_json(raw)
            check(type(envelope.get("schema")) is int and envelope["schema"] == 1 and isinstance(envelope.get("items"), list) and
                  envelope.get("initial_sha256") == digest(initial_bytes), "回放 schema 或原始 receipt 绑定不符")
            run = evidence.run(result["run_id"], result["run_attempt"])
            evidence.source(run, result)
            expected = dict(result)
            collect_failures(evidence, run, expected)
            check(same(expected.get("failure_candidates"), result.get("failure_candidates")) and
                  same(expected.get("unresolved_failures"), result.get("unresolved_failures")), "原始失败源与认证 API 不符")
            prior = result.get("ancestor")
            if prior is not None:
                check(same(evidence.verify(prior.get("run_id"), prior.get("run_attempt")), prior),
                      "原始绿点的 Full run/attempt 证明不能重新核验")
        except (ValueError, KeyError, OSError, TypeError, subprocess.SubprocessError) as error:
            source_error = str(error)
        items = []
        for candidate in result.get("failure_candidates", []):
            item_id = replay.candidate_id(candidate)
            item = {"id": item_id, "status": "unknown", "candidate": candidate, "good_sha": None,
                    "bad_sha": result["sha"], "commit": None, "reason": source_error or "命名失败尚未复现"}
            if raw is not None:
                item["evidence"] = artifact_ref(result, "localization", "localization.json", raw)
                item["evidence"]["item_id"] = item_id
            if not source_error:
                try:
                    matches = [row for row in envelope["items"] if isinstance(row, dict) and row.get("id") == item_id]
                    check(len(matches) == 1 and matches[0].get("directory") == item_id and same(matches[0].get("candidate"), candidate), "独立测试回放缺失、重复或借用其他测试证据")
                    local = verify_localization(result, matches[0], args.localization / item_id, args.root,
                                                remaining=lambda: deadline - time.monotonic())
                    item.update({key: local[key] for key in ("original_good_sha", "original_bad_sha", "scope", "continuation") if key in local})
                    item["evidence_status"] = "reconstructed" if local.get("localization") == "reproduced" else "unverified"
                    if local.get("localization") == "reproduced":
                        verified = attribute(local, merged_pulls(api, evidence, local["commit"]), evidence.repo)
                        item.update({key: verified[key] for key in ("status", "pr", "good_sha", "bad_sha", "commit", "reason", "transition") if key in verified})
                    else:
                        item["reason"] = local.get("reason", "回放结果未知")
                except (ValueError, KeyError, OSError, TypeError, StopIteration, subprocess.SubprocessError) as error:
                    item["reason"] = "此测试证明不足：" + str(error)
            items.append(item)
        items.extend({**issue, "status": "unknown", "good_sha": None, "bad_sha": result["sha"], "commit": None}
                     for issue in result.get("unresolved_failures", []))
        result["attribution"] = combine_attribution(result, items)
        refs = result.setdefault("evidence_artifacts", [])
        refs.append(artifact_ref(result, "initial", "receipt.json", initial_bytes))
        if raw is not None:
            refs.append(artifact_ref(result, "localization", "localization.json", raw))
        api_root = getattr(api, "output", None)
        if api_root and (api_root / "index.jsonl").exists():
            refs.append(artifact_ref(result, "verification", "api-finish/index.jsonl", (api_root / "index.jsonl").read_bytes()))
    compact_receipt(result)
    check(len(json.dumps(result).encode()) <= 2 * 1024 * 1024, "紧凑 receipt 超过上限，原始证据仍独立保留")
    write_json(args.output / "receipt.json", result)
    if result["status"] != "success":
        print(json.dumps(result, sort_keys=True))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as stream:
            attribution = result["attribution"]
            stream.write(f"## 最终归因：{attribution['status']}\n\n{attribution.get('reason', '')}\n")
            for pr in sorted({item["pr"] for item in attribution.get("items", []) if item.get("status") == "confirmed"}):
                stream.write(f"\n[独立核验 PR #{pr}](https://github.com/{evidence.repo}/pull/{pr})\n")


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
    evidence_output = args.output.parent / (args.output.name + "-evidence") if args.command == "finish" else args.output
    api = API(evidence_output / ("api-" + args.command))
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
