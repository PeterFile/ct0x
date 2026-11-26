"""Private durable state; local consumers never contact GitHub."""
from dataclasses import dataclass
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import time


def encode(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def now():
    return datetime.now(timezone.utc).isoformat()


def label(value):
    if not isinstance(value, str) or not value.strip() or len(value) > 512 or any(ord(c) < 32 for c in value):
        raise ValueError("本地身份或回执引用无效")
    return value


@dataclass(frozen=True)
class Key:
    repo: str
    sha: str
    run_id: int
    attempt: int
    job: str

    def values(self):
        return (self.repo, self.sha, self.run_id, self.attempt, self.job)


class ReceiverLock:
    def __init__(self, path):
        self.path = str(Path(path).resolve()) + ".receiver.lock"
        self.fd = None

    def __enter__(self):
        self.fd = os.open(self.path, os.O_CREAT | os.O_RDWR, 0o600)
        try:
            fcntl.flock(self.fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(self.fd)
            self.fd = None
            raise RuntimeError("该数据库已有观察接收进程") from None
        return self

    def __exit__(self, *_):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None
        # Never unlink a lock file: another process may already hold its inode.


class Store:
    def __init__(self, path):
        self.path = Path(path).absolute()
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.path.is_symlink():
            raise ValueError("数据库不能是符号链接")
        try:
            fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
        except FileExistsError:
            pass
        else:
            os.close(fd)
        # Do not open/close an existing DB outside SQLite: close() can drop its POSIX locks.
        if self.path.stat().st_mode & 0o077:
            raise ValueError("数据库权限必须仅允许当前用户访问（0600）")
        self.db = sqlite3.connect(self.path, timeout=5)
        self.db.row_factory = sqlite3.Row
        # Rollback journaling also avoids the WAL-reset bug in older bundled SQLite.
        self.db.execute("PRAGMA journal_mode=DELETE")
        self.db.execute("PRAGMA synchronous=FULL")
        self.db.execute("PRAGMA foreign_keys=ON")
        self.db.executescript("""
            CREATE TABLE IF NOT EXISTS config (id INTEGER PRIMARY KEY CHECK(id=1), repo TEXT, coordinator TEXT);
            CREATE TABLE IF NOT EXISTS assignments (
                repo TEXT, pr INTEGER, session TEXT, evidence TEXT, created_at TEXT,
                PRIMARY KEY(repo,pr,session,evidence));
            CREATE TABLE IF NOT EXISTS cache (path TEXT PRIMARY KEY, etag TEXT, body TEXT, link TEXT);
            CREATE TABLE IF NOT EXISTS observations (
                repo TEXT, run_id INTEGER, attempt INTEGER, body TEXT, updated_at TEXT,
                PRIMARY KEY(repo,run_id,attempt));
            CREATE TABLE IF NOT EXISTS targets (
                repo TEXT, run_id INTEGER, attempt INTEGER, kind TEXT, sha TEXT, tree TEXT,
                PRIMARY KEY(repo,run_id,attempt,kind));
            CREATE TABLE IF NOT EXISTS problems (scope TEXT PRIMARY KEY, reason TEXT, updated_at TEXT);
            CREATE TABLE IF NOT EXISTS events (
                repo TEXT, sha TEXT, run_id INTEGER, attempt INTEGER, job TEXT, payload TEXT,
                PRIMARY KEY(repo,sha,run_id,attempt,job));
            CREATE TABLE IF NOT EXISTS actions (
                id TEXT PRIMARY KEY, repo TEXT, sha TEXT, run_id INTEGER, attempt INTEGER, job TEXT,
                kind TEXT, payload TEXT, created_at TEXT,
                UNIQUE(repo,sha,run_id,attempt,job,kind));
            CREATE UNIQUE INDEX IF NOT EXISTS first_blocker ON actions(repo,sha,run_id,attempt)
                WHERE kind='first_failure';
            CREATE UNIQUE INDEX IF NOT EXISTS binary_ready ON actions(repo,sha,run_id,attempt)
                WHERE kind='release_binary_ready';
            CREATE TABLE IF NOT EXISTS outbox (
                action_id TEXT REFERENCES actions(id), recipient TEXT, receipt_id TEXT UNIQUE,
                state TEXT CHECK(state IN ('pending','delivered','acked')), delivered_at TEXT, acked_at TEXT,
                PRIMARY KEY(action_id,recipient));
            CREATE TABLE IF NOT EXISTS inbox (
                receipt_id TEXT PRIMARY KEY REFERENCES outbox(receipt_id), recipient TEXT,
                payload TEXT, delivered_at TEXT NOT NULL, acked_at TEXT);
            CREATE INDEX IF NOT EXISTS inbox_recipient ON inbox(recipient);
            CREATE TABLE IF NOT EXISTS waits (recipient TEXT PRIMARY KEY, selector TEXT NOT NULL);
        """)

    def close(self):
        self.db.close()

    def configure(self, repo, coordinator):
        import re
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or any(p in (".", "..") for p in repo.split("/")):
            raise ValueError("仓库必须为 owner/repo")
        label(coordinator)
        with self.db:
            row = self.db.execute("SELECT repo,coordinator FROM config WHERE id=1").fetchone()
            if row and tuple(row) != (repo, coordinator):
                raise ValueError("数据库已绑定其他仓库或协调会话；请使用独立数据库")
            self.db.execute("INSERT OR IGNORE INTO config VALUES(1,?,?)", (repo, coordinator))

    def config(self):
        row = self.db.execute("SELECT repo,coordinator FROM config WHERE id=1").fetchone()
        if not row:
            raise ValueError("请先初始化本地仓库和协调会话绑定")
        return tuple(row)

    def assign(self, repo, pr, session, evidence):
        if repo != self.config()[0] or type(pr) is not int or pr < 1:
            raise ValueError("PR 指派必须属于已绑定仓库且编号为正整数")
        label(session)
        label(evidence)
        with self.db:
            self.db.execute("INSERT OR IGNORE INTO assignments VALUES(?,?,?,?,?)", (repo, pr, session, evidence, now()))

    def author(self, repo, pr):
        rows = self.db.execute("SELECT DISTINCT session FROM assignments WHERE repo=? AND pr=?", (repo, pr)).fetchall()
        return rows[0][0] if len(rows) == 1 else None

    def cache_get(self, path):
        row = self.db.execute("SELECT etag,body,link FROM cache WHERE path=?", (path,)).fetchone()
        return (row[0], json.loads(row[1]), row[2]) if row else None

    def cache_put(self, path, etag, body, link):
        with self.db:
            self.db.execute("INSERT OR REPLACE INTO cache VALUES(?,?,?,?)", (path, etag, encode(body), link))

    def problem(self, scope, reason=None):
        with self.db:
            if reason:
                self.db.execute("INSERT OR REPLACE INTO problems VALUES(?,?,?)", (scope, reason, now()))
            else:
                self.db.execute("DELETE FROM problems WHERE scope=?", (scope,))

    def observe(self, repo, run_id, attempt, body):
        with self.db:
            old = self.db.execute("SELECT body FROM observations WHERE repo=? AND run_id=? AND attempt=?", (repo, run_id, attempt)).fetchone()
            if old:
                previous = json.loads(old[0])["run"]
                if any(previous.get(k) != body["run"].get(k) for k in ("path", "workflow_id", "head_sha", "event")):
                    raise ValueError("同一 attempt 的来源身份冲突，保持未知")
            self.db.execute("INSERT OR REPLACE INTO observations VALUES(?,?,?,?,?)", (repo, run_id, attempt, encode(body), now()))

    def bind_target(self, repo, run_id, attempt, kind, sha, tree):
        with self.db:
            self.db.execute("INSERT OR IGNORE INTO targets VALUES(?,?,?,?,?,?)", (repo, run_id, attempt, kind, sha, tree))
            row = self.db.execute("SELECT sha,tree FROM targets WHERE repo=? AND run_id=? AND attempt=? AND kind=?", (repo, run_id, attempt, kind)).fetchone()
            if tuple(row) != (sha, tree):
                raise ValueError("同一 attempt 的目标 SHA 或 tree 冲突，保持未知")

    def known_runs(self, repo):
        return {r[0] for r in self.db.execute("SELECT DISTINCT run_id FROM observations WHERE repo=?", (repo,))}

    def event(self, key, payload):
        with self.db:
            old = self.db.execute("SELECT payload FROM events WHERE repo=? AND sha=? AND run_id=? AND attempt=? AND job=?", key.values()).fetchone()
            if old:
                previous = json.loads(old[0])
                if previous.get("status") == "completed" and payload.get("status") != "completed":
                    return previous
                if (previous.get("status") == "completed" and payload.get("status") == "completed"
                        and previous.get("conclusion") != payload.get("conclusion")):
                    raise ValueError("同一作业或整轮终态冲突，保持未知")
                if previous.get("status") == "completed" and previous.get("logs_pending") is False:
                    payload = dict(payload, logs_pending=False, tests=previous.get("tests", []))
            self.db.execute("INSERT OR REPLACE INTO events VALUES(?,?,?,?,?,?)", (*key.values(), encode(payload)))
        return payload

    def publish(self, key, kind, payload, recipients):
        payload = dict(payload, repo=key.repo, sha=key.sha, run_id=key.run_id, attempt=key.attempt,
                       action=kind, job=key.job)
        action_id = hashlib.sha256(encode([*key.values(), kind]).encode()).hexdigest()
        with self.db:
            self.db.execute("INSERT OR IGNORE INTO actions VALUES(?,?,?,?,?,?,?,?,?)",
                            (action_id, *key.values(), kind, encode(payload), now()))
            row = self.db.execute("SELECT payload FROM actions WHERE id=?", (action_id,)).fetchone()
            if not row:  # A different job already won first-blocker arbitration.
                return
            previous = json.loads(row[0])
            if any(previous.get(k) != payload.get(k) for k in ("source_head", "workflow_sha", "tested_sha")):
                raise ValueError("已发布动作的来源或目标身份冲突，保持未知")
            if previous.get("pr") != payload.get("pr"):
                return  # Unverified original PR routes are never retroactively guessed.
            for recipient in sorted(set(recipients)):
                label(recipient)
                receipt_id = hashlib.sha256(encode([action_id, recipient]).encode()).hexdigest()
                self.db.execute("INSERT OR IGNORE INTO outbox VALUES(?,?,?,'pending',NULL,NULL)", (action_id, recipient, receipt_id))

    def deliver(self, scope, coordinator, author_pr=None):
        with self.db:
            self.db.execute("BEGIN IMMEDIATE")
            rows = self.db.execute("SELECT o.*,a.payload FROM outbox o JOIN actions a ON a.id=o.action_id "
                                   "WHERE o.state='pending' AND a.repo=? AND a.sha=? AND a.run_id=? AND a.attempt=?", scope).fetchall()
            for row in rows:
                payload = json.loads(row["payload"])
                if row["recipient"] != coordinator:
                    if (author_pr is None or payload.get("pr") != author_pr or
                            self.author(scope[0], author_pr) != row["recipient"]):
                        continue
                stamp = now()
                self.db.execute("INSERT OR IGNORE INTO inbox VALUES(?,?,?,?,NULL)",
                                (row["receipt_id"], row["recipient"], row["payload"], stamp))
                self.db.execute("UPDATE outbox SET state='delivered',delivered_at=? WHERE receipt_id=? AND state='pending'",
                                (stamp, row["receipt_id"]))

    @staticmethod
    def decode_rows(rows):
        result = []
        for row in rows:
            item = dict(row)
            if "payload" in item:
                item["payload"] = json.loads(item["payload"])
            result.append(item)
        return result

    def actions(self):
        return self.decode_rows(self.db.execute("SELECT * FROM actions ORDER BY created_at,id"))

    def events(self):
        return self.decode_rows(self.db.execute("SELECT * FROM events ORDER BY run_id,attempt,job"))

    def outbox(self):
        return self.decode_rows(self.db.execute("SELECT o.*,a.payload FROM outbox o JOIN actions a ON a.id=o.action_id ORDER BY a.created_at,o.recipient"))

    def inbox(self, recipient):
        label(recipient)
        return self.decode_rows(self.db.execute("SELECT i.*,o.state FROM inbox i JOIN outbox o USING(receipt_id) WHERE i.recipient=? ORDER BY i.delivered_at,i.receipt_id", (recipient,)))

    def ack(self, recipient, receipt_id):
        with self.db:
            row = self.db.execute("SELECT payload,acked_at FROM inbox WHERE recipient=? AND receipt_id=?", (recipient, receipt_id)).fetchone()
            if not row:
                raise ValueError("收件人与回执不匹配，或尚未持久投递")
            if row["acked_at"]:
                return
            stamp = now()
            self.db.execute("UPDATE inbox SET acked_at=? WHERE recipient=? AND receipt_id=?", (stamp, recipient, receipt_id))
            self.db.execute("UPDATE outbox SET state='acked',acked_at=? WHERE recipient=? AND receipt_id=?", (stamp, recipient, receipt_id))
            payload = json.loads(row["payload"])
            selector = {k: payload[k] for k in ("repo", "sha", "run_id", "attempt", "action")}
            self.db.execute("DELETE FROM waits WHERE recipient=? AND selector=?", (recipient, encode(selector)))

    def cancel_wait(self, recipient):
        with self.db:
            self.db.execute("DELETE FROM waits WHERE recipient=?", (label(recipient),))

    def wait(self, recipient, selector, timeout=300):
        label(recipient)
        if set(selector) != {"repo", "sha", "run_id", "attempt", "action"} or not 0 <= timeout <= 86400:
            raise ValueError("待续办必须精确绑定仓库、SHA、run、attempt、action 及有界超时")
        serialized = encode(selector)
        with self.db:
            self.db.execute("INSERT OR IGNORE INTO waits VALUES(?,?)", (recipient, serialized))
            row = self.db.execute("SELECT selector FROM waits WHERE recipient=?", (recipient,)).fetchone()
            if row[0] != serialized:
                raise ValueError("该收件人已有另一项待续办；请先确认或取消原等待")
        deadline = time.monotonic() + timeout
        while True:
            for receipt in self.inbox(recipient):
                if receipt["state"] == "delivered" and all(receipt["payload"].get(k) == v for k, v in selector.items()):
                    return receipt
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            time.sleep(min(0.25, remaining))
