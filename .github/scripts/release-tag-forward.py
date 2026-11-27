#!/usr/bin/env python3
"""One-shot historical rc-tag consumer, authorized by a private observer receipt."""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import subprocess
import sys
from urllib.parse import quote, urlencode

_spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
core = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(core)
control = core.load_script("release-control")
check = core.check
WORKFLOW_API = "/actions/workflows/release-candidate.yml"
ACTION = "release_tag_forward"
ACTIVE = ("queued", "in_progress", "waiting", "pending", "requested")


def canonical(value):
    # Exact encoding used by observer/store.py, including Unicode and no newline.
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def identity(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def timestamp(value):
    check(isinstance(value, str), "持久时间戳缺失")
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    check(parsed.tzinfo is not None, "持久时间戳没有时区")
    return parsed.astimezone(timezone.utc)


def private_file(path):
    path = Path(path)
    check(path.is_absolute() and not path.is_symlink(), "数据库必须是私有绝对路径且不能是符号链接")
    info = path.stat()
    check(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_mode & 0o077 == 0,
          "数据库必须是当前用户的私有普通文件（0600）")
    return path


def valid_tag(ref):
    prefix = "refs/tags/rc/"
    check(isinstance(ref, str) and ref.startswith(prefix) and 0 < len(ref[len(prefix):])
          and "/" not in ref[len(prefix):] and len(ref) <= 1024
          and not re.search(r"[\x00\ud800-\udfff]", ref), "标签必须精确匹配单层 rc/*")
    # Git is authoritative; argv (never shell) preserves Unicode, '+' and other legal names.
    result = subprocess.run(["git", "check-ref-format", ref], capture_output=True, timeout=10)
    check(result.returncode == 0, "Git 标签名称非法")


def load_receipt(path, repo, coordinator, receipt):
    path = private_file(path)
    core.digest(receipt)
    check(isinstance(coordinator, str) and 0 < len(coordinator) <= 512 and coordinator.strip()
          and all(ord(c) >= 32 for c in coordinator), "协调器身份非法")
    # mode=ro must not create, initialize, ACK, or otherwise mutate the observer DB.
    db = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True, timeout=5)
    db.row_factory = sqlite3.Row
    try:
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        config = db.execute("SELECT repo,coordinator FROM config WHERE id=1").fetchone()
        check(config is not None and tuple(config) == (repo, coordinator), "观察器仓库或协调器绑定不符")
        row = db.execute("SELECT a.*,o.state AS delivery_state,o.recipient AS out_recipient,"
                         "o.delivered_at AS out_delivered,i.recipient AS in_recipient,i.payload AS in_payload,"
                         "i.delivered_at AS in_delivered FROM inbox i JOIN outbox o ON o.receipt_id=i.receipt_id "
                         "JOIN actions a ON a.id=o.action_id WHERE i.receipt_id=?", (receipt,)).fetchone()
        check(row is not None and row["out_recipient"] == row["in_recipient"] == coordinator
              and row["delivery_state"] in ("delivered", "acked") and row["out_delivered"] and row["in_delivered"]
              and row["payload"] == row["in_payload"], "回执尚未持久投递、已被修改或收件人不符")
        payload = core.decode(row["payload"].encode(), core.MAX_RECEIPT)
        check(isinstance(payload, dict) and row["repo"] == payload.get("repo") == repo
              and row["kind"] == payload.get("action") == ACTION
              and row["run_id"] is None and row["attempt"] is None
              and "run_id" in payload and payload["run_id"] is None
              and "attempt" in payload and payload["attempt"] is None
              and "tested_sha" in payload and payload["tested_sha"] is None, "不是原生无 run 身份的标签动作")
        sha, obj, ref = core.sha(row["sha"]), core.sha(payload.get("tag_object")), payload.get("tag")
        valid_tag(ref)
        job = f"@tag:{ref}:{obj}"
        check(payload.get("sha") == payload.get("source_head") == sha and row["job"] == payload.get("job") == job
              and payload.get("workflow") == core.WORKFLOW and payload.get("native_workflow_absent") is True,
              "动作目标、原始 ref/object 或工作流属性不符")
        action_id = identity([repo, sha, None, None, job, ACTION])
        check(row["id"] == action_id and receipt == identity([action_id, coordinator]), "动作或回执 ID 与稳定身份不符")
        baseline = db.execute("SELECT completed_at FROM rc_ref_baselines WHERE repo=?", (repo,)).fetchone()
        candidates = db.execute("SELECT * FROM rc_ref_candidates WHERE repo=? AND ref=?", (repo, ref)).fetchall()
        check(baseline is not None and len(candidates) == 1, "缺少原始基线或标签曾改变对象")
        candidate = candidates[0]
        check(candidate["tag_object"] == obj and candidate["object_type"] == payload.get("tag_object_type")
              and candidate["object_type"] in ("tag", "commit") and candidate["sha"] == sha
              and candidate["state"] in ("pending", "queued", "forwarded"), "候选不是基线之后已准备好的原始标签动作")
        detected = timestamp(row["created_at"])
        check(timestamp(baseline[0]) <= detected and timestamp(baseline[0]) <= timestamp(candidate["updated_at"]),
              "标签动作早于持久基线")
        proof = payload.get("native_workflow_absence", {})
        check(isinstance(proof, dict) and proof.get("verified") is True and proof.get("sha") == sha
              and proof.get("path") == core.WORKFLOW, "观察器缺少原生目标树属性")
        return dict(repo=repo, sha=sha, tag=ref, tag_object=obj, object_type=candidate["object_type"],
                    tree=core.sha(proof.get("tree")), master_sha=core.sha(payload.get("master_sha")),
                    receipt=receipt, action_id=action_id, detected_at=detected.isoformat(), observer_path=str(path.resolve()))
    finally:
        db.close()


class HTTPError(OSError):
    def __init__(self, status):
        self.status = status
        super().__init__(f"GitHub API 返回 HTTP {status}")


class API(control.AdmissionAPI):
    def __init__(self, repo):
        self.repo = core.repository(repo)
        check(all(part not in (".", "..") for part in self.repo.split("/")), "仓库身份非法")
        self.prefix, self.trust = "/repos/" + self.repo, None
        self._objects = {}  # Core provenance cache; mutable refs are still read live.

    def request(self, method, path, data=None, binary=False):
        check(path in ("/user", self.prefix) or path.startswith(self.prefix + "/"), "API 请求越过仓库边界")
        check("\n" not in path and "\r" not in path and path.isascii(), "API 路径必须已转义")
        if method == "POST":
            check(path == self.prefix + WORKFLOW_API + "/dispatches" and isinstance(data, dict)
                  and set(data) == {"ref", "inputs", "return_run_details"} and data["ref"] == "master"
                  and data["return_run_details"] is True and isinstance(data["inputs"], dict)
                  and set(data["inputs"]) == {"sha"}, "只允许固定 SHA 的 master 候选 dispatch")
            core.sha(data["inputs"]["sha"])
        else:
            check(method == "GET" and data is None, "禁止其他远端写操作")
        command = ["gh", "api", path, "--hostname", "github.com", "--method", method, "--include",
                   "--header", "Accept: application/vnd.github+json", "--header", "X-GitHub-Api-Version: 2022-11-28"]
        if data is not None:
            command += ["--input", "-"]
        env = os.environ.copy()
        for name in ("GH_DEBUG", "GH_HTTP_LOG", "GH_FORCE_TTY"):
            env.pop(name, None)
        env.update(GH_HOST="github.com", GH_REPO=self.repo, GH_PROMPT_DISABLED="1", NO_COLOR="1")
        try:
            process = subprocess.run(command, input=core.encoded(data) if data is not None else None,
                                     capture_output=True, timeout=60, env=env)
        except (OSError, subprocess.TimeoutExpired):
            raise OSError("gh api 未返回可核实响应；写操作结果可能不确定") from None
        raw, status = process.stdout, None
        while raw.startswith(b"HTTP/"):
            match = re.match(rb"HTTP/[^\s]+ ([0-9]{3})[^\r\n]*\r?\n", raw)
            boundary = re.search(rb"\r?\n\r?\n", raw)
            check(match is not None and boundary is not None, "gh api HTTP 响应头不完整")
            status, raw = int(match[1]), raw[boundary.end():]
        if status is None:
            raise OSError("gh api 缺少原生 HTTP 状态；结果不确定")
        if status >= 400:
            raise HTTPError(status)
        if process.returncode or status not in (200, 204):
            raise OSError("gh api 响应未确认成功；结果不确定")
        limit = 160 << 20 if binary else 8 << 20
        check(len(raw) <= limit, "API 响应超限")
        value = raw if binary else core.decode(raw, limit) if raw.strip() else None
        return (status, value) if method == "POST" else value

    def dispatch(self, sha):
        # No retries here, including for 5xx, timeout, empty 204, or a malformed 200.
        return self.request("POST", self.prefix + WORKFLOW_API + "/dispatches",
                            {"ref": "master", "inputs": {"sha": core.sha(sha)}, "return_run_details": True})

    def check_owner(self, sha, status):
        owner = super().check_owner(sha, status)
        trusted_run(self, self.run(owner))
        # The durable commit-scoped status and admission window outlive artifacts.
        return owner


def commit(api, sha):
    value = api.get(api.prefix + "/git/commits/" + core.sha(sha))
    check(value.get("sha") == sha, "目标不是指定的确切提交")
    core.sha(value["tree"]["sha"])
    return value


def ancestor(api, target, master):
    comparison = api.get(api.prefix + f"/compare/{core.sha(target)}...{core.sha(master)}")
    check(comparison.get("status") in ("ahead", "identical")
          and comparison.get("merge_base_commit", {}).get("sha") == target, "提交不在远端 master 快照历史内")


def exact_ref(api, action):
    value = api.get(api.prefix + "/git/ref/" + quote(action["tag"][5:], safe=""))
    check(isinstance(value, dict) and value.get("ref") == action["tag"], "原生标签引用不符")
    obj = value["object"]
    check(obj.get("sha") == action["tag_object"] and obj.get("type") == action["object_type"], "标签已移动或原始对象类型不符")
    seen = set()
    while obj["type"] == "tag":
        raw = core.sha(obj["sha"])
        check(raw not in seen and len(seen) < 16, "标签剥离循环或深度超限")
        seen.add(raw)
        tag = api.get(api.prefix + "/git/tags/" + raw)
        check(tag.get("sha") == raw, "annotated tag 对象不符")
        obj = tag["object"]
    check(obj["type"] == "commit" and core.sha(obj["sha"]) == action["sha"], "标签剥离结果不是授权的确切提交")
    target = commit(api, action["sha"])
    check(target["tree"]["sha"] == action["tree"], "目标树与观察器的原始属性不符")
    return target


def workflow_blob(api, tree):
    parts = core.WORKFLOW.split("/")
    for index, name in enumerate(parts):
        value = api.get(api.prefix + "/git/trees/" + core.sha(tree))
        check(value.get("sha") == tree and value.get("truncated") is False and isinstance(value.get("tree"), list),
              "原生树身份不符或清单不完整")
        rows = [row for row in value["tree"] if row.get("path") == name]
        check(len(rows) <= 1, "原生树含重复路径")
        if not rows:
            return None
        row, tree = rows[0], core.sha(rows[0]["sha"])
        check((row.get("type") == "tree" and row.get("mode") == "040000") if index < len(parts) - 1 else
              (row.get("type") == "blob" and row.get("mode") in ("100644", "100755")), "工作流路径不是普通原生目录或文件")
    return tree


def has_workflow(api, value):
    path = api.prefix + "/contents/" + quote(core.WORKFLOW, safe="") + "?ref=" + core.sha(value["sha"])
    try:
        content = api.get(path)
    except HTTPError as error:
        if error.status != 404:
            raise
        check(workflow_blob(api, value["tree"]["sha"]) is None, "contents 404 与确切目标树矛盾")
        return False
    blob = workflow_blob(api, value["tree"]["sha"])
    check(isinstance(content, dict) and content.get("type") == "file" and content.get("path") == core.WORKFLOW
          and blob is not None and content.get("sha") == blob, "contents 与确切目标树的工作流不符")
    return True


def master_snapshot(api, target):
    master = core.sha(api.get(api.prefix + "/commits/master")["sha"])
    ancestor(api, target, master)
    check(has_workflow(api, commit(api, master)), "当前 master 没有可信候选工作流")
    workflow = api.get(api.prefix + WORKFLOW_API)
    check(workflow.get("path") == core.WORKFLOW and workflow.get("state") == "active", "canonical 候选工作流未启用或路径不符")
    api.trust.update(master_sha=master, workflow_id=core.number(workflow["id"]))
    return master


def trusted_run(api, run):
    core.native_candidate(run, api.repo)
    check(run.get("workflow_id") == api.trust["workflow_id"]
          and run.get("actor", {}).get("id") == run.get("triggering_actor", {}).get("id") == api.trust["owner_id"],
          "候选运行不是 owner 的原生可信工作流")
    if run["event"] == "workflow_dispatch":
        check(run.get("head_branch") == "master", "已有 dispatch 不来自 master")
    else:
        valid_tag("refs/tags/" + run.get("head_branch", ""))
    source = core.sha(run["head_sha"])
    ancestor(api, source, api.trust["master_sha"])
    check(has_workflow(api, commit(api, source)), "已有运行源码没有可信工作流")


def native_proof(api, run):
    trusted_run(api, run)
    run_id = core.number(run["id"])
    rows = [a for a in api.artifacts(run_id) if a.get("name") == f"release-intent-{run_id}-1"]
    if not rows or all(a.get("expired") is True for a in rows):
        return None
    intent = control.read_intent(api, run_id, 1)
    check(intent.get("kind") == "release-intent" and intent.get("workflow_sha") == run["head_sha"], "原生 intent 工作流身份不符")
    target = commit(api, core.sha(intent["sha"]))
    check(intent.get("tree") == target["tree"]["sha"], "原生 intent 与实际目标树不符")
    ancestor(api, target["sha"], api.trust["master_sha"])
    return intent


def find_native(api, action, since, locator=None):
    owner = control.canonical_status(api, api.repo, action["sha"])
    if owner is not None:
        return owner, False
    floor = timestamp(since).replace(microsecond=0)
    runs = {}
    # These are bounded candidate locators, not an exhaustive history/dedup oracle.
    for status in (*ACTIVE, "completed"):
        query = urlencode(dict(event="workflow_dispatch", branch="master", status=status,
                               created=">=" + floor.strftime("%Y-%m-%dT%H:%M:%SZ"), per_page=100, page=1))
        value = api.get(api.prefix + WORKFLOW_API + "/runs?" + query)
        check(isinstance(value, dict) and isinstance(value.get("workflow_runs"), list), "原生候选查询形状不符")
        for run in value["workflow_runs"]:
            if timestamp(run.get("created_at")) >= floor:
                runs[core.number(run["id"])] = run
    unknown = False
    if locator is not None and locator not in runs:
        try:
            run = api.run(locator)
        except HTTPError as error:
            if error.status != 404:
                raise
            unknown = True
        else:
            check(timestamp(run.get("created_at")) >= floor, "定位器运行早于原始提交标记")
            runs[locator] = run
    for run_id, listed in runs.items():
        title = listed.get("display_title", "")
        named_shas = re.findall(r"(?<![0-9a-f])[0-9a-f]{40}(?![0-9a-f])", title)
        matching_title = action["sha"] in named_shas
        if run_id != locator and named_shas and not matching_title:
            continue
        if run_id != locator and not matching_title and listed.get("status") not in ACTIVE:
            continue
        run = api.run(run_id)
        proof = native_proof(api, run)
        if proof is not None and proof["sha"] == action["sha"]:
            check(proof["tree"] == action["tree"], "已有候选目标树不符")
            return run_id, False
        if proof is None:
            unknown = True
        elif run_id == locator:
            raise ValueError("dispatch 返回的运行不是本次目标")
    return None, unknown


class State:
    """Per-target process lock for preparation; permanent SQLite marker only at POST."""
    def __init__(self, path):
        path = Path(path)
        check(path.is_absolute(), "状态数据库必须使用私有绝对路径")
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        try:
            fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        except FileExistsError:
            pass
        else:
            os.close(fd)
        self.path = private_file(path)
        self.db = sqlite3.connect(path, timeout=5, isolation_level=None)
        self.db.row_factory = sqlite3.Row
        self.db.execute("PRAGMA synchronous=FULL")
        try:
            with self.transaction():
                app = self.db.execute("PRAGMA application_id").fetchone()[0]
                check(app in (0, 0x52434632), "状态版本不符；必须保留旧数据库并人工对账")
                if not app:
                    check(not self.db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall(), "拒绝复用其他数据库")
                    self.db.execute("PRAGMA application_id=1380140594")
                self.db.execute("CREATE TABLE IF NOT EXISTS actions (receipt TEXT PRIMARY KEY, action_id TEXT NOT NULL, "
                                "repo TEXT NOT NULL,sha TEXT NOT NULL,tag TEXT NOT NULL,tag_object TEXT NOT NULL, "
                                "observer_path TEXT NOT NULL,coordinator TEXT NOT NULL)")
                self.db.execute("CREATE TABLE IF NOT EXISTS forwards (repo TEXT,sha TEXT,tag TEXT NOT NULL,tag_object TEXT NOT NULL, "
                                "tree TEXT NOT NULL,master_sha TEXT NOT NULL,attempted_at TEXT NOT NULL,state TEXT NOT NULL, "
                                "http_status INTEGER,locator INTEGER,run_id INTEGER,PRIMARY KEY(repo,sha))")
        except BaseException:
            self.db.close()
            raise

    @contextmanager
    def transaction(self):
        self.db.execute("BEGIN IMMEDIATE")
        try:
            yield
            self.db.execute("COMMIT")
        except BaseException:
            self.db.execute("ROLLBACK")
            raise

    def __enter__(self):
        return self

    def __exit__(self, *unused):
        self.db.close()

    @contextmanager
    def target_lock(self, action):
        directory = Path(str(self.path) + ".locks")
        directory.mkdir(mode=0o700, exist_ok=True)
        check(not directory.is_symlink() and directory.stat().st_mode & 0o077 == 0, "状态锁目录必须私有")
        path = directory / (identity([action["repo"], action["sha"]]) + ".lock")
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                yield False
            else:
                yield True
        finally:
            os.close(fd)  # Never unlink a lock inode; process death safely releases preparation.

    def bind(self, action, coordinator):
        values = tuple(action[k] for k in ("receipt", "action_id", "repo", "sha", "tag", "tag_object", "observer_path")) + (coordinator,)
        with self.transaction():
            self.db.execute("INSERT OR IGNORE INTO actions VALUES(?,?,?,?,?,?,?,?)", values)
            row = self.db.execute("SELECT * FROM actions WHERE receipt=?", (action["receipt"],)).fetchone()
            check(tuple(row) == values, "本地动作来源身份已改变")

    def row(self, action):
        return self.db.execute("SELECT * FROM forwards WHERE repo=? AND sha=?", (action["repo"], action["sha"])).fetchone()

    def mark(self, action, master):
        with self.transaction():
            result = self.db.execute("INSERT OR IGNORE INTO forwards (repo,sha,tag,tag_object,tree,master_sha,attempted_at,state) "
                                     "VALUES (?,?,?,?,?,?,?,'pending')", (action["repo"], action["sha"], action["tag"],
                                     action["tag_object"], action["tree"], master, datetime.now(timezone.utc).isoformat()))
            return result.rowcount == 1

    def update(self, action, **values):
        check(set(values) <= {"state", "http_status", "locator", "run_id"}, "状态字段非法")
        with self.transaction():
            self.db.execute("UPDATE forwards SET " + ",".join(key + "=?" for key in values) + " WHERE repo=? AND sha=?",
                            (*values.values(), action["repo"], action["sha"]))


def result(action, status, row=None, run_id=None):
    value = {"repo": action["repo"], "sha": action["sha"], "tag": action["tag"], "tag_object": action["tag_object"],
             "receipt": action["receipt"], "status": status, "submitted": row is not None}
    if row is not None:
        value.update(http_status=row["http_status"], attempted_at=row["attempted_at"])
        run_id = run_id or row["run_id"]
    if run_id is not None:
        value["run_id"] = run_id
    return value


def locator_from_response(api, body):
    if not isinstance(body, dict) or type(body.get("workflow_run_id")) is not int:
        return None
    run_id = core.number(body["workflow_run_id"])
    check(body.get("run_url") == f"https://api.github.com{api.prefix}/actions/runs/{run_id}"
          and body.get("html_url") == f"https://github.com/{api.repo}/actions/runs/{run_id}", "dispatch 定位器仓库不符")
    return run_id


def consume(api, observer_db, receipt, coordinator, state_path):
    action = load_receipt(observer_db, api.repo, coordinator, receipt)
    check(Path(state_path).resolve() != Path(observer_db).resolve(), "写状态不能复用观察器数据库")
    with State(state_path) as store:
        store.bind(action, coordinator)
        with store.target_lock(action) as locked:
            row = store.row(action)
            if not locked:
                return result(action, "busy", row)
            repository, account = api.get(api.prefix), api.get("/user")
            check(repository.get("full_name") == api.repo and repository.get("private") is False
                  and repository.get("default_branch") == "master", "必须是已认证的公开 master 仓库")
            owner = core.number(repository["owner"]["id"])
            check(account.get("id") == owner, "gh 当前账户不是仓库 owner")
            api.trust = {"owner_id": owner}
            target = exact_ref(api, action)
            ancestor(api, target["sha"], action["master_sha"])
            master = master_snapshot(api, target["sha"])
            if has_workflow(api, target):
                return result(action, "native", row)
            if row:
                check(row["tree"] == action["tree"], "持久提交标记目标树不符")
            since = row["attempted_at"] if row else action["detected_at"]
            native, unknown = find_native(api, action, since, row["locator"] if row else None)
            if native is not None:
                if row and row["state"] != "rejected":
                    store.update(action, state="confirmed", run_id=native)
                    return result(action, "forwarded", store.row(action), native)
                return result(action, "existing", row, native)
            if row:
                return result(action, "rejected" if row["state"] == "rejected" else "pending", row)
            if unknown:
                return result(action, "pending")
            # Harmless master advancement is allowed, but both snapshots must contain the target.
            exact_ref(api, action)
            master = master_snapshot(api, target["sha"])
            native, unknown = find_native(api, action, action["detected_at"])
            if native is not None:
                return result(action, "existing", run_id=native)
            if unknown:
                return result(action, "pending")
            # No skip or preparation path above this point leaves a submitted reservation.
            if not store.mark(action, master):
                return result(action, "pending", store.row(action))
            status, body = None, None
            try:
                status, body = api.dispatch(action["sha"])
            except HTTPError as error:
                status = error.status
                if status in (400, 401, 403, 404, 405, 422):
                    store.update(action, state="rejected", http_status=status)
                    return result(action, "rejected", store.row(action))
            except (OSError, ValueError):
                pass
            if status is not None:
                store.update(action, http_status=status)
            locator = locator_from_response(api, body) if status == 200 else None
            if locator is not None:
                store.update(action, locator=locator)
            row = store.row(action)
            # GET-only reconciliation, after the original attempted_at. Never repeat the POST.
            try:
                native, _ = find_native(api, action, row["attempted_at"], locator)
            except OSError:
                native = None
            if native is not None:
                store.update(action, state="confirmed", run_id=native)
            return result(action, "forwarded" if native is not None else "pending", store.row(action), native)


def main():
    parser = argparse.ArgumentParser(description="一次性消费私有观察器回执，转发历史 rc/* 标签；不监听、不发布、不 ACK。")
    parser.add_argument("--repo", required=True)
    parser.add_argument("--observer-db", type=Path, required=True, help="唯一观察器已存在的私有 SQLite 绝对路径，只读")
    parser.add_argument("--coordinator", required=True, help="数据库已绑定的协调器身份")
    parser.add_argument("--receipt", required=True, help="已投递 inbox 的原始 receipt_id，不接受外部 JSON")
    parser.add_argument("--state", type=Path, required=True, help="本消费者持久私有 SQLite 绝对路径，所有调用共用，禁止删除后重放")
    args = parser.parse_args()
    value = consume(API(args.repo), args.observer_db, args.receipt, args.coordinator, args.state)
    print(json.dumps(value, sort_keys=True, separators=(",", ":")))
    return 2 if value["status"] in ("pending", "busy") else 1 if value["status"] == "rejected" else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, TypeError, OSError, sqlite3.Error, subprocess.TimeoutExpired):
        print("历史标签转发失败；检查私有原始回执及原生证据。已提交 pending 只能对账，禁止删除状态后重放。", file=sys.stderr)
        sys.exit(1)
