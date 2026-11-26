#!/usr/bin/env python3
"""One authenticated receiver, durable local fanout, no command execution from evidence."""
import argparse
from datetime import datetime, timezone
import io
import json
import os
import re
import select
import sqlite3
import sys
import time
from urllib.parse import quote, urlsplit
import zipfile

sys.dont_write_bytecode = True
from github import API, Unknown
from store import Key, ReceiverLock, Store

MERGE = ".github/workflows/merge-gate.yml"
PERIODIC = ".github/workflows/periodic-full.yml"
RELEASE = ".github/workflows/release-candidate.yml"
WORKFLOWS = {MERGE, PERIODIC, RELEASE}
BLOCKING = {"failure", "timed_out", "action_required"}
CONCLUSIONS = BLOCKING | {"success", "cancelled", "skipped", "neutral", "stale", "startup_failure"}
ACTIVE = {"queued", "in_progress", "waiting", "pending", "requested"}
ACTIONS = {"first_failure", "terminal", "job_details", "attribution", "release_binary_ready"}


def positive(value):
    return type(value) is int and value > 0


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


def test_name(value):
    return (isinstance(value, str) and re.fullmatch(r"(?:Test|Example|Fuzz).{1,240}", value) is not None
            and all(ord(c) >= 32 for c in value))


def extract_tests(text):
    tests = set()
    for line in text.splitlines():
        match = re.search(r"(?:^|\s)--- FAIL: (.{1,240}?) \([0-9.]+s\)(?:\s|$)", line)
        if match and test_name(match[1]):
            tests.add((match[1], ""))
        start = line.find("{")
        if start >= 0:
            try:
                item = json.loads(line[start:])
            except (ValueError, RecursionError):
                continue
            if not isinstance(item, dict) or item.get("Action") != "fail":
                continue
            name, package = item.get("Test"), item.get("Package")
            if (test_name(name) and (package is None or isinstance(package, str) and re.fullmatch(r"[A-Za-z0-9_./-]{1,240}", package))):
                tests.add((name, package or ""))
        if len(tests) >= 1000:
            break
    return [{"name": name, "package": package or None} for name, package in sorted(tests)]


def edge_hint(line):
    if len(line) > 8192:
        return False
    try:
        item = json.loads(line)
        return isinstance(item, dict) and item.get("event") in {"workflow_job.completed", "workflow_run.completed", "run.completed"}
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

    def __enter__(self):
        self.lock = ReceiverLock(self.store.path)
        self.lock.__enter__()
        return self

    def __exit__(self, *args):
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

    def trusted_source(self, item):
        workflow = self.api.get(self.root + f"/actions/workflows/{item['workflow_id']}")
        require(workflow.get("id") == item["workflow_id"] and workflow.get("path") == item["path"])
        event, branch, path = item.get("event"), item.get("head_branch"), item["path"]
        if path == MERGE:
            require(event == "pull_request")
            return
        require(item.get("head_repository", {}).get("full_name") == self.repo)
        if path == PERIODIC:
            require(branch == "master" and event in {"schedule", "workflow_dispatch", "push"})
        elif event == "workflow_dispatch":
            require(branch == "master")
        else:
            require(event == "push" and isinstance(branch, str) and re.fullmatch(r"rc/[A-Za-z0-9_./-]+", branch) is not None)
            ref = self.api.get(self.root + "/git/ref/tags/" + quote(branch, safe="/"))
            require(ref.get("ref") == "refs/tags/" + branch)
            obj = ref.get("object", {})
            for _ in range(5):
                if obj.get("type") != "tag":
                    break
                require(sha(obj.get("sha")))
                obj = self.api.get(self.root + "/git/tags/" + obj["sha"]).get("object", {})
            require(obj.get("type") == "commit" and obj.get("sha") == item["head_sha"])
        self.contains(item["head_sha"], self.master_sha())

    def jobs(self, run):
        items = self.api.pages(self.root + f"/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs", "jobs")
        unique = {}
        for item in items:
            require(isinstance(item, dict) and positive(item.get("id")))
            require(item.get("run_id") == run["id"] and type(item.get("run_id")) is int and
                    item.get("run_attempt") == run["run_attempt"] and type(item.get("run_attempt")) is int and
                    item.get("head_sha") == run["head_sha"])
            require(item.get("status") in ACTIVE | {"completed"})
            trusted_url(item.get("html_url"), self.repo, f"actions/runs/{run['id']}/job/{item['id']}")
            if item.get("status") == "completed" and item.get("conclusion") in CONCLUSIONS:
                timestamp(item.get("completed_at"))
            if item["id"] in unique:
                require(unique[item["id"]] == item)
            unique[item["id"]] = item
        return list(unique.values())

    def valid_pr(self, item, number, merged=False, commit=None):
        require(isinstance(item, dict) and type(item.get("number")) is int and item["number"] == number)
        require(item.get("base", {}).get("ref") == "master" and item.get("base", {}).get("repo", {}).get("full_name") == self.repo)
        trusted_url(item.get("html_url"), self.repo, f"pull/{number}")
        if merged:
            require(item.get("merge_commit_sha") == commit and isinstance(item.get("merged_at"), str))
            timestamp(item["merged_at"])

    def merge_recipients(self, run):
        recipients, number = [self.coordinator], None
        try:
            linked = run.get("pull_requests")
            require(isinstance(linked, list) and len(linked) == 1 and positive(linked[0].get("number")))
            number = linked[0]["number"]
            item = self.api.get(self.root + f"/pulls/{number}")
            self.valid_pr(item, number)
            require(linked[0].get("head", {}).get("sha") == run["head_sha"] == item.get("head", {}).get("sha"))
            require(item.get("head", {}).get("repo", {}).get("full_name") == run.get("head_repository", {}).get("full_name"))
            author = self.store.author(self.repo, number)
            if author:
                recipients.append(author)
            self.store.problem(f"route:{run['id']}:{run['run_attempt']}", None if author else "本地作者指派缺失或冲突；仅交协调者")
        except (Unknown, AttributeError, TypeError):
            number = None
            self.store.problem(f"route:{run['id']}:{run['run_attempt']}", "PR 来源或 head 无法核实；仅交协调者")
        return recipients, number

    def receipt(self, run, kind):
        name = f"{kind}-{run['id']}-{run['run_attempt']}"
        artifacts = self.api.pages(self.root + f"/actions/runs/{run['id']}/artifacts", "artifacts")
        matches = [a for a in artifacts if isinstance(a, dict) and a.get("name") == name]
        require(len(matches) == 1, "目标或归因证据尚未唯一绑定，等待后补")
        artifact = matches[0]
        require(positive(artifact.get("id")) and artifact.get("expired") is False)
        require(artifact.get("workflow_run", {}).get("id") == run["id"] and
                artifact.get("workflow_run", {}).get("head_sha") == run["head_sha"])
        limit = 128 * 1024 * 1024 if kind == "release-target" else 1024 * 1024
        body = self.api.blob(self.root + f"/actions/artifacts/{artifact['id']}/zip", limit=limit)
        try:
            with zipfile.ZipFile(io.BytesIO(body)) as archive:
                names = archive.namelist()
                allowed = [{"receipt.json"}]
                if kind == "release-target":
                    allowed.append({"receipt.json", "workspace"})
                require(len(names) == len(set(names)) and set(names) in allowed)
                receipt_member = archive.getinfo("receipt.json")
                require(receipt_member.file_size <= 128 * 1024)
                data = json.loads(archive.read(receipt_member))
        except (ValueError, UnicodeError, zipfile.BadZipFile, RuntimeError):
            raise Unknown("证据压缩包无效，保持未知") from None
        require(isinstance(data, dict) and type(data.get("schema")) is int and data["schema"] == 1 and
                data.get("kind") == kind and data.get("repo") == self.repo and
                type(data.get("run_id")) is int and data["run_id"] == run["id"] and
                type(data.get("run_attempt")) is int and data["run_attempt"] == run["run_attempt"] and
                sha(data.get("sha")) and sha(data.get("tree")))
        if kind == "release-target":
            if run.get("event") == "push":
                require(data["sha"] == run["head_sha"], "候选目标与已核实的 rc 标签不一致，保持未知")
            if "workflow_sha" in data:
                require(sha(data["workflow_sha"]))
                self.contains(data["workflow_sha"], self.master_sha())
                if data["workflow_sha"] != run["head_sha"]:
                    expected = f"{self.repo}/{RELEASE}@refs/heads/master"
                    require(any(isinstance(r, dict) and r.get("path") == expected and r.get("sha") == data["workflow_sha"]
                                for r in run.get("referenced_workflows", [])), "工作流源码 SHA 缺少受信 API master 引用")
        commit = self.api.get(self.root + "/commits/" + data["sha"])
        require(commit.get("sha") == data["sha"] and commit.get("commit", {}).get("tree", {}).get("sha") == data["tree"])
        self.contains(data["sha"], self.master_sha())
        if kind == "periodic-full":
            require(data.get("status") in {"success", "failure", "skipped"} and type(data.get("verified")) is bool)
            require(data["status"] != "failure" or data["verified"] is False)
            require(data["status"] != "success" or data["verified"] is True)
        self.store.bind_target(self.repo, run["id"], run["run_attempt"], kind, data["sha"], data["tree"])
        return data

    def attribution(self, run, receipt, payload):
        attr = receipt.get("attribution", {})
        require(receipt.get("status") == "failure" and isinstance(attr, dict) and attr.get("status") == "confirmed")
        require(positive(attr.get("pr")) and all(sha(attr.get(k)) for k in ("good_sha", "bad_sha", "commit")))
        require(attr["good_sha"] != attr["commit"])
        self.contains(attr["good_sha"], attr["commit"])
        self.contains(attr["commit"], attr["bad_sha"])
        self.contains(attr["bad_sha"], receipt["sha"])
        associated = self.api.pages(self.root + f"/commits/{attr['commit']}/pulls")
        candidates = []
        for item in associated:
            try:
                require(positive(item.get("number")))
                self.valid_pr(item, item["number"], merged=True, commit=attr["commit"])
                candidates.append(item)
            except (Unknown, AttributeError):
                continue
        require(len(candidates) == 1 and candidates[0]["number"] == attr["pr"], "合入 master 的 PR 不唯一，禁止转发归因")
        item = self.api.get(self.root + f"/pulls/{attr['pr']}")
        self.valid_pr(item, attr["pr"], merged=True, commit=attr["commit"])
        recipients = [self.coordinator]
        author = self.store.author(self.repo, attr["pr"])
        if author:
            recipients.append(author)
        detail = dict(payload, tested_sha=receipt["sha"], tested_tree=receipt["tree"], pr=attr["pr"],
                      good_sha=attr["good_sha"], bad_sha=attr["bad_sha"], commit=attr["commit"],
                      evidence_url=run["html_url"] + f"/attempts/{run['run_attempt']}", pr_url=item["html_url"])
        key = Key(self.repo, run["head_sha"], run["id"], run["run_attempt"], "@attribution")
        self.store.event(key, detail)
        self.store.publish(key, "attribution", detail, recipients)
        return attr["pr"]

    def process(self, run, jobs, deliver):
        self.trusted_source(run)
        workflow, attempt, run_id = run["path"], run["run_attempt"], run["id"]
        tested = None
        control = run["head_sha"]
        scope_sha = run["head_sha"]
        if workflow == RELEASE:
            target = self.receipt(run, "release-target")
            tested = scope_sha = target["sha"]
            control = target.get("workflow_sha", run["head_sha"])
        recipients, pr = self.merge_recipients(run) if workflow == MERGE else ([self.coordinator], None)
        base = dict(source_head=run["head_sha"], workflow_sha=control, tested_sha=tested, workflow=workflow, pr=pr)
        delivery_scope = (self.repo, scope_sha, run_id, attempt)
        failures = []
        for item in jobs:
            if item["status"] == "completed" and item.get("conclusion") not in CONCLUSIONS:
                continue
            key = Key(self.repo, scope_sha, run_id, attempt, str(item["id"]))
            payload = dict(base, status=item["status"], conclusion=item.get("conclusion"), completed_at=item.get("completed_at"),
                           job_id=item["id"], job_url=item["html_url"], job_name=str(item.get("name", ""))[:256],
                           logs_pending=item.get("conclusion") in BLOCKING, tests=[])
            payload = self.store.event(key, payload)
            if payload.get("status") == "completed" and payload.get("conclusion") in BLOCKING:
                failures.append((key, payload))
        failures.sort(key=lambda entry: (timestamp(entry[1]["completed_at"]), int(entry[0].job)))
        if failures:
            key, payload = failures[0]
            self.store.publish(key, "first_failure", payload, recipients)
        if workflow == RELEASE:
            ready = [j for j in jobs if j.get("name") == "Release candidate binary"]
            if len(ready) == 1 and ready[0]["status"] == "completed" and ready[0].get("conclusion") == "success":
                item = ready[0]
                payload = dict(base, job_id=item["id"], job_url=item["html_url"],
                               message="候选二进制已就绪，请协调者按本地权威指派续办真实本机 6A/B/C；此通知不是验收结果")
                self.store.publish(Key(self.repo, scope_sha, run_id, attempt, str(item["id"])), "release_binary_ready", payload, recipients)
        terminal = (run["status"] == "completed" and run.get("conclusion") in CONCLUSIONS and
                    all(j["status"] == "completed" and j.get("conclusion") in CONCLUSIONS for j in jobs))
        if run.get("conclusion") == "success":
            terminal = terminal and bool(jobs) and all(j.get("conclusion") in {"success", "skipped", "neutral"} for j in jobs)
        if terminal:
            key = Key(self.repo, scope_sha, run_id, attempt, "@terminal")
            payload = dict(base, status="completed", conclusion=run["conclusion"], job_id=None, synthetic=not failures,
                           failed_jobs=[dict(job_id=p["job_id"], job_url=p["job_url"]) for _, p in failures])
            self.store.event(key, payload)
            self.store.publish(key, "terminal", payload, recipients)
        # Initial blocking notices must not wait for log or localization downloads.
        if deliver:
            self.store.deliver(delivery_scope, self.coordinator, pr)
        for key, payload in failures:
            if not payload.get("logs_pending"):
                self.store.publish(key, "job_details", payload, recipients)
                continue
            scope = f"logs:{run_id}:{attempt}:{key.job}"
            try:
                text = self.api.blob(self.root + f"/actions/jobs/{key.job}/logs").decode("utf-8", errors="replace")
                detail = dict(payload, logs_pending=False, tests=extract_tests(text))
                self.store.event(key, detail)
                self.store.publish(key, "job_details", detail, recipients)
                self.store.problem(scope)
            except Unknown as error:
                self.store.problem(scope, str(error))
        if workflow == PERIODIC and failures:
            scope = f"attribution:{run_id}:{attempt}"
            try:
                receipt = self.receipt(run, "periodic-full")
                pr = self.attribution(run, receipt, base)
                self.store.problem(scope)
            except Unknown as error:
                self.store.problem(scope, str(error))
        if deliver:
            self.store.deliver(delivery_scope, self.coordinator, pr)
        inconclusive = (any(j["status"] == "completed" and j.get("conclusion") not in CONCLUSIONS for j in jobs)
                        or run["status"] == "completed" and not terminal)
        self.store.problem(f"conclusion:{run_id}:{attempt}", "运行或作业结论未知、未齐或相互矛盾，稍后重试" if inconclusive else None)
        return not inconclusive

    def reconcile(self, deliver=True):
        if self.lock is None:
            raise RuntimeError("对账必须持有单接收进程锁")
        self.master = None
        try:
            identity = self.api.get("/user", conditional=False)
            require(isinstance(identity, dict) and positive(identity.get("id")) and isinstance(identity.get("login"), str) and bool(identity["login"]),
                    "缺少已认证身份，保持未知")
            summaries = self.api.pages(self.root + "/actions/runs", "workflow_runs")
            require(all(isinstance(x, dict) for x in summaries))
            run_ids = self.store.known_runs(self.repo) | {x["id"] for x in summaries if x.get("path") in WORKFLOWS and positive(x.get("id"))}
            self.store.problem("discovery")
        except Unknown as error:
            self.store.problem("discovery", str(error))
            return False
        healthy = True
        for run_id in sorted(run_ids, reverse=True):
            scope = f"run:{run_id}"
            try:
                current = self.api.get(self.root + f"/actions/runs/{run_id}")
                self.validate_run(current, run_id)
                self.store.problem(scope)
            except (Unknown, AttributeError, TypeError) as error:
                self.store.problem(scope, str(error) if isinstance(error, Unknown) else "API 运行形状无效，保持未知")
                healthy = False
                continue
            for attempt in range(1, current["run_attempt"] + 1):
                scope = f"run:{run_id}:attempt:{attempt}"
                try:
                    item = self.api.get(self.root + f"/actions/runs/{run_id}/attempts/{attempt}")
                    self.validate_run(item, run_id, attempt)
                    require(all(item.get(k) == current.get(k) for k in ("path", "workflow_id", "head_sha", "event")))
                    jobs = self.jobs(item)
                    self.store.observe(self.repo, run_id, attempt, dict(run=item, jobs=jobs))
                    complete = self.process(item, jobs, deliver)
                    healthy = healthy and complete
                    self.store.problem(scope)
                except (Unknown, ValueError, AttributeError, TypeError, KeyError) as error:
                    self.store.problem(scope, str(error) if isinstance(error, (Unknown, ValueError)) else "API attempt 形状无效，保持未知")
                    healthy = False
        return healthy


def watch(observer, interval, edge_stdin=False):
    require(5 <= interval <= 3600, "轮询间隔必须在 5 至 3600 秒之间")
    fd = sys.stdin.fileno() if edge_stdin else None
    buffer = b""
    failures = 0
    while True:
        healthy = observer.reconcile()
        failures = 0 if healthy else min(failures + 1, 4)
        started = time.monotonic()
        deadline = started + min(3600, interval * 2 ** failures)
        while time.monotonic() < deadline:
            remaining = deadline - time.monotonic()
            if fd is None:
                time.sleep(max(0, remaining))
            elif select.select([fd], [], [], max(0, remaining))[0]:
                chunk = os.read(fd, 4096)
                if not chunk:
                    fd = None  # A lost forwarder falls back to conditional polling.
                    continue
                buffer += chunk
                while b"\n" in buffer:
                    line, buffer = buffer.split(b"\n", 1)
                    if edge_hint(line):
                        deadline = min(deadline, started + 1)
                if len(buffer) > 8192:
                    buffer = b""


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
    waiting = commands.add_parser("wait", help="独立后台等待一项精确续办，不调用 GitHub")
    waiting.add_argument("--recipient", required=True)
    waiting.add_argument("--repo", required=True)
    waiting.add_argument("--sha", required=True)
    waiting.add_argument("--run-id", type=int, required=True)
    waiting.add_argument("--attempt", type=int, required=True)
    waiting.add_argument("--action", choices=sorted(ACTIONS), required=True)
    waiting.add_argument("--timeout", type=float, default=300)
    cancel = commands.add_parser("cancel-wait", help="取消本地等待登记，不确认投递或取消 CI")
    cancel.add_argument("--recipient", required=True)
    ack = commands.add_parser("ack", help="接收方已明确开始处理后确认指定回执")
    ack.add_argument("--recipient", required=True)
    ack.add_argument("--receipt", required=True)
    store = None
    try:
        args = parser.parse_args(argv)
        store = Store(args.db)
        command = args.command
        result = None
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
            result = store.inbox(args.recipient)
        elif command == "ack":
            store.ack(args.recipient, args.receipt)
        elif command == "cancel-wait":
            store.cancel_wait(args.recipient)
        elif command == "status":
            result = dict(problems=[dict(x) for x in store.db.execute("SELECT * FROM problems ORDER BY scope")],
                          outbox=store.outbox(), observations=store.db.execute("SELECT count(*) FROM observations").fetchone()[0])
        elif command == "wait":
            require(sha(args.sha) and positive(args.run_id) and positive(args.attempt) and args.repo == store.config()[0], "待续办身份无效")
            selector = dict(repo=args.repo, sha=args.sha, run_id=args.run_id, attempt=args.attempt, action=args.action)
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
