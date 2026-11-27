#!/usr/bin/env python3
"""Small shared GitHub/provenance primitives; no model execution or publication."""
from datetime import datetime
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

WORKFLOW = ".github/workflows/release-candidate.yml"
INTAKE = ".github/workflows/release-receipt.yml"
CONTEXT = "release-candidate/first-owner-v1"
DESCRIPTION = "首个 owner 已登记；尚未验证候选"
BOT = {"login": "github-actions[bot]", "id": 41898282, "type": "Bot"}
MAX_RECEIPT = 24576


def check(condition, message):
    if not condition:
        raise ValueError(message)


def exact(value, pattern, message):
    check(isinstance(value, str) and re.fullmatch(pattern, value) is not None, message)
    return value


def sha(value):
    return exact(value, r"[0-9a-f]{40}", "提交或树 SHA 非法")


def digest(value):
    return exact(value, r"[0-9a-f]{64}", "SHA256 非法")


def number(value):
    check(type(value) is int and 0 < value < 2 ** 63, "原生 ID 或 attempt 非法")
    return value


def repository(value):
    return exact(value, r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", "仓库身份非法")


def rc_ref(value):
    exact(value, r"refs/tags/rc/[^/]+", "只接受单层 rc/* 标签")
    name = value[len("refs/tags/rc/"):]
    check(not name.startswith(".") and not name.endswith((".", ".lock"))
          and ".." not in name and "@{" not in name
          and re.search(r"[\x00-\x20\x7f~^:?*\[\\]", name) is None, "rc/* 标签不符合 Git ref 格式")
    return value


def timestamp(value):
    exact(value, r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?(?:Z|[+-][0-9]{2}:[0-9]{2})",
          "缺少或非法的原生时间戳")
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def hashed(data):
    return hashlib.sha256(data).hexdigest()


def file_hash(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def encoded(value):
    return (json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(",", ":")) + "\n").encode()


def decode(data, limit=MAX_RECEIPT):
    check(len(data) <= limit, "回执超过允许大小")
    def unique(pairs):
        result = {}
        for key, value in pairs:
            check(key not in result, "JSON 含重复字段")
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=unique)


def save(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("xb") as output:
        output.write(encoded(value))


def load_script(name, directory=None):
    path = (Path(directory) if directory else Path(__file__).parent) / (name + ".py")
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def git(root, *args):
    try:
        return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.PIPE, timeout=120).decode().strip()
    except (subprocess.SubprocessError, OSError):
        raise OSError("本地 Git 身份查询失败；来源未知") from None


def require_jobs(jobs, run_id, attempt, names):
    result = []
    for name in names:
        rows = [row for row in jobs if row["name"] == name]
        check(len(rows) == 1, "缺少或重复的本次 attempt 作业：" + name)
        job = rows[0]
        check(job.get("run_id") == run_id and job.get("run_attempt") == attempt
              and job.get("status") == "completed" and job.get("conclusion") == "success",
              "作业没有本次 attempt 的成功原生证据：" + name)
        number(job["id"])
        result.append(job)
    return result


def unpack(data, expected_digest, names, max_bytes=160 << 20):
    check(expected_digest == "sha256:" + hashed(data), "原生 artifact ZIP 摘要不符")
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        entries = archive.infolist()
        check(len(entries) == len(names) and {item.filename for item in entries} == set(names), "artifact 根文件形状不符")
        check(all(not item.is_dir() and item.file_size <= max_bytes and
                  (item.external_attr >> 16) & 0o170000 in (0, 0o100000) for item in entries), "artifact 文件类型非法")
        check(sum(item.file_size for item in entries) <= max_bytes, "artifact 解压大小超限")
        return {item.filename: archive.read(item) for item in entries}


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        parsed = urllib.parse.urlsplit(newurl)
        check(parsed.scheme == "https" and parsed.hostname and
              (parsed.hostname.endswith(".blob.core.windows.net") or parsed.hostname.endswith(".actions.githubusercontent.com")
               or parsed.hostname == "api.github.com"), "artifact 跳转主机非法")
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if parsed.hostname != "api.github.com":
            redirected.remove_header("Authorization")
        return redirected


class GitHub:
    def __init__(self, repo, token=None):
        self.repo = repository(repo)
        self.prefix = "/repos/" + self.repo
        self.token = token or os.environ.get("GITHUB_TOKEN") or os.environ.get("RELEASE_TOKEN")
        check(bool(self.token), "缺少 GitHub 授权；令牌只应从本地环境注入")
        self.opener = urllib.request.build_opener(SafeRedirect())
        self._objects = {}

    def request(self, method, path, data=None, binary=False):
        check(path == self.prefix or path.startswith(self.prefix + "/"), "API 请求越过仓库边界")
        headers = {"Accept": "application/vnd.github+json", "Authorization": "Bearer " + self.token,
                   "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "release-candidate"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request("https://api.github.com" + path, data=encoded(data) if data is not None else None,
                                         headers=headers, method=method)
        # A write is never automatically repeated, including after a timeout.
        for index in range(2 if method == "GET" else 1):
            try:
                with self.opener.open(request, timeout=45) as response:
                    limit = 160 << 20 if binary else 8 << 20
                    raw = response.read(limit + 1)
                    status = getattr(response, "status", 200)
                check(len(raw) <= limit, "API 响应超限")
                if status == 204:
                    check(not raw, "API 204 响应意外包含正文")
                    return None
                check(bool(raw), "API 成功响应缺少正文")
                return raw if binary else decode(raw, limit)
            except (urllib.error.URLError, TimeoutError) as error:
                if index == 0 and method == "GET" and (not isinstance(error, urllib.error.HTTPError) or error.code >= 500):
                    time.sleep(1)
                    continue
                reason = "HTTP " + str(error.code) if isinstance(error, urllib.error.HTTPError) else "网络或超时"
                raise OSError(f"GitHub API {method} 请求失败（{reason}）；结果未知，写入不得盲目重试") from None

    def get(self, path):
        return self.request("GET", path)

    def immutable(self, path):
        suffix = path[len(self.prefix):] if path.startswith(self.prefix) else ""
        exact(suffix, r"/(?:git/(?:commits|trees|tags)/[0-9a-f]{40}(?:\?recursive=1)?|compare/[0-9a-f]{40}\.\.\.[0-9a-f]{40})",
              "只能缓存不可变的 Git 对象")
        if path not in self._objects:
            self._objects[path] = self.get(path)
        return self._objects[path]

    def pages(self, path, key=None):
        result = []
        for page in range(1, 101):
            value = self.get(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
            rows = value[key] if key else value
            check(isinstance(rows, list), "API 分页格式非法")
            result.extend(rows)
            if len(rows) < 100:
                return result
        raise ValueError("API 分页超过安全预算；拒绝不完整证据")

    def run(self, run_id, attempt=None):
        suffix = f"/attempts/{number(attempt)}" if attempt else ""
        run = self.get(f"{self.prefix}/actions/runs/{number(run_id)}" + suffix)
        check(run.get("id") == run_id and run.get("repository", {}).get("full_name") == self.repo
              and run.get("head_repository", {}).get("full_name") == self.repo, "原生 workflow run 仓库不符")
        if attempt:
            check(run.get("run_attempt") == attempt, "原生 attempt 不符")
        return run

    def jobs(self, run_id, attempt):
        run = self.run(run_id, attempt)
        boundary = timestamp(run.get("run_started_at"))
        rows = self.pages(f"{self.prefix}/actions/runs/{number(run_id)}/attempts/{number(attempt)}/jobs", "jobs")
        result, previous = [], None
        for row in rows:
            if row.get("run_id") != run_id or row.get("run_attempt") != attempt or not row.get("started_at"):
                continue
            start = timestamp(row["started_at"])
            if start < boundary:
                continue
            end = timestamp(row["completed_at"]) if row.get("completed_at") else None
            if end is None:
                if row.get("status") != "in_progress":
                    continue
            elif end < start or (attempt > 1 and end <= boundary):
                continue
            # REST can relabel inherited jobs (including their IDs). At the
            # second-resolution boundary, also exclude prior native identities.
            if attempt > 1 and start == boundary:
                if previous is None:
                    previous = self.jobs(run_id, attempt - 1)
                if any(old["id"] == row["id"] or (old["name"] == row["name"] and
                       timestamp(old["started_at"]) == start) for old in previous):
                    continue
            number(row["id"])
            result.append(row)
        return result

    def artifacts(self, run_id):
        return self.pages(f"{self.prefix}/actions/runs/{number(run_id)}/artifacts", "artifacts")

    def artifact(self, run_id, name, producer=None):
        rows = [row for row in self.artifacts(run_id) if row["name"] == name]
        check(len(rows) == 1, "缺少或重复的 artifact：" + name)
        row = rows[0]
        number(row["id"])
        check(row.get("expired") is False and re.fullmatch(r"sha256:[0-9a-f]{64}", row.get("digest", "")), "artifact 已过期或无原生摘要")
        check(row.get("workflow_run", {}).get("id") == run_id, "artifact 所属 run 不符")
        if producer:
            check(producer.get("run_id") == run_id and producer.get("status") == "completed"
                  and producer.get("conclusion") == "success", "artifact 生产作业不属于成功的本次 run")
            number(producer.get("run_attempt"))
            start, end = timestamp(producer.get("started_at")), timestamp(producer.get("completed_at"))
            created, updated = timestamp(row.get("created_at")), timestamp(row.get("updated_at"))
            check(start <= created <= updated <= end, "artifact 不是该 attempt 作业生成")
        return row

    def download(self, artifact, names, max_bytes=160 << 20):
        raw = self.request("GET", f"{self.prefix}/actions/artifacts/{number(artifact['id'])}/zip", binary=True)
        return unpack(raw, artifact["digest"], names, max_bytes)

    def receipt(self, run_id, name, producer=None):
        artifact = self.artifact(run_id, name, producer)
        raw = self.download(artifact, {"receipt.json"}, MAX_RECEIPT)["receipt.json"]
        return decode(raw), artifact, hashed(raw)


def native_candidate(run, repo=None):
    expected = repository(repo or run.get("repository", {}).get("full_name"))
    check(run.get("repository", {}).get("full_name") == expected
          and run.get("head_repository", {}).get("full_name") == expected, "原生 workflow run 仓库不符")
    check(run.get("path") == WORKFLOW and run.get("event") in ("push", "workflow_dispatch"), "不是受信任的候选工作流")
    branch = run.get("head_branch")
    if run["event"] == "workflow_dispatch":
        check(branch == "master", "原生 dispatch 不是 master")
    else:
        check(isinstance(branch, str), "原生标签 ref 缺失")
        rc_ref("refs/tags/" + branch)
    sha(run.get("head_sha"))


def scope(value, repo, sha_value, tree, run_id, attempt):
    check(value.get("schema") == 1 and value.get("repo") == repository(repo)
          and value.get("sha") == sha(sha_value) and value.get("tree") == sha(tree)
          and type(value.get("run_id")) is int and value["run_id"] == number(run_id)
          and type(value.get("run_attempt")) is int and value["run_attempt"] == number(attempt), "回执提交、树、run 或 attempt 不符")


def outputs(value):
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as stream:
        for key, item in value.items():
            item = json.dumps(item, separators=(",", ":")) if isinstance(item, (dict, list, bool)) else str(item)
            check("\n" not in item and "\r" not in item, "输出含换行")
            stream.write(f"{key}={item}\n")
