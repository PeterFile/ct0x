"""Private durable state; local consumers never contact GitHub."""
from dataclasses import dataclass
from datetime import datetime, timezone
import fcntl
import hashlib
from functools import wraps
import json
import os
from pathlib import Path
import sqlite3
import time

SCHEMA_VERSION = 6


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


def retry_busy(method):
    @wraps(method)
    def run(self, *args, **kwargs):
        for attempt in range(2):
            try:
                return method(self, *args, **kwargs)
            except sqlite3.OperationalError as error:
                if attempt or not any(word in str(error).lower() for word in ("locked", "busy")):
                    raise
                self.db.rollback()
                time.sleep(0.05)
    return run


class Store:
    def __init__(self, path, readonly=False, initialize=True):
        self.path = Path(path).absolute()
        if self.path.is_symlink():
            raise ValueError("数据库不能是符号链接")
        if initialize and not readonly:
            self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            try:
                fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
            except FileExistsError:
                pass
            else:
                os.close(fd)
        # Do not open/close an existing DB outside SQLite: close() can drop its POSIX locks.
        if self.path.stat().st_mode & 0o077:
            raise ValueError("数据库权限必须仅允许当前用户访问（0600）")
        mode = "ro" if readonly else "rw"
        self.db = sqlite3.connect(self.path.as_uri() + "?mode=" + mode, uri=True, timeout=0.5)
        self.db.row_factory = sqlite3.Row
        if readonly:
            return
        self.db.execute("PRAGMA synchronous=FULL")
        self.db.execute("PRAGMA foreign_keys=ON")
        if not initialize:
            return
        version = self.db.execute("PRAGMA user_version").fetchone()[0]
        orphan = self.db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='waits_legacy'").fetchone()
        if version >= SCHEMA_VERSION and not orphan:
            return
        try:
            with ReceiverLock(self.path):
                self._initialize_schema()
        except BaseException:
            self.db.close()
            raise

    def _initialize_schema(self):
        # Only locked initialization/migration changes journal mode or schema.
        if self.db.execute("PRAGMA journal_mode").fetchone()[0] != "delete":
            self.db.execute("PRAGMA journal_mode=DELETE")
        self.db.executescript("""
            BEGIN IMMEDIATE;
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
            CREATE TABLE IF NOT EXISTS delivery_payloads (
                action_id TEXT REFERENCES actions(id), recipient TEXT, payload TEXT NOT NULL,
                PRIMARY KEY(action_id,recipient));
            CREATE TABLE IF NOT EXISTS inbox (
                receipt_id TEXT PRIMARY KEY REFERENCES outbox(receipt_id), recipient TEXT,
                payload TEXT, delivered_at TEXT NOT NULL, acked_at TEXT);
            CREATE INDEX IF NOT EXISTS inbox_recipient ON inbox(recipient);
            CREATE INDEX IF NOT EXISTS inbox_pending_read ON outbox(recipient,state,receipt_id);
            CREATE TABLE IF NOT EXISTS waits (recipient TEXT, selector TEXT NOT NULL, PRIMARY KEY(recipient,selector));
            CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, body TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS tracked_runs (
                repo TEXT, run_id INTEGER, workflow TEXT, summary TEXT, dirty INTEGER DEFAULT 1,
                retry_at REAL DEFAULT 0, retries INTEGER DEFAULT 0, checked_at TEXT, priority INTEGER DEFAULT 1,
                PRIMARY KEY(repo,run_id));
            CREATE TABLE IF NOT EXISTS roots (
                repo TEXT, run_id INTEGER, attempt INTEGER, sha TEXT, source_head TEXT,
                PRIMARY KEY(repo,run_id,attempt));
            CREATE TABLE IF NOT EXISTS causes (
                repo TEXT, commit_sha TEXT, pr INTEGER, body TEXT,
                PRIMARY KEY(repo,commit_sha,pr));
            CREATE TABLE IF NOT EXISTS cause_owners (
                repo TEXT, commit_sha TEXT, pr INTEGER, recipient TEXT,
                PRIMARY KEY(repo,commit_sha,pr,recipient));
        """)
        with self.db:
            tables = {r[0] for r in self.db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            recover = ["waits_legacy"] if "waits_legacy" in tables else []
            columns = self.db.execute("PRAGMA table_info(waits)").fetchall()
            if next(c[5] for c in columns if c[1] == "selector") == 0:
                temporary = "waits_upgrade" if recover else "waits_legacy"
                self.db.execute("ALTER TABLE waits RENAME TO " + temporary)
                self.db.execute("CREATE TABLE waits (recipient TEXT,selector TEXT NOT NULL,PRIMARY KEY(recipient,selector))")
                recover.append(temporary)
            for table in recover:
                if {r[1] for r in self.db.execute("PRAGMA table_info(" + table + ")")} != {"recipient", "selector"}:
                    raise ValueError("旧等待表结构冲突；保留原表，需受信 operator 核查")
                rows = self.db.execute("SELECT recipient,selector FROM " + table).fetchall()
                if any(not isinstance(value, str) or not value for row in rows for value in row):
                    raise ValueError("旧等待记录无效；保留原表，不猜测修复")
                self.db.execute("INSERT OR IGNORE INTO waits SELECT recipient,selector FROM " + table)
                self.db.execute("DROP TABLE " + table)
            if "priority" not in {c[1] for c in self.db.execute("PRAGMA table_info(tracked_runs)")}:
                self.db.execute("ALTER TABLE tracked_runs ADD COLUMN priority INTEGER DEFAULT 1")
            self.db.execute("CREATE INDEX IF NOT EXISTS tracked_hot ON tracked_runs(repo,dirty,priority,retry_at,checked_at)")
            self.db.execute("PRAGMA user_version=" + str(SCHEMA_VERSION))

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

    def bind_pr(self, repo, run_id, attempt, proof):
        source = proof["source"]
        baseline_key = f"author-source:{repo}:{proof['pr']}"
        baseline = self.meta_get(baseline_key)
        if baseline and any(v is not None and source.get(k) != v for k, v in baseline.items()):
            raise ValueError("PR 来源 ref 或仓库已改变；保留原指派，仅交协调者")
        key = f"pr-route:{repo}:{run_id}:{attempt}"
        previous = self.meta_get(key)
        if previous and previous != proof:
            raise ValueError("同一原生执行的 PR 来源绑定冲突，保持未知")
        with self.db:
            if not previous:
                self.db.execute("INSERT INTO meta VALUES(?,?)", (key, encode(proof)))
            if source != baseline and self.author(repo, proof["pr"]):
                self.db.execute("INSERT OR REPLACE INTO meta VALUES(?,?)", (baseline_key, encode(source)))

    def cache_get(self, path):
        row = self.db.execute("SELECT etag,body,link FROM cache WHERE path=?", (path,)).fetchone()
        return (row[0], json.loads(row[1]), row[2]) if row else None

    def cache_put(self, path, etag, body, link):
        if self.cache_get(path) == (etag, body, link):
            return
        with self.db:
            self.db.execute("INSERT OR REPLACE INTO cache VALUES(?,?,?,?)", (path, etag, encode(body), link))

    def problem(self, scope, reason=None):
        previous = self.db.execute("SELECT reason FROM problems WHERE scope=?", (scope,)).fetchone()
        if (previous[0] if previous else None) == (reason or None):
            return
        with self.db:
            if reason:
                self.db.execute("INSERT OR REPLACE INTO problems VALUES(?,?,?)", (scope, reason, now()))
            else:
                self.db.execute("DELETE FROM problems WHERE scope=?", (scope,))

    def observe(self, repo, run_id, attempt, body):
        with self.db:
            old = self.db.execute("SELECT body FROM observations WHERE repo=? AND run_id=? AND attempt=?", (repo, run_id, attempt)).fetchone()
            if old:
                previous = json.loads(old[0])
                if any(previous["run"].get(k) != body["run"].get(k) for k in ("path", "workflow_id", "head_sha", "event")):
                    raise ValueError("同一 attempt 的来源身份冲突，保持未知")
                if previous == body:
                    return
            self.db.execute("INSERT OR REPLACE INTO observations VALUES(?,?,?,?,?)", (repo, run_id, attempt, encode(body), now()))

    def meta_get(self, key, default=None):
        row = self.db.execute("SELECT body FROM meta WHERE key=?", (key,)).fetchone()
        return json.loads(row[0]) if row else default

    def meta_put(self, key, value):
        previous = self.db.execute("SELECT body FROM meta WHERE key=?", (key,)).fetchone()
        if previous and json.loads(previous[0]) == value:
            return
        with self.db:
            self.db.execute("INSERT OR REPLACE INTO meta VALUES(?,?)", (key, encode(value)))

    def track(self, repo, summary):
        identity = {k: summary.get(k) for k in ("id", "path", "head_sha", "run_attempt", "status", "conclusion")}
        with self.db:
            previous = self.db.execute("SELECT summary FROM tracked_runs WHERE repo=? AND run_id=?", (repo, summary["id"])).fetchone()
            if previous:
                known = json.loads(previous[0])
                if (known.get("run_attempt") or 0) > (identity["run_attempt"] or 0) or all(known.get(k) == v for k, v in identity.items()):
                    return
            active = summary.get("status") in {"queued", "in_progress", "waiting", "pending", "requested"}
            blocking = summary.get("conclusion") in {"failure", "timed_out", "action_required", "startup_failure"}
            self.db.execute("INSERT INTO tracked_runs(repo,run_id,workflow,summary,priority) VALUES(?,?,?,?,?) "
                            "ON CONFLICT(repo,run_id) DO UPDATE SET dirty=CASE WHEN summary!=excluded.summary THEN 1 ELSE dirty END,"
                            "retry_at=CASE WHEN summary!=excluded.summary THEN 0 ELSE retry_at END,summary=excluded.summary,"
                            "priority=MIN(priority,excluded.priority)",
                            (repo, summary["id"], summary["path"], encode(identity), 0 if active or blocking else 1))

    def queued(self, repo, force=False):
        return [dict(r) for r in self.db.execute("SELECT * FROM tracked_runs WHERE repo=? AND dirty=1 AND (? OR retry_at<=?) ORDER BY priority,checked_at IS NOT NULL,checked_at,run_id DESC", (repo, force, time.time()))]

    def checked(self, repo, run_id, active, pending):
        with self.db:
            row = self.db.execute("SELECT * FROM tracked_runs WHERE repo=? AND run_id=?", (repo, run_id)).fetchone()
            retries = min((row["retries"] if row else 0) + 1, 7) if pending and not active else 0
            due = time.time() + min(3600, 30 * 2 ** (retries - 1)) if retries else 0
            state = (bool(active or pending), due, retries, 0 if active or pending else 1)
            if row and tuple(row[k] for k in ("dirty", "retry_at", "retries", "priority")) == state and row["checked_at"]:
                checked = datetime.fromisoformat(row["checked_at"]).timestamp()
                if time.time() - checked < 300:
                    return
            self.db.execute("UPDATE tracked_runs SET dirty=?,retry_at=?,retries=?,priority=?,checked_at=? WHERE repo=? AND run_id=?",
                            (*state, now(), repo, run_id))

    def root_sha(self, repo, run_id, attempt, source_head, preferred=None):
        with self.db:
            row = self.db.execute("SELECT sha,source_head FROM roots WHERE repo=? AND run_id=? AND attempt=?", (repo, run_id, attempt)).fetchone()
            if row:
                if row[1] != source_head:
                    raise ValueError("同一 attempt 的 source head 冲突")
                return row[0]
            old = self.db.execute("SELECT DISTINCT sha FROM actions WHERE repo=? AND run_id=? AND attempt=? AND kind='first_failure'", (repo, run_id, attempt)).fetchall()
            value = old[0][0] if len(old) == 1 else preferred or source_head
            if len(old) > 1:
                raise ValueError("已有首报来源不唯一，保留原记录")
            self.db.execute("INSERT INTO roots VALUES(?,?,?,?,?)", (repo, run_id, attempt, value, source_head))
            return value

    def target(self, repo, run_id, attempt):
        return self.meta_get(f"target:{repo}:{run_id}:{attempt}")

    def bind_target(self, repo, run_id, attempt, kind, sha, tree, proof=None):
        with self.db:
            row = self.db.execute("SELECT sha,tree FROM targets WHERE repo=? AND run_id=? AND attempt=? AND kind=?", (repo, run_id, attempt, kind)).fetchone()
            if row is None:
                self.db.execute("INSERT INTO targets VALUES(?,?,?,?,?,?)", (repo, run_id, attempt, kind, sha, tree))
            elif tuple(row) != (sha, tree):
                raise ValueError("同一 attempt 的目标 SHA 或 tree 冲突，保持未知")
            others = self.db.execute("SELECT sha,tree FROM targets WHERE repo=? AND run_id=? AND attempt=?", (repo, run_id, attempt))
            if any(tuple(r) != (sha, tree) for r in others):
                raise ValueError("同一 attempt 的多个回执目标冲突，保持未知")
            if proof is not None:
                proof, previous = dict(proof), self.target(repo, run_id, attempt) or {}
                for name in ("source_head", "workflow_sha", "pr", "pr_head", "source"):
                    known = previous.get(name)
                    if known is not None:
                        if proof.get(name) is not None and proof[name] != known:
                            raise ValueError("同一执行的控制源码或 PR 来源绑定发生冲突，保持未知")
                        proof[name] = known
                bound = dict(proof, sha=sha, tree=tree)
                if bound != previous:
                    self.db.execute("INSERT OR REPLACE INTO meta VALUES(?,?)", (f"target:{repo}:{run_id}:{attempt}", encode(bound)))

    def known_runs(self, repo):
        return {r[0] for r in self.db.execute("SELECT run_id FROM observations WHERE repo=? UNION SELECT run_id FROM actions WHERE repo=? AND run_id IS NOT NULL", (repo, repo))}

    def event(self, key, payload):
        return self.event_many([(key, payload)])[0]

    def event_many(self, records):
        return self._save_events(list(records))

    @retry_busy
    def _save_events(self, records):
        result = []
        with self.db:
            for key, payload in records:
                old = self.db.execute("SELECT payload FROM events WHERE repo=? AND sha=? AND run_id=? AND attempt=? AND job=?", key.values()).fetchone()
                if old:
                    previous = json.loads(old[0])
                    known = {"success", "failure", "timed_out", "action_required", "cancelled", "skipped", "neutral", "stale", "startup_failure"}
                    terminal = previous.get("status") == "completed" and previous.get("conclusion") in known
                    if terminal and (payload.get("status") != "completed" or payload.get("conclusion") not in known):
                        payload = previous
                    elif terminal and previous.get("conclusion") != payload.get("conclusion"):
                        raise ValueError("同一作业或整轮终态冲突，保持未知")
                    if terminal and previous.get("logs_pending") is False:
                        payload = dict(payload, logs_pending=False, tests=previous.get("tests", []))
                    if payload == previous:
                        result.append(previous)
                        continue
                self.db.execute("INSERT OR REPLACE INTO events VALUES(?,?,?,?,?,?)", (*key.values(), encode(payload)))
                result.append(payload)
        return result

    def publish(self, key, kind, payload, recipients, cause=None):
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
            old_control = previous.get("workflow_sha")
            # Upgrade old plain PR/push facts for comparison only; never rewrite their receipts.
            if (old_control is None and previous.get("workflow") in {".github/workflows/merge-gate.yml", ".github/workflows/periodic-full.yml"}
                    and payload.get("event") in {"pull_request", "push"}
                    and previous.get("source_head") == payload.get("source_head") == payload.get("workflow_sha")):
                old_control = previous["source_head"]
            target = self.target(key.repo, key.run_id, key.attempt) if previous.get("tested_sha") is None and payload.get("tested_sha") else None
            if (old_control is None and previous.get("workflow") == ".github/workflows/release-candidate.yml" and target
                    and target.get("workflow_sha") == payload.get("workflow_sha") and target.get("source_head") == payload.get("source_head")):
                old_control = target["workflow_sha"]
            same_target = (previous.get("tested_sha") == payload.get("tested_sha") or
                           target is not None and target["sha"] == payload["tested_sha"])
            if (previous.get("source_head") != payload.get("source_head") or old_control != payload.get("workflow_sha")
                    or not same_target or previous.get("workflow") != payload.get("workflow")):
                raise ValueError("已发布动作的来源或目标身份冲突，保持未知")
            route = self.meta_get(f"pr-route:{key.repo}:{key.run_id}:{key.attempt}", {})
            verified_route = (previous.get("pr") is None and payload.get("workflow") == ".github/workflows/merge-gate.yml"
                              and payload.get("event") in {"pull_request", "workflow_dispatch"} and route.get("pr") == payload.get("pr")
                              and route.get("pr") is not None and route.get("source_head") == payload.get("source_head")
                              and route.get("workflow") == payload.get("workflow") and route.get("event") == payload.get("event"))
            if previous.get("pr") != payload.get("pr") and not verified_route:
                return action_id  # Unproved route changes never rewrite or fan out historical messages.
            if cause:
                owner, evidence = cause
                self.db.execute("INSERT OR REPLACE INTO causes VALUES(?,?,?,?)", (*owner[:3], encode(evidence)))
            for recipient in sorted(set(recipients)):
                label(recipient)
                if cause and recipient == owner[3]:
                    claimed = self.db.execute("INSERT OR IGNORE INTO cause_owners VALUES(?,?,?,?)", owner)
                    if not claimed.rowcount:
                        continue
                receipt_id = hashlib.sha256(encode([action_id, recipient]).encode()).hexdigest()
                if verified_route and recipient != self.config()[1]:
                    delivered = self.db.execute("SELECT 1 FROM inbox WHERE receipt_id=?", (receipt_id,)).fetchone()
                    if not delivered:
                        self.db.execute("INSERT OR IGNORE INTO delivery_payloads VALUES(?,?,?)", (action_id, recipient, encode(payload)))
                self.db.execute("INSERT OR IGNORE INTO outbox VALUES(?,?,?,'pending',NULL,NULL)", (action_id, recipient, receipt_id))
        return action_id

    def deliver(self, scope, coordinator, author_pr=None):
        if scope[2] is None or scope[3] is None:
            raise ValueError("无 run 的标签待办必须按独立 tag key 核验和投递")
        ids = [r[0] for r in self.db.execute("SELECT id FROM actions WHERE repo=? AND sha=? AND run_id=? AND attempt=?", scope)]
        self.deliver_actions({identity: author_pr for identity in ids}, coordinator)

    @retry_busy
    def deliver_actions(self, verified, coordinator):
        delivered = 0
        with self.db:
            self.db.execute("BEGIN IMMEDIATE")
            for identity, author_pr in verified.items():
                rows = self.db.execute("SELECT o.*,a.repo,COALESCE(d.payload,a.payload) AS payload FROM outbox o JOIN actions a ON a.id=o.action_id LEFT JOIN delivery_payloads d ON d.action_id=o.action_id AND d.recipient=o.recipient WHERE o.state='pending' AND a.id=?", (identity,)).fetchall()
                delivered += self._deliver_rows(rows, coordinator, author_pr)
        return delivered

    def _deliver_rows(self, rows, coordinator, author_pr):
        delivered = 0
        for row in rows:
            payload = json.loads(row["payload"])
            if row["recipient"] != coordinator:
                routed_pr = payload.get("pr")
                if routed_pr is None and payload.get("workflow") == ".github/workflows/merge-gate.yml" and payload.get("event") == "workflow_dispatch":
                    binding = self.target(row["repo"], payload["run_id"], payload["attempt"])
                    if binding and binding.get("provenance") == "merge-plan" and binding.get("source_head") == payload.get("source_head"):
                        routed_pr = binding.get("pr")
                if (author_pr is None or routed_pr != author_pr or self.author(row["repo"], author_pr) != row["recipient"]):
                    continue
            stamp = now()
            self.db.execute("INSERT OR IGNORE INTO inbox VALUES(?,?,?,?,NULL)",
                            (row["receipt_id"], row["recipient"], row["payload"], stamp))
            self.db.execute("UPDATE outbox SET state='delivered',delivered_at=? WHERE receipt_id=? AND state='pending'",
                            (stamp, row["receipt_id"]))
            delivered += 1
        return delivered

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
        return self.decode_rows(self.db.execute("SELECT o.*,COALESCE(d.payload,a.payload) AS payload FROM outbox o JOIN actions a ON a.id=o.action_id LEFT JOIN delivery_payloads d ON d.action_id=o.action_id AND d.recipient=o.recipient ORDER BY a.created_at,o.recipient"))

    @retry_busy
    def inbox(self, recipient, pending=False, limit=None, after=None, receipt_id=None):
        label(recipient)
        for value in (after, receipt_id):
            if value is not None and (not isinstance(value, str) or len(value) != 64 or any(c not in "0123456789abcdef" for c in value)):
                raise ValueError("回执或分页游标必须是完整回执 ID")
        if limit is not None and (type(limit) is not int or not 1 <= limit <= 256):
            raise ValueError("收件分页大小必须在 1 至 256 之间")
        if receipt_id is not None and (pending or after is not None or limit is not None):
            raise ValueError("单条查询不能与分页参数混用")
        conditions, values = ["o.recipient=?", "i.recipient=o.recipient",
                              "NOT EXISTS (SELECT 1 FROM meta m WHERE m.key='release-duplicate-superseded:'||i.receipt_id)"], [recipient]
        if pending:
            conditions += ["o.state='delivered'", "i.acked_at IS NULL"]
            limit = 32 if limit is None else limit
        if after is not None:
            conditions.append("o.receipt_id>?")
            values.append(after)
        if receipt_id is not None:
            conditions.append("o.receipt_id=?")
            values.append(receipt_id)
        ordered = pending or limit is not None or after is not None
        query = "SELECT i.*,o.state FROM outbox o JOIN inbox i USING(receipt_id) WHERE " + " AND ".join(conditions)
        query += " ORDER BY o.receipt_id" if ordered else " ORDER BY i.delivered_at,i.receipt_id"
        if limit is not None:
            query += " LIMIT ?"
            values.append(limit)
        return self.decode_rows(self.db.execute(query, values))

    @retry_busy
    def ack(self, recipient, receipt_id):
        with self.db:
            self.db.execute("BEGIN IMMEDIATE")
            row = self.db.execute("SELECT payload,acked_at FROM inbox WHERE recipient=? AND receipt_id=?", (recipient, receipt_id)).fetchone()
            if not row:
                raise ValueError("收件人与回执不匹配，或尚未持久投递")
            if row["acked_at"]:
                return
            stamp = now()
            self.db.execute("UPDATE inbox SET acked_at=? WHERE recipient=? AND receipt_id=?", (stamp, recipient, receipt_id))
            self.db.execute("UPDATE outbox SET state='acked',acked_at=? WHERE recipient=? AND receipt_id=?", (stamp, recipient, receipt_id))
            payload = json.loads(row["payload"])
            for waiting in self.db.execute("SELECT selector FROM waits WHERE recipient=?", (recipient,)).fetchall():
                if self.matches(payload, json.loads(waiting[0])):
                    self.db.execute("DELETE FROM waits WHERE recipient=? AND selector=?", (recipient, waiting[0]))

    def matches(self, payload, selector):
        if any(payload.get(k) != v for k, v in selector.items() if k != "sha"):
            return False
        positive = (payload.get("action") in {"release_binary_ready", "release_full_ready"} or
                    payload.get("action") in {"terminal", "merge_gate_result"} and payload.get("conclusion") == "success")
        if positive and payload.get("tested_sha") is not None:
            return payload["tested_sha"] == selector["sha"]
        if not positive and (payload.get("sha") == selector["sha"] or payload.get("tested_sha") == selector["sha"]):
            return True
        target = self.target(payload["repo"], payload["run_id"], payload["attempt"])
        return bool(target and target["sha"] == selector["sha"] and target.get("source_head") == payload.get("source_head"))

    def waiting_recipients(self, key, action, payload, eligible):
        source = dict(payload, repo=key.repo, sha=key.sha, run_id=key.run_id, attempt=key.attempt, job=key.job, action=action)
        result = []
        for recipient in set(eligible):
            for row in self.db.execute("SELECT selector FROM waits WHERE recipient=?", (recipient,)):
                try:
                    selector = json.loads(row[0])
                    if selector.get("job") == key.job and self.matches(source, selector):
                        result.append(recipient)
                        break
                except (ValueError, TypeError, AttributeError):
                    continue
        return result

    @retry_busy
    def cancel_wait(self, recipient, selector=None):
        with self.db:
            if selector is None:
                rows = self.db.execute("SELECT selector FROM waits WHERE recipient=?", (label(recipient),)).fetchall()
                if len(rows) > 1:
                    raise ValueError("存在多项等待；取消时必须指定精确来源")
                selector = json.loads(rows[0][0]) if rows else {}
            self.db.execute("DELETE FROM waits WHERE recipient=? AND selector=?", (recipient, encode(selector)))

    @retry_busy
    def register_wait(self, recipient, selector):
        with self.db:
            self.db.execute("INSERT OR IGNORE INTO waits VALUES(?,?)", (recipient, encode(selector)))

    def wait(self, recipient, selector, timeout=300):
        label(recipient)
        required = {"repo", "sha", "run_id", "attempt", "action"}
        if not required <= set(selector) <= required | {"job"} or not 0 <= timeout <= 86400:
            raise ValueError("待续办必须精确绑定仓库、SHA、run、attempt、action 及有界超时")
        self.register_wait(recipient, selector)
        deadline = time.monotonic() + timeout
        while True:
            for receipt in self.inbox(recipient):
                if receipt["state"] == "delivered" and self.matches(receipt["payload"], selector):
                    target = self.target(receipt["payload"]["repo"], receipt["payload"]["run_id"], receipt["payload"]["attempt"])
                    return dict(receipt, binding=target) if target else receipt
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            time.sleep(min(0.25, remaining))
