#!/usr/bin/env python3
"""One authenticated receiver, durable local fanout, no command execution from evidence."""
import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
import os
import re
import select
import sqlite3
import stat
import sys
import time
import unicodedata
from urllib.parse import quote, urlsplit
import zipfile

sys.dont_write_bytecode = True
from github import API, Unknown
from store import Key, ReceiverLock, Store

MERGE = ".github/workflows/merge-gate.yml"
PERIODIC = ".github/workflows/periodic-full.yml"
RELEASE = ".github/workflows/release-candidate.yml"
PUBLISHER = ".github/workflows/merge-conflict.yml"
WORKFLOWS = {MERGE, PERIODIC, RELEASE, PUBLISHER}
BLOCKING = {"failure", "timed_out", "action_required"}
CONCLUSIONS = BLOCKING | {"success", "cancelled", "skipped", "neutral", "stale", "startup_failure"}
ACTIVE = {"queued", "in_progress", "waiting", "pending", "requested"}
ACTIONS = {"first_failure", "terminal", "job_details", "attribution", "release_binary_ready", "release_full_ready",
           "release_tag_forward", "merge_gate_failure", "merge_gate_result"}


def positive(value):
    return type(value) is int and 0 < value < 2 ** 63


def sha(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{40}", value) is not None


def require(condition, message="受信 API 身份或证据不完整，保持未知"):
    if not condition:
        raise Unknown(message)


def timestamp(value):
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
        require(result.tzinfo is not None)
        return result.astimezone(timezone.utc)
    except (ValueError, AttributeError, TypeError):
        raise Unknown("作业完成时间无效，保持未知") from None


def trusted_url(value, repo, suffix):
    require(isinstance(value, str))
    parsed = urlsplit(value)
    require(parsed.scheme == "https" and parsed.netloc == "github.com" and
            parsed.path == f"/{repo}/{suffix}" and not parsed.query and not parsed.fragment)
    return value


def safe_text(value):
    value = re.sub(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|.)", "", value)
    if value.isascii():
        return re.sub(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]", "", value)
    return "".join(c for c in value if c in "\t\r\n" or unicodedata.category(c) not in {"Cc", "Cf", "Cs", "Zl", "Zp"})


def test_name(value):
    return (isinstance(value, str) and re.fullmatch(r"(?:Test|Example|Fuzz).{1,240}", value) is not None
            and all(ord(c) >= 32 for c in value))


class FailureSignatures:
    """Bounded Go JSON subset of periodic-replay.failure_records; other forms stay unknown."""
    package = re.compile(r"agentworkspace/(?:cmd/workspace|internal/[A-Za-z][A-Za-z0-9_]*)")
    test = re.compile(r"Test[A-Z_][A-Za-z0-9_]*")
    problem = re.compile(r"panic:|timed out|deadline exceeded|i/o timeout|fatal error:|signal: killed|\[build failed\]", re.I)

    def __init__(self):
        self.groups, self.failed, self.bad = {}, set(), set()
        self.unknown = self.overflow = False
        self.size = self.messages = self.named = 0

    @staticmethod
    def unique_pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError("duplicate Go JSON field")
            result[key] = value
        return result

    def feed(self, line):
        self.size += len(line)
        if self.size > 16 * 1024 * 1024 or self.overflow:
            self.overflow = True
            return
        line = re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z ", "", line).rstrip()
        if not line.startswith("{"):
            self.unknown |= bool(self.problem.search(line))
            return
        try:
            row = json.loads(line, object_pairs_hook=self.unique_pairs)
        except (ValueError, RecursionError):
            self.unknown = True
            return
        if not isinstance(row, dict):
            return
        package, name, action = row.get("Package"), row.get("Test"), row.get("Action")
        if not isinstance(action, str):
            self.unknown = True
            return
        if not isinstance(package, str) or not self.package.fullmatch(package):
            self.unknown |= action in {"fail", "build-fail"}
            return
        if "Test" in row and not isinstance(name, str):
            self.bad.add(package)
            return
        if action == "fail" and not name:
            self.failed.add(package)
        output = row.get("Output", "")
        if isinstance(output, str) and self.problem.search(output):
            self.bad.add(package)
        if not isinstance(name, str) or not self.test.fullmatch(name.split("/", 1)[0]):
            self.unknown |= action == "fail" and bool(name)
            return
        key = (package, name.split("/", 1)[0])
        if key not in self.groups and len(self.groups) >= 1000:
            self.overflow = True
            return
        group = self.groups.setdefault(key, {"tests": set(), "messages": set()})
        if action == "fail":
            self.named += name not in group["tests"]
            if self.named > 1000 or len(name) > 240 or name != safe_text(name):
                self.overflow = True
                return
            group["tests"].add(name)
        if action == "output" and isinstance(output, str):
            for line in output.splitlines():
                match = re.match(r"\s*(?:[^\s:]+/)*[^\s:]+\.go:\d+(?::\d+)?:\s*(.+)", line)
                if match:
                    message = match[1].strip()
                    if not message:
                        continue
                    if len(message) > 4096 or message != safe_text(message):
                        self.bad.add(package)
                        continue
                    if message not in group["messages"]:
                        self.messages += len(message)
                        if self.messages > 1024 * 1024:
                            self.overflow = True
                            return
                        group["messages"].add(message)

    def result(self, include_signature=False):
        records, covered = [], set()
        if self.overflow:
            return {"records": [], "unknown": True}
        for (package, test), group in sorted(self.groups.items()):
            if group["tests"] and group["messages"] and package in self.failed and package not in self.bad:
                signature = {key: sorted(values) for key, values in group.items()}
                record = dict(package=package, test=test, signature_sha256=hashlib.sha256(json.dumps(signature, sort_keys=True).encode()).hexdigest())
                if include_signature:
                    record["signature"] = signature
                records.append(record)
                covered.add(package)
        unresolved = self.unknown or bool(self.bad or self.failed - covered) or any(
            group["tests"] and (package not in self.failed or not group["messages"]) for (package, _), group in self.groups.items())
        return {"records": records, "unknown": bool(unresolved or not records)}


class TestLog:
    def __init__(self):
        self.tests = {}
        self.timeout = self.running = False
        self.packages = set()
        self.signatures = FailureSignatures()

    def add(self, name, package=None, outcome="failed"):
        name = safe_text(name) if isinstance(name, str) else name
        if test_name(name) and (package is None or isinstance(package, str) and re.fullmatch(r"[A-Za-z0-9_./-]{1,240}", package)):
            key = (name, package or "")
            if len(self.tests) < 1000 or key in self.tests:
                old = self.tests.get(key)
                self.tests[key] = "failed" if old == "failed" else outcome

    def feed(self, line, nested=False, package=None):
        if not nested:
            self.signatures.feed(line)
        line = re.sub(r"^\d{4}-\d\d-\d\dT\S+\s+", "", safe_text(line)).strip()
        start = line.find("{")
        if start >= 0:
            try:
                item = json.loads(line[start:])
            except (ValueError, RecursionError):
                item = None
            if isinstance(item, dict):
                package = item.get("Package")
                if item.get("Action") == "fail":
                    self.add(item.get("Test"), package)
                    if isinstance(package, str) and re.fullmatch(r"[A-Za-z0-9_./-]{1,240}", package) and len(self.packages) < 100:
                        self.packages.add(package)
                if not nested and isinstance(item.get("Output"), str):
                    for output in item["Output"].splitlines():
                        self.feed(output, nested=True, package=package)
                return  # JSON framing is not an output line and must not reset timeout state.
        if "panic: test timed out after " in line:
            self.timeout = True
        if self.timeout and line == "running tests:":
            self.running = True
        elif self.running:
            match = re.fullmatch(r"(.{1,240}?) \([0-9.]+s\)", line)
            if match:
                self.add(match[1], package, outcome="running_at_timeout")
            elif line:
                self.running = False
        match = re.search(r"(?:^|\s)--- FAIL: (.{1,240}?) \([0-9.]+s\)(?:\s|$)", line)
        if match:
            self.add(match[1], package)

    def result(self):
        named = {name for name, package in self.tests if package}
        return [dict(name=name, package=package or None, outcome=outcome)
                for (name, package), outcome in sorted(self.tests.items()) if package or name not in named]


class FirstNamesReady(Exception):
    """Stop a streaming first read after authenticated named evidence is available."""


def extract_tests(text):
    parser = TestLog()
    for line in text.splitlines():
        parser.feed(line)
    return parser.result()


def edge_hint(line):
    if len(line) > 1024 * 1024:
        return False
    try:
        item = json.loads(line)
        return isinstance(item, dict) and (item.get("event") in {"workflow_job.completed", "workflow_run.completed", "run.completed"}
                or item.get("action") == "completed" and any(isinstance(item.get(k), dict) for k in ("workflow_job", "workflow_run")))
    except (ValueError, TypeError, RecursionError):
        return False


class Observer:
    def __init__(self, store, api=None):
        self.store = store
        self.api = api or API(store)
        self.repo, self.coordinator = store.config()
        self.root = f"/repos/{self.repo}"
        self.lock = None
        self.master = None
        self.first = True
        self.sender = None
        self.native_heads = {}

    def pump(self):
        from codex_receiver import pump
        require(not self.store.db.in_transaction, "原生投递不能在 SQLite 写事务中启动")
        self.sender = pump(self.store) or self.sender

    def __enter__(self):
        self.lock = ReceiverLock(self.store.path)
        self.lock.__enter__()
        return self

    def __exit__(self, *args):
        try:
            if self.sender is not None:
                from codex_receiver import QUEUE_TIMEOUT
                self.sender.join(QUEUE_TIMEOUT + 1)
        finally:
            self.lock.__exit__(*args)
            self.lock = None

    def validate_run(self, item, run_id, attempt=None):
        require(isinstance(item, dict) and item.get("id") == run_id and positive(item.get("id")))
        require(item.get("repository", {}).get("full_name") == self.repo)
        require(item.get("path") in WORKFLOWS and positive(item.get("workflow_id")))
        require(positive(item.get("run_attempt")) and item["run_attempt"] <= 1000 and sha(item.get("head_sha")))
        if attempt is not None:
            require(item["run_attempt"] == attempt)
        require(item.get("status") in ACTIVE | {"completed"})
        trusted_url(item.get("html_url"), self.repo, f"actions/runs/{run_id}")

    def master_sha(self):
        if self.master is None:
            item = self.api.get(self.root + "/commits/master")
            require(isinstance(item, dict) and sha(item.get("sha")))
            self.master = item["sha"]
        return self.master

    def contains(self, ancestor, descendant):
        require(sha(ancestor) and sha(descendant))
        item = self.api.get(self.root + f"/compare/{ancestor}...{descendant}")
        require(isinstance(item, dict) and item.get("status") == ("identical" if ancestor == descendant else "ahead")
                and item.get("merge_base_commit", {}).get("sha") == ancestor,
                "提交包含关系无法核实，保持未知")

    def release_commit(self, run):
        if run.get("event") != "push":
            return run["head_sha"]
        from rc_refs import valid_ref
        branch = run.get("head_branch")
        require(isinstance(branch, str) and valid_ref("refs/tags/" + branch))
        ref = self.api.get(self.root + "/git/ref/tags/" + quote(branch, safe="/"))
        require(isinstance(ref, dict) and ref.get("ref") == "refs/tags/" + branch)
        obj, seen = ref.get("object", {}), set()
        require(isinstance(obj, dict))
        raw = obj.get("sha")
        for depth in range(9):
            require(isinstance(obj, dict) and sha(obj.get("sha")))
            if obj.get("type") == "commit":
                break
            require(obj.get("type") == "tag" and depth < 8 and obj["sha"] not in seen, "标签对象类型、循环或深度无效，保持未知")
            seen.add(obj["sha"])
            tag = self.api.get(self.root + "/git/tags/" + obj["sha"])
            require(isinstance(tag, dict) and tag.get("sha") == obj["sha"])
            obj = tag.get("object", {})
        require(obj.get("type") == "commit" and run["head_sha"] in {raw, obj["sha"]})
        head = run.get("head_commit")
        require(run["head_sha"] == obj["sha"] or isinstance(head, dict), "原始标签对象缺少原生提交绑定")
        if head is not None:
            require(isinstance(head, dict) and head.get("id") == obj["sha"] and sha(head.get("tree_id")))
            commit = self.api.get(self.root + "/git/commits/" + obj["sha"])
            require(isinstance(commit, dict) and commit.get("sha") == obj["sha"] and
                    isinstance(commit.get("tree"), dict) and commit["tree"].get("sha") == head["tree_id"])
        return obj["sha"]

    def trusted_source(self, item):
        workflow = self.api.get(self.root + f"/actions/workflows/{item['workflow_id']}")
        require(workflow.get("id") == item["workflow_id"] and workflow.get("path") == item["path"])
        event, branch, path = item.get("event"), item.get("head_branch"), item["path"]
        if path == MERGE and event == "pull_request":
            return
        if path == MERGE:
            require(event == "workflow_dispatch" and branch == "master" and item.get("head_repository", {}).get("full_name") == self.repo)
            self.contains(item["head_sha"], self.master_sha())
            return
        require(item.get("head_repository", {}).get("full_name") == self.repo)
        if path == PERIODIC:
            require(branch == "master" and event in {"schedule", "workflow_dispatch", "push"})
        elif event == "workflow_dispatch":
            require(branch == "master")
        else:
            require(path == RELEASE and event == "push")
            commit = self.release_commit(item)
            self.contains(commit, self.master_sha())
            if not hasattr(self, "native_heads"):
                self.native_heads = {}
            self.native_heads[(item.get("id"), item.get("run_attempt"))] = commit
            return
        self.contains(item["head_sha"], self.master_sha())

    def origin(self, run, item):
        try:
            start = timestamp(run.get("run_started_at"))
            finished = timestamp(item["completed_at"]) if item.get("completed_at") else None
            began = timestamp(item["started_at"]) if item.get("started_at") else None
            if finished and finished < start:
                return "inherited" if began is None or began <= finished else "unknown"
            if item.get("run_attempt", run["run_attempt"]) < run["run_attempt"]:
                return "unknown"
            if began and began >= start and (not finished or finished >= began):
                return "current"
            if run["run_attempt"] == 1 and began is None and finished and finished >= start:
                return "current"
        except Unknown:
            pass
        return "unknown"

    def jobs(self, run, on_page=None):
        unique = {}
        endpoint = self.root + f"/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs"
        for batch in self.api.batches(endpoint, "jobs"):
            invalid = False
            for item in batch:
                try:
                    require(isinstance(item, dict) and positive(item.get("id")))
                    require(item.get("run_id") == run["id"] and type(item.get("run_id")) is int and
                            positive(item.get("run_attempt")) and item["run_attempt"] <= run["run_attempt"] and
                            item.get("head_sha") in {run["head_sha"], self.native_heads.get((run["id"], run["run_attempt"]), run["head_sha"])})
                    require(item.get("status") in ACTIVE | {"completed"})
                    trusted_url(item.get("html_url"), self.repo, f"actions/runs/{run['id']}/job/{item['id']}")
                    if item.get("status") == "completed" and item.get("conclusion") in CONCLUSIONS:
                        timestamp(item.get("completed_at"))
                    if item["id"] in unique:
                        require(unique[item["id"]] == item)
                    unique[item["id"]] = item
                except Unknown:
                    invalid = True
            if on_page:
                on_page(list(unique.values()))
            require(not invalid, "部分作业身份无效；已核实的独立首报不受影响")
        return list(unique.values())

    def valid_pr(self, item, number, merged=False, commit=None):
        require(isinstance(item, dict) and type(item.get("number")) is int and item["number"] == number)
        base = item.get("base", {})
        require(base.get("repo", {}).get("full_name") == self.repo and isinstance(base.get("ref"), str)
                and re.fullmatch(r"[^\x00-\x20\x7f\\]{1,1024}", base["ref"]) is not None)
        trusted_url(item.get("html_url"), self.repo, f"pull/{number}")
        if merged:
            require(base["ref"] == "master")
            require(item.get("merge_commit_sha") == commit and isinstance(item.get("merged_at"), str))
            timestamp(item["merged_at"])

    def merge_recipients(self, run):
        recipients, number = [self.coordinator], None
        try:
            linked = run.get("pull_requests", [])
            require(isinstance(linked, list))
            if not linked and positive(run.get("check_suite_id")):
                suite = self.api.get(self.root + f"/check-suites/{run['check_suite_id']}")
                require(suite.get("head_sha") == run["head_sha"])
                linked = suite.get("pull_requests", [])
                require(isinstance(linked, list))
            if not linked:
                associated = self.api.pages(self.root + f"/commits/{run['head_sha']}/pulls")
                candidates = {}
                for candidate in associated:
                    try:
                        require(positive(candidate.get("number")))
                        self.valid_pr(candidate, candidate["number"])
                        require(candidate.get("head", {}).get("sha") == run["head_sha"] and
                                candidate.get("head", {}).get("repo", {}).get("full_name") == run.get("head_repository", {}).get("full_name"))
                        candidates[candidate["number"]] = candidate
                    except (Unknown, AttributeError):
                        continue
                linked = list(candidates.values())
            require(len(linked) == 1 and positive(linked[0].get("number")))
            number = linked[0]["number"]
            item = self.api.get(self.root + f"/pulls/{number}")
            self.valid_pr(item, number)
            require(linked[0].get("head", {}).get("sha") == run["head_sha"] == item.get("head", {}).get("sha"))
            require(item.get("head", {}).get("repo", {}).get("full_name") == run.get("head_repository", {}).get("full_name"))
            source = self.pr_source(item)
            if source["repo_id"] is not None:
                require(positive(source["repo_id"]))
            if source["ref"] is not None:
                require(source["ref"] == run.get("head_branch"))
            native_id = run.get("head_repository", {}).get("id")
            require(native_id is None or native_id == source["repo_id"] and positive(native_id))
            self.store.bind_pr(self.repo, run["id"], run["run_attempt"], dict(pr=number, source_head=run["head_sha"],
                               workflow=run["path"], event=run["event"], source=source))
            author = self.store.author(self.repo, number)
            if author:
                recipients.append(author)
            self.store.problem(f"route:{run['id']}:{run['run_attempt']}", None if author else "本地作者指派缺失或冲突；仅交协调者")
        except (Unknown, ValueError, AttributeError, TypeError):
            number = None
            self.store.problem(f"route:{run['id']}:{run['run_attempt']}", "PR 来源或 head 无法核实；仅交协调者")
        return recipients, number

    def identity_steps(self, run, producer, artifact):
        require(run["path"] == MERGE and positive(producer.get("id")) and type(producer.get("run_id")) is int and
                producer["run_id"] == run["id"] and type(producer.get("run_attempt")) is int and
                producer["run_attempt"] == run["run_attempt"] and producer.get("head_sha") == run["head_sha"] and
                producer.get("status") in {"in_progress", "completed"})
        steps = producer.get("steps")
        require(isinstance(steps, list) and 0 < len(steps) <= 1000, "早期身份缺少原生步骤证据")
        require(all(isinstance(step, dict) and positive(step.get("number")) for step in steps))
        require(len({step["number"] for step in steps}) == len(steps), "原生步骤编号冲突")
        identity = [step for step in steps if step.get("name") == "Preserve true merge identity before fallible planning"]
        require(len(identity) == 1, "早期身份生成步骤不唯一")
        identity = identity[0]
        upload = [step for step in steps if step["number"] == identity["number"] + 1 and step.get("name") == "Run actions/upload-artifact@v7"]
        require(len(upload) == 1, "早期身份上传步骤不匹配")
        upload = upload[0]
        require(all(step.get("status") == "completed" and step.get("conclusion") == "success" for step in (identity, upload)),
                "早期身份生成或上传未成功，保持未知")
        require(timestamp(run.get("run_started_at")) <= timestamp(producer.get("started_at")) <=
                timestamp(identity.get("started_at")) <= timestamp(identity.get("completed_at")) <=
                timestamp(upload.get("started_at")) <= timestamp(artifact.get("created_at")) <= timestamp(upload.get("completed_at")),
                "早期身份不在已核实的生成和上传时窗内")
        if producer.get("status") == "completed":
            require(timestamp(upload["completed_at"]) <= timestamp(producer.get("completed_at")))
        if artifact.get("updated_at") is not None:
            require(timestamp(artifact["created_at"]) <= timestamp(artifact["updated_at"]) <= timestamp(upload["completed_at"]))
        fields = ("number", "name", "started_at", "completed_at")
        return dict(producer_job_id=producer["id"], identity_step={k: identity[k] for k in fields}, upload_step={k: upload[k] for k in fields})

    def receipt(self, run, kind, jobs):
        producers = {"merge-plan": "Merge plan", "merge-identity": "Merge plan", "release-intent": "Release normalize",
                     "periodic-full-plan": "Resolve trusted master", "periodic-full-initial": "Periodic Full evidence",
                     "periodic-full": "Periodic attribution evidence"}
        require(kind in producers)
        producer = [j for j in jobs if j.get("name") == producers[kind]]
        require(len(producer) == 1 and self.origin(run, producer[0]) == "current", "证据 producer 不属于本 attempt")
        producer = producer[0]
        if kind != "merge-identity":
            require(producer.get("status") == "completed" and producer.get("conclusion") == "success",
                    "证据 producer 未在本 attempt 成功执行，等待后补")
        name = f"{kind}-{run['id']}-{run['run_attempt']}"
        artifacts = self.api.pages(self.root + f"/actions/runs/{run['id']}/artifacts", "artifacts")
        matches = [a for a in artifacts if isinstance(a, dict) and a.get("name") == name]
        require(len(matches) == 1, "目标或归因证据尚未唯一绑定，等待后补")
        artifact = matches[0]
        require(positive(artifact.get("id")) and artifact.get("expired") is False)
        native_control = run["head_sha"]
        artifact_heads = {native_control}
        if kind == "release-intent" and run["path"] == RELEASE and run.get("event") == "push":
            native_control = self.release_commit(run)
            self.contains(native_control, self.master_sha())
            artifact_heads.add(native_control)
        origin = artifact.get("workflow_run")
        require(isinstance(origin, dict) and positive(origin.get("id")) and origin["id"] == run["id"] and
                sha(origin.get("head_sha")) and origin["head_sha"] in artifact_heads)
        created = timestamp(artifact.get("created_at"))
        step_proof = None
        if kind == "merge-identity":
            step_proof = self.identity_steps(run, producer, artifact)
        else:
            require(timestamp(run.get("run_started_at")) <= created and
                    timestamp(producer.get("started_at")) <= created <= timestamp(producer.get("completed_at")),
                    "artifact 时间不属于本 attempt 的指定 producer")
        limit = (2 if kind in {"release-intent", "merge-plan", "merge-identity", "periodic-full-plan"} else 32) * 1024 * 1024
        body = self.api.blob(self.root + f"/actions/artifacts/{artifact['id']}/zip", limit=limit)
        require(artifact.get("digest") == "sha256:" + hashlib.sha256(body).hexdigest(), "artifact 摘要无法核实")
        member = "plan.json" if kind == "periodic-full-plan" else kind + ".json" if kind in {"merge-plan", "merge-identity"} else "receipt.json"
        try:
            with zipfile.ZipFile(io.BytesIO(body)) as archive:
                names = archive.namelist()
                require(len(names) == len(set(names)) and member in names and len(names) <= 4096)
                for info in archive.infolist():
                    require(not info.filename.startswith("/") and "\\" not in info.filename and
                            ".." not in info.filename.split("/") and "\x00" not in info.filename and
                            stat.S_IFMT(info.external_attr >> 16) in {0, stat.S_IFREG, stat.S_IFDIR})
                if kind in {"release-intent", "merge-identity"}:
                    require(names == [member], "只读取精确的轻量身份或 intent，不读取候选二进制包")
                info = archive.getinfo(member)
                require(not info.is_dir() and info.file_size <= (65536 if kind == "merge-identity" else 2 * 1024 * 1024) and not info.flag_bits & 1)
                raw = archive.read(info)
                data = json.loads(raw, object_pairs_hook=FailureSignatures.unique_pairs if kind == "merge-identity" else None)
        except (ValueError, UnicodeError, zipfile.BadZipFile, RuntimeError, KeyError):
            raise Unknown("证据压缩包无效，保持未知") from None
        require(isinstance(data, dict))
        identity = data.get("identity") if kind == "periodic-full-plan" else data
        require(isinstance(identity, dict) and identity.get("repo") == self.repo and sha(identity.get("sha")) and sha(identity.get("tree")))
        for key, expected in (("run_id", run["id"]), ("run_attempt", run["run_attempt"])):
            value = identity.get(key)
            if kind == "merge-identity":
                require(type(value) is str and value == str(expected))
            else:
                require(type(value) is int and value == expected or kind == "merge-plan" and type(value) is str and value == str(expected))
        control = run["head_sha"]
        if kind == "merge-identity":
            fields = {"schema", "kind", "repo", "pr", "head", "base", "sha", "tree", "run_id", "run_attempt", "source", "controller_sha", "policy_digest"}
            require(set(data) == fields and type(data["schema"]) is int and data["schema"] == 1 and data["kind"] == kind and
                    isinstance(data["policy_digest"], str) and re.fullmatch(r"[0-9a-f]{64}", data["policy_digest"]) is not None,
                    "早期合并身份 schema 无效；不能解释为计划或测试通过")
        if kind in {"merge-plan", "merge-identity"}:
            require(run["path"] == MERGE and positive(identity.get("pr")) and sha(identity.get("head")) and sha(identity.get("base")))
            pr = self.api.get(self.root + f"/pulls/{identity['pr']}")
            self.valid_pr(pr, identity["pr"])
            require(pr.get("head", {}).get("sha") == identity["head"] and
                    pr.get("base", {}).get("sha") == identity.get("base") and pr.get("merge_commit_sha") == identity["sha"])
            if run.get("event") == "pull_request":
                require(identity["head"] == run["head_sha"] and
                        pr.get("head", {}).get("repo", {}).get("full_name") == run.get("head_repository", {}).get("full_name") and
                        data.get("controller_sha") in {None, ""})
            else:
                require(run.get("event") == "workflow_dispatch" and data.get("controller_sha") == run["head_sha"])
            if run.get("event") == "workflow_dispatch" or "source" in data:
                require(isinstance(data.get("source"), dict) and positive(data["source"].get("repo_id")) and
                        data["source"] == self.pr_source(pr) and positive(pr["head"]["repo"].get("id")))
            if kind == "merge-identity":
                require(pr.get("state") == "open" and pr.get("merged") is False and pr.get("merged_at") is None, "早期身份的 PR 已不是当前开放来源")
                require(isinstance(data.get("source"), dict) and set(data["source"]) == {"repo_id", "repo", "ref", "base_ref"})
                require(all(isinstance(data["source"].get(k), str) and 0 < len(data["source"][k]) <= 1024 and
                            data["source"][k] == safe_text(data["source"][k]) and not re.search(r"[\x00-\x20\x7f\\]", data["source"][k]) for k in ("ref", "base_ref")))
                if run.get("event") == "pull_request":
                    route = self.store.meta_get(f"pr-route:{self.repo}:{run['id']}:{run['run_attempt']}", {})
                    require(data["controller_sha"] == "" and route.get("pr") == data["pr"] and
                            route.get("source_head") == run["head_sha"] and route.get("event") == "pull_request" and
                            route.get("source") == data["source"], "早期身份与原生 PR 来源不符")
        elif kind == "periodic-full-plan":
            require(run["path"] == PERIODIC and identity.get("head") == identity["sha"] and sha(identity.get("base")))
        else:
            require(type(data.get("schema")) is int and data["schema"] == 1 and
                    data.get("kind") == ("periodic-full" if kind == "periodic-full-initial" else kind))
            if kind == "release-intent":
                require(run["path"] == RELEASE and sha(data.get("workflow_sha")))
                control = data["workflow_sha"]
                if run.get("event") == "push":
                    require(data["sha"] == native_control, "发布目标与已核实的标签提交不符")
                self.contains(control, self.master_sha())
                if control not in {run["head_sha"], native_control}:
                    expected = f"{self.repo}/{RELEASE}@refs/heads/master"
                    require(any(isinstance(r, dict) and r.get("path") == expected and r.get("sha") == control
                                for r in run.get("referenced_workflows", [])), "工作流源码 SHA 缺少受信 API master 引用")
            else:
                require(run["path"] == PERIODIC and data.get("status") in {"success", "failure", "skipped"} and type(data.get("verified")) is bool)
                require(data["status"] != "failure" or data["verified"] is False)
                require(data["status"] != "success" or data["verified"] is True)
        commit = self.api.get(self.root + "/commits/" + identity["sha"])
        require(commit.get("sha") == identity["sha"] and commit.get("commit", {}).get("tree", {}).get("sha") == identity["tree"])
        if kind in {"merge-plan", "merge-identity"}:
            require([p.get("sha") for p in commit.get("parents", [])] == [identity["base"], identity["head"]])
        else:
            self.contains(identity["sha"], self.master_sha())
            if kind == "periodic-full-plan":
                require(bool(commit.get("parents")) and commit["parents"][0].get("sha") == identity["base"])
        proof = dict(source_head=run["head_sha"], workflow_sha=control, provenance=kind, artifact_id=artifact["id"], digest=artifact["digest"])
        if kind in {"merge-plan", "merge-identity"}:
            proof.update(pr=identity["pr"], pr_head=identity["head"], source=data.get("source"))
            previous = self.store.target(self.repo, run["id"], run["run_attempt"]) or {}
            if kind == "merge-identity":
                proof["early_identity"] = dict(step_proof, schema=1, artifact_id=artifact["id"], artifact_name=name, digest=artifact["digest"],
                                               identity_sha256=hashlib.sha256(raw).hexdigest(), policy_digest=data["policy_digest"],
                                               run_id=run["id"], run_attempt=run["run_attempt"], source_head=run["head_sha"],
                                               controller_sha=data["controller_sha"], sha=identity["sha"], tree=identity["tree"])
                require(not previous.get("early_identity") or previous["early_identity"] == proof["early_identity"], "已核实的早期身份发生冲突，保持未知")
            elif previous.get("early_identity"):
                proof["early_identity"] = previous["early_identity"]
                require("policy_digest" not in data or data["policy_digest"] == previous["early_identity"]["policy_digest"],
                        "计划策略摘要与早期身份冲突，保持未知")
        self.store.bind_target(self.repo, run["id"], run["run_attempt"], kind, identity["sha"], identity["tree"], proof)
        return dict(data, sha=identity["sha"], tree=identity["tree"], workflow_sha=control)

    def publish(self, context, key, kind, payload, recipients=None, author_pr=None, cause=None):
        identity = self.store.publish(key, kind, payload, recipients if recipients is not None else context["recipients"], cause=cause)
        if identity:
            route = author_pr if author_pr is not None else context["pr"]
            if context["deliver"]:
                if self.store.deliver_actions({identity: route}, self.coordinator):
                    self.pump()
        return identity

    @staticmethod
    def pr_source(pr):
        return dict(repo_id=pr.get("head", {}).get("repo", {}).get("id"), repo=pr.get("head", {}).get("repo", {}).get("full_name"),
                    ref=pr.get("head", {}).get("ref"), base_ref=pr.get("base", {}).get("ref"))

    def dispatch_recipients(self, run, target):
        recipients, number = [self.coordinator], None
        scope = f"route:{run['id']}:{run['run_attempt']}"
        try:
            require(target is not None and positive(target.get("pr")) and sha(target.get("pr_head")))
            pr = self.api.get(self.root + f"/pulls/{target['pr']}")
            self.valid_pr(pr, target["pr"])
            require(pr.get("head", {}).get("sha") == target["pr_head"] and self.pr_source(pr) == target.get("source"))
            number = target["pr"]
            self.store.bind_pr(self.repo, run["id"], run["run_attempt"], dict(pr=number, source_head=run["head_sha"],
                               workflow=run["path"], event=run["event"], source=self.pr_source(pr)))
            author = self.store.author(self.repo, number)
            if author:
                recipients.append(author)
            self.store.problem(scope, None if author else "本地作者指派缺失或冲突；仅交协调者")
        except (Unknown, ValueError, AttributeError, TypeError):
            number = None
            self.store.problem(scope, "控制器的 PR 计划或当前来源未核实；仅交协调者")
        return recipients, number

    def prepare(self, run, deliver):
        self.trusted_source(run)
        recipients, pr = self.merge_recipients(run) if run["path"] == MERGE and run.get("event") == "pull_request" else ([self.coordinator], None)
        target = self.store.target(self.repo, run["id"], run["run_attempt"])
        if target:
            require(target.get("source_head") == run["head_sha"])
        if run["path"] == MERGE and run.get("event") == "workflow_dispatch":
            recipients, pr = self.dispatch_recipients(run, target)
        scope = self.store.root_sha(self.repo, run["id"], run["run_attempt"], run["head_sha"], target["sha"] if target else None)
        base = dict(source_head=run["head_sha"], workflow_sha=target["workflow_sha"] if target else None if run["path"] == RELEASE else run["head_sha"],
                    tested_sha=target["sha"] if target else None, tested_tree=target["tree"] if target else None,
                    target_unknown=target is None, provenance=target["provenance"] if target else "source_only",
                    workflow=run["path"], event=run.get("event"), pr=pr)
        if target and target.get("early_identity"):
            base["identity_evidence"] = target["early_identity"]
        summaries = {}
        for row in self.store.db.execute("SELECT job,payload FROM events WHERE repo=? AND sha=? AND run_id=? AND attempt=?", (self.repo, scope, run["id"], run["run_attempt"])):
            summary = json.loads(row["payload"]).get("log_summary")
            if isinstance(summary, dict):
                summaries[row["job"]] = summary
        return dict(run=run, base=base, scope=scope, pr=pr, recipients=recipients, deliver=deliver, log_seen=set(), log_summaries=summaries,
                    route_pending=run["path"] == MERGE and pr is None)

    def job_fact(self, context, item):
        run = context["run"]
        key = Key(self.repo, context["scope"], run["id"], run["run_attempt"], str(item["id"]))
        payload = dict(context["base"], status=item["status"], conclusion=item.get("conclusion"),
                       started_at=item.get("started_at"), completed_at=item.get("completed_at"),
                       work_origin=self.origin(run, item), job_id=item["id"], job_url=item["html_url"],
                       job_name=safe_text(str(item.get("name", "")))[:256], logs_pending=item.get("conclusion") in BLOCKING, tests=[])
        if key.job in context.get("log_summaries", {}):
            payload["log_summary"] = context["log_summaries"][key.job]
        return key, payload

    def job_payload(self, context, item):
        key, payload = self.job_fact(context, item)
        return key, self.store.event(key, payload)

    def needs_logs(self, context, payload):
        summary = payload.get("log_summary", {})
        return bool(payload.get("logs_pending") or
                    context["run"]["path"] == RELEASE and self.full_lane(payload.get("job_name"), "Release Full") and
                    (summary.get("signature_schema") != 1 or not summary.get("complete")))

    def read_logs(self, context, key, payload, timeout=5, first=False):
        if key.job in context["log_seen"] or not self.needs_logs(context, payload):
            return payload
        context["log_seen"].add(key.job)
        parser = TestLog()
        for item in payload.get("tests", []):
            parser.add(item["name"], item.get("package"), item.get("outcome", "failed"))
        summary = payload.get("log_summary", {})
        parser.timeout = bool(summary.get("timeout"))
        parser.packages.update(summary.get("failed_packages", []))
        def consume(line):
            parser.feed(line)
            if first and parser.tests and hasattr(getattr(self.api, "transport", None), "stream"):
                raise FirstNamesReady
        scope = f"logs:{key.run_id}:{key.attempt}:{key.job}"
        problem = "日志读取已停止、超时或截断；详情稍后补齐"
        try:
            complete = self.api.lines(self.root + f"/actions/jobs/{key.job}/logs", consume, timeout=timeout)
        except FirstNamesReady:
            complete = False
        except Unknown as error:
            complete, problem = False, str(error)
        detail = dict(payload, logs_pending=not complete, tests=parser.result(),
                      log_summary=dict(complete=complete, timeout=parser.timeout, failed_packages=sorted(parser.packages),
                                       signature_schema=1, signatures=parser.signatures.result()))
        context.setdefault("log_summaries", {})[key.job] = detail["log_summary"]
        self.store.problem(scope, None if complete else problem)
        return self.store.event(key, detail)

    def notice(self, context, jobs):
        from release_duplicates import classify
        classify(self, context, jobs)
        if context.get("release_duplicate"):
            return
        run = context["run"]
        failures = [j for j in jobs if j["status"] == "completed" and j.get("conclusion") in BLOCKING and self.origin(run, j) == "current"]
        failures.sort(key=lambda j: (timestamp(j["completed_at"]), j["id"]))
        old = self.store.db.execute("SELECT * FROM actions WHERE repo=? AND run_id=? AND attempt=? AND kind='first_failure'", (self.repo, run["id"], run["run_attempt"])).fetchone()
        if old:
            if json.loads(old["payload"]).get("tested_sha") and context["base"]["tested_sha"] is None:
                return  # An old target-bound pending action needs its target reverified first.
            failures = [j for j in failures if str(j["id"]) == old["job"]]
        if not failures:
            return
        key, payload = self.job_payload(context, failures[0])
        delivery = self.store.db.execute("SELECT COUNT(*),COUNT(i.receipt_id) FROM outbox o LEFT JOIN inbox i ON i.receipt_id=o.receipt_id WHERE o.action_id=?", (old["id"],)).fetchone() if old else (0, 0)
        prepared, delivered = bool(delivery[0]), bool(delivery[1])
        # Reserve the verified winner durably, without any deliverable recipient, before log I/O.
        if not self.store.publish(key, "first_failure", payload, []):
            return
        if key.job not in context["log_seen"] and self.needs_logs(context, payload):
            self.pump()  # Previously prepared native deliveries must not wait behind this download.
        payload = self.read_logs(context, key, payload, timeout=15 if delivered else 5, first=not delivered)
        identity = self.publish(context, key, "first_failure", payload)
        if prepared and json.loads(old["payload"]).get("logs_pending") and not payload.get("logs_pending"):
            detail = dict(payload, update_of=identity, detail_kind="logs")
            self.publish(context, key, "job_details", detail)

    def attribution(self, context, evidence_run, receipt, kind="periodic-full"):
        attr = receipt.get("attribution", {})
        require(receipt.get("status") == "failure" and isinstance(attr, dict))
        items = attr.get("items", [attr])
        require(isinstance(items, list) and len(items) <= 1000)
        confirmed, unknown = 0, False
        for index, item in enumerate(items):
            scope = f"attribution:{context['run']['id']}:{context['run']['run_attempt']}:{index}"
            try:
                require(isinstance(item, dict) and item.get("status") == "confirmed", "此项归因尚未确认")
                self.attribution_item(context, evidence_run, receipt, kind, item)
                self.store.problem(scope)
                confirmed += 1
            except (Unknown, AttributeError, TypeError) as error:
                self.store.problem(scope, str(error) if isinstance(error, Unknown) else "归因项形状无效")
                unknown = True
        return confirmed > 0 and not unknown

    def attribution_item(self, context, evidence_run, receipt, kind, attr, failure=None):
        require(positive(attr.get("pr")) and all(sha(attr.get(k)) for k in ("good_sha", "bad_sha", "commit")))
        require(attr["good_sha"] != attr["commit"])
        self.contains(attr["good_sha"], attr["commit"])
        self.contains(attr["commit"], attr["bad_sha"])
        self.contains(attr["bad_sha"], receipt["sha"])
        require(context["base"]["tested_sha"] == receipt["sha"], "归因与当前 tested SHA 不符")
        associated = self.api.pages(self.root + f"/commits/{attr['commit']}/pulls")
        candidates = {}
        for item in associated:
            try:
                require(positive(item.get("number")))
                self.valid_pr(item, item["number"], merged=True, commit=attr["commit"])
                candidates[item["number"]] = item
            except (Unknown, AttributeError):
                continue
        require(len(candidates) == 1 and attr["pr"] in candidates, "合入 master 的 PR 不唯一，禁止转发归因")
        item = self.api.get(self.root + f"/pulls/{attr['pr']}")
        self.valid_pr(item, attr["pr"], merged=True, commit=attr["commit"])
        recipients = [self.coordinator]
        author = self.store.author(self.repo, attr["pr"])
        owner = (self.repo, attr["commit"], attr["pr"], author)
        if author:
            recipients.append(author)
        run = context["run"]
        detail = dict(context["base"], pr=attr["pr"], good_sha=attr["good_sha"], bad_sha=attr["bad_sha"], commit=attr["commit"],
                      evidence_run_id=evidence_run["id"], evidence_attempt=evidence_run["run_attempt"],
                      evidence_url=evidence_run["html_url"] + f"/attempts/{evidence_run['run_attempt']}", pr_url=item["html_url"])
        job = f"@attribution:{attr['commit']}:{attr['pr']}"
        if failure is not None:
            detail.update(failure=failure, job_id=failure["job_id"], job_name=failure["job_name"],
                          tests=[dict(name=failure["test"], package=failure["package"], outcome="failed")])
            digest = hashlib.sha256(json.dumps(failure, sort_keys=True).encode()).hexdigest()[:16]
            job += ":" + digest
        legacy = self.store.db.execute("SELECT sha,payload FROM actions WHERE repo=? AND run_id=? AND attempt=? AND job='@attribution' AND kind='attribution'", (self.repo, run["id"], run["run_attempt"])).fetchone()
        scope_sha = context["scope"]
        if failure is None and legacy and all(json.loads(legacy["payload"]).get(k) == attr[k] for k in ("commit", "pr")):
            scope_sha, job = legacy["sha"], "@attribution"
        key = Key(self.repo, scope_sha, run["id"], run["run_attempt"], job)
        self.store.event(key, detail)
        saved = dict(run_id=evidence_run["id"], attempt=evidence_run["run_attempt"], kind=kind, tested_sha=receipt["sha"])
        self.publish(context, key, "attribution", detail, recipients, attr["pr"], cause=(owner, saved))

    @staticmethod
    def full_lane(name, prefix):
        if isinstance(name, str) and name.startswith(prefix + " / "):
            lane = name[len(prefix) + 3:]
            if re.fullmatch(r"Go [A-Za-z0-9_ -]{1,200}", lane):
                return lane
        return None

    def source_signatures(self, run, job, expected, tested_sha):
        require(isinstance(expected, str) and re.fullmatch(r"[0-9a-f]{64}", expected) is not None, "来源日志摘要缺失")
        identity = dict(run_id=run["id"], attempt=run["run_attempt"], source_head=run["head_sha"], tested_sha=tested_sha,
                        job={k: job.get(k) for k in ("id", "run_id", "run_attempt", "head_sha", "name", "started_at", "completed_at", "status", "conclusion")})
        key = f"failure-proof:{self.repo}:{run['id']}:{run['run_attempt']}:{job['id']}"
        cached = self.store.meta_get(key, {})
        # The caller reauthenticates the run, completed job and receipt before reusing this immutable proof.
        if cached.get("schema") == 1 and cached.get("identity") == identity and cached.get("log_sha256") == expected:
            return cached["signatures"]["records"]
        body = self.api.blob(self.root + f"/actions/jobs/{job['id']}/logs", limit=16 * 1024 * 1024)
        require(hashlib.sha256(body).hexdigest() == expected, "来源日志与归因摘要不符")
        parser = FailureSignatures()
        for line in io.BytesIO(body):
            require(len(line) <= 65536, "来源日志行超限，归因保持未知")
            parser.feed(line.decode("utf-8"))
        signatures = parser.result(include_signature=True)
        self.store.meta_put(key, dict(schema=1, identity=identity, log_sha256=expected, signatures=signatures))
        return signatures["records"]

    def reuse_attribution(self, context, jobs):
        current, complete = {}, True
        for job in jobs:
            if job["status"] != "completed" or job.get("conclusion") not in BLOCKING or self.origin(context["run"], job) != "current":
                continue
            _, payload = self.job_payload(context, job)
            lane = self.full_lane(job.get("name"), "Release Full")
            summary = payload.get("log_summary", {})
            signatures = summary.get("signatures", {})
            records = signatures.get("records", []) if lane and job.get("conclusion") == "failure" and summary.get("signature_schema") == 1 and summary.get("complete") and not payload.get("logs_pending") else []
            complete = complete and bool(records) and not signatures.get("unknown", True)
            for record in records:
                key = (job["id"], record["package"], record["test"], record["signature_sha256"])
                current[key] = dict(record, lane=lane, job_id=job["id"], job_name=job["name"])
        if not current:
            return False
        origins, matches = set(), {key: [] for key in current}
        for row in self.store.db.execute("SELECT body FROM causes WHERE repo=?", (self.repo,)):
            body = json.loads(row[0])
            if body.get("tested_sha") == context["base"]["tested_sha"]:
                origins.add((body["run_id"], body["attempt"], body["kind"]))
        for run_id, attempt, kind in sorted(origins):
            scope = f"reuse-source:{context['run']['id']}:{context['run']['run_attempt']}:{run_id}:{attempt}"
            try:
                item = self.api.get(self.root + f"/actions/runs/{run_id}/attempts/{attempt}")
                self.validate_run(item, run_id, attempt)
                require(item["path"] == PERIODIC and kind in {"periodic-full", "periodic-full-initial"})
                self.trusted_source(item)
                source_jobs = self.jobs(item)
                receipt = self.receipt(item, kind, source_jobs)
                require(receipt.get("status") == "failure" and receipt["sha"] == context["base"]["tested_sha"] and
                        receipt["tree"] == context["base"]["tested_tree"], "归因受测对象与当前发布不符")
                attr = receipt.get("attribution", {})
                require(isinstance(attr, dict))
                items = attr.get("items", [attr])
                require(isinstance(items, list) and len(items) <= 1000)
                for index, attr in enumerate(items):
                    item_scope = scope + f":{index}"
                    try:
                        require(isinstance(attr, dict) and attr.get("status") == "confirmed" and positive(attr.get("pr")) and
                                all(sha(attr.get(k)) for k in ("good_sha", "bad_sha", "commit")), "此项归因尚未确认")
                        candidate = attr.get("candidate", {})
                        require(isinstance(candidate, dict))
                        sources = attr.get("sources", candidate.get("sources"))
                        require(isinstance(sources, list) and 0 < len(sources) <= 1000, "缺少逐项原生失败来源")
                        bound, seen = [], set()
                        for source in sources:
                            require(isinstance(source, dict) and positive(source.get("id")) and source["id"] not in seen and
                                    type(source.get("run_id")) is int and source["run_id"] == run_id and
                                    type(source.get("run_attempt")) is int and source["run_attempt"] == attempt)
                            seen.add(source["id"])
                            native = [job for job in source_jobs if job["id"] == source["id"]]
                            require(len(native) == 1)
                            job = native[0]
                            lane = self.full_lane(job.get("name"), "Full suite")
                            require(lane is not None and job["status"] == "completed" and job.get("conclusion") == "failure" and
                                    self.origin(item, job) == "current" and source.get("name", job["name"]) == job["name"])
                            records = self.source_signatures(item, job, source.get("log_sha256"), receipt["sha"])
                            records = [r for r in records if r["test"] == candidate.get("test") and r["package"] == candidate.get("package")]
                            require(len(records) == 1, "来源不是完整具名测试失败")
                            record = records[0]
                            expected = candidate.get("signature_sha256")
                            if "signature" in candidate:
                                require(candidate["signature"] == record["signature"], "来源断言文本不符")
                                expected = expected or record["signature_sha256"]
                            require(expected == record["signature_sha256"], "归因签名摘要与原生失败不符")
                            if "candidate_sha256" in attr:
                                original = {k: v for k, v in candidate.items() if k != "signature_sha256"}
                                original.update(signature=record["signature"], sources=sources)
                                require(attr["candidate_sha256"] == hashlib.sha256(json.dumps(original, sort_keys=True).encode()).hexdigest(), "归因候选身份不符")
                            bound.append(dict(record, lane=lane, source_job_id=job["id"], source_job_name=job["name"]))
                        for key, failure in current.items():
                            same = [r for r in bound if all(r[k] == failure[k] for k in ("lane", "package", "test", "signature_sha256"))]
                            if len(same) == 1:
                                matches[key].append((item, receipt, kind, attr, dict(failure, source_job_id=same[0]["source_job_id"], source_job_name=same[0]["source_job_name"])))
                        self.store.problem(item_scope)
                    except (Unknown, ValueError, KeyError, TypeError, AttributeError) as error:
                        self.store.problem(item_scope, str(error) if isinstance(error, Unknown) else "逐项来源或签名无效；不猜测归因")
                self.store.problem(scope)
            except (Unknown, ValueError, KeyError, TypeError, AttributeError) as error:
                self.store.problem(scope, str(error) if isinstance(error, Unknown) else "归因来源无效；不猜测归因")
        for candidates in matches.values():
            owners = {(entry[3].get("commit"), entry[3].get("pr")) for entry in candidates}
            if len(owners) != 1:
                complete = False
                continue
            item, receipt, kind, attr, failure = candidates[0]
            try:
                self.attribution_item(context, item, receipt, kind, attr, failure=failure)
            except (Unknown, ValueError, KeyError, TypeError, AttributeError):
                complete = False
        return complete

    def terminal(self, context, jobs):
        run = context["run"]
        valid = (run["status"] == "completed" and run.get("conclusion") in CONCLUSIONS and
                 all(j["status"] == "completed" and j.get("conclusion") in CONCLUSIONS for j in jobs))
        if run.get("conclusion") == "success":
            valid = valid and (run["path"] != MERGE or context["base"]["provenance"] == "merge-plan")
            valid = valid and not context["base"]["target_unknown"] and bool(jobs) and all(
                j.get("conclusion") in {"success", "skipped", "neutral"} and self.origin(run, j) == "current" for j in jobs)
        if valid:
            failures = [j for j in jobs if j.get("conclusion") in BLOCKING and self.origin(run, j) == "current"]
            payload = dict(context["base"], status="completed", conclusion=run["conclusion"], job_id=None,
                           synthetic=not failures, failed_jobs=[dict(job_id=j["id"], job_url=j["html_url"]) for j in failures],
                           inherited_jobs=[j["id"] for j in jobs if self.origin(run, j) == "inherited"])
            key = Key(self.repo, context["scope"], run["id"], run["run_attempt"], "@terminal")
            if context.get("release_duplicate"):
                from release_duplicates import reconcile_terminal
                payload.update(investigation=False, release_duplicate=context["release_duplicate"])
                reconcile_terminal(self.store, key, payload, context["recipients"])
            self.store.event(key, payload)
            self.publish(context, key, "terminal", payload)
        return bool(valid)

    def process(self, run, jobs, deliver, context=None):
        context = context or self.prepare(run, deliver)
        run_id, attempt, workflow = run["id"], run["run_attempt"], run["path"]
        self.notice(context, jobs)
        self.store.event_many([self.job_fact(context, item) for item in jobs])
        if context.get("release_duplicate"):
            terminal = self.terminal(context, jobs)
            self.process_pending = bool(context.get("release_duplicate_pending") or not terminal)
            return not self.process_pending
        if run.get("conclusion") != "success":
            self.terminal(context, jobs)  # Negative terminal notices do not wait for artifacts.
        kinds = {MERGE: ("merge-plan", "merge-identity"), RELEASE: ("release-intent",),
                 PERIODIC: ("periodic-full-plan", "periodic-full", "periodic-full-initial")}[workflow]
        receipts = {}
        errors = []
        for kind in kinds:
            try:
                receipts[kind] = self.receipt(run, kind, jobs)
                if kind == "merge-plan":
                    break
            except Unknown as error:
                errors.append(str(error))
        target = self.store.target(self.repo, run_id, attempt)
        if target:
            context["base"].update(tested_sha=target["sha"], tested_tree=target["tree"], workflow_sha=target["workflow_sha"],
                                   target_unknown=False, provenance=target["provenance"])
            if target.get("early_identity"):
                context["base"]["identity_evidence"] = target["early_identity"]
            if workflow == MERGE and run.get("event") == "workflow_dispatch":
                context["recipients"], context["pr"] = self.dispatch_recipients(run, target)
                context["base"]["pr"] = context["pr"]
                context["route_pending"] = context["pr"] is None
        self.store.problem(f"target:{run_id}:{attempt}", None if target else "; ".join(errors))
        if target:
            self.store.event_many([self.job_fact(context, item) for item in jobs])
        self.notice(context, jobs)
        first = self.store.db.execute("SELECT * FROM actions WHERE repo=? AND run_id=? AND attempt=? AND kind='first_failure'", (self.repo, run_id, attempt)).fetchone()
        if first and target and json.loads(first["payload"]).get("tested_sha") is None:
            payload = dict(json.loads(first["payload"]), **context["base"], update_of=first["id"], detail_kind="target_binding")
            self.publish(context, Key(self.repo, context["scope"], run_id, attempt, "@target"), "job_details", payload)
        failures = [j for j in jobs if j["status"] == "completed" and j.get("conclusion") in BLOCKING and self.origin(run, j) == "current"]
        pending_logs = False
        extra_read = False
        for item in failures:
            key, payload = self.job_payload(context, item)
            if first and str(item["id"]) != first["job"]:
                if self.needs_logs(context, payload) and not extra_read:
                    payload = self.read_logs(context, key, payload, timeout=15)
                    extra_read = True
                if not payload.get("logs_pending"):
                    subscribers = self.store.waiting_recipients(key, "job_details", payload, context["recipients"])
                    if subscribers:
                        self.publish(context, key, "job_details", dict(payload, update_of=first["id"], detail_kind="logs"), subscribers)
            pending_logs = pending_logs or payload.get("logs_pending", False)
        if workflow == RELEASE and target:
            self.contains(target["sha"], self.master_sha())
            for name, action in (("Release candidate binary", "release_binary_ready"), ("Release Full evidence", "release_full_ready")):
                ready = [j for j in jobs if j.get("name") == name]
                if len(ready) == 1 and ready[0]["status"] == "completed" and ready[0].get("conclusion") == "success" and self.origin(run, ready[0]) == "current":
                    item = ready[0]
                    payload = dict(context["base"], status="completed", conclusion="success", job_name=name,
                                   job_id=item["id"], job_url=item["html_url"], completed_at=item["completed_at"],
                                   message="发布阶段作业已成功；仅供既有授权调度，不代表 Full 验证、真机 6A/B/C 或最终验收")
                    self.publish(context, Key(self.repo, target["sha"], run_id, attempt, str(item["id"])), action, payload, [self.coordinator])
        attribution_pending = False
        if workflow in {PERIODIC, RELEASE} and failures:
            scope = f"attribution:{run_id}:{attempt}"
            try:
                candidates = [(k, v) for k, v in receipts.items() if k in {"periodic-full", "periodic-full-initial"}]
                if workflow == PERIODIC:
                    require(bool(candidates), "归因证据尚未发布")
                    kind, receipt = candidates[0]
                    attribution_pending = not self.attribution(context, run, receipt, kind)
                else:
                    require(target is not None, "发布目标尚未核实")
                    attribution_pending = not self.reuse_attribution(context, jobs)
                self.store.problem(scope, "部分归因未知；已确认项已独立投递" if attribution_pending else None)
            except Unknown as error:
                attribution_pending = True
                self.store.problem(scope, str(error))
        terminal = self.terminal(context, jobs)
        inconclusive = (any(j["status"] == "completed" and (j.get("conclusion") not in CONCLUSIONS or self.origin(run, j) == "unknown") for j in jobs)
                        or run["status"] == "completed" and not terminal)
        inherited = [j["id"] for j in jobs if self.origin(run, j) != "current"]
        self.store.problem(f"origin:{run_id}:{attempt}", "继承或执行时间未知的作业不能作为本 attempt 新动作：" + str(inherited) if inherited else None)
        self.store.problem(f"conclusion:{run_id}:{attempt}", "运行或作业结论未知、未齐或相互矛盾，稍后重试" if inconclusive else None)
        self.process_pending = bool(inconclusive or not target or pending_logs or attribution_pending or context["route_pending"])
        return not self.process_pending

    def seed(self):
        healthy = True
        for run_id in self.store.known_runs(self.repo):
            if self.store.db.execute("SELECT 1 FROM tracked_runs WHERE repo=? AND run_id=?", (self.repo, run_id)).fetchone():
                continue
            scope = f"legacy-source:{run_id}"
            try:
                row = self.store.db.execute("SELECT body FROM observations WHERE repo=? AND run_id=? ORDER BY attempt DESC LIMIT 1", (self.repo, run_id)).fetchone()
                if row:
                    summary = json.loads(row[0])["run"]
                else:
                    row = self.store.db.execute("SELECT payload FROM actions WHERE repo=? AND run_id=? LIMIT 1", (self.repo, run_id)).fetchone()
                    payload = json.loads(row[0])
                    summary = dict(id=run_id, path=payload["workflow"], head_sha=payload.get("source_head"), run_attempt=payload["attempt"])
                require(summary.get("path") in WORKFLOWS and sha(summary.get("head_sha")) and positive(summary.get("run_attempt")))
                self.store.track(self.repo, summary)
                self.store.problem(scope)
            except (Unknown, ValueError, KeyError, TypeError, AttributeError):
                self.store.problem(scope, "旧待办缺少可核实的来源字段；保留原记录，不影响其他来源")
                healthy = False
        dirty = self.store.db.execute("SELECT run_id FROM tracked_runs WHERE repo=? AND (dirty!=1 OR priority!=0) AND run_id IN (SELECT a.run_id FROM actions a JOIN outbox o ON a.id=o.action_id WHERE a.repo=? AND a.run_id IS NOT NULL AND o.state='pending')", (self.repo, self.repo)).fetchall()
        if dirty:
            with self.store.db:
                self.store.db.executemany("UPDATE tracked_runs SET dirty=1,priority=0 WHERE repo=? AND run_id=?", [(self.repo, r[0]) for r in dirty])
        return healthy

    def reconcile_run(self, run_id, deliver, force):
        scope = f"run:{run_id}"
        try:
            current = self.api.get(self.root + f"/actions/runs/{run_id}")
            self.validate_run(current, run_id)
            previous = self.store.db.execute("SELECT summary FROM tracked_runs WHERE repo=? AND run_id=?", (self.repo, run_id)).fetchone()
            require(not previous or current["run_attempt"] >= (json.loads(previous[0]).get("run_attempt") or 1), "运行 attempt 倒退，保持未知")
            changed = not previous or any(json.loads(previous[0]).get(k) != current.get(k) for k in ("run_attempt", "status", "conclusion"))
            self.store.track(self.repo, current)
            self.store.problem(scope)
        except (Unknown, ValueError, KeyError, AttributeError, TypeError) as error:
            self.store.problem(scope, str(error) if isinstance(error, Unknown) else "API 运行形状无效，保持未知")
            self.store.checked(self.repo, run_id, False, True)
            return False
        healthy, unfinished = True, set()
        candidates = []
        for attempt in range(1, current["run_attempt"] + 1):
            state = self.store.meta_get(f"attempt:{self.repo}:{run_id}:{attempt}", {})
            held = self.store.db.execute("SELECT 1 FROM outbox o JOIN actions a ON a.id=o.action_id WHERE a.repo=? AND a.run_id=? AND a.attempt=? AND o.state='pending' LIMIT 1", (self.repo, run_id, attempt)).fetchone()
            if not state.get("settled") or held:
                unfinished.add(attempt)
                if (force or held or state.get("retry_at", 0) <= time.time() or
                        attempt == current["run_attempt"] and (changed or current["status"] in ACTIVE)):
                    candidates.append((attempt, state))
        candidates.sort(key=lambda x: (x[0] != current["run_attempt"], x[1].get("checked_at", 0)))
        backlog = len(candidates) > 4
        for attempt, state in candidates[:4]:
            scope = f"run:{run_id}:attempt:{attempt}"
            settled, active = False, False
            try:
                item = self.api.get(self.root + f"/actions/runs/{run_id}/attempts/{attempt}")
                self.validate_run(item, run_id, attempt)
                active = item["status"] in ACTIVE
                require(all(item.get(k) == current.get(k) for k in ("path", "workflow_id", "head_sha", "event")))
                old = self.store.db.execute("SELECT body FROM observations WHERE repo=? AND run_id=? AND attempt=?", (self.repo, run_id, attempt)).fetchone()
                if old:
                    prior = json.loads(old[0])["run"]
                    require(all(prior.get(k) == item.get(k) for k in ("path", "workflow_id", "head_sha", "event")), "同一 attempt 的来源身份冲突，保持未知")
                if item["path"] == PUBLISHER:
                    from merge_reports import reconcile_publisher
                    jobs = self.api.pages(self.root + f"/actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
                    self.store.observe(self.repo, run_id, attempt, dict(run=item, jobs=jobs, jobs_fresh=True))
                    complete = reconcile_publisher(self, item, jobs, deliver=deliver)
                    self.process_pending = not complete
                else:
                    context = self.prepare(item, deliver)
                    jobs = self.jobs(item, lambda batch: self.notice(context, batch))
                    self.store.observe(self.repo, run_id, attempt, dict(run=item, jobs=jobs, jobs_fresh=True))
                    complete = self.process(item, jobs, deliver, context)
                healthy = healthy and complete
                settled = item["status"] == "completed" and not self.process_pending
                self.store.problem(scope)
            except (Unknown, ValueError, AttributeError, TypeError, KeyError) as error:
                self.store.problem(scope, str(error) if isinstance(error, (Unknown, ValueError)) else "API attempt 形状无效，保持未知")
                healthy = False
            tries = 0 if settled or active else min(7, state.get("tries", 0) + 1)
            retry_at = time.time() + min(3600, 30 * 2 ** (tries - 1)) if tries else 0
            checked_at = int(time.time() // 300) * 300 if active else time.time()
            self.store.meta_put(f"attempt:{self.repo}:{run_id}:{attempt}", dict(settled=settled, checked_at=checked_at, retry_at=retry_at, tries=tries))
            held = self.store.db.execute("SELECT 1 FROM outbox o JOIN actions a ON a.id=o.action_id WHERE a.repo=? AND a.run_id=? AND a.attempt=? AND o.state='pending' LIMIT 1", (self.repo, run_id, attempt)).fetchone()
            if settled and not held:
                unfinished.discard(attempt)
        self.store.checked(self.repo, run_id, current["status"] in ACTIVE or backlog, bool(unfinished))
        return healthy

    def reconcile(self, deliver=True, force=False):
        if self.lock is None:
            raise RuntimeError("对账必须持有单接收进程锁")
        try:
            return self._reconcile(deliver, force)
        finally:
            self.pump()  # Reconcile prior durable deliveries even during API outages.

    def _reconcile(self, deliver, force):
        self.master = None
        self.native_heads.clear()
        try:
            identity = self.api.get("/user")
            require(isinstance(identity, dict) and positive(identity.get("id")) and isinstance(identity.get("login"), str) and bool(identity["login"].strip()),
                    "缺少已认证身份，保持未知")
            self.store.problem("identity")
        except Unknown as error:
            self.store.problem("identity", str(error))
            return False
        force = force or self.first
        self.first = False
        seen, healthy = set(), self.seed()
        def drain():
            nonlocal healthy
            for row in self.store.queued(self.repo, force):
                run_id = row["run_id"]
                if run_id in seen or len(seen) >= 20:
                    continue
                seen.add(run_id)
                healthy = self.reconcile_run(run_id, deliver, force) and healthy
        drain()
        for workflow in (MERGE, PERIODIC, RELEASE, PUBLISHER):
            scope = "discovery:" + workflow
            metadata_known = False
            try:
                metadata = self.api.get(self.root + "/actions/workflows/" + workflow.rsplit("/", 1)[1])
                require(isinstance(metadata, dict) and metadata.get("path") == workflow and positive(metadata.get("id")))
                metadata_known = True
                endpoint = self.root + f"/actions/workflows/{metadata['id']}/runs"
                summaries, more, total = self.api.page(endpoint, "workflow_runs")
                def register(batch):
                    nonlocal healthy
                    for summary in batch:
                        if isinstance(summary, dict) and summary.get("path") == workflow and summary.get("workflow_id") == metadata["id"]:
                            try:
                                self.validate_run(summary, summary.get("id"))
                                self.store.track(self.repo, summary)
                            except Unknown as error:
                                self.store.problem(f"summary:{workflow}:{summary.get('id')}", str(error))
                                healthy = False
                    drain()
                register(summaries)
                key = f"collect:{self.repo}:{metadata['id']}"
                checkpoint = self.store.meta_get(key, {})
                page = checkpoint.get("page", 2) if checkpoint.get("total") == total else 2
                for _ in range(2):
                    if not more:
                        page = 2
                        break
                    batch, next_more, page_total = self.api.page(endpoint, "workflow_runs", page)
                    require(page_total == total, "历史采集期间总量改变，下轮从头核实")
                    register(batch)
                    page = page + 1 if next_more else 2
                    if not next_more:
                        break
                self.store.meta_put(key, dict(page=page, total=total, checked_at=int(time.time() // 300) * 300, unfiltered=True))
                self.store.problem(scope)
            except Unknown as error:
                absent = not metadata_known and error.status == 404
                self.store.problem(scope, None if absent else str(error))
                healthy = healthy and absent
        # A bounded direct audit also catches old reruns if an unchanged list omits their update.
        audit = self.store.db.execute("SELECT run_id FROM tracked_runs WHERE repo=? AND dirty=0 ORDER BY checked_at LIMIT 1", (self.repo,)).fetchone()
        if audit and audit[0] not in seen and len(seen) < 20:
            healthy = self.reconcile_run(audit[0], deliver, force) and healthy
        from rc_refs import reconcile_tags
        try:
            healthy = reconcile_tags(self, deliver=deliver, auth_context={"repo": self.repo, "identity": identity}) and healthy
        except (Unknown, ValueError, sqlite3.Error):
            self.store.problem("rc-refs", "标签采集失败；已核实的 workflow 动作不受影响")
            healthy = False
        return healthy


def watch(observer, interval, edge_stdin=False):
    require(5 <= interval <= 3600, "轮询间隔必须在 5 至 3600 秒之间")
    fd = sys.stdin.fileno() if edge_stdin else None
    buffer = b""
    dropping = False
    while True:
        observer.reconcile()
        started = time.monotonic()
        deadline = started + interval  # Per-source/API backoff must not delay unrelated work.
        while time.monotonic() < deadline:
            remaining = deadline - time.monotonic()
            if fd is None:
                time.sleep(max(0, remaining))
            elif select.select([fd], [], [], max(0, remaining))[0]:
                chunk = os.read(fd, 65536)
                if not chunk:
                    fd = None  # A lost forwarder falls back to conditional polling.
                    continue
                buffer += chunk
                while b"\n" in buffer:
                    line, buffer = buffer.split(b"\n", 1)
                    if not dropping and edge_hint(line):
                        deadline = min(deadline, started + 1)
                    dropping = False
                if len(buffer) > 1024 * 1024:
                    buffer = b""
                    dropping = True


class Parser(argparse.ArgumentParser):
    def error(self, _message):
        raise ValueError("命令参数无效；请使用 --help 查看用法")


def main(argv=None):
    parser = Parser(description="单进程 CI 观察器与本地持久收件箱")
    parser.add_argument("--db", required=True, help="私有 SQLite 文件")
    commands = parser.add_subparsers(dest="command", required=True, parser_class=Parser)
    init = commands.add_parser("init", help="登记本地权威部署绑定")
    init.add_argument("--repo", required=True)
    init.add_argument("--coordinator", required=True)
    assign = commands.add_parser("assign", help="登记受信 operator 的 PR 创建或指派回执")
    assign.add_argument("--repo", required=True)
    assign.add_argument("--pr", type=int, required=True)
    assign.add_argument("--author-session", required=True)
    assign.add_argument("--evidence", required=True)
    commands.add_parser("once", aliases=["reconcile"], help="先 API 对账，再持久投递")
    watcher = commands.add_parser("watch", help="单接收进程，条件轮询及可选边沿唤醒")
    watcher.add_argument("--interval", type=float, default=30)
    watcher.add_argument("--edge-stdin", action="store_true")
    commands.add_parser("status", help="查看未知原因与本地投递状态")
    inbox = commands.add_parser("inbox", help="只读消费本地收件箱，不自动确认")
    inbox.add_argument("--recipient", required=True)
    inbox.add_argument("--pending", action="store_true", help="仅返回已投递且未确认的收据")
    inbox.add_argument("--limit", type=int, help="按回执 ID 升序，限制为 1 至 256 条")
    inbox.add_argument("--after", help="排除该完整回执 ID 及之前的结果")
    inbox.add_argument("--receipt", help="仅查当前收件人的一条收据，包含已确认状态")
    waiting = commands.add_parser("wait", help="独立后台等待一项精确续办，不调用 GitHub")
    waiting.add_argument("--recipient", required=True)
    waiting.add_argument("--repo", required=True)
    waiting.add_argument("--sha", required=True)
    waiting.add_argument("--run-id", type=int)
    waiting.add_argument("--attempt", type=int)
    waiting.add_argument("--action", choices=sorted(ACTIONS), required=True)
    waiting.add_argument("--job", help="可选的精确 job 身份；tag 动作必须指定完整 @tag key")
    waiting.add_argument("--timeout", type=float, default=300)
    cancel = commands.add_parser("cancel-wait", help="取消本地等待登记，不确认投递或取消 CI")
    cancel.add_argument("--recipient", required=True)
    cancel.add_argument("--repo")
    cancel.add_argument("--sha")
    cancel.add_argument("--run-id", type=int)
    cancel.add_argument("--attempt", type=int)
    cancel.add_argument("--action", choices=sorted(ACTIONS))
    cancel.add_argument("--job")
    ack = commands.add_parser("ack", help="接收方已明确开始处理后确认指定回执")
    ack.add_argument("--recipient", required=True)
    ack.add_argument("--receipt", required=True)
    store = None
    try:
        args = parser.parse_args(argv)
        command = args.command
        store = Store(args.db, readonly=command in {"inbox", "status"},
                      initialize=command in {"init", "once", "reconcile", "watch"})
        result = None
        selector = None
        if command == "wait" or command == "cancel-wait" and any(getattr(args, k) is not None for k in ("repo", "sha", "run_id", "attempt", "action", "job")):
            require(sha(args.sha) and args.repo == store.config()[0] and args.action in ACTIONS, "待续办身份无效")
            if args.action == "release_tag_forward":
                from rc_refs import valid_ref
                require(args.run_id is None and args.attempt is None and isinstance(args.job, str) and args.job.startswith("@tag:"), "标签等待必须使用无 run 的精确 tag key")
                ref, separator, obj = args.job[5:].rpartition(":")
                require(separator and sha(obj) and valid_ref(ref), "标签等待身份无效")
            else:
                require(positive(args.run_id) and positive(args.attempt), "workflow 等待必须指定 run 与 attempt")
            selector = dict(repo=args.repo, sha=args.sha, run_id=args.run_id, attempt=args.attempt, action=args.action)
            if args.job is not None:
                require(isinstance(args.job, str) and 0 < len(args.job) <= 1500 and not any(ord(c) < 32 for c in args.job), "等待的 job 身份无效")
                selector["job"] = args.job
        if command == "init":
            store.configure(args.repo, args.coordinator)
        elif command == "assign":
            store.assign(args.repo, args.pr, args.author_session, args.evidence)
        elif command in {"once", "reconcile", "watch"}:
            with Observer(store) as observer:
                if command == "watch":
                    watch(observer, args.interval, args.edge_stdin)
                elif not observer.reconcile():
                    print("对账尚未完整；已保留未知及待投递记录，请查看 status 后重试", file=sys.stderr)
                    return 2
        elif command == "inbox":
            result = store.inbox(args.recipient, pending=args.pending, limit=args.limit, after=args.after, receipt_id=args.receipt)
        elif command == "ack":
            store.ack(args.recipient, args.receipt)
        elif command == "cancel-wait":
            store.cancel_wait(args.recipient, selector)
        elif command == "status":
            result = dict(problems=[dict(x) for x in store.db.execute("SELECT * FROM problems ORDER BY scope")],
                          outbox=store.outbox(), observations=store.db.execute("SELECT count(*) FROM observations").fetchone()[0])
        elif command == "wait":
            result = store.wait(args.recipient, selector, args.timeout)
            if result is None:
                print("等待已到期，登记仍保留；可重试或使用 cancel-wait", file=sys.stderr)
                return 3
        if result is not None:
            print(json.dumps(result, ensure_ascii=False, sort_keys=True))
        return 0
    except KeyboardInterrupt:
        return 130
    except (Unknown, ValueError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        return 2
    except (OSError, sqlite3.Error, zipfile.BadZipFile):
        print("本地文件、数据库、进程或证据不可用；请检查权限并保留数据库后重试", file=sys.stderr)
        return 2
    finally:
        if store is not None:
            store.close()


if __name__ == "__main__":
    sys.exit(main())
