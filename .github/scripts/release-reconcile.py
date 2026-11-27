#!/usr/bin/env python3
"""Recover an exact machine lease from local original evidence; never execute native work."""
import argparse
from contextlib import closing, contextmanager, ExitStack
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
from urllib.parse import unquote, urlsplit

EXTERNAL = frozenset(("6c-prepare", "6a-execute", "6c-execute", "6b-prepare", "6b-execute"))
STAGES = EXTERNAL | {"reserved", "record-inputs", "install-binary", "binary-before", "clean-snapshot", "seed-helper",
                     "agy-binding", "6c-audit", "unknown-race", "binary-after", "normalize", "complete"}
DATABASES = ("acceptance-6a-release/data", "acceptance-6b-release/data",
             "acceptance-6c-release/acceptance-000/data", "acceptance-6c-release/acceptance-200/data")
# A normal child exit and physical settlement are not a candidate success receipt.
PHASES = {
    "6c-prepare": ("driver_6c", "acceptance-6c-release", "6c-prepare.stdout", "6c-prepare.stderr"),
    "6a-execute": ("driver_6a", "acceptance-6a-release", "6a.stdout", "6a.stderr"),
    "6c-execute": ("driver_6c", "acceptance-6c-release", "6c.stdout", "6c.stderr"),
    "6b-prepare": ("prepare_6b", "acceptance-6b-release", "6b-prepare.stdout", "6b-prepare.stderr"),
    "6b-execute": ("driver_6b", "acceptance-6b-release", "6b-audit.json", "6b.stderr"),
}


def closed_ports(ports):
    for port in ports:
        result = subprocess.run(["/usr/sbin/lsof", "-nP", "-iTCP:" + str(port), "-sTCP:LISTEN"],
                                stdin=subprocess.DEVNULL, capture_output=True, timeout=15)
        check(result.returncode == 1 and not result.stdout.strip() and not result.stderr.strip(), "原始服务端口仍占用或不能验证")


@contextmanager
def sqlite_copies(finalizer, root, databases, *, scratch):
    """Hold original workspace locks; let SQLite touch only bounded private copies."""
    info = scratch.stat(follow_symlinks=False)
    check(scratch == scratch.resolve(strict=True) and stat.S_ISDIR(info.st_mode)
          and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o700, "SQLite 检查需要原始作用域外的私有暂存目录")

    def metadata(info):
        return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid, info.st_nlink,
                info.st_size, info.st_mtime_ns, info.st_ctime_ns)

    def capture(directory, destination=None):
        result = {}
        for suffix in ("", "-wal", "-shm", "-journal"):
            name = "workspace.db" + suffix
            try:
                descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
            except FileNotFoundError:
                check(bool(suffix), "缺少原始 SQLite 主文件")
                result[suffix] = None
                continue
            with os.fdopen(descriptor, "rb") as source, ExitStack() as output:
                before = os.fstat(source.fileno())
                check(stat.S_ISREG(before.st_mode) and before.st_size <= 512 << 20, "原始 SQLite 文件类型或大小不符")
                target = None
                # SHM is a derived WAL index. Let SQLite rebuild it only in the private copy.
                if destination is not None and suffix != "-shm":
                    target = output.enter_context(os.fdopen(os.open(destination / name,
                        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb"))
                digest, size = hashlib.sha256(), 0
                while chunk := source.read(1 << 20):
                    if size == 0 and suffix == "-journal":
                        # A nonzero header may reference a super-journal outside the copy; fail closed.
                        check(not any(chunk[:8]), "原始 SQLite 可能有 hot rollback journal，保留未知状态")
                    size += len(chunk)
                    check(size <= 512 << 20, "原始 SQLite 文件读取期间变大")
                    digest.update(chunk)
                    if target is not None:
                        target.write(chunk)
                check(size == before.st_size and metadata(os.fstat(source.fileno())) == metadata(before)
                      and metadata(os.stat(name, dir_fd=directory, follow_symlinks=False)) == metadata(before),
                      "复制或复核期间原始 SQLite 文件改变")
                result[suffix] = (metadata(before), digest.hexdigest())
        return result

    with ExitStack() as stack:
        scopes = []
        for path in databases:
            missing = str(path.relative_to(root)) + "：原始 DB/OS 锁或 SQLite 文件不可验证"
            try:
                database = finalizer.raw_file(root, path, 512 << 20)
                lock = finalizer.raw_file(root, path.with_name("workspace.lock"))
                check(database == path and lock == path.with_name("workspace.lock"), missing)
                stream = stack.enter_context(os.fdopen(os.open(lock, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb"))
                lock_info = os.fstat(stream.fileno())
                check(stat.S_ISREG(lock_info.st_mode) and lock_info.st_size <= 32 << 20, missing)
                fcntl.flock(stream, fcntl.LOCK_SH | fcntl.LOCK_NB)
                directory = os.open(database.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
                stack.callback(os.close, directory)
                scopes.append((database, lock, stream, metadata(lock_info), directory, os.fstat(directory)))
            except (ValueError, OSError):
                raise Unproven(missing) from None
        temporary = Path(stack.enter_context(tempfile.TemporaryDirectory(dir=scratch, prefix=".settlement-sqlite-")))
        copies, snapshots = {}, {}
        for index, (database, _, _, _, directory, _) in enumerate(scopes):
            destination = temporary / str(index)
            destination.mkdir(mode=0o700)
            snapshots[database] = capture(directory, destination)
            copies[database] = destination / "workspace.db"

        connect = sqlite3.connect
        def copied_connect(database, *args, **kwargs):
            name = os.fsdecode(database)
            if name.startswith("file:"):
                uri = urlsplit(name)
                check(not uri.netloc and uri.query in ("", "mode=ro"), "原始 SQLite 连接参数不符")
                path = Path(unquote(uri.path)).resolve(strict=True)
                check(path in copies, "SQLite 检查越过本次已锁定数据库")
                target = copies[path].as_uri() + ("?" + uri.query if uri.query else "")
            else:
                path = Path(name).resolve(strict=True)
                check(path in copies, "SQLite 检查越过本次已锁定数据库")
                target = copies[path]
            return connect(target, *args, **kwargs)

        # Original auditors share sqlite3 in this synchronous CLI. Preserve their original paths and receipts.
        sqlite3.connect = copied_connect
        try:
            yield {database: snapshot[""][1] for database, snapshot in snapshots.items()}
        finally:
            sqlite3.connect = connect
            for database, lock, stream, lock_info, directory, directory_info in scopes:
                current = database.parent.stat(follow_symlinks=False)
                check((current.st_dev, current.st_ino, current.st_mode) ==
                      (directory_info.st_dev, directory_info.st_ino, directory_info.st_mode)
                      and metadata(os.fstat(stream.fileno())) == lock_info
                      and metadata(lock.stat(follow_symlinks=False)) == lock_info
                      and capture(directory) == snapshots[database], "物理复核期间原始 SQLite 字节或 DB/OS 锁身份改变")


def settled_database(machine, finalizer, original, root, *, prepare_only):
    """Inspect original native termination, never the candidate's acceptance assertions."""
    database = finalizer.raw_file(root, root / "data/workspace.db", 512 << 20)
    logs = sorted(root.glob("workspace*.stdout.log"))
    check(bool(logs), "缺少原始服务启动及端口证据")
    ports = []
    for path in logs:
        raw = finalizer.raw_file(root, path).read_text()
        found = re.findall(r"本地 HTTP 进程已启动： http://127\.0\.0\.1:([0-9]+)\b", raw)
        check(len(found) == 1 and 1024 <= int(found[0]) <= 65535
              and (not prepare_only or "执行已禁用" in raw), "原始服务启动作用域或执行模式不符")
        ports.append(int(found[0]))
    closed_ports(ports)
    scratch = root.parent.parent if root.parent.name == "acceptance-6c-release" else root.parent
    with sqlite_copies(finalizer, root, [database], scratch=scratch) as hashes, \
         closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        check([tuple(row) for row in db.execute("PRAGMA quick_check")] == [("ok",)]
              and not db.execute("PRAGMA foreign_key_check").fetchall()
              and db.execute("SELECT COUNT(*) FROM resource_claims").fetchone()[0] == 0, "原始数据库不健康或资源 claim 未结清")
        turns = db.execute("SELECT * FROM turns").fetchmany(1001)
        deliveries = db.execute("SELECT * FROM deliveries").fetchmany(1001)
        check(len(turns) <= 1000 and len(deliveries) == len(turns)
              and (not turns if prepare_only else bool(turns)), "原始数据库存在未接纳投递、prepare 越界执行或缺少原生终态证据")
        proofs = []
        for turn in turns:
            state, session, raw_receipt = turn["state"], turn["native_session_id"], turn["receipt_json"]
            check(state in ("succeeded", "failed", "aborted") and turn["accepted_at"]
                  and isinstance(session, str) and bool(session) and isinstance(raw_receipt, str) and len(raw_receipt) <= 1 << 20,
                  "原始 Turn 仍活跃、取消中、unknown 或缺少有界终态回执")
            receipt = json.loads(raw_receipt, object_pairs_hook=unique)
            check(isinstance(receipt, dict) and session not in (turn["id"], "fixture", "native-session"), "原生回执或会话身份不符")
            machine.terminal(receipt, session, {state})
            check(type(receipt.get("exit_code")) is int and receipt.get("logs_truncated") is False, "原生 child 退出或输出证据不完整")
            delivery = [row for row in deliveries if row["id"] == turn["delivery_id"]]
            check(len(delivery) == 1 and delivery[0]["state"] == "accepted" and delivery[0]["turn_id"] == turn["id"]
                  and delivery[0]["group_id"] == turn["group_id"] and delivery[0]["recipient_id"] == turn["member_id"]
                  and delivery[0]["conversation_id"] == turn["conversation_id"], "原始 delivery、群组、成员或 Turn 身份不符")
            runtime = root / "data/runtime" / turn["id"]
            for field, name in (("stdout_path", "stdout.jsonl"), ("stderr_path", "stderr.log")):
                check(finalizer.raw_file(root, Path(receipt[field])) == (runtime / name).resolve(strict=True), "原生输出不属于本次 Turn")
            transcript = original["native_transcript"](root / "data", dict(turn), receipt)
            check(bool(transcript), "缺少完整原生 transcript")
            path = runtime / "gemini/aw-app/brain" / session / ".system_generated/logs/transcript_full.jsonl"
            path = finalizer.raw_file(root, path)
            protocol = runtime / "stdout.jsonl"
            proofs.append(machine.native_proof(root, turn["id"], session, protocol, path,
                                               machine.core.file_hash(protocol), machine.core.file_hash(path)))
        if root.name == "acceptance-6a-release":
            report = json.loads(finalizer.raw_file(root, root / "6a-receipt.json").read_bytes(), object_pairs_hook=unique)
            entries = [report[name] for name in ("positive_turn", "negative_turn") if name in report]
            check(report.get("loopback_closed") is True and report["workspace_shutdown"].get("graceful") is True
                  and report["workspace_shutdown"].get("exit_code") == 0
                  and {entry["id"] for entry in entries} == {turn["id"] for turn in turns}, "6A 原始服务退出或 Turn 清单不符")
            for entry in entries:
                turn = next(turn for turn in turns if turn["id"] == entry["id"])
                check(all(entry[key] == turn[key] for key in entry if key in turn.keys())
                      and entry["native_session_id"] == turn["native_session_id"]
                      and entry["execution_receipt"] == original["redact_receipt"](json.loads(turn["receipt_json"])), "6A 原始终态回执与实际数据不符")
    return {"sha256": hashes[database], "ports": ports, "proofs": proofs}


class Unproven(ValueError):
    """Only fixed, sanitized descriptions of missing original proof belong here."""


class Busy(ValueError):
    pass


def check(value, message):
    if not value:
        raise Unproven(message)


def identity(repo, run_id, attempt):
    check(isinstance(repo, str) and re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo)
          and all(type(value) is int and 0 < value < 2 ** 63 for value in (run_id, attempt)), "仓库、run 或 attempt 非法")
    return {"repo": repo, "run_id": run_id, "run_attempt": attempt}


def exact_owner(value, expected):
    return isinstance(value, dict) and set(value) == set(expected) and all(
        type(value[key]) is type(item) and value[key] == item for key, item in expected.items())


def unique(pairs):
    value = dict(pairs)
    check(len(value) == len(pairs), "本机记录有重复 JSON 字段")
    return value


def private_file(descriptor):
    info = os.fstat(descriptor)
    check(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o600
          and info.st_nlink == 1, "本机记录或执行锁的所有者、权限、类型不符")


def read(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as stream:
        private_file(stream.fileno())
        data = stream.read((1 << 20) + 1)
    check(len(data) <= 1 << 20, "本机记录过大")
    return json.loads(data, object_pairs_hook=unique)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def save(path, value, *, replace=False):
    # Publish only complete, fsynced bytes. A failed publication cannot authorize a command.
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".lease-write-", delete=False) as stream:
        temporary = Path(stream.name)
        stream.write((json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode())
        stream.flush()
        os.fsync(stream.fileno())
    try:
        if replace:
            os.replace(temporary, path)
        else:
            try:
                os.link(temporary, path, follow_symlinks=False)
            except FileExistsError:
                check(read(path) == value, "已有本机诊断或结清记录冲突")
    finally:
        temporary.unlink(missing_ok=True)
    sync_directory(path.parent)


@contextmanager
def flock(path, *, create=False, wait=False):
    descriptor = os.open(path, os.O_NOFOLLOW | (os.O_RDWR | os.O_CREAT if create else os.O_RDONLY), 0o600)
    try:
        private_file(descriptor)
        os.set_inheritable(descriptor, False)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
        except BlockingIOError:
            raise Busy("本机登记或原始 worker 仍运行") from None
        yield descriptor
    finally:
        os.close(descriptor)


@contextmanager
def mutex(machine, *, wait=False):
    check(machine.STATE_ROOT == machine.host.registry_root(), "不能切换固定用户机器登记目录")
    machine.private_directory(machine.STATE_ROOT)
    with flock(machine.STATE_ROOT / "lease-mutation.lock", create=True, wait=wait):
        yield


def registered(machine, owner):
    permanent = machine.claim_path(machine.STATE_ROOT, owner["repo"], owner["run_id"], owner["run_attempt"])
    machine.private_directory(permanent)
    value = read(permanent / "claim.json")
    check(isinstance(value, dict) and set(value) == set(owner) | {"claim"}
          and all(type(value[key]) is type(item) and value[key] == item for key, item in owner.items()), "固定认领身份不符")
    claim = Path(value["claim"])
    check(claim.is_absolute() and claim == claim.resolve(strict=True)
          and claim == machine.claim_path(claim.parent.parent, owner["repo"], owner["run_id"], owner["run_attempt"]), "固定认领路径不符")
    machine.authorized_host(claim)
    check(exact_owner(read(claim / "reserved.json"), owner), "原始 reserved.json 认领身份不符")
    return claim, permanent


def sync_reservation(machine, claim):
    bound, permanent = registered(machine, read(claim / "reserved.json"))
    check(bound == claim, "不能同步其他认领")
    active = machine.STATE_ROOT / "machine-active"
    for path in (claim / "reserved.json", permanent / "claim.json", active / "owner.json"):
        with os.fdopen(os.open(path, os.O_RDWR | os.O_NOFOLLOW), "rb") as stream:
            private_file(stream.fileno())
            os.fsync(stream.fileno())
    for path in (claim, claim.parent, claim.parent.parent, permanent, permanent.parent, active, machine.STATE_ROOT):
        sync_directory(path)


class Lease:
    def __init__(self, machine, owner, claim, args, *, intent=None, worker=None):
        self.machine, self.owner, self.claim, self.args = machine, owner, claim, args
        bound, self.permanent = registered(machine, owner)
        check(bound == claim, "worker 不属于固定认领")
        self.active = machine.STATE_ROOT / "machine-active"
        self.archive = self.permanent / "released-machine-active"
        prep_path = claim / "lease-prep.json"
        if intent is not None:
            machine.verify_control_files(intent)
            machine.private_directory(self.active)
            check(exact_owner(read(self.active / "owner.json"), owner), "不能记录其他 owner 的准备阶段")
            info, worker_info = self.active.stat(), os.fstat(worker)
            save(prep_path, owner | {"schema": 1, "kind": "release-native-prep", "claim": str(claim),
                                    "active": [info.st_dev, info.st_ino], "worker": [worker_info.st_dev, worker_info.st_ino], "intent": intent})
        self.prep = read(prep_path)
        check(isinstance(self.prep, dict) and set(self.prep) == set(owner) | {"schema", "kind", "claim", "active", "worker", "intent"}
              and type(self.prep["schema"]) is int and self.prep["schema"] == 1 and self.prep["kind"] == "release-native-prep"
              and all(type(self.prep[key]) is type(item) and self.prep[key] == item for key, item in owner.items())
              and self.prep["claim"] == str(claim), "lease-prep.json 身份或格式不符")
        worker_info = (claim / "worker.lock").stat() if worker is None else os.fstat(worker)
        check(self.prep["worker"] == [worker_info.st_dev, worker_info.st_ino], "worker.lock inode 改变；不能推断原始 worker 已死亡")
        self.intent = self.prep["intent"]
        check(isinstance(self.intent, dict) and all(type(self.intent.get(key)) is type(item) and self.intent.get(key) == item
                                                   for key, item in owner.items()), "准备阶段的控制 intent 身份不符")
        machine.verify_control_files(self.intent)
        self.base = owner | {"schema": 1, "prep_sha256": machine.core.file_hash(prep_path)}
        self.settled = False

    def event(self, name):
        value = read(self.claim / name)
        check(isinstance(value, dict) and all(type(value.get(key)) is type(item) and value.get(key) == item
                                             for key, item in self.base.items()), "阶段或结清记录未绑定原始准备身份")
        return value

    def stage(self, name):
        check(name in STAGES and not self.settled, "阶段非法或本次已结清，禁止重放")
        save(self.claim / "lease-stage.json", self.base | {"stage": name}, replace=True)

    def stage_name(self):
        name = self.event("lease-stage.json")["stage"] if (self.claim / "lease-stage.json").exists() else "reserved"
        check(name in STAGES, "原始准备阶段记录不符")
        return name

    def external(self, phase, *, inputs=None):
        check(phase in EXTERNAL and not self.settled, "外部阶段非法或本次已结清")
        path = self.claim / ("lease-external-" + phase + ".json")
        check(not path.exists(), "同一外部阶段禁止重放")
        self.stage(phase)
        detail = {} if inputs is None else {"inputs": inputs, "started_ns": time.monotonic_ns()}
        save(path, self.base | {"stage": phase} | detail)

    def native_inputs(self):
        machine = self.machine
        machine.verify_control_files(self.intent)
        check(read(self.claim / "intent.json") == self.intent, "原始 intent 与认领不符")
        paths, _, bundle = machine.source_manifest(self.args.harness_root)
        check(read(self.claim / "source-manifest.json") == bundle, "原始驱动或支持源码改变")
        source, binary = self.claim / "source", self.claim / "workspace"
        check(machine.core.git(source, "rev-parse", "HEAD") == self.intent["sha"]
              and machine.core.git(source, "rev-parse", "HEAD^{tree}") == self.intent["tree"]
              and not machine.core.git(source, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all"), "原始源码快照改变")
        before = read(self.claim / "binary-before.json")
        actual = machine.evidence.binary_identity(self.args.go, binary, self.intent, machine.native_environment())
        check(actual == before, "原始执行二进制或构建身份改变")
        return paths, {"artifact_sha256": actual["sha256"], "source_manifest_sha256": machine.core.file_hash(self.claim / "source-manifest.json")}

    def snapshot(self, phase):
        _, relative, stdout, stderr = PHASES[phase]
        root = self.claim / relative
        check(root.is_dir() and not root.is_symlink(), "外部阶段缺少原始数据作用域")
        files = {}
        for path in sorted(root.rglob("*")):
            check(not path.is_symlink() and (path.is_file() or path.is_dir()), "原始阶段包含未知文件类型或链接")
            if path.is_file():
                check(path.stat().st_size <= 512 << 20 and len(files) < 10000, "原始阶段文件过大或过多")
                files[str(path.relative_to(self.claim))] = self.machine.core.file_hash(path)
        for name in (stdout, stderr):
            path = self.claim / name
            check(path.is_file() and not path.is_symlink(), "缺少原始 child 输出")
            files[name] = self.machine.core.file_hash(path)
        databases = {}
        for name in files:
            if Path(name).name == "workspace.db":
                check(str(Path(name).parent) in DATABASES, "数据库不属于本次固定阶段")
                path, lock = self.claim / name, self.claim / Path(name).parent / "workspace.lock"
                info, lock_info = path.stat(), lock.stat()
                databases[str(Path(name).parent)] = [info.st_dev, info.st_ino, lock_info.st_dev, lock_info.st_ino]
        check(bool(databases), "缺少已启动阶段的原始数据库，不能推断外部终止")
        return {"files": files, "databases": databases}

    def run(self, phase, command, stdout, stderr):
        paths, inputs = self.native_inputs()
        driver, _, out, err = PHASES[phase]
        check(Path(command[0]).resolve(strict=True) == paths[driver].resolve(strict=True)
              and stdout == self.claim / out and stderr == self.claim / err, "阶段驱动或输出未绑定本次认领")
        self.external(phase, inputs=inputs)
        try:
            self.machine.driver(command, stdout, stderr)
        except subprocess.CalledProcessError as error:
            # A signal/timeout is not evidence that Python cleanup or native reaping ran.
            if type(error.returncode) is int and error.returncode >= 0:
                try:
                    self.finish(phase, error.returncode, inputs)
                except (ValueError, KeyError, OSError, TypeError, sqlite3.Error, subprocess.SubprocessError):
                    pass  # Preserve the child failure even if its physical proof is incomplete.
            raise
        else:
            self.finish(phase, 0, inputs)

    def finish(self, phase, code, inputs):
        start = self.claim / ("lease-external-" + phase + ".json")
        end = self.base | {"stage": phase, "start_sha256": self.machine.core.file_hash(start), "inputs": None,
                           "exit_code": code, "direct_child_reaped": True, "ended_ns": time.monotonic_ns(), "scope": None}
        try:
            end["inputs"] = self.native_inputs()[1]
            check(end["inputs"] == inputs, "外部执行期间原始输入改变")
            end["scope"] = self.snapshot(phase)
        finally:
            # Keep actual child completion even when physical settlement cannot be proved.
            save(self.claim / ("lease-end-" + phase + ".json"), end)

    def partial_proof(self):
        phases = self.phases()
        for phase in phases:
            check((self.claim / ("lease-end-" + phase + ".json")).is_file(),
                  "complete.json 或 lease-end-" + phase + ".json：缺少原始 child 结束确认，外部终态未知")
        check({path.name.removeprefix("lease-end-").removesuffix(".json") for path in self.claim.glob("lease-end-*.json")} == phases,
              "阶段结束记录与实际开始清单不符")
        paths, inputs = self.native_inputs()
        records, ends, latest = {}, {}, {}
        for phase in PHASES:
            if phase not in phases:
                continue
            start = self.event("lease-external-" + phase + ".json")
            path = self.claim / ("lease-end-" + phase + ".json")
            end = self.event(path.name)
            check(start.get("stage") == end.get("stage") == phase and start.get("inputs") == end.get("inputs") == inputs
                  and end.get("start_sha256") == self.machine.core.file_hash(self.claim / ("lease-external-" + phase + ".json"))
                  and type(start.get("started_ns")) is int and type(end.get("ended_ns")) is int
                  and 0 < start["started_ns"] <= end["ended_ns"]
                  and type(end.get("exit_code")) is int and end["exit_code"] >= 0 and end.get("direct_child_reaped") is True,
                  "阶段开始、结束、输入或 child 终态未绑定原始认领")
            records[phase], ends[phase] = (start, end), self.machine.core.file_hash(path)
            latest[PHASES[phase][1]] = phase
        check("6c-prepare" in phases, "缺少首个外部阶段的原始记录")
        for phase in phases - {"6c-prepare"}:
            prerequisites = {"6c-prepare"}
            if phase.startswith("6b-"):
                prerequisites |= {"6a-execute", "6c-execute"}
            if phase == "6b-execute":
                prerequisites.add("6b-prepare")
            check(prerequisites <= phases and all(records[item][1]["exit_code"] == 0
                  and records[item][1]["ended_ns"] <= records[phase][0]["started_ns"] for item in prerequisites), "外部阶段前置完成或时序不符")
        check(all(not (self.claim / relative).exists() or relative in latest for _, relative, _, _ in PHASES.values()),
              "未开始阶段存在未知原始工作，禁止忽略其作用域")
        finalizer = self.machine.core.load_script("release-finalize")
        databases = {}
        with finalizer.originals(paths, "6a") as original:
            for relative, phase in latest.items():
                scope = records[phase][1]["scope"]
                check(self.snapshot(phase) == scope, "原始阶段结束后数据、协议、transcript、输出或 DB/OS 锁身份改变")
                expected = {name for name in DATABASES if name.startswith(relative + "/")}
                actual = set(scope["databases"])
                check(actual <= expected and (phase.endswith("prepare") and records[phase][1]["exit_code"] != 0 or actual == expected),
                      "外部阶段数据库作用域不完整")
                for name in sorted(actual):
                    databases[name] = settled_database(self.machine, finalizer, original, (self.claim / name).parent,
                                                       prepare_only=phase.endswith("prepare"))
                check(self.snapshot(phase) == scope, "物理复核期间原始阶段字节改变")
        check(self.native_inputs()[1] == inputs, "物理复核期间原始输入改变")
        return {"external_phases": sorted(phases), "ends": ends, "databases": databases}

    def settle_partial(self):
        with mutex(self.machine, wait=True):
            self.settle_locked("native-settled", self.partial_proof())

    def phases(self):
        result = set()
        for path in self.claim.glob("lease-external-*.json"):
            phase = self.event(path.name).get("stage")
            check(phase in EXTERNAL and path.name == "lease-external-" + phase + ".json", "外部阶段标记不完整或来源未知")
            result.add(phase)
        return result

    def archived(self):
        if not self.archive.exists():
            return False
        self.machine.private_directory(self.archive)
        check(exact_owner(read(self.archive / "owner.json"), self.owner)
              and [self.archive.stat().st_dev, self.archive.stat().st_ino] == self.prep["active"], "已归档机器锁身份不符")
        self.event("lease-settlement.json")
        return True

    def settle_locked(self, kind, proof):
        # The registry mutex excludes every new reservation during verify-owner + rename.
        self.machine.private_directory(self.active)
        check(exact_owner(read(self.active / "owner.json"), self.owner)
              and [self.active.stat().st_dev, self.active.stat().st_ino] == self.prep["active"], "不能释放其他或未知 owner 的机器锁")
        path = self.claim / "lease-settlement.json"
        if path.exists():
            previous = self.event(path.name)
            prep_kinds = {"pre-native-failed", "pre-native-stopped", "pre-native-dead"}
            check((previous.get("kind") == kind or previous.get("kind") in prep_kinds and kind in prep_kinds)
                  and previous.get("stage") == self.stage_name() and previous.get("proof") == proof, "已有结清事实与本次原始证据不符")
        else:
            save(path, self.base | {"kind": kind, "stage": self.stage_name(), "proof": proof})
        check(not self.archive.exists(), "不能覆盖已有机器锁归档")
        os.rename(self.active, self.archive)
        # Keep the exact old owner directory, avoiding unlink-owner/rmdir crash windows.
        sync_directory(self.permanent)
        sync_directory(self.machine.STATE_ROOT)
        self.settled = True

    def complete_proof(self):
        check((self.claim / "complete.json").is_file(), "complete.json：缺少完整原始 native 完成回执，不能推断外部终止")
        check(self.phases() == EXTERNAL, "外部阶段登记不完整，无法绑定完整 native 终态")
        receipt = read(self.claim / "receipt.json")
        digest = self.machine.core.file_hash(self.claim / "receipt.json")
        check(read(self.claim / "complete.json") == {"receipt_sha256": digest}, "complete.json：原始回执摘要不符")
        finalizer = self.machine.core.load_script("release-finalize")
        paths = [self.claim / relative / "workspace.db" for relative in DATABASES]
        databases = {}
        with sqlite_copies(finalizer, self.claim, paths, scratch=self.claim) as hashes:
            try:
                actual = finalizer.verify_local(self.args, None, self.intent, self.claim,
                    {"artifact_sha256": receipt["artifact_sha256"]}, receipt["target_artifact"])
            except (ValueError, KeyError, OSError, subprocess.SubprocessError, sqlite3.Error):
                raise Unproven("finalizer.verify_local：缺少通过原始源码、二进制、6A/B/C 协议与审计、unknown guard 的独立复核") from None
            check(isinstance(actual, dict) and actual == receipt, "原始 finalizer 没有返回匹配的完整 native 回执")
            for relative, database in zip(DATABASES, paths):
                missing = relative + "/workspace.db：必须无活跃或 unknown Turn、无资源 claim，且原生终态已结清"
                try:
                    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
                        db.execute("PRAGMA query_only=ON")
                        db.execute("BEGIN")
                        check(db.execute("PRAGMA quick_check").fetchall() == [("ok",)]
                              and not db.execute("PRAGMA foreign_key_check").fetchall()
                              and db.execute("SELECT COUNT(*) FROM resource_claims").fetchone()[0] == 0, missing)
                        turns = db.execute("SELECT state,native_session_id,receipt_json FROM turns").fetchmany(1001)
                        check(0 < len(turns) <= 1000, missing)
                        for state, session, raw in turns:
                            check(state in ("succeeded", "failed", "aborted") and isinstance(session, str) and bool(session)
                                  and isinstance(raw, str) and len(raw) <= 1 << 20, missing)
                            terminal = json.loads(raw, object_pairs_hook=unique)
                            check(isinstance(terminal, dict), missing)
                            self.machine.terminal(terminal, session, {state})
                        databases[relative] = hashes[database]
                except (ValueError, KeyError, OSError, sqlite3.Error):
                    raise Unproven(missing) from None
        check(self.machine.core.file_hash(self.claim / "receipt.json") == digest, "复核期间原始完成回执改变")
        return {"receipt_sha256": digest, "databases": databases}

    def complete(self):
        with mutex(self.machine, wait=True):
            self.settle_locked("native-complete", self.complete_proof())


@contextmanager
def execution(machine, args, intent):
    """The long-lived worker flock is never inherited by drivers or services."""
    machine = SimpleNamespace(**machine) if isinstance(machine, dict) else machine
    owner = identity(intent["repo"], args.run_id, args.attempt)
    check(all(type(intent.get(key)) is type(item) and intent.get(key) == item for key, item in owner.items()), "准备 intent 与本次参数不符")
    machine.verify_control_files(intent)
    machine.authorized_host(args.work_dir)
    with ExitStack() as stack:
        with mutex(machine):
            claim = machine.reserve(args.work_dir, owner["repo"], owner["run_id"], owner["run_attempt"])
            sync_reservation(machine, claim)
            worker = stack.enter_context(flock(claim / "worker.lock", create=True))
            os.fsync(worker)
            lease = Lease(machine, owner, claim, args, intent=intent, worker=worker)
            lease.stage("record-inputs")
        try:
            yield lease
        except Exception as error:
            external = lease.phases()
            save(claim / "lease-failure.json", lease.base | {"stage": lease.stage_name(),
                 "category": "external-unknown" if external else "pre-native", "exception": type(error).__name__})
            if not external:
                with mutex(machine, wait=True):
                    lease.settle_locked("pre-native-failed", {"external_phases": []})
            else:
                try:
                    lease.settle_partial()
                except (ValueError, KeyError, OSError, TypeError, sqlite3.Error, subprocess.SubprocessError):
                    pass  # Keep both the original failure and the unknown machine lock.
            raise
        else:
            if not lease.settled and not lease.phases():
                with mutex(machine, wait=True):
                    lease.settle_locked("pre-native-stopped", {"external_phases": []})


def reconcile(machine, repo, run_id, attempt, *, go):
    owner = identity(repo, run_id, attempt)
    try:
        check(machine.STATE_ROOT == machine.host.registry_root(), "不能切换固定用户机器登记目录")
        claim, _ = registered(machine, owner)
        if (claim / "worker.lock").is_file():
            with flock(claim / "worker.lock"):
                pass  # A live initializing worker is busy even before prep publication.
        check((claim / "lease-prep.json").is_file() and (claim / "worker.lock").is_file(),
              "lease-prep.json/worker.lock 缺失：reserve 与 worker 附着间的崩溃窗口不明确，禁止释放")
        args = SimpleNamespace(work_dir=claim.parent.parent, run_id=run_id, attempt=attempt, go=go, harness_root=None)
        # Authenticate immutable control bytes before opening any writable registry file.
        Lease(machine, owner, claim, args)
        with mutex(machine):
            with flock(claim / "worker.lock") as worker:
                lease = Lease(machine, owner, claim, args, worker=worker)
                if not lease.archived():
                    phases = lease.phases()
                    if phases:
                        if (claim / "complete.json").exists():
                            lease.settle_locked("native-complete", lease.complete_proof())
                        else:
                            lease.settle_locked("native-settled", lease.partial_proof())
                    else:
                        lease.settle_locked("pre-native-dead", {"external_phases": []})
        return owner | {"status": "settled"}
    except Busy:
        return owner | {"status": "busy"}
    except Unproven as error:
        return owner | {"status": "unknown", "missing": [str(error)]}
    except (ValueError, KeyError, OSError, TypeError, sqlite3.Error, subprocess.SubprocessError):
        return owner | {"status": "unknown", "missing": ["固定 registry/claim/owner 或原始私有记录缺失、被更改或不可验证"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("reconcile",))
    parser.add_argument("--repo", required=True)
    parser.add_argument("--run-id", type=int, required=True)
    parser.add_argument("--attempt", type=int, required=True)
    parser.add_argument("--go", type=Path, default=Path(shutil.which("go") or "go"))
    args = parser.parse_args()
    check(sys.flags.optimize == 0, "原始验收依赖 assertions，禁止优化模式")
    os.umask(0o077)
    spec = importlib.util.spec_from_file_location("release_core", Path(__file__).with_name("release-core.py"))
    core = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(core)
    result = reconcile(core.load_script("release-machine"), args.repo, args.run_id, args.attempt, go=args.go)
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return {"settled": 0, "busy": 75, "unknown": 1}[result["status"]]


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, TypeError, sqlite3.Error, subprocess.SubprocessError):
        print("机器认领未结清；原始身份或终态不可验证，保留未知状态，禁止强制解锁。", file=sys.stderr)
        sys.exit(1)
